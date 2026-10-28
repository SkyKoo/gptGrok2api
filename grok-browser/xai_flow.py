"""Native xAI signup flow kept inside the CFM Grok browser worker.

The page, Castle state, Turnstile widget, cookies and final SSO must belong to
one browser context.  This module intentionally keeps that context alive while
the Go registration engine waits for the mailbox code.
"""
from __future__ import annotations

import asyncio
import re
import time
import uuid
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import urlparse

import cloakbrowser

from common.browser import browser_kwargs
from turnstile.solve import _click_turnstile_checkbox

_ALLOWED_HOSTS = {"accounts.x.ai", "auth.x.ai", "auth.grokusercontent.com", "grok.com", "www.grok.com"}
_COOKIE_SUFFIXES = ("x.ai", "grok.com", "grokusercontent.com")
_SESSION_TTL = 20 * 60


class XaiBrowserFlowError(RuntimeError):
    def __init__(self, message: str, *, stage: str, retryable: bool = False, diagnostic: dict | None = None):
        super().__init__(message)
        self.stage = stage or "browser"
        self.retryable = retryable
        self.diagnostic = diagnostic or {}


@dataclass
class BrowserSession:
    session_id: str
    browser: Any
    page: Any
    signup_url: str
    initial_response_status: int = 0
    touched_at: float = field(default_factory=time.monotonic)
    lock: asyncio.Lock = field(default_factory=asyncio.Lock)

    async def close(self) -> None:
        try:
            await self.browser.close()
        except Exception:
            pass


_sessions: dict[str, BrowserSession] = {}
_sessions_lock = asyncio.Lock()


def _safe_url(value: str, stage: str) -> str:
    parsed = urlparse(str(value or "").strip())
    if parsed.scheme != "https" or (parsed.hostname or "").lower() not in _ALLOWED_HOSTS:
        raise XaiBrowserFlowError("unsafe xAI browser URL", stage=stage)
    return parsed.geturl()


async def _session(session_id: str) -> BrowserSession:
    async with _sessions_lock:
        current = _sessions.get(str(session_id or "").strip())
    if current is None or time.monotonic() - current.touched_at > _SESSION_TTL:
        if current is not None:
            await close_session(session_id)
        raise XaiBrowserFlowError("browser session expired", stage="browser_session", retryable=True)
    current.touched_at = time.monotonic()
    return current


async def cleanup_expired_sessions() -> None:
    cutoff = time.monotonic() - _SESSION_TTL
    async with _sessions_lock:
        expired = [item for item in _sessions.values() if item.touched_at < cutoff]
        for item in expired:
            _sessions.pop(item.session_id, None)
    for item in expired:
        await item.close()


async def start_session(*, signup_url: str, proxy: str = "", timezone: str = "", locale: str = "", profile: dict | None = None, timeout_s: int = 180) -> dict:
    await cleanup_expired_sessions()
    target = _safe_url(signup_url, "browser_start")
    browser = await cloakbrowser.launch_async(**browser_kwargs("TURNSTILE", proxy or None, timezone=timezone or None, locale=locale or None, profile=profile))
    try:
        page = await browser.new_page()
        response = await page.goto(target, wait_until="domcontentloaded", timeout=max(30_000, min(180_000, int(timeout_s) * 1000)))
        response_status = int(response.status) if response is not None else 0
        session_id = f"xai-{uuid.uuid4().hex}"
        current = BrowserSession(session_id=session_id, browser=browser, page=page, signup_url=target, initial_response_status=response_status)
        async with _sessions_lock:
            _sessions[session_id] = current
        return {"ok": True, "session_id": session_id, "url": page.url, "user_agent": await _user_agent(page), "cookies": await cookie_snapshot(page), "diagnostic": await _page_diagnostic(page, response_status=response_status)}
    except Exception:
        await browser.close()
        raise


async def _pause(page: Any, milliseconds: int) -> None:
    try:
        await page.wait_for_timeout(milliseconds)
    except Exception:
        await asyncio.sleep(milliseconds / 1000)


async def _body_text(page: Any) -> str:
    try:
        return str(await page.locator("body").inner_text(timeout=3_000) or "")
    except Exception:
        return ""


def _safe_page_url(raw: str) -> str:
    value = str(raw or "").strip()
    try:
        parsed = urlparse(value)
    except Exception:
        return "unknown"
    if parsed.scheme in {"http", "https"} and parsed.hostname:
        path = parsed.path or "/"
        return f"{parsed.scheme}://{parsed.hostname}{path}"[:256]
    if parsed.scheme == "chrome-error":
        return "chrome-error://page"
    return "unknown"


async def _page_diagnostic(page: Any, *, body: str | None = None, response_status: int = 0) -> dict:
    """Return bounded page metadata without persisting HTML, cookies or input."""
    url = _safe_page_url(getattr(page, "url", ""))
    try:
        title = re.sub(r"\s+", " ", str(await page.title() or "")).strip()[:160]
    except Exception:
        title = ""
    text = body if body is not None else await _body_text(page)
    normalized = re.sub(r"\s+", " ", str(text or "")).strip()
    lowered = f"{title} {normalized}".lower()
    proxy_error = url.startswith("chrome-error://") or bool(re.search(
        r"err_proxy|proxy(?: connection)? (?:failed|refused|timed out)|unable to connect to the proxy",
        lowered,
    ))
    cloudflare = bool(re.search(
        r"attention required.*cloudflare|just a moment|challenge-platform|cf-chl-|cloudflare",
        lowered,
    )) or (response_status in {403, 429} and "cloudflare" in lowered)
    reason = "proxy_connection_failed" if proxy_error else "cloudflare_challenge" if cloudflare else ""
    return {
        "url": url,
        "title": title,
        "http_status": int(response_status or 0),
        "cloudflare_challenge": cloudflare,
        "proxy_connection_failed": proxy_error,
        "reason": reason,
    }


def _page_issue(diagnostic: dict) -> str:
    reason = str(diagnostic.get("reason") or "").strip()
    return reason if reason in {"proxy_connection_failed", "cloudflare_challenge"} else ""


async def _first_visible(page: Any, selectors: tuple[str, ...]) -> Any | None:
    for selector in selectors:
        try:
            locator = page.locator(selector)
            for index in range(await locator.count()):
                candidate = locator.nth(index)
                if await candidate.is_visible():
                    return candidate
        except Exception:
            continue
    return None


async def _click_any(page: Any, names: tuple[str, ...]) -> bool:
    for name in names:
        try:
            button = page.get_by_role("button", name=name, exact=True).first
            if await button.is_visible():
                await button.click(timeout=3_000)
                return True
        except Exception:
            pass
        try:
            locator = page.locator(f'button:has-text("{name}"), [role="button"]:has-text("{name}"), input[type="submit"][value="{name}"]').first
            if await locator.is_visible():
                await locator.click(timeout=3_000, force=True)
                return True
        except Exception:
            pass
    return False


async def _click_submit(page: Any, names: tuple[str, ...]) -> bool:
    if await _click_any(page, names):
        return True
    button = await _first_visible(page, ('button[type="submit"]', 'input[type="submit"]'))
    if button is None:
        return False
    await button.click(timeout=5_000, force=True)
    return True


async def _fill_first(page: Any, selectors: tuple[str, ...], value: str) -> bool:
    field = await _first_visible(page, selectors)
    if field is None:
        return False
    await field.fill(str(value))
    return True


async def _set_native_input_value(page: Any, selector: str, value: str) -> bool:
    return bool(await page.evaluate("""([selector, value]) => {
      const field = document.querySelector(selector);
      if (!(field instanceof HTMLInputElement)) return false;
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set;
      if (setter) setter.call(field, value); else field.value = value;
      field.dispatchEvent(new Event('input', {bubbles: true}));
      field.dispatchEvent(new Event('change', {bubbles: true}));
      return true;
    }""", [selector, str(value)]))


async def _wait_for_email_verification(page: Any, timeout_s: int = 45) -> None:
    deadline = time.monotonic() + timeout_s
    last = ""
    while time.monotonic() < deadline:
        last = await _body_text(page)
        if await _first_visible(page, ('input[autocomplete="one-time-code"]', 'input[name="code"]', 'input[inputmode="numeric"]')):
            return
        if re.search(r"验证您的邮箱|验证码|Verify your email|verification code", last, re.I):
            return
        reason = _classify_email_stage_error(last)
        if reason:
            raise XaiBrowserFlowError(reason, stage="send_code", retryable=True)
        await _pause(page, 300)
    diagnostic = await _page_diagnostic(page, body=last)
    reason = _page_issue(diagnostic) or "verification_page_missing"
    raise XaiBrowserFlowError(reason, stage="send_code", retryable=True, diagnostic=diagnostic)


def _classify_email_stage_error(text: str) -> str | None:
    """Return a stable, non-sensitive reason for a rejected signup page.

    The previous implementation persisted a slice of page text.  That made
    the Go task journal noisy and could accidentally retain user-facing page
    content.  Stable reason codes are enough to distinguish rate limits,
    duplicate emails and unavailable signup pages while keeping diagnostics
    safe to persist.
    """
    normalized = re.sub(r"\s+", " ", str(text or "")).strip()
    if not normalized:
        return None
    if re.search(r"请求过于频繁|too many|rate limit|rate limited|try again later", normalized, re.I):
        return "signup_rate_limited"
    if re.search(r"already.*email|email.*already|已存在|已注册|已经注册", normalized, re.I):
        return "email_already_registered"
    if re.search(r"无法注册|signup.*unavailable|cannot.*sign up|can't.*sign up|registration.*unavailable", normalized, re.I):
        return "signup_unavailable"
    return None


async def send_email(session_id: str, email: str) -> dict:
    current = await _session(session_id)
    async with current.lock:
        page = current.page
        await _click_any(page, ("Reject all", "Accept all", "全部拒绝", "接受全部"))
        field = await _first_visible(page, ('input[type="email"]', 'input[name="email"]', 'input[autocomplete="email"]'))
        # A mailbox timeout may trigger a resend on the already-open code
        # page.  Keep the same browser session instead of restarting it.
        if field is None and await _first_visible(page, ('input[autocomplete="one-time-code"]', 'input[name="code"]', 'input[inputmode="numeric"]')):
            await _click_any(page, ("Resend code", "Send again", "重新发送验证码", "重新发送"))
            return {"ok": True, "url": page.url, "cookies": await cookie_snapshot(page), "user_agent": await _user_agent(page)}
        deadline = time.monotonic() + 30
        while field is None and time.monotonic() < deadline:
            if await _click_any(page, ("Sign up with email", "使用邮箱注册")):
                await _pause(page, 700)
            field = await _first_visible(page, ('input[type="email"]', 'input[name="email"]', 'input[autocomplete="email"]'))
            if field is None:
                await _pause(page, 400)
        if field is None:
            diagnostic = await _page_diagnostic(page)
            raise XaiBrowserFlowError("email_input_missing", stage="send_code", retryable=True, diagnostic=diagnostic)
        await field.fill(str(email).strip())
        if not await _click_submit(page, ("Sign up", "Continue", "注册", "继续")):
            diagnostic = await _page_diagnostic(page)
            raise XaiBrowserFlowError("email_submit_button_missing", stage="send_code", retryable=True, diagnostic=diagnostic)
        await _wait_for_email_verification(page)
        return {"ok": True, "url": page.url, "cookies": await cookie_snapshot(page), "user_agent": await _user_agent(page)}


async def _fill_code(page: Any, code: str) -> bool:
    normalized = re.sub(r"[^A-Za-z0-9]", "", str(code or ""))
    selectors = ('input[autocomplete="one-time-code"]', 'input[name="code"]', 'input[inputmode="numeric"]', 'input[inputmode="text"]', 'input[type="text"]')
    for selector in selectors:
        try:
            locator = page.locator(selector)
            visible = [locator.nth(i) for i in range(await locator.count()) if await locator.nth(i).is_visible()]
        except Exception:
            continue
        if not visible:
            continue
        if len(visible) == 1:
            await visible[0].fill(normalized)
        else:
            for field, char in zip(visible, normalized):
                await field.fill(char)
        return True
    return False


async def _wait_for_profile(page: Any, timeout_s: int = 45) -> None:
    deadline = time.monotonic() + timeout_s
    last = ""
    while time.monotonic() < deadline:
        if await _first_visible(page, ('input[name="givenName"]', 'input[autocomplete="given-name"]')):
            return
        last = await _body_text(page)
        if re.search(r"验证码.*(无效|错误|过期)|invalid.*code|expired.*code", last, re.I):
            raise XaiBrowserFlowError("verification_code_invalid", stage="validate_code")
        await _pause(page, 300)
    diagnostic = await _page_diagnostic(page, body=last)
    reason = _page_issue(diagnostic) or "profile_form_missing"
    raise XaiBrowserFlowError(reason, stage="validate_code", retryable=True, diagnostic=diagnostic)


async def verify_email(session_id: str, email: str, code: str) -> dict:
    del email
    current = await _session(session_id)
    async with current.lock:
        if not await _fill_code(current.page, code):
            raise XaiBrowserFlowError("verification code input not found", stage="validate_code", retryable=True)
        await _click_any(current.page, ("Confirm email", "Verify email", "确认邮箱", "验证邮箱", "Continue", "继续"))
        await _wait_for_profile(current.page)
        return {"ok": True, "url": current.page.url, "cookies": await cookie_snapshot(current.page), "user_agent": await _user_agent(current.page)}


async def _native_turnstile_token(page: Any, timeout_s: int = 45) -> str:
    deadline = time.monotonic() + timeout_s
    clicks = 0
    while time.monotonic() < deadline:
        try:
            token = await page.evaluate("""() => window.__cfmGrokTurnstileToken || document.querySelector('[name=cf-turnstile-response]')?.value || ''""")
        except Exception:
            token = ""
        if token:
            return str(token)
        if clicks < 3 and await _click_turnstile_checkbox(page):
            clicks += 1
        await _pause(page, 500)
    raise XaiBrowserFlowError("turnstile_incomplete", stage="turnstile", retryable=True)


async def _wait_for_success(page: Any, timeout_s: int = 75) -> None:
    deadline = time.monotonic() + timeout_s
    last = ""
    while time.monotonic() < deadline:
        parsed = urlparse(str(page.url or ""))
        last = await _body_text(page)
        if parsed.path.startswith("/account") and "/sign-" not in parsed.path:
            return
        if re.search(r"管理您的账户|欢迎|Manage your account|Welcome", last, re.I) and not re.search(r"完成注册|Complete your sign up", last, re.I):
            return
        reason = _classify_email_stage_error(last)
        if reason:
            raise XaiBrowserFlowError(reason, stage="signup")
        await _pause(page, 500)
    diagnostic = await _page_diagnostic(page, body=last)
    reason = _page_issue(diagnostic) or "account_page_missing"
    raise XaiBrowserFlowError(reason, stage="signup", retryable=True, diagnostic=diagnostic)


async def signup(session_id: str, *, email: str, password: str, code: str, given_name: str, family_name: str, turnstile_token: str = "") -> dict:
    del email, code
    current = await _session(session_id)
    async with current.lock:
        page = current.page
        fields = [
            (("input[name=givenName]", 'input[autocomplete="given-name"]'), given_name),
            (("input[name=familyName]", 'input[autocomplete="family-name"]'), family_name),
            (("input[name=password]", 'input[type=password]', 'input[autocomplete="new-password"]'), password),
        ]
        for selectors, value in fields:
            if not await _fill_first(page, selectors, value):
                raise XaiBrowserFlowError("signup_profile_field_missing", stage="signup", retryable=True)
        await _set_native_input_value(page, "input[name=password]", password)
        await _pause(page, 900)
        token = await _native_turnstile_token(page)
        if not token and turnstile_token:
            token = turnstile_token
        if not await _click_submit(page, ("Complete sign up", "Create account", "Continue", "完成注册", "创建账户", "继续", "Submit")):
            raise XaiBrowserFlowError("signup_submit_button_missing", stage="signup", retryable=True)
        await _wait_for_success(page)
        cookies = await cookie_snapshot(page)
        sso = _cookie_value(cookies, "sso", "sso-rw")
        if not sso:
            raise XaiBrowserFlowError("account page has no xAI SSO cookie", stage="sso", retryable=True)
        return {"ok": True, "sso": sso, "sso_rw": _cookie_value(cookies, "sso-rw"), "redirect_url": page.url, "cookies": cookies, "user_agent": await _user_agent(page)}


async def _user_agent(page: Any) -> str:
    try:
        return str(await page.evaluate("() => navigator.userAgent") or "")
    except Exception:
        return ""


async def cookie_snapshot(page: Any) -> list[dict]:
    output = []
    for item in await page.context.cookies():
        domain = str(item.get("domain") or "").lower().lstrip(".")
        if domain and not any(domain == suffix or domain.endswith("." + suffix) for suffix in _COOKIE_SUFFIXES):
            continue
        output.append({"name": str(item.get("name") or ""), "value": str(item.get("value") or ""), "domain": str(item.get("domain") or ""), "path": str(item.get("path") or "/"), "secure": bool(item.get("secure", True)), "httpOnly": bool(item.get("httpOnly", False)), "sameSite": str(item.get("sameSite") or "")})
    return output[:128]


def _cookie_value(cookies: list[dict], *names: str) -> str:
    for name in names:
        for item in cookies:
            if item.get("name") == name and item.get("value"):
                return str(item["value"])
    return ""


async def close_session(session_id: str) -> bool:
    async with _sessions_lock:
        current = _sessions.pop(str(session_id or "").strip(), None)
    if current is None:
        return False
    await current.close()
    return True


__all__ = ["XaiBrowserFlowError", "cleanup_expired_sessions", "start_session", "send_email", "verify_email", "signup", "close_session", "cookie_snapshot"]
