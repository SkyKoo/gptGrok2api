import asyncio
import sys
import types
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.modules.setdefault("cloakbrowser", types.ModuleType("cloakbrowser"))

import xai_flow


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
        if "return window.__cfmGrokTurnstileToken" in script:
            return self.token
        return self.state

    async def title(self):
        return "Create Your xAI Account"

    def locator(self, selector):
        raise RuntimeError("locator not available in unit fake")


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
