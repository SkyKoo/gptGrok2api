import base64
import json
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

from grok_relogin import (
    AGE_LABEL, AGE_PROMPT, EMAIL, EMAIL_ENTRY, FORM_SUBMIT, PASSWORD, SIGN_IN, SUBMIT, VERIFICATION,
    ReloginTrace, _session, relogin,
)


def fixture_token(claims):
    payload = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    return "eyJhbGciOiJIUzI1NiJ9." + payload + ".fixture"


FRESH = fixture_token({"sub": "fixture-account"})
WRAPPER = fixture_token({"config": {"success_url": "https://grok.com/"}})


class LoginControl:
    def __init__(self, page, name):
        self.page = page
        self.name = name

    async def all(self):
        return [self] if self.name in self.page.controls else []

    async def is_visible(self):
        return True

    async def count(self):
        return int(self.name in self.page.controls)

    def locator(self, selector):
        if selector == FORM_SUBMIT and self.name == "owning_form":
            return LoginControl(self.page, "native_submit")
        return LoginControl(self.page, "owning_form" if self.name == PASSWORD else "absent")

    def get_by_role(self, role, name):
        return LoginControl(self.page, "named_form_action")

    def and_(self, other):
        return LoginControl(self.page, self.name if self.name == other.name else "absent")

    async def input_value(self, timeout):
        if self.page.outcome == "lost_input":
            return ""
        return self.page.submissions[-1][1]

    async def fill(self, value, timeout):
        if self.name == EMAIL and self.page.state == 2:
            raise TimeoutError("email is read-only after submitting it")
        self.page.submissions.append((self.name, value))

    async def click(self, timeout):
        if self.page.outcome == "form_owned" and self.page.state == 2 and self.name != "native_submit":
            raise AssertionError("password must be submitted through its owning form")
        self.page.state += 1


class LoginPage:
    def __init__(self, outcome):
        self.state = 0
        self.outcome = outcome
        self.context = self
        self.submissions = []
        self.closed = False

    async def new_context(self, **kwargs):
        return self

    async def new_page(self):
        return self

    async def close(self):
        self.closed = True

    @property
    def url(self):
        if self.outcome == "untrusted" and self.state == 2:
            return "https://accounts.x.ai.example.test/sign-in"
        if self.outcome in ("success", "readonly_email", "form_owned") and self.state == 3:
            return "https://grok.com/"
        if self.outcome == "xai_account" and self.state == 3:
            return "https://accounts.x.ai/account/profile"
        return SIGN_IN

    @property
    def controls(self):
        controls = ({
            0: {"email_entry"},
            1: {EMAIL, "submit"},
            2: {PASSWORD, "submit", EMAIL} if self.outcome == "readonly_email" else {PASSWORD, "submit"},
            3: {VERIFICATION} if self.outcome == "verification" else set(),
        })[self.state]
        if self.outcome in ("form_owned", "conflicting_actions") and self.state == 2:
            controls |= {"owning_form", "native_submit"}
            if self.outcome == "conflicting_actions":
                controls.add("named_form_action")
        return controls

    async def goto(self, url, **kwargs):
        self.destination = url

    def on(self, event, handler):
        pass

    def locator(self, selector):
        return LoginControl(self, selector)

    def get_by_role(self, role, name):
        if name in (AGE_PROMPT, AGE_LABEL):
            return LoginControl(self, (role, name))
        control = {EMAIL_ENTRY: "email_entry", SUBMIT: "submit"}[name]
        return LoginControl(self, control if role == "button" else "unavailable")

    async def cookies(self, urls):
        expected = ["https://accounts.x.ai/account/profile"] if self.outcome == "xai_account" else ["https://grok.com/"]
        if urls != expected:
            raise AssertionError("session lookup was not scoped to the signed-in first-party page")
        domain = ".x.ai" if self.outcome == "xai_account" else ".grok.com"
        return [
            {"name": "sso", "value": FRESH, "domain": ".example.test", "secure": True},
            {"name": "sso", "value": WRAPPER if self.outcome == "wrapper" else FRESH, "domain": domain, "secure": True},
        ]


class TestGrokRelogin(unittest.IsolatedAsyncioTestCase):
    def test_password_response_count_follows_request_start(self):
        trace = ReloginTrace("task_response_order")
        email_post = Mock(method="POST", url=SIGN_IN)
        password_post = Mock(method="POST", url=SIGN_IN)
        trace.observe_request(email_post)
        trace.password_attempted = True
        trace.observe_request(password_post)
        trace.observe_response(SimpleNamespace(request=email_post, url=SIGN_IN, status=200))
        self.assertEqual((trace.password_requests, trace.password_posts), (1, 0))
        self.assertEqual(trace.password_status, 0)
        trace.observe_response(SimpleNamespace(request=password_post, url=SIGN_IN, status=429))
        self.assertEqual((trace.password_requests, trace.password_posts), (1, 1))
        self.assertEqual(trace.password_status, 429)

    def test_verification_target_is_recorded_without_request_url(self):
        trace = ReloginTrace("task_verification")
        brunhild = Mock(url="https://brunhild.challenges.cloudflare.com/private?k=secret",
                                  failure="net::ERR_ADDRESS_UNREACHABLE")
        turnstile = SimpleNamespace(url="https://challenges.cloudflare.com/private?k=secret", status=403)
        ignored = Mock(url="https://brunhild.challenges.cloudflare.com.example.test/private",
                                  failure="raw failure secret")
        with self.assertLogs("cfm-browser-worker.grok-relogin", level="INFO") as captured:
            trace.enter("password_fill")
            trace.observe_failure(brunhild)
            trace.observe_failure(brunhild)
            trace.observe_failure(ignored)
            trace.observe_response(turnstile)
        self.assertEqual(len(captured.output), 3)
        self.assertIn("stage=password_fill target=brunhild kind=network_unreachable chromium=net::ERR_ADDRESS_UNREACHABLE http=0", captured.output[1])
        self.assertIn("stage=password_fill target=turnstile kind=http_error chromium=none http=403", captured.output[2])
        self.assertNotIn("private", "\n".join(captured.output))
        self.assertNotIn("secret", "\n".join(captured.output))

    async def test_failure_logs_phase_without_exception_text_or_credentials(self):
        page = LoginPage("success")

        async def timeout(url, **kwargs):
            raise TimeoutError("private-email@example.test and private-password")

        page.goto = timeout
        with self.assertLogs("cfm-browser-worker.grok-relogin", level="INFO") as captured:
            result = await relogin(page, "private-email@example.test", "private-password", "task_diagnostic")
        self.assertEqual(result, {"code": "login_timeout"})
        self.assertIn("task=task_diagnostic stage=navigation outcome=login_timeout", captured.output[-1])
        self.assertIn("password_attempted=false exception=timeout", captured.output[-1])
        self.assertNotIn("private-", "\n".join(captured.output))

    async def test_rejected_entry_releases_browser_without_submitting(self):
        for status, expected in [(403, "login_entry_forbidden"), (429, "upstream_rate_limited")]:
            with self.subTest(status=status):
                page = LoginPage("success")
                page.goto = AsyncMock(return_value=SimpleNamespace(status=status))
                result = await relogin(page, "fixture@example.test", "fixture-password")
                self.assertEqual(result, {"code": expected})
                self.assertEqual(page.submissions, [])
                self.assertTrue(page.closed)

    async def test_forbidden_page_with_visible_verification_requests_manual_action(self):
        class VerificationPage(LoginPage):
            @property
            def controls(self):
                return {VERIFICATION}

        page = VerificationPage("verification")
        page.goto = AsyncMock(return_value=SimpleNamespace(status=403))
        self.assertEqual(await relogin(page, "", ""), {"code": "verification_required"})
        self.assertTrue(page.closed)

    async def test_timeout_after_password_click_reports_unconfirmed_session(self):
        class WaitPage(LoginPage):
            def locator(self, selector):
                if self.state == 3:
                    raise TimeoutError("fixture session deadline")
                return super().locator(selector)

        page = WaitPage("form_owned")
        self.assertEqual(await relogin(page, "fixture@example.test", "fixture-password"),
                         {"code": "session_wait_timeout"})
        self.assertEqual(page.state, 3)
        self.assertTrue(page.closed)

    async def test_password_page_timeout_distinguishes_observed_requests(self):
        class StalledPage(LoginPage):
            def on(self, event, handler):
                if event == "request":
                    self.observe_request = handler

            @property
            def controls(self):
                return {PASSWORD} if self.state == 3 else super().controls

        for observed, expected in [(False, "password_request_unobserved"), (True, "session_wait_timeout")]:
            with self.subTest(observed=observed):
                page = StalledPage("stalled")
                page.cookies = AsyncMock()
                waits_after_click = 0

                async def tick(_):
                    nonlocal waits_after_click
                    if page.state == 3:
                        waits_after_click += 1
                        if waits_after_click == 1 and observed:
                            page.observe_request(Mock(method="POST", url=SIGN_IN))
                        if waits_after_click == 2:
                            raise TimeoutError()

                with patch("grok_relogin.asyncio.sleep", tick):
                    result = await relogin(page, "fixture@example.test", "fixture-password")
                self.assertEqual(result, {"code": expected})
                page.cookies.assert_not_awaited()
                self.assertEqual(page.state, 3)
                self.assertTrue(page.closed)

    async def test_official_login_returns_only_scoped_session(self):
        page = LoginPage("success")
        result = await relogin(page, "fixture@example.test", "fixture-password")
        self.assertEqual(page.destination, SIGN_IN)
        self.assertEqual(page.submissions, [(EMAIL, "fixture@example.test"), (PASSWORD, "fixture-password")])
        self.assertEqual(result, {"sso": FRESH, "sso_rw": ""})
        self.assertTrue(page.closed)

    async def test_extra_verification_leaves_without_submitting_again(self):
        page = LoginPage("verification")
        self.assertEqual(await relogin(page, "fixture@example.test", "fixture-password"), {"code": "verification_required"})
        self.assertEqual(len(page.submissions), 2)
        self.assertTrue(page.closed)

    async def test_age_form_stops_before_session_lookup(self):
        class AgePage(LoginPage):
            @property
            def controls(self):
                return self.age_controls if self.state == 3 else super().controls

        heading = ("heading", AGE_PROMPT)
        for age_controls, requires_age in [
            ({heading, ("textbox", AGE_LABEL)}, True),
            ({heading, ("spinbutton", AGE_LABEL)}, True),
            ({heading}, False),
            ({("textbox", AGE_LABEL)}, False),
        ]:
            with self.subTest(age_controls=age_controls):
                page = AgePage("success")
                page.age_controls = age_controls
                page.cookies = AsyncMock(wraps=page.cookies)
                result = await relogin(page, "fixture@example.test", "fixture-password")
                if requires_age:
                    self.assertEqual(result, {"code": "age_confirmation_required"})
                    page.cookies.assert_not_awaited()
                else:
                    self.assertEqual(result, {"sso": FRESH, "sso_rw": ""})
                self.assertEqual(len(page.submissions), 2)
                self.assertEqual(page.state, 3)
                self.assertTrue(page.closed)

    async def test_password_stage_does_not_refill_previously_submitted_email(self):
        page = LoginPage("readonly_email")
        result = await relogin(page, "fixture@example.test", "fixture-password")
        self.assertEqual(result["sso"], FRESH)
        self.assertEqual(page.submissions, [(EMAIL, "fixture@example.test"), (PASSWORD, "fixture-password")])

    async def test_untrusted_navigation_never_receives_password(self):
        page = LoginPage("untrusted")
        self.assertEqual(await relogin(page, "fixture@example.test", "fixture-password"), {"code": "login_page_unrecognized"})
        self.assertEqual(page.submissions, [(EMAIL, "fixture@example.test")])
        self.assertTrue(page.closed)

    async def test_password_can_complete_on_official_account_page(self):
        page = LoginPage("xai_account")
        self.assertEqual(await relogin(page, "fixture@example.test", "fixture-password"), {"sso": FRESH, "sso_rw": ""})

    async def test_session_rejects_intermediate_wrapper(self):
        self.assertIsNone(await _session(LoginPage("wrapper"), "grok"))

    async def test_uses_the_password_forms_submit_control(self):
        page = LoginPage("form_owned")
        result = await relogin(page, "fixture@example.test", "fixture-password")
        self.assertEqual(result["sso"], FRESH)

    async def test_conflicting_form_actions_stop_before_password_click(self):
        page = LoginPage("conflicting_actions")
        self.assertEqual(await relogin(page, "fixture@example.test", "fixture-password"),
                         {"code": "login_action_ambiguous"})
        self.assertEqual(page.state, 2)
        self.assertTrue(page.closed)

    async def test_does_not_submit_a_password_the_page_did_not_keep(self):
        page = LoginPage("lost_input")
        result = await relogin(page, "fixture@example.test", "fixture-password")
        self.assertEqual(result, {"code": "password_input_unconfirmed"})
        self.assertEqual(page.state, 2)
