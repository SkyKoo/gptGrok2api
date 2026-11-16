import asyncio
import os
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import oauth_device as flow


class DeviceURLTests(unittest.TestCase):
    def test_exception_categories_are_fixed_and_do_not_return_secret_messages(self):
        browser_error = type("Error", (Exception,), {"__module__":"playwright._impl._errors"})
        for error, expected in (
                (TypeError("SECRET"), "type_error"), (AttributeError("SECRET"), "attribute_error"),
                (ValueError("SECRET"), "value_error"), (RuntimeError("strict mode violation: SECRET"), "strict_mode"),
                (browser_error("SECRET"), "browser_error"), (RuntimeError("SECRET"), "unknown")):
            with self.subTest(expected=expected):
                self.assertEqual(flow._exception_type(error), expected)
                self.assertNotIn("SECRET", str(flow.FlowDiagnostics().fields(error)))

    def test_diagnostic_boundary_rejects_dynamic_text_and_preserves_submission(self):
        secret = "https://accounts.x.ai/oauth2/device?user_code=SECRET sso=COOKIE selected@example.test"
        value = {key: secret for key in ("status", "code", "stage", "detail", "operation", "exception_type",
                                         "page_path", "network_error_codes", "consent_state", "elapsed_ms")}
        value.update(consent_submitted=True, arbitrary_secret=secret, network_failures=-10)
        clean = flow.safe_device_result(value)
        self.assertNotIn("SECRET", str(clean))
        self.assertNotIn("COOKIE", str(clean))
        self.assertNotIn("selected@example.test", str(clean))
        self.assertEqual(clean["consent_state"], "attempted")
        self.assertTrue(clean["consent_submitted"])
        self.assertEqual(clean["code"], "oauth_worker_result_unknown")
        self.assertEqual(clean["network_failures"], 0)
        self.assertEqual(flow.safe_device_result({})["consent_state"], "unknown")
        self.assertEqual(flow.safe_task_id(secret), "")
        self.assertEqual(flow.safe_task_id("task_20261116090339.000000000"), "task_20261116090339.000000000")

    def test_only_the_exact_device_origin_code_and_path_are_allowed(self):
        flow.validate_device_url("https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH", "ABCD-EFGH")
        bad = [
            "https://accounts.x.ai.evil.invalid/oauth2/device?user_code=ABCD-EFGH",
            "http://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH",
            "https://evil@accounts.x.ai/oauth2/device?user_code=ABCD-EFGH",
            "https://accounts.x.ai:444/oauth2/device?user_code=ABCD-EFGH",
            "https://accounts.x.ai/oauth2/device?user_code=OTHER-ONE",
            "https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH&user_code=ABCD-EFGH",
            "https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH&redirect=",
            "https://accounts.x.ai/sign-up?user_code=ABCD-EFGH",
        ]
        for url in bad:
            with self.subTest(url=url), self.assertRaises(ValueError):
                flow.validate_device_url(url, "ABCD-EFGH")


class ConfirmationTests(unittest.IsolatedAsyncioTestCase):
    def browser_for(self, page):
        page.goto = AsyncMock()
        page.main_frame = object()
        context = SimpleNamespace(add_cookies=AsyncMock(), new_page=AsyncMock(return_value=page), route=AsyncMock())
        return SimpleNamespace(new_context=AsyncMock(return_value=context), close=AsyncMock())

    async def authorize(self, browser, **kwargs):
        return await flow.authorize_device(
            verification_uri_complete="https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH",
            user_code="ABCD-EFGH", sso="selected-secret", expected_email="selected@example.test", browser=browser, **kwargs)

    def page(self, path="/oauth2/device/consent", text="Allow Grok Build access selected@example.test"):
        button = SimpleNamespace(is_visible=AsyncMock(return_value=True), is_enabled=AsyncMock(return_value=True), click=AsyncMock())
        buttons = SimpleNamespace(count=AsyncMock(return_value=1), nth=Mock(return_value=button))
        body = SimpleNamespace(inner_text=AsyncMock(return_value=text))
        field = SimpleNamespace(count=AsyncMock(return_value=1), input_value=AsyncMock(return_value="ABCD-EFGH"))
        form = SimpleNamespace(count=AsyncMock(return_value=1), get_attribute=AsyncMock(return_value="POST"), locator=Mock(return_value=field), get_by_role=Mock(return_value=buttons))
        page = SimpleNamespace(url="https://accounts.x.ai" + path, title=AsyncMock(return_value="Grok"), get_by_role=Mock(return_value=buttons))
        page.locator = Mock(side_effect=lambda value: body if value == "body" else form)
        return page, button, form, field

    async def confirm(self, page):
        return await flow.confirm_on_page(page, user_code="ABCD-EFGH", expected_email="selected@example.test", timeout_s=0.01)

    async def test_login_mfa_untrusted_and_wrong_account_never_approve(self):
        cases = [("/sign-in", "", "oauth_login_required"), ("/mfa/verify", "", "oauth_verification_required"), ("/oauth2/device/consent", "Allow Grok Build other@example.test", "oauth_identity_mismatch"), ("/oauth2/device/consent", "Approve another app selected@example.test", "oauth_consent_unrecognized")]
        for path, text, code in cases:
            with self.subTest(path=path, code=code):
                page, button, _, _ = self.page(path, text)
                outcome = await self.confirm(page)
                self.assertEqual(outcome["code"], code)
                self.assertIn(outcome["stage"], {"page_navigation", "page_observation", "consent_page"})
                button.click.assert_not_awaited()
        page, button, _, _ = self.page()
        page.url = "https://accounts.x.ai.evil.invalid/oauth2/device/consent"
        outcome = await self.confirm(page)
        self.assertEqual(outcome["status"], "failed")
        self.assertEqual(outcome["detail"], "untrusted_destination")
        button.click.assert_not_awaited()

    async def test_wrong_code_and_wrong_form_method_are_rejected(self):
        for method, code in [("GET", "ABCD-EFGH"), ("POST", "WRNG-CODE")]:
            page, button, form, field = self.page("/oauth2/device")
            form.get_attribute.return_value = method
            field.input_value.return_value = code
            self.assertEqual((await self.confirm(page))["status"], "failed")
            button.click.assert_not_awaited()

    async def test_consent_timeout_is_unknown_and_never_replayed(self):
        page, button, _, _ = self.page()
        button.click.side_effect = TimeoutError("sensitive URL or cookie must not escape")
        outcome = await self.confirm(page)
        self.assertEqual(outcome["status"], "uncertain")
        self.assertEqual(outcome["code"], "oauth_consent_click_timeout")
        self.assertEqual(outcome["stage"], "consent_submit")
        self.assertTrue(outcome["consent_submitted"])
        self.assertNotIn("sensitive URL", str(outcome))
        button.click.assert_awaited_once()

    async def test_accept_once_wait_for_done_and_never_infer_success_from_click(self):
        page, button, _, _ = self.page()
        async def finish(**kwargs): page.url = "https://accounts.x.ai/oauth2/device/done"
        button.click.side_effect = finish
        with patch.object(flow.asyncio, "sleep", AsyncMock()):
            outcome = await self.confirm(page)
            self.assertEqual(outcome["status"], "approved")
            self.assertEqual(outcome["stage"], "consent_callback")
            self.assertEqual(outcome["detail"], "callback_received")
            self.assertEqual(outcome["page_path"], "/oauth2/device/done")
            self.assertNotIn("selected@example.test", str(outcome))
        button.click.assert_awaited_once()
        page, button, _, _ = self.page()
        with patch.object(flow.asyncio, "sleep", AsyncMock()):
            outcome = await self.confirm(page)
            self.assertEqual(outcome["status"], "uncertain")
            self.assertEqual(outcome["code"], "oauth_callback_timeout")
        button.click.assert_awaited_once()

    async def test_isolated_host_cookies_and_cleanup_on_failure(self):
        page = SimpleNamespace(main_frame=object(), url="https://accounts.x.ai/oauth2/device", goto=AsyncMock(side_effect=RuntimeError("secret")))
        context = SimpleNamespace(add_cookies=AsyncMock(), new_page=AsyncMock(return_value=page), route=AsyncMock(), close=AsyncMock())
        browser = SimpleNamespace(new_context=AsyncMock(return_value=context), close=AsyncMock())
        module = SimpleNamespace(launch_async=AsyncMock(return_value=browser))
        with patch.dict(sys.modules, {"cloakbrowser": module}), patch.dict(os.environ, {"TURNSTILE_PROXY":"http://signup-only.example.test:8080"}):
            outcome = await flow.authorize_device(verification_uri_complete="https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH", user_code="ABCD-EFGH", sso="selected-secret", expected_email="selected@example.test")
        self.assertEqual(outcome["code"], "oauth_page_navigation_failed")
        self.assertEqual(outcome["stage"], "page_navigation")
        self.assertEqual(outcome["detail"], "navigation_failed")
        self.assertNotIn("selected-secret", str(outcome))
        self.assertNotIn("selected@example.test", str(outcome))
        cookies = context.add_cookies.await_args.args[0]
        self.assertEqual({c["url"] for c in cookies}, {"https://accounts.x.ai", "https://auth.x.ai"})
        self.assertTrue(all("domain" not in c and c["secure"] and c["httpOnly"] for c in cookies))
        self.assertNotIn("proxy", module.launch_async.await_args.kwargs)
        # Closing the whole browser owns/cleans its contexts as well.
        context.close.assert_not_awaited()
        browser.close.assert_awaited_once()

    async def test_navigation_timeout_is_classified_without_page_content(self):
        page = SimpleNamespace(
            main_frame=object(),
            url="https://accounts.x.ai/oauth2/device",
            goto=AsyncMock(side_effect=asyncio.TimeoutError()),
        )
        context = SimpleNamespace(
            add_cookies=AsyncMock(),
            new_page=AsyncMock(return_value=page),
            route=AsyncMock(),
        )
        browser = SimpleNamespace(new_context=AsyncMock(return_value=context), close=AsyncMock())
        outcome = await flow.authorize_device(
            verification_uri_complete="https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH",
            user_code="ABCD-EFGH",
            sso="selected-secret",
            expected_email="selected@example.test",
            browser=browser,
        )
        self.assertEqual(outcome["status"], "failed")
        self.assertEqual(outcome["code"], "oauth_page_navigation_timeout")
        self.assertEqual(outcome["stage"], "page_navigation")
        self.assertEqual(outcome["detail"], "navigation_timeout")
        self.assertEqual(outcome["page_path"], "/oauth2/device")
        self.assertNotIn("selected-secret", str(outcome))
        self.assertNotIn("selected@example.test", str(outcome))
        browser.close.assert_awaited_once()

    async def test_readonly_observation_failures_stop_before_consent_with_precise_safe_operation(self):
        for operation in ("form_count", "form_method", "code_count", "code_value", "button_count", "button_visible", "button_enabled"):
            with self.subTest(operation=operation):
                page, button, form, field = self.page("/oauth2/device")
                page.url += "?user_code=ABCD-EFGH"
                buttons = form.get_by_role.return_value
                target = {"form_count":form.count, "form_method":form.get_attribute, "code_count":field.count,
                          "code_value":field.input_value, "button_count":buttons.count,
                          "button_visible":button.is_visible, "button_enabled":button.is_enabled}[operation]
                target.side_effect = RuntimeError("Execution context was destroyed; selected-secret ABCD-EFGH selected@example.test")
                browser = self.browser_for(page)
                outcome = await self.authorize(browser)
                self.assertEqual(outcome["status"], "failed")
                self.assertEqual(outcome["code"], "oauth_page_observation_failed")
                self.assertEqual(outcome["operation"], operation)
                self.assertEqual(outcome["exception_type"], "navigation")
                self.assertEqual(outcome["consent_state"], "not_attempted")
                self.assertFalse(outcome["consent_submitted"])
                self.assertEqual(outcome["page_path"], "/oauth2/device")
                for secret in ("selected-secret", "ABCD-EFGH", "selected@example.test", "Execution context"):
                    self.assertNotIn(secret, str(outcome))
                button.click.assert_not_awaited()
                browser.close.assert_awaited_once()

    async def test_observation_timeout_and_closed_page_have_fixed_classification(self):
        for error, category, code in (
                (TimeoutError("PRIVATE"), "timeout", "oauth_page_observation_timeout"),
                (RuntimeError("Target page, context or browser has been closed PRIVATE"), "page_closed", "oauth_page_observation_failed"),
                (ValueError("PRIVATE"), "value_error", "oauth_page_observation_failed")):
            with self.subTest(category=category):
                page, button, _, _ = self.page()
                button.is_visible.side_effect = error
                outcome = await self.authorize(self.browser_for(page))
                self.assertEqual(outcome["exception_type"], category)
                self.assertEqual(outcome["code"], code)
                self.assertEqual(outcome["status"], "failed")
                self.assertNotIn("PRIVATE", str(outcome))

    async def test_synchronous_observation_api_error_keeps_exact_operation(self):
        page, button, form, _ = self.page("/oauth2/device")
        form.get_attribute = Mock(side_effect=TypeError("PRIVATE unexpected keyword"))
        outcome = await self.authorize(self.browser_for(page))
        self.assertEqual(outcome["operation"], "form_method")
        self.assertEqual(outcome["exception_type"], "type_error")
        self.assertEqual(outcome["consent_state"], "not_attempted")
        button.click.assert_not_awaited()

    async def test_observation_after_allow_preserves_attempted_and_never_reclicks(self):
        page, button, _, _ = self.page()
        async def submitted(**_):
            page.title.side_effect = RuntimeError("Execution context was destroyed PRIVATE")
        button.click.side_effect = submitted
        browser = self.browser_for(page)
        with patch.object(flow.asyncio, "sleep", AsyncMock()):
            outcome = await self.authorize(browser)
        self.assertEqual(outcome["status"], "uncertain")
        self.assertEqual(outcome["code"], "oauth_page_observation_failed")
        self.assertEqual(outcome["operation"], "read_title")
        self.assertEqual(outcome["exception_type"], "navigation")
        self.assertEqual(outcome["consent_state"], "attempted")
        self.assertTrue(outcome["consent_submitted"])
        button.click.assert_awaited_once()
        browser.close.assert_awaited_once()

    async def test_outer_exception_and_deadline_preserve_attempted_state(self):
        for timeout in (False, True):
            with self.subTest(timeout=timeout):
                page, _, _, _ = self.page()
                browser = self.browser_for(page)
                async def failed(*args, diagnostics, **kwargs):
                    diagnostics.consent_state = "attempted"
                    diagnostics.operation = "wait_callback"
                    if timeout:
                        await asyncio.Event().wait()
                    raise RuntimeError("PRIVATE")
                with patch.object(flow, "confirm_on_page", failed):
                    outcome = await self.authorize(browser, timeout_s=.01)
                self.assertEqual(outcome["status"], "uncertain")
                self.assertEqual(outcome["consent_state"], "attempted")
                self.assertTrue(outcome["consent_submitted"])
                self.assertEqual(outcome["operation"], "wait_callback")
                self.assertEqual(outcome["page_path"], "/oauth2/device/consent")
                self.assertEqual(outcome["exception_type"], "timeout" if timeout else "unknown")
                browser.close.assert_awaited_once()

    async def test_unknown_submission_stays_uncertain_and_cleanup_cannot_replace_result(self):
        for operation in ("button_visible", "read_body", "read_title"):
            with self.subTest(operation=operation):
                page, button, _, _ = self.page()
                target = {"button_visible":button.is_visible, "read_body":page.locator("body").inner_text,
                          "read_title":page.title}[operation]
                target.side_effect = RuntimeError("PRIVATE")
                browser = self.browser_for(page)
                browser.close.side_effect = RuntimeError("PRIVATE CLOSE")
                outcome = await self.authorize(browser, diagnostics=flow.FlowDiagnostics(consent_state="unknown"))
                self.assertEqual(outcome["status"], "uncertain")
                self.assertEqual(outcome["consent_state"], "unknown")
                self.assertEqual(outcome["code"], "oauth_page_observation_failed")
                browser.close.assert_awaited_once()

    async def test_cancelled_request_keeps_submission_state_and_closes_browser(self):
        page, _, _, _ = self.page()
        browser = self.browser_for(page)
        state = flow.FlowDiagnostics()
        entered = asyncio.Event()
        async def suspended(*args, diagnostics, **kwargs):
            diagnostics.consent_state = "attempted"
            entered.set()
            await asyncio.Event().wait()
        with patch.object(flow, "confirm_on_page", suspended):
            task = asyncio.create_task(self.authorize(browser, diagnostics=state))
            await asyncio.wait_for(entered.wait(), 1)
            task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await task
        self.assertEqual(state.consent_state, "attempted")
        browser.close.assert_awaited_once()


if __name__ == "__main__": unittest.main()
