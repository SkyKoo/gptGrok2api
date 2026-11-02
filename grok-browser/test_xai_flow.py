import asyncio
import sys
import types
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.modules.setdefault("cloakbrowser", types.ModuleType("cloakbrowser"))

import xai_flow




class SignupSuccessTests(unittest.IsolatedAsyncioTestCase):
    def page(self, url="https://grok.com/", cookies=None):
        if cookies is None:
            cookies = [{"name": "sso", "value": "test-session", "domain": ".grok.com", "path": "/"}]
        return SimpleNamespace(url=url, context=SimpleNamespace(cookies=AsyncMock(return_value=cookies)))

    async def wait(self, page, body="Sign Out"):
        with (
            patch.object(xai_flow, "_body_text", AsyncMock(return_value=body)),
            patch.object(xai_flow, "_pause", AsyncMock()),
            patch.object(xai_flow, "_page_diagnostic", AsyncMock(return_value={})),
            patch.object(xai_flow, "time", SimpleNamespace(monotonic=Mock(side_effect=[0, 0, 2]))),
        ):
            return await xai_flow._wait_for_success(page, timeout_s=1)

    async def test_authenticated_grok_landing_and_onboarding_are_accepted(self):
        for host in ("grok.com", "www.grok.com"):
            for name in ("sso", "sso-rw"):
                with self.subTest(host=host, cookie=name):
                    page = self.page("https://" + host + "/", [{"name": name, "value": "test-session", "domain": ".grok.com"}])
                    await self.wait(page, "Name Age Continue Sign Out")
                    page.context.cookies.assert_awaited_once_with([page.url])

    async def test_legacy_account_destinations_remain_supported_with_sso(self):
        for url, body in (("https://accounts.x.ai/account", "Settings"), ("https://accounts.x.ai/", "Manage your account")):
            with self.subTest(url=url):
                await self.wait(self.page(url, [{"name": "sso", "value": "test-session", "domain": ".x.ai"}]), body)

    async def test_public_landing_sign_out_or_welcome_without_sso_is_rejected(self):
        for body in ("Ask Grok", "Sign Out", "Welcome"):
            with self.subTest(body=body):
                with self.assertRaises(xai_flow.XaiBrowserFlowError) as error:
                    await self.wait(self.page(cookies=[]), body)
                self.assertEqual(str(error.exception), "account_page_missing")
                self.assertFalse(error.exception.diagnostic["sso_cookie_present"])
                self.assertTrue(error.exception.diagnostic["grok_landing"])

    async def test_untrusted_destination_cannot_pass_with_account_text_and_cookie(self):
        for url in ("https://grok.com.evil.invalid/account", "https://evil.invalid/account", "http://grok.com/"):
            with self.subTest(url=url):
                page = self.page(url)
                with self.assertRaises(xai_flow.XaiBrowserFlowError):
                    await self.wait(page, "Welcome Sign Out")
                page.context.cookies.assert_not_awaited()

    async def test_wrong_domain_empty_and_non_session_cookies_are_rejected(self):
        cases = [
            {"name": "sso", "value": "test-session", "domain": ".x.ai"},
            {"name": "sso", "value": "test-session", "domain": ".grok.com.evil.invalid"},
            {"name": "sso", "value": "test-session", "domain": ".com"},
            {"name": "sso", "value": "test-session", "domain": ""},
            {"name": "sso", "value": "  ", "domain": ".grok.com"},
            {"name": "cf_clearance", "value": "test-session", "domain": ".grok.com"},
        ]
        for cookie in cases:
            with self.subTest(cookie=cookie):
                with self.assertRaises(xai_flow.XaiBrowserFlowError) as error:
                    await self.wait(self.page(cookies=[cookie]))
                self.assertNotIn("test-session", str(error.exception.diagnostic))

    async def test_cookie_does_not_complete_pending_signup_form(self):
        with self.assertRaises(xai_flow.XaiBrowserFlowError):
            await self.wait(self.page(), "Welcome Complete your sign up")

    async def test_waits_for_cookie_after_redirect(self):
        page = self.page()
        page.context.cookies.side_effect = [[], [{"name": "sso", "value": "test-session", "domain": ".grok.com"}]]
        with (
            patch.object(xai_flow, "_body_text", AsyncMock(return_value="Sign Out")),
            patch.object(xai_flow, "_pause", AsyncMock()) as pause,
            patch.object(xai_flow, "time", SimpleNamespace(monotonic=Mock(side_effect=[0, 0, 0.5]))),
        ):
            await xai_flow._wait_for_success(page, timeout_s=1)
        pause.assert_awaited_once()

    async def test_signup_returns_credentials_after_grok_redirect(self):
        page = self.page()
        session = SimpleNamespace(page=page, lock=asyncio.Lock())
        with (
            patch.object(xai_flow, "_session", AsyncMock(return_value=session)),
            patch.object(xai_flow, "_fill_first", AsyncMock(return_value=True)),
            patch.object(xai_flow, "_set_native_input_value", AsyncMock()),
            patch.object(xai_flow, "_pause", AsyncMock()),
            patch.object(xai_flow, "_native_turnstile_token", AsyncMock(return_value="test-token")),
            patch.object(xai_flow, "_click_submit", AsyncMock(return_value=True)),
            patch.object(xai_flow, "_body_text", AsyncMock(return_value="Name Age Continue Sign Out")),
            patch.object(xai_flow, "_user_agent", AsyncMock(return_value="test-UA")),
        ):
            result = await xai_flow.signup("test", email="person@example.invalid", password="test-password", code="test-code", given_name="Test", family_name="Person")
        self.assertTrue(result["ok"])
        self.assertEqual(result["sso"], "test-session")
        self.assertEqual(result["redirect_url"], "https://grok.com/")


class NativeBrowserContextTests(unittest.IsolatedAsyncioTestCase):
    async def test_start_uses_native_context_even_with_old_protocol_profile(self):
        page = SimpleNamespace(
            url="https://accounts.x.ai/sign-up",
            add_init_script=AsyncMock(),
            on=Mock(),
            goto=AsyncMock(return_value=SimpleNamespace(status=200)),
        )
        context = SimpleNamespace(new_page=AsyncMock(return_value=page), close=AsyncMock())
        browser = SimpleNamespace(new_context=AsyncMock(return_value=context), close=AsyncMock())
        profile = {
            "user_agent": "Mozilla/5.0 Chrome/131.0.0.0 Safari/537.36",
            "navigator_language": "zh-CN",
            "timezone_iana": "UTC",
            "inner_width": 1680, "inner_height": 963, "device_pixel_ratio": 2,
        }
        with (
            patch.object(xai_flow.cloakbrowser, "launch_async", AsyncMock(return_value=browser), create=True) as launch,
            patch.object(xai_flow, "cleanup_expired_sessions", AsyncMock()),
            patch.object(xai_flow, "_user_agent", AsyncMock(return_value="engine-UA")),
            patch.object(xai_flow, "cookie_snapshot", AsyncMock(return_value=[])),
            patch.object(xai_flow, "_page_diagnostic", AsyncMock(return_value={})),
            patch.object(xai_flow, "_sessions", {}),
        ):
            result = await xai_flow.start_session(signup_url=page.url, proxy="http://proxy.example:8080", profile=profile)
            try:
                browser.new_context.assert_awaited_once_with(no_viewport=True)
                self.assertEqual(launch.await_args.kwargs["locale"], "en-US")
                self.assertEqual(launch.await_args.kwargs["timezone"], "UTC")
                self.assertEqual(launch.await_args.kwargs["proxy"], "http://proxy.example:8080")
                page.add_init_script.assert_awaited_once_with(xai_flow._TURNSTILE_CAPTURE_INIT_JS)
                self.assertEqual(result["user_agent"], "engine-UA")
            finally:
                await xai_flow.close_session(result["session_id"])
            # Closing the whole browser owns/cleans its contexts as well.
            context.close.assert_not_awaited()
            browser.close.assert_awaited_once()


class XaiEmailErrorTests(unittest.TestCase):
    def test_classifies_email_signup_unavailable_page(self):
        self.assertEqual(
            xai_flow._classify_email_stage_error(
                "Email sign-up isn’t available right now. Sign up another way."
            ),
            "signup_unavailable",
        )

    def test_maps_upstream_email_signup_error_to_safe_code(self):
        self.assertEqual(
            xai_flow._upstream_email_error("account:email-signup-unavailable"),
            ("signup_unavailable", "email_signup_unavailable"),
        )
        self.assertIsNone(xai_flow._upstream_email_error("password=secret"))


class XaiEmailSubmitCaptureTests(unittest.IsolatedAsyncioTestCase):
    async def test_only_post_response_for_armed_submit_is_captured(self):
        state = {}
        capture = xai_flow._EmailSubmitCapture(state)
        ignored_request = SimpleNamespace(url="https://accounts.x.ai/background", method="POST")
        submit_request = SimpleNamespace(url="https://accounts.x.ai/api/send-email", method="POST")
        capture.request(ignored_request)
        capture.arm()
        capture.request(submit_request)
        capture.response(
            SimpleNamespace(
                url=submit_request.url,
                status=400,
                request=submit_request,
                text=AsyncMock(return_value='{"code":"account:email-signup-unavailable"}'),
            ),
        )
        await capture.drain()
        self.assertEqual(state["reason"], "signup_unavailable")
        self.assertEqual(state["method"], "POST")
        self.assertEqual(state["path"], "/api/send-email")

        unrelated = {}
        other_capture = xai_flow._EmailSubmitCapture(unrelated)
        other_capture.arm()
        other_capture.response(
            SimpleNamespace(
                url=ignored_request.url,
                status=400,
                request=ignored_request,
                text=AsyncMock(return_value='{"code":"account:email-signup-unavailable"}'),
            ),
        )
        await other_capture.drain()
        self.assertEqual(unrelated, {})

    async def test_capture_listener_ignores_unarmed_and_get_requests(self):
        state = {}
        capture = xai_flow._EmailSubmitCapture(state)
        request = SimpleNamespace(url="https://accounts.x.ai/api/send-email", method="GET")
        capture.arm()
        capture.request(request)
        capture.response(SimpleNamespace(request=request, url=request.url, status=400, text=AsyncMock(return_value='{"code":"account:email-signup-unavailable"}')))
        await capture.drain()
        self.assertEqual(state, {})

    async def test_resend_click_failure_is_not_reported_as_success(self):
        page = SimpleNamespace(
            url="https://accounts.x.ai/sign-up",
            on=lambda *args: None,
            remove_listener=lambda *args: None,
        )
        session = SimpleNamespace(page=page, lock=asyncio.Lock())
        with patch.object(xai_flow, "_session", new=AsyncMock(return_value=session)), \
             patch.object(xai_flow, "_first_visible", new=AsyncMock(side_effect=[None, object()])), \
             patch.object(xai_flow, "_click_any", new=AsyncMock(side_effect=[False, False])), \
             patch.object(xai_flow, "_page_diagnostic", new=AsyncMock(return_value={})):
            with self.assertRaises(xai_flow.XaiBrowserFlowError) as raised:
                await xai_flow.send_email("xai-test", "person@example.invalid")
        self.assertEqual(str(raised.exception), "resend_button_missing")


class FakeTurnstilePage:
    url = "https://accounts.x.ai/sign-up"

    def __init__(self, state=None, token=""):
        self.state = state or {}
        self.token = token

    async def evaluate(self, script, *args):
        if script == xai_flow._TURNSTILE_RESPONSE_JS:
            return self.token
        return self.state

    async def title(self):
        return "Create Your xAI Account"

    def locator(self, selector):
        raise RuntimeError("locator not available in unit fake")


class FakeEmailField:
    def __init__(self):
        self.events = []

    async def click(self, **kwargs):
        self.events.append(("click", kwargs))

    async def fill(self, value):
        self.events.append(("fill", value))

    async def press_sequentially(self, value, delay=0):
        self.events.append(("type", value, delay))


class XaiEmailTypingTests(unittest.IsolatedAsyncioTestCase):
    async def test_email_is_typed_after_focus(self):
        field = FakeEmailField()
        await xai_flow._fill_email_like_user(field, "person@example.test")
        self.assertEqual([event[0] for event in field.events], ["click", "fill", "type"])
        self.assertEqual(field.events[-1][1:], ("person@example.test", 35))


class XaiTurnstileTests(unittest.IsolatedAsyncioTestCase):
    async def test_reads_token_from_captured_turnstile_callback(self):
        page = FakeTurnstilePage(
            state={"token_present": True, "iframe_count": 1, "visible_iframe_count": 1},
            token="turnstile-token",
        )
        with patch.object(xai_flow, "_click_turnstile_checkbox", new=AsyncMock(return_value=False)):
            token = await xai_flow._native_turnstile_token(page, timeout_s=1)
        self.assertEqual(token, "turnstile-token")

    async def test_timeout_contains_bounded_turnstile_state(self):
        page = FakeTurnstilePage(
            state={
                "token_present": False,
                "callback_seen": False,
                "widget_id_present": True,
                "widget_count": 1,
                "iframe_count": 2,
                "visible_iframe_count": 1,
                "ready": True,
            }
        )
        with patch.object(xai_flow, "_click_turnstile_checkbox", new=AsyncMock(return_value=False)):
            with self.assertRaises(xai_flow.XaiBrowserFlowError) as raised:
                await xai_flow._native_turnstile_token(page, timeout_s=0)
        self.assertEqual(raised.exception.stage, "turnstile")
        self.assertEqual(raised.exception.diagnostic["turnstile_frames"], 0)
        self.assertEqual(raised.exception.diagnostic["turnstile_ready"], False)


class XaiRefusalSemanticsTests(unittest.IsolatedAsyncioTestCase):
    async def test_recorded_upstream_refusal_stops_before_code_polling(self):
        upstream = {"reason":"signup_unavailable", "code":"email_signup_unavailable", "status":400}
        with patch.object(xai_flow, "_page_diagnostic", new=AsyncMock(return_value={"upstream_status":400})):
            with self.assertRaises(xai_flow.XaiBrowserFlowError) as raised:
                await xai_flow._wait_for_email_verification(FakeTurnstilePage(), upstream=upstream)
        self.assertFalse(raised.exception.retryable)
        self.assertEqual(raised.exception.stage,"send_code")
        self.assertEqual(str(raised.exception),"signup_unavailable")

    async def test_page_diagnostic_includes_only_passive_network_evidence(self):
        from network_diagnostics import NetworkDiagnostics
        from types import SimpleNamespace
        page=FakeTurnstilePage()
        page._cfm_network_diagnostics=NetworkDiagnostics()
        page._cfm_network_diagnostics.failed(SimpleNamespace(url='https://brunhild.challenges.cloudflare.com/private?secret=1', failure='net::ERR_TUNNEL_CONNECTION_FAILED'))
        result=await xai_flow._page_diagnostic(page,body='')
        self.assertEqual(result['network_failures'][0]['kind'],'proxy_tunnel_failed')
        self.assertNotIn('secret',str(result))

if __name__ == "__main__":
    unittest.main()
