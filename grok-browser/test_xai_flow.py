import sys
import types
import unittest
from pathlib import Path
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


if __name__ == "__main__":
    unittest.main()
