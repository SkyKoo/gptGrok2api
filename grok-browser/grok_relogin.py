"""Sign into an existing Grok account in a fresh, reserved browser context."""
import asyncio
import base64
import json
import logging
import re
import time
from urllib.parse import urlsplit

from common.browser import native_browser_context_kwargs
from network_diagnostics import NetworkDiagnostics


SIGN_IN = "https://accounts.x.ai/sign-in?redirect=grok-com"
AUTH_HOSTS = {"accounts.x.ai", "auth.x.ai"}
HANDOFF_HOSTS = {"auth.grokusercontent.com"}
APP_HOSTS = {"grok.com", "www.grok.com"}
EMAIL = 'input[type="email"], input[autocomplete="username"], input[name="email"]'
PASSWORD = 'input[type="password"]:not([autocomplete="new-password"])'
VERIFICATION = ('input[autocomplete="one-time-code"], input[name="otp"], input[name="code"], '
                'input[name="verificationCode"], input[name="emailCode"], input[type="tel"], '
                'iframe[src*="challenges.cloudflare.com"]')
ALERTS = '[role="alert"], [aria-live="assertive"]'
FORM_SUBMIT = 'button[type="submit"], input[type="submit"], button:not([type])'
EMAIL_ENTRY = re.compile(r"^(?:log ?in|sign ?in|continue) with email$|^使用邮箱(?:登录|继续)$", re.I)
SUBMIT = re.compile(r"^(?:log ?in|sign ?in|continue|next|登录|继续|下一步)$", re.I)
log = logging.getLogger("cfm-browser-worker.grok-relogin")


def _exception_category(exc):
    if isinstance(exc, TimeoutError) or type(exc).__name__ == "TimeoutError":
        return "timeout"
    if isinstance(exc, AttributeError):
        return "attribute_error"
    if isinstance(exc, TypeError):
        return "type_error"
    text = str(exc).lower()
    if "strict mode violation" in text:
        return "strict_mode"
    if "target page, context or browser has been closed" in text:
        return "closed"
    if "execution context was destroyed" in text or "frame was detached" in text:
        return "navigation_race"
    if "net::err_" in text:
        return "network_error"
    return "browser_error" if type(exc).__module__.startswith("playwright.") else "unknown"


class ReloginTrace:
    def __init__(self, task_id):
        self.task = task_id if re.fullmatch(r"task_[a-zA-Z0-9_.-]{1,64}", task_id) else "none"
        self.stage = ""
        self.started = time.monotonic()
        self.password_attempted = False
        self.navigation_status = 0
        self.auth_status = 0
        self.password_posts = 0
        self.password_requests = 0
        self.submit_kind = "none"
        self.page_kind = "none"
        self.form = "unknown"
        self.network = NetworkDiagnostics()

    def enter(self, stage):
        if stage != self.stage:
            self.stage = stage
            self.write("started")

    def observe_response(self, response):
        self.network.response(response)
        try:
            if (urlsplit(response.url).hostname in AUTH_HOSTS and response.request.method == "POST"
                    and 100 <= response.status <= 599):
                self.auth_status = response.status
                if self.password_attempted:
                    self.password_posts += 1
        except (AttributeError, ValueError):
            pass

    def observe_request(self, request):
        try:
            if self.password_attempted and request.method == "POST" and urlsplit(request.url).hostname in AUTH_HOSTS:
                self.password_requests += 1
        except (AttributeError, ValueError):
            pass

    def write(self, outcome, exception="none"):
        faults = self.network.snapshot()
        last = faults[-1] if faults else {}
        network_host = last.get("host", "")
        network_scope = "xai_auth" if network_host in AUTH_HOSTS else "verification_service" if network_host else "none"
        log.info(
            "grok_relogin task=%s stage=%s outcome=%s elapsed_ms=%d password_attempted=%s exception=%s nav_http=%d auth_http=%d network=%s network_http=%d page=%s form=%s password_posts=%d password_requests=%d submit=%s network_scope=%s",
            self.task, self.stage, outcome, int((time.monotonic() - self.started) * 1000),
            str(self.password_attempted).lower(), exception, self.navigation_status, self.auth_status,
            last.get("kind", "none"), last.get("status", 0), self.page_kind, self.form, self.password_posts,
            self.password_requests, self.submit_kind, network_scope,
        )


def _trusted(page, hosts):
    url = urlsplit(page.url)
    return (url.scheme == "https" and url.hostname in hosts and url.port in (None, 443)
            and not url.username and not url.password)


def _on_sign_in(page):
    return _trusted(page, AUTH_HOSTS) and not re.search(r"(?:sign-up|signup|register|reset|recover)", urlsplit(page.url).path, re.I)


def _page_kind(page):
    path = urlsplit(page.url).path
    if _trusted(page, APP_HOSTS):
        return "grok_sign_in" if re.match(r"^/(?:sign-in|sign-up|login|auth)(?:/|$)", path) else "grok"
    if _trusted(page, HANDOFF_HOSTS):
        return "handoff"
    if _trusted(page, AUTH_HOSTS):
        if re.match(r"^/accounts?(?:/|$)", path) and _on_sign_in(page):
            return "xai_account"
        return "xai_auth"
    return "other"


async def _single_visible(locator):
    matches = [item for item in await locator.all() if await item.is_visible()]
    return matches[0] if len(matches) == 1 else None


async def _submit(page, trace, password, field):
    trace.enter("password_submit_control" if password else "email_submit_control")
    form = field.locator("xpath=ancestor::form[1]")
    owns_form = await form.count() == 1
    button = await _single_visible(form.locator(FORM_SUBMIT)) if owns_form else None
    if button is None:
        actions = form if owns_form else page
        button = await _single_visible(actions.get_by_role("button", name=SUBMIT))
    if button is None or not _on_sign_in(page):
        return False
    trace.enter("password_submit" if password else "email_submit")
    trace.password_attempted = password
    trace.submit_kind = "form" if owns_form else "page_button"
    await button.click(timeout=5000)
    return True


def _session_cookie(value):
    if not re.fullmatch(r"eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+", value) or len(value) > 8192:
        return False
    payload = value.split(".")[1]
    try:
        claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
    except (ValueError, UnicodeError):
        return False
    if not isinstance(claims, dict) or "success_url" in claims:
        return False
    config = claims.get("config")
    return not isinstance(config, dict) or not ({"success_url", "token"} & config.keys())


async def _session(page, page_kind):
    if page_kind not in ("grok", "xai_account"):
        return None
    scope = "https://grok.com/" if page_kind == "grok" else page.url
    host = urlsplit(scope).hostname
    allowed_domains = {"grok.com"} if page_kind == "grok" else {host, "x.ai"}
    items = await page.context.cookies([scope])
    cookies = {}
    for item in items:
        name, value = item.get("name"), item.get("value", "")
        if (name in {"sso", "sso-rw", "sso_jwt"} and item.get("secure")
                and item.get("domain", "").lstrip(".").lower() in allowed_domains
                and _session_cookie(value)):
            if name in cookies and cookies[name] != value:
                return None
            cookies[name] = value
    token = cookies.get("sso") or cookies.get("sso_jwt") or cookies.get("sso-rw")
    if token:
        return {"sso": token, "sso_rw": cookies.get("sso-rw", "")}
    return None


async def _password_error(page, trace):
    if trace.password_posts and trace.auth_status == 429:
        return "upstream_rate_limited"
    for alert in (await page.locator(ALERTS).all())[:5]:
        if await alert.is_visible():
            text = await alert.inner_text(timeout=1000)
            if re.search(r"\b(?:invalid|incorrect|wrong) (?:email or )?password\b|\bpassword (?:is )?(?:invalid|incorrect|wrong)\b", text, re.I):
                return "credentials_rejected"
    return ""


async def _sign_in(page, email, password, trace):
    trace.enter("navigation")
    response = await page.goto(SIGN_IN, wait_until="domcontentloaded", timeout=25000)
    if response is not None and 100 <= response.status <= 599:
        trace.navigation_status = response.status
    entry_clicked = email_submitted = password_submitted = False
    while True:
        trace.page_kind = _page_kind(page)
        trace.enter("session_wait" if password_submitted else "password_wait" if email_submitted else "email_form_wait" if entry_clicked else "email_entry_wait")
        if password_submitted and trace.page_kind == "handoff":
            trace.form = "none"
            await asyncio.sleep(.25)
            continue
        if trace.page_kind != "other":
            for verification in await page.locator(VERIFICATION).all():
                if await verification.is_visible():
                    trace.form = "verification"
                    return {"code": "verification_required"}
        if password_submitted and trace.page_kind in ("grok", "xai_account"):
            password_controls = await page.locator(PASSWORD).all()
            if not any([await field.is_visible() for field in password_controls]) and (session := await _session(page, trace.page_kind)):
                trace.form = "none"
                return session
        if not _on_sign_in(page):
            if not password_submitted or not _trusted(page, APP_HOSTS):
                return {"code": "login_page_unrecognized"}
            await asyncio.sleep(.25)
            continue
        email_input = await _single_visible(page.locator(EMAIL))
        password_input = await _single_visible(page.locator(PASSWORD))
        trace.form = "password" if password_input is not None else "email" if email_input is not None else "none"
        if password_submitted and (error := await _password_error(page, trace)):
            return {"code": error}
        if password_input is not None and not password_submitted and (email_input is not None or email_submitted):
            # After a separate email submit, xAI can keep the email field on the
            # password page as a read-only account label. Only fill a combined
            # form when no email has been submitted yet.
            if email_input is not None and not email_submitted and _on_sign_in(page):
                trace.enter("email_fill")
                await email_input.fill(email, timeout=5000)
            if not _on_sign_in(page):
                return {"code": "login_page_unrecognized"}
            trace.enter("password_fill")
            await password_input.fill(password, timeout=5000)
            if await password_input.input_value(timeout=1000) != password:
                return {"code": "password_input_unconfirmed"}
            if not await _submit(page, trace, password=True, field=password_input):
                return {"code": "login_page_unrecognized"}
            password_submitted = True
        elif email_input is not None and not email_submitted and not password_submitted:
            if not _on_sign_in(page):
                return {"code": "login_page_unrecognized"}
            trace.enter("email_fill")
            await email_input.fill(email, timeout=5000)
            if not await _submit(page, trace, password=False, field=email_input):
                return {"code": "login_page_unrecognized"}
            email_submitted = True
        elif not entry_clicked and not email_submitted and not password_submitted:
            entry = await _single_visible(page.get_by_role("button", name=EMAIL_ENTRY))
            if entry is None:
                entry = await _single_visible(page.get_by_role("link", name=EMAIL_ENTRY))
            if entry is not None and _on_sign_in(page):
                trace.enter("email_entry_click")
                await entry.click(timeout=5000)
                entry_clicked = True
        await asyncio.sleep(.25)


async def relogin(browser, email, password, task_id=""):
    context = None
    trace = ReloginTrace(task_id)
    try:
        async with asyncio.timeout(60):
            trace.enter("browser_context")
            context = await browser.new_context(**native_browser_context_kwargs())
            trace.enter("browser_page")
            page = await context.new_page()
            page.on("request", trace.observe_request)
            page.on("requestfailed", trace.network.failed)
            page.on("response", trace.observe_response)
            result = await _sign_in(page, email, password, trace)
            trace.write(result.get("code", "success"))
            return result
    except Exception as exc:
        category = _exception_category(exc)
        code = "login_timeout" if category == "timeout" else "login_failed"
        trace.write(code, category)
        return {"code": code}
    finally:
        if context is not None:
            try:
                async with asyncio.timeout(5):
                    await context.close()
            except Exception as exc:
                trace.enter("browser_context_close")
                trace.write("close_failed", _exception_category(exc))
