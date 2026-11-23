import unittest

from grok_relogin import EMAIL, EMAIL_ENTRY, PASSWORD, SIGN_IN, SUBMIT, VERIFICATION, relogin


class LoginControl:
    def __init__(self, page, name):
        self.page = page
        self.name = name

    async def all(self):
        return [self] if self.name in self.page.controls else []

    async def is_visible(self):
        return True

    async def fill(self, value, timeout):
        if self.name == EMAIL and self.page.state == 2:
            raise TimeoutError("email is read-only after submitting it")
        self.page.submissions.append((self.name, value))

    async def click(self, timeout):
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
        if self.outcome in ("success", "readonly_email") and self.state == 3:
            return "https://grok.com/"
        return SIGN_IN

    @property
    def controls(self):
        return ({
            0: {"email_entry"},
            1: {EMAIL, "submit"},
            2: {PASSWORD, "submit", EMAIL} if self.outcome == "readonly_email" else {PASSWORD, "submit"},
            3: {VERIFICATION} if self.outcome == "verification" else set(),
        })[self.state]

    async def goto(self, url, **kwargs):
        self.destination = url

    def on(self, event, handler):
        pass

    def locator(self, selector):
        return LoginControl(self, selector)

    def get_by_role(self, role, name):
        control = {EMAIL_ENTRY: "email_entry", SUBMIT: "submit"}[name]
        return LoginControl(self, control if role == "button" else "unavailable")

    async def cookies(self, urls):
        if urls != ["https://grok.com/"]:
            raise AssertionError("session lookup was not scoped to Grok")
        return [
            {"name": "sso", "value": "not-grok", "domain": ".x.ai", "secure": True},
            {"name": "sso", "value": "fresh-token", "domain": ".grok.com", "secure": True},
            {"name": "sso-rw", "value": "fresh-rw", "domain": ".grok.com", "secure": True},
        ]


class TestGrokRelogin(unittest.IsolatedAsyncioTestCase):
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

    async def test_official_login_returns_only_scoped_session(self):
        page = LoginPage("success")
        result = await relogin(page, "fixture@example.test", "fixture-password")
        self.assertEqual(page.destination, SIGN_IN)
        self.assertEqual(page.submissions, [(EMAIL, "fixture@example.test"), (PASSWORD, "fixture-password")])
        self.assertEqual(result, {"sso": "fresh-token", "sso_rw": "fresh-rw"})
        self.assertTrue(page.closed)

    async def test_extra_verification_leaves_without_submitting_again(self):
        page = LoginPage("verification")
        self.assertEqual(await relogin(page, "fixture@example.test", "fixture-password"), {"code": "verification_required"})
        self.assertEqual(len(page.submissions), 2)
        self.assertTrue(page.closed)

    async def test_password_stage_does_not_refill_previously_submitted_email(self):
        page = LoginPage("readonly_email")
        result = await relogin(page, "fixture@example.test", "fixture-password")
        self.assertEqual(result["sso"], "fresh-token")
        self.assertEqual(page.submissions, [(EMAIL, "fixture@example.test"), (PASSWORD, "fixture-password")])

    async def test_untrusted_navigation_never_receives_password(self):
        page = LoginPage("untrusted")
        self.assertEqual(await relogin(page, "fixture@example.test", "fixture-password"), {"code": "login_page_unrecognized"})
        self.assertEqual(page.submissions, [(EMAIL, "fixture@example.test")])
        self.assertTrue(page.closed)
