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
                button.click.assert_not_awaited()
        page, button, _, _ = self.page()
        page.url = "https://accounts.x.ai.evil.invalid/oauth2/device/consent"
        self.assertEqual((await self.confirm(page))["status"], "failed")
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
        self.assertEqual(outcome, {"status":"uncertain", "code":"oauth_worker_result_unknown"})
        button.click.assert_awaited_once()

    async def test_accept_once_wait_for_done_and_never_infer_success_from_click(self):
        page, button, _, _ = self.page()
        async def finish(**kwargs): page.url = "https://accounts.x.ai/oauth2/device/done"
        button.click.side_effect = finish
        with patch.object(flow.asyncio, "sleep", AsyncMock()):
            self.assertEqual((await self.confirm(page))["status"], "approved")
        button.click.assert_awaited_once()
        page, button, _, _ = self.page()
        with patch.object(flow.asyncio, "sleep", AsyncMock()):
            self.assertEqual((await self.confirm(page))["status"], "uncertain")
        button.click.assert_awaited_once()

    async def test_isolated_host_cookies_and_cleanup_on_failure(self):
        page = SimpleNamespace(main_frame=object(), goto=AsyncMock(side_effect=RuntimeError("secret")))
        context = SimpleNamespace(add_cookies=AsyncMock(), new_page=AsyncMock(return_value=page), route=AsyncMock(), close=AsyncMock())
        browser = SimpleNamespace(new_context=AsyncMock(return_value=context), close=AsyncMock())
        module = SimpleNamespace(launch_async=AsyncMock(return_value=browser))
        with patch.dict(sys.modules, {"cloakbrowser": module}), patch.dict(os.environ, {"TURNSTILE_PROXY":"http://signup-only.example.test:8080"}):
            outcome = await flow.authorize_device(verification_uri_complete="https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH", user_code="ABCD-EFGH", sso="selected-secret", expected_email="selected@example.test")
        self.assertEqual(outcome["code"], "oauth_worker_result_unknown")
        cookies = context.add_cookies.await_args.args[0]
        self.assertEqual({c["url"] for c in cookies}, {"https://accounts.x.ai", "https://auth.x.ai"})
        self.assertTrue(all("domain" not in c and c["secure"] and c["httpOnly"] for c in cookies))
        self.assertNotIn("proxy", module.launch_async.await_args.kwargs)
        context.close.assert_awaited_once()
        browser.close.assert_awaited_once()


if __name__ == "__main__": unittest.main()
