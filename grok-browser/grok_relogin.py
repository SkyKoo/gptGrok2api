"""Sign into an existing Grok account in a fresh, reserved browser context."""
import asyncio
import logging
import re
import time
from urllib.parse import urlsplit

from common.browser import native_browser_context_kwargs
from network_diagnostics import NetworkDiagnostics


SIGN_IN = "https://accounts.x.ai/sign-in?redirect=grok-com"
AUTH_HOSTS = {"accounts.x.ai", "auth.x.ai"}
APP_HOSTS = {"grok.com", "www.grok.com"}
EMAIL = 'input[type="email"], input[autocomplete="username"], input[name="email"]'
PASSWORD = 'input[type="password"]:not([autocomplete="new-password"])'
VERIFICATION = ('input[autocomplete="one-time-code"], input[name="otp"], input[name="code"], '
                'input[type="tel"], iframe[src*="challenges.cloudflare.com"]')
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
        except (AttributeError, ValueError):
            pass

    def write(self, outcome, exception="none"):
        faults = self.network.snapshot()
        last = faults[-1] if faults else {}
        log.info(
            "grok_relogin task=%s stage=%s outcome=%s elapsed_ms=%d password_attempted=%s exception=%s nav_http=%d auth_http=%d network=%s network_http=%d",
            self.task, self.stage, outcome, int((time.monotonic() - self.started) * 1000),
            str(self.password_attempted).lower(), exception, self.navigation_status, self.auth_status,
            last.get("kind", "none"), last.get("status", 0),
        )


def _trusted(page, hosts):
    url = urlsplit(page.url)
    return (url.scheme == "https" and url.hostname in hosts and url.port in (None, 443)
            and not url.username and not url.password)


def _on_sign_in(page):
    return _trusted(page, AUTH_HOSTS) and not re.search(r"(?:sign-up|signup|register|reset|recover)", urlsplit(page.url).path, re.I)


async def _single_visible(locator):
    matches = [item for item in await locator.all() if await item.is_visible()]
    return matches[0] if len(matches) == 1 else None


async def _submit(page, trace, password):
    trace.enter("password_submit_control" if password else "email_submit_control")
    button = await _single_visible(page.get_by_role("button", name=SUBMIT))
    if button is None or not _on_sign_in(page):
        return False
    trace.enter("password_submit" if password else "email_submit")
    trace.password_attempted = password
    await button.click(timeout=5000)
    return True


async def _session(page):
    if not _trusted(page, APP_HOSTS) or urlsplit(page.url).path not in ("", "/"):
        return None
    items = await page.context.cookies(["https://grok.com/"])
    cookies = {}
    for item in items:
        name, value = item.get("name"), item.get("value", "")
        if (name in {"sso", "sso-rw"} and item.get("secure")
                and item.get("domain", "").lstrip(".") == "grok.com"
                and re.fullmatch(r"[\x21-\x2b\x2d-\x3a\x3c-\x7e]{1,8192}", value)):
            if name in cookies and cookies[name] != value:
                return None
            cookies[name] = value
    if cookies.get("sso"):
        return {"sso": cookies["sso"], "sso_rw": cookies.get("sso-rw", "")}
    return None


async def _sign_in(page, email, password, trace):
    trace.enter("navigation")
    response = await page.goto(SIGN_IN, wait_until="domcontentloaded", timeout=25000)
    if response is not None and 100 <= response.status <= 599:
        trace.navigation_status = response.status
    entry_clicked = email_submitted = password_submitted = False
    while True:
        trace.enter("session_wait" if password_submitted else "password_wait" if email_submitted else "email_form_wait" if entry_clicked else "email_entry_wait")
        if password_submitted and (session := await _session(page)):
            return session
        if not _on_sign_in(page):
            if not password_submitted or not _trusted(page, APP_HOSTS):
                return {"code": "login_page_unrecognized"}
            await asyncio.sleep(.25)
            continue
        for verification in await page.locator(VERIFICATION).all():
            if await verification.is_visible():
                return {"code": "verification_required"}
        email_input = await _single_visible(page.locator(EMAIL))
        password_input = await _single_visible(page.locator(PASSWORD))
        if password_input is not None and not password_submitted and (email_input is not None or email_submitted):
            if email_input is not None and _on_sign_in(page):
                trace.enter("email_fill")
                await email_input.fill(email, timeout=5000)
            if not _on_sign_in(page):
                return {"code": "login_page_unrecognized"}
            trace.enter("password_fill")
            await password_input.fill(password, timeout=5000)
            if not await _submit(page, trace, password=True):
                return {"code": "login_page_unrecognized"}
            password_submitted = True
        elif email_input is not None and not email_submitted and not password_submitted:
            if not _on_sign_in(page):
                return {"code": "login_page_unrecognized"}
            trace.enter("email_fill")
            await email_input.fill(email, timeout=5000)
            if not await _submit(page, trace, password=False):
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
