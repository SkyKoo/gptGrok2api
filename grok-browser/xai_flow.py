"""Native xAI signup flow kept inside the CFM Grok browser worker.

The page, Castle state, Turnstile widget, cookies and final SSO must belong to
one browser context.  This module intentionally keeps that context alive while
the Go registration engine waits for the mailbox code.
"""
from __future__ import annotations

import asyncio
import json
import re
import time
import uuid
from collections import deque
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import urlparse

import cloakbrowser

from signup_errors import REVIEW_REASONS
from network_diagnostics import NetworkDiagnostics

from common.browser import browser_kwargs, native_browser_context_kwargs
from turnstile.solve import _click_turnstile_checkbox

_ALLOWED_HOSTS = {"accounts.x.ai", "auth.x.ai", "auth.grokusercontent.com", "grok.com", "www.grok.com"}
_COOKIE_SUFFIXES = ("x.ai", "grok.com", "grokusercontent.com")
_SESSION_TTL = 20 * 60

# Capture the callback and widget id before the xAI page renders Turnstile.
# The native page owns the widget, so reading only a top-level hidden input is
# unreliable when the response is kept inside the challenge iframe.  This
# wrapper preserves the page callback and records only bounded, non-secret
# state in window properties for the worker to inspect later.
_TURNSTILE_CAPTURE_INIT_JS = r"""
(() => {
  window.__cfmGrokTurnstileToken = '';
  window.__cfmGrokTurnstileWidgetId = '';
  window.__cfmGrokTurnstileCallbackSeen = false;
  const install = () => {
    const api = window.turnstile;
    if (!api || typeof api.render !== 'function' || api.render.__cfmWrapped) return;
    const original = api.render.bind(api);
    const wrapped = (container, options = {}) => {
      const next = {...options};
      const callback = next.callback;
      next.callback = (token) => {
        window.__cfmGrokTurnstileCallbackSeen = true;
        window.__cfmGrokTurnstileToken = String(token || '');
        if (typeof callback === 'function') return callback(token);
      };
      const widgetId = original(container, next);
      window.__cfmGrokTurnstileWidgetId = String(widgetId ?? '');
      return widgetId;
    };
    wrapped.__cfmWrapped = true;
    try { api.render = wrapped; } catch (_) {}
  };
  install();
  // The page/context owns this timer. Keep capturing while the user waits
  // for email verification; closing the context releases the timer.
  setInterval(install, 100);
})();
"""


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
    context: Any
    page: Any
    signup_url: str
    initial_response_status: int = 0
    touched_at: float = field(default_factory=time.monotonic)
    lock: asyncio.Lock = field(default_factory=asyncio.Lock)

    async def close(self) -> None:
        try:
            await self.page.close()
        except Exception:
            pass
        try:
            await self.context.close()
        except Exception:
            pass
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
    browser = await cloakbrowser.launch_async(
        **browser_kwargs("TURNSTILE", proxy or None, timezone=timezone or None, locale=locale or None, profile=profile)
    )
    context = None
    try:
        context = await browser.new_context(**native_browser_context_kwargs())
        page = await context.new_page()
        await page.add_init_script(_TURNSTILE_CAPTURE_INIT_JS)
        page._cfm_network_diagnostics = NetworkDiagnostics()
        page.on("request", page._cfm_network_diagnostics.started)
        page.on("requestfinished", page._cfm_network_diagnostics.finished)
        page.on("requestfailed", page._cfm_network_diagnostics.failed)
        page.on("response", page._cfm_network_diagnostics.response)
        response = await page.goto(target, wait_until="domcontentloaded", timeout=max(30_000, min(180_000, int(timeout_s) * 1000)))
        response_status = int(response.status) if response is not None else 0
        session_id = f"xai-{uuid.uuid4().hex}"
        current = BrowserSession(session_id=session_id, browser=browser, context=context, page=page, signup_url=target, initial_response_status=response_status)
        async with _sessions_lock:
            _sessions[session_id] = current
        return {"ok": True, "session_id": session_id, "url": page.url, "user_agent": await _user_agent(page), "cookies": await cookie_snapshot(page), "diagnostic": await _page_diagnostic(page, response_status=response_status)}
    except Exception:
        if context is not None:
            try:
                await context.close()
            except Exception:
                pass
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


async def _visible_control_summary(page: Any) -> dict:
    """Return names and labels of visible controls, never their values."""
    try:
        result = await page.evaluate("""() => {
          const visible = (element) => {
            const rect = element.getBoundingClientRect();
            const style = window.getComputedStyle(element);
            return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none';
          };
          const inputs = Array.from(document.querySelectorAll('input')).filter(visible).slice(0, 16).map((element) => ({
            type: element.getAttribute('type') || '',
            name: element.getAttribute('name') || '',
            autocomplete: element.getAttribute('autocomplete') || '',
            inputmode: element.getAttribute('inputmode') || '',
            aria: element.getAttribute('aria-label') || '',
          }));
          const buttons = Array.from(document.querySelectorAll('button, [role="button"], input[type="submit"]')).filter(visible).slice(0, 16).map((element) => ({
            type: element.getAttribute('type') || '',
            aria: element.getAttribute('aria-label') || '',
            text: element.tagName === 'INPUT' ? (element.getAttribute('value') || '') : (element.innerText || ''),
          })).map((item) => ({ ...item, text: String(item.text || '').replace(/\\s+/g, ' ').trim().slice(0, 100) }));
          return { inputs, buttons };
        }""")
        return result if isinstance(result, dict) else {}
    except Exception:
        return {}


def _upstream_email_error(code: str) -> tuple[str, str] | None:
    """Map known xAI email-submit errors to safe stable diagnostics."""
    normalized = re.sub(r"\s+", "", str(code or "").strip().lower())
    known = {
        "account:email-signup-unavailable": ("signup_unavailable", "email_signup_unavailable"),
        "account:email-already-exists": ("email_already_registered", "email_already_exists"),
        "account:email-invalid": ("email_invalid", "email_invalid"),
        "account:rate-limited": ("signup_rate_limited", "rate_limited"),
    }
    return known.get(normalized)


async def _capture_email_submit_response(response: Any, state: dict) -> None:
    """Capture only a bounded, allow-listed xAI response code."""
    try:
        parsed = urlparse(str(response.url or ""))
        request = getattr(response, "request", None)
        method = str(getattr(request, "method", "") or "").upper()
        if method not in {"POST", "PUT", "PATCH"}:
            return
        if (parsed.hostname or "").lower() != "accounts.x.ai" or int(response.status) < 400:
            return
        payload = json.loads(await response.text())
        code = payload.get("code") if isinstance(payload, dict) else ""
        mapped = _upstream_email_error(str(code))
        if mapped is None:
            return
        reason, safe_code = mapped
        state.update({
            "reason": reason,
            "code": safe_code,
            "status": int(response.status),
            "method": method,
            "path": (parsed.path or "/")[:160],
        })
    except Exception:
        return


class _EmailSubmitCapture:
    """Associate xAI error responses with the current email submit action."""

    def __init__(self, state: dict):
        self.state = state
        self.armed = False
        self._requests: set[int] = set()
        self._tasks: set[asyncio.Task] = set()

    def arm(self) -> None:
        self.armed = True

    def disarm(self) -> None:
        self.armed = False

    def request(self, request: Any) -> None:
        if not self.armed:
            return
        try:
            parsed = urlparse(str(request.url or ""))
            method = str(request.method or "").upper()
            if (parsed.hostname or "").lower() == "accounts.x.ai" and method in {"POST", "PUT", "PATCH"}:
                self._requests.add(id(request))
        except Exception:
            return

    def response(self, response: Any) -> None:
        request = getattr(response, "request", None)
        if id(request) not in self._requests:
            return
        task = asyncio.create_task(_capture_email_submit_response(response, self.state))
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    async def drain(self) -> None:
        if self._tasks:
            await asyncio.gather(*tuple(self._tasks), return_exceptions=True)


async def _page_diagnostic(page: Any, *, body: str | None = None, response_status: int = 0, upstream: dict | None = None) -> dict:
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
    reason = "proxy_connection_failed" if proxy_error else "cloudflare_challenge" if cloudflare else _classify_email_stage_error(normalized) or ""
    controls = await _visible_control_summary(page)
    diagnostic = {
        "url": url,
        "title": title,
        "http_status": int(response_status or 0),
        "cloudflare_challenge": cloudflare,
        "proxy_connection_failed": proxy_error,
        "reason": reason,
        "visible_controls": controls,
    }
    network = getattr(page, "_cfm_network_diagnostics", None)
    if isinstance(network, NetworkDiagnostics):
        diagnostic["network_failures"] = network.snapshot()
        diagnostic["page_scripts"] = network.loading_snapshot()
    entry = getattr(page, "_cfm_email_entry", None)
    if isinstance(entry, dict):
        diagnostic["email_entry"] = dict(entry)
    if isinstance(upstream, dict):
        if upstream.get("status"):
            diagnostic["upstream_status"] = int(upstream["status"])
        if upstream.get("code"):
            diagnostic["upstream_code"] = str(upstream["code"])
        if upstream.get("method"):
            diagnostic["upstream_method"] = str(upstream["method"])
        if upstream.get("path"):
            diagnostic["upstream_path"] = str(upstream["path"])
        if upstream.get("reason"):
            diagnostic["reason"] = str(upstream["reason"])
    return diagnostic


def _page_issue(diagnostic: dict) -> str:
    reason = str(diagnostic.get("reason") or "").strip()
    return reason if reason in {
        "proxy_connection_failed",
        "cloudflare_challenge",
        "signup_rate_limited",
        "email_already_registered",
        "signup_unavailable",
        "email_invalid",
    } else ""


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
    try:
        await button.click(timeout=5_000)
    except Exception:
        # Keep the fallback for unusual xAI markup, but prefer a normal
        # user-like click whenever the submit control is visible.
        await button.click(timeout=5_000, force=True)
    return True


async def _fill_email_like_user(field: Any, email: str) -> None:
    value = str(email or "").strip()
    try:
        await field.click(timeout=5_000)
        await field.fill("")
        press_sequentially = getattr(field, "press_sequentially", None)
        if callable(press_sequentially):
            await press_sequentially(value, delay=35)
            return
    except Exception:
        pass
    await field.fill(value)


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


async def _wait_for_email_verification(page: Any, timeout_s: int = 45, upstream: dict | None = None) -> None:
    deadline = time.monotonic() + timeout_s
    last = ""
    while time.monotonic() < deadline:
        if isinstance(upstream, dict) and upstream.get("reason"):
            diagnostic = await _page_diagnostic(page, upstream=upstream)
            raise XaiBrowserFlowError(str(upstream["reason"]), stage="send_code", retryable=upstream["reason"] not in REVIEW_REASONS, diagnostic=diagnostic)
        last = await _body_text(page)
        if await _first_visible(page, ('input[autocomplete="one-time-code"]', 'input[name="code"]', 'input[name="verificationCode"]', 'input[name="verification_code"]', 'input[inputmode="numeric"]')):
            return
        if re.search(r"验证您的邮箱|验证码|Verify your email|verification code", last, re.I):
            return
        reason = await _submitted_email_error(page) or _classify_email_stage_error(last)
        if reason:
            diagnostic = await _page_diagnostic(page, body=last, upstream=upstream)
            raise XaiBrowserFlowError(reason, stage="send_code", retryable=reason not in REVIEW_REASONS, diagnostic=diagnostic)
        await _pause(page, 300)
    diagnostic = await _page_diagnostic(page, body=last, upstream=upstream)
    reason = _page_issue(diagnostic) or "verification_page_missing"
    raise XaiBrowserFlowError(reason, stage="send_code", retryable=reason not in REVIEW_REASONS, diagnostic=diagnostic)


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
    if re.search(
        r"无法注册|signup.*unavailable|sign[- ]?up.*(?:isn['’]t|is not|not)\s+available|"
        r"(?:email|e-mail).*sign[- ]?up.*(?:isn['’]t|is not|not)\s+available|"
        r"cannot.*sign up|can't.*sign up|registration.*unavailable",
        normalized,
        re.I,
    ):
        return "signup_unavailable"
    return None


_EMAIL_INPUTS = ('input[type="email"]', 'input[name="email"]', 'input[autocomplete="email"]')

# xAI renders the entry button before React attaches its handler. Checking
# visibility/actionability alone accepts clicks which the page silently drops.
_EMAIL_ENTRY_STATE_JS = """element => {
  let handler = false;
  for (let node = element; node && node !== document; node = node.parentElement) {
    if (typeof node.onclick === 'function') handler = true;
    for (const key of Object.keys(node)) {
      if (!key.startsWith('__reactProps')) continue;
      const props = node[key];
      if (typeof props?.onClick === 'function') handler = true;
    }
  }
  return {document_state: document.readyState, handler_ready: handler,
    disabled: !!element.disabled || element.getAttribute('aria-disabled') === 'true'};
}"""


async def _wait_for_email_form(page: Any, timeout_s: float = 30) -> Any:
    """Wait for a live entry handler and verify the resulting email form.

    This helper never fills or submits a mailbox and never force-clicks an
    obscured entry. A click is only progress; the visible input is success.
    """
    started = time.monotonic()
    deadline = started + timeout_s
    samples: deque = deque(maxlen=8)
    progress = {"click_attempts": 0, "clicks": 0, "last_click_error": "", "form_visible": False}
    page._cfm_email_entry = progress
    next_consent = started
    while time.monotonic() < deadline:
        field = await _first_visible(page, _EMAIL_INPUTS)
        if field is not None:
            progress.update(form_visible=True, elapsed_ms=int((time.monotonic() - started) * 1000))
            return field
        # Consent may arrive after DOMContentLoaded; retry its normal dismissal
        # while still on the entry page, rather than clicking through its overlay.
        if time.monotonic() >= next_consent:
            await _dismiss_entry_consent(page, deadline)
            next_consent = time.monotonic() + 2
        button = await _first_visible(page, ('button:has-text("Sign up with email")', 'button:has-text("使用邮箱注册")', '[role="button"]:has-text("Sign up with email")'))
        if button is not None:
            try:
                state = await button.evaluate(_EMAIL_ENTRY_STATE_JS, timeout=max(1, min(1000, int((deadline - time.monotonic()) * 1000))))
                if not samples or state != samples[-1]["state"]:
                    samples.append({"elapsed_ms": int((time.monotonic() - started) * 1000), "state": state})
                progress["states"] = list(samples)
                remaining = deadline - time.monotonic()
                if state.get("handler_ready") and not state.get("disabled") and remaining > 0:
                    progress["click_attempts"] += 1
                    await button.click(timeout=max(1, min(1500, int(remaining * 1000))))
                    progress["clicks"] += 1
            except Exception as exc:
                # Exception messages can contain URLs or page content.
                progress["last_click_error"] = "timeout" if type(exc).__name__ == "TimeoutError" else "interaction_error"
        remaining = deadline - time.monotonic()
        if remaining > 0:
            await _pause(page, min(400, max(1, int(remaining * 1000))))
    field = await _first_visible(page, _EMAIL_INPUTS)
    progress.update(form_visible=field is not None, elapsed_ms=int((time.monotonic() - started) * 1000))
    if field is not None:
        return field
    diagnostic = await _page_diagnostic(page)
    raise XaiBrowserFlowError("email_input_missing", stage="send_code", retryable=True, diagnostic=diagnostic)


async def _dismiss_entry_consent(page: Any, deadline: float) -> None:
    for name in ("Reject all", "Accept all", "全部拒绝", "接受全部"):
        if time.monotonic() >= deadline:
            return
        try:
            button = page.get_by_role("button", name=name, exact=True).first
            if await button.is_visible():
                await button.click(timeout=max(1, min(1000, int((deadline - time.monotonic()) * 1000))))
                return
        except Exception:
            continue


async def _submitted_email_error(page: Any) -> str | None:
    """Read duplicate-mailbox errors only from the submitted form's feedback."""
    try:
        text = await page.evaluate(r"""() => {
          const visible = e => e && e.getBoundingClientRect().width > 0 && getComputedStyle(e).visibility !== 'hidden';
          const email = [...document.querySelectorAll('input[type="email"], input[name="email"], input[autocomplete="email"]')].find(visible);
          if (!email) return '';
          const nodes = [...(email.form || email.parentElement).querySelectorAll('[role="alert"], [aria-live="assertive"]')];
          if (email.getAttribute('aria-invalid') === 'true') {
            for (const id of (email.getAttribute('aria-describedby') || '').split(/\s+/)) {
              if (id) nodes.push(document.getElementById(id));
            }
          }
          return nodes.filter(visible).slice(0, 8).map(e => e.innerText || '').join(' ').slice(0, 2000);
        }""")
    except Exception:
        return None
    # Keep nouns and rejection adjacent. "Sign up with email ... Already have
    # an account?" is navigation copy, not an error, even inside a live region.
    if re.search(r"\b(?:e-?mail(?: address)?|account)\s+(?:(?:is|has)\s+)?already\s+(?:registered|exists|in use|been (?:registered|used)|used|taken)\b|(?:电子)?邮箱(?:地址)?(?:已存在|已注册|已经注册|已被注册)", str(text or ""), re.I):
        return "email_already_registered"
    return None


async def send_email(session_id: str, email: str) -> dict:
    current = await _session(session_id)
    async with current.lock:
        page = current.page
        await _click_any(page, ("Reject all", "Accept all", "全部拒绝", "接受全部"))
        field = await _first_visible(page, ('input[type="email"]', 'input[name="email"]', 'input[autocomplete="email"]'))
        # A mailbox timeout may trigger a resend on the already-open code
        # page.  Keep the same browser session instead of restarting it.
        if field is None and await _first_visible(page, ('input[autocomplete="one-time-code"]', 'input[name="code"]', 'input[name="verificationCode"]', 'input[name="verification_code"]', 'input[inputmode="numeric"]')):
            upstream: dict = {}
            capture = _EmailSubmitCapture(upstream)
            page.on("request", capture.request)
            page.on("response", capture.response)
            try:
                capture.arm()
                if not await _click_any(page, ("Resend code", "Send again", "重新发送验证码", "重新发送")):
                    diagnostic = await _page_diagnostic(page)
                    raise XaiBrowserFlowError("resend_button_missing", stage="send_code", retryable=True, diagnostic=diagnostic)
                await _pause(page, 250)
                await _wait_for_email_verification(page, upstream=upstream)
                await capture.drain()
                if upstream.get("reason"):
                    diagnostic = await _page_diagnostic(page, upstream=upstream)
                    raise XaiBrowserFlowError(str(upstream["reason"]), stage="send_code", retryable=upstream["reason"] not in REVIEW_REASONS, diagnostic=diagnostic)
            finally:
                capture.disarm()
                await capture.drain()
                try:
                    page.remove_listener("request", capture.request)
                    page.remove_listener("response", capture.response)
                except Exception:
                    pass
            return {"ok": True, "url": page.url, "cookies": await cookie_snapshot(page), "user_agent": await _user_agent(page)}
        if field is None:
            field = await _wait_for_email_form(page)
        upstream: dict = {}
        capture = _EmailSubmitCapture(upstream)
        page.on("request", capture.request)
        page.on("response", capture.response)
        try:
            await _fill_email_like_user(field, email)
            # Give React/Castle time to consume the same input and focus
            # events that a real Chrome user produces before submitting.
            await _pause(page, 900)
            capture.arm()
            if not await _click_submit(page, ("Sign up", "Continue", "注册", "继续")):
                diagnostic = await _page_diagnostic(page)
                raise XaiBrowserFlowError("email_submit_button_missing", stage="send_code", retryable=True, diagnostic=diagnostic)
            # The submit request can return a structured xAI error while the
            # page remains on the email form. Capture it before the generic
            # verification-page timeout masks the actual cause.
            await _pause(page, 250)
            await _wait_for_email_verification(page, upstream=upstream)
        finally:
            capture.disarm()
            await capture.drain()
            try:
                page.remove_listener("request", capture.request)
                page.remove_listener("response", capture.response)
            except Exception:
                pass
        if upstream.get("reason"):
            diagnostic = await _page_diagnostic(page, upstream=upstream)
            raise XaiBrowserFlowError(str(upstream["reason"]), stage="send_code", retryable=upstream["reason"] not in REVIEW_REASONS, diagnostic=diagnostic)
        return {"ok": True, "url": page.url, "cookies": await cookie_snapshot(page), "user_agent": await _user_agent(page)}


async def _fill_code(page: Any, code: str) -> bool:
    normalized = re.sub(r"[^A-Za-z0-9]", "", str(code or ""))
    selectors = ('input[autocomplete="one-time-code"]', 'input[name="code"]', 'input[name="verificationCode"]', 'input[name="verification_code"]', 'input[inputmode="numeric"]', 'input[inputmode="text"]', 'input[type="text"]')
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
        if await _first_visible(page, ('input[name="givenName"]', 'input[name="firstName"]', 'input[autocomplete="given-name"]')):
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
            diagnostic = await _page_diagnostic(current.page)
            raise XaiBrowserFlowError("verification_code_input_missing", stage="validate_code", retryable=True, diagnostic=diagnostic)
        if not await _click_submit(current.page, ("Confirm email", "Verify email", "Verify email address", "Confirm", "Submit code", "确认邮箱", "验证邮箱", "继续", "Continue")):
            diagnostic = await _page_diagnostic(current.page)
            raise XaiBrowserFlowError("verification_submit_button_missing", stage="validate_code", retryable=True, diagnostic=diagnostic)
        await _wait_for_profile(current.page)
        return {"ok": True, "url": current.page.url, "cookies": await cookie_snapshot(current.page), "user_agent": await _user_agent(current.page)}


_TURNSTILE_DOM_HELPERS_JS = r"""
  const roots = [document];
  for (let i = 0; i < roots.length; i++) {
    for (const element of roots[i].querySelectorAll('*')) {
      if (element.shadowRoot) roots.push(element.shadowRoot);
    }
  }
  const queryAll = (selector) => roots.flatMap((root) => Array.from(root.querySelectorAll(selector)));
  const readToken = () => {
    const id = String(window.__cfmGrokTurnstileWidgetId ?? '');
    let response = '';
    const api = window.turnstile;
    if (typeof api?.getResponse === 'function') {
      try { response = String((id ? api.getResponse(id) : api.getResponse()) || ''); } catch (_) {}
      if (!response && id) {
        try { response = String(api.getResponse() || ''); } catch (_) {}
      }
    }
    return response || String(window.__cfmGrokTurnstileToken || '') || queryAll(
      'input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]'
    ).map((element) => String(element.value || '')).find(Boolean) || '';
  };
"""

_TURNSTILE_RESPONSE_JS = "() => {" + _TURNSTILE_DOM_HELPERS_JS + "return readToken(); }"

_TURNSTILE_STATE_JS = "() => {" + _TURNSTILE_DOM_HELPERS_JS + r"""
  const visible = (element) => {
    const rect = element.getBoundingClientRect();
    const style = window.getComputedStyle(element);
    return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none';
  };
  const frames = queryAll('iframe').filter((element) => {
    try {
      const host = new URL(element.src).hostname;
      return host === 'challenges.cloudflare.com' || host.endsWith('.challenges.cloudflare.com');
    } catch (_) { return false; }
  });
  return {
    token_present: Boolean(readToken()),
    callback_seen: Boolean(window.__cfmGrokTurnstileCallbackSeen),
    widget_id_present: Boolean(String(window.__cfmGrokTurnstileWidgetId ?? '')),
    widget_count: queryAll('[data-sitekey], .cf-turnstile, [data-cfm-grok-turnstile]').length,
    iframe_count: frames.length,
    visible_iframe_count: frames.filter(visible).length,
    visible_response_input_count: queryAll(
      'input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]'
    ).filter(visible).length,
    ready: Boolean(window.turnstile?.render && window.turnstile?.getResponse),
  };
}
"""


async def _turnstile_state(page: Any) -> dict:
    try:
        state = await page.evaluate(_TURNSTILE_STATE_JS)
    except Exception:
        state = {}
    if not isinstance(state, dict):
        state = {}
    # Playwright's frame tree also includes frames in closed shadow roots.
    frames, visible_frames = 0, 0
    for frame in getattr(page, "frames", ()):
        try:
            host = (urlparse(str(frame.url or "")).hostname or "").lower()
            if host != "challenges.cloudflare.com" and not host.endswith(".challenges.cloudflare.com"):
                continue
            frames += 1
            element = await frame.frame_element()
            if await element.is_visible():
                visible_frames += 1
        except Exception:
            continue
    state["iframe_count"] = max(int(state.get("iframe_count") or 0), frames)
    state["visible_iframe_count"] = max(int(state.get("visible_iframe_count") or 0), visible_frames)
    return state


async def _native_turnstile_token(page: Any, timeout_s: int = 90) -> str:
    deadline = time.monotonic() + timeout_s
    clicks = 0
    last_state: dict = {}
    while time.monotonic() < deadline:
        last_state = await _turnstile_state(page)
        if last_state.get("token_present"):
            # Read the value only after the bounded state probe confirms that a
            # token exists. The token is never included in diagnostics/logs.
            try:
                token = await page.evaluate(_TURNSTILE_RESPONSE_JS)
            except Exception:
                token = ""
            if token:
                return str(token)
        remaining = deadline - time.monotonic()
        if clicks < 4 and remaining > 0:
            try:
                clicked = await asyncio.wait_for(
                    _click_turnstile_checkbox(page, attempts=1, require_checkbox=True), timeout=min(3.0, remaining)
                )
            except asyncio.TimeoutError:
                clicked = False
            if clicked:
                clicks += 1
        remaining = deadline - time.monotonic()
        if remaining > 0:
            await _pause(page, min(500, int(remaining * 1000)))
    diagnostic = await _page_diagnostic(page)
    diagnostic.update({
        "reason": diagnostic.get("reason") or "turnstile_incomplete",
        "turnstile_clicks": clicks,
        "turnstile_frames": int(last_state.get("iframe_count") or 0),
        "turnstile_visible_frames": int(last_state.get("visible_iframe_count") or 0),
        "turnstile_widgets": int(last_state.get("widget_count") or 0),
        "turnstile_ready": bool(last_state.get("ready")),
        "turnstile_callback_seen": bool(last_state.get("callback_seen")),
        "turnstile_token_present": bool(last_state.get("token_present")),
    })
    raise XaiBrowserFlowError("turnstile_incomplete", stage="turnstile", retryable=False, diagnostic=diagnostic)


async def _signup_success_state(page: Any, body: str) -> dict[str, bool]:
    url = str(page.url or "")
    parsed = urlparse(url)
    host = (parsed.hostname or "").lower()
    trusted = parsed.scheme == "https" and host in _ALLOWED_HOSTS
    grok_landing = trusted and host in {"grok.com", "www.grok.com"} and parsed.path in {"", "/"}
    account_page = trusted and (
        (parsed.path.startswith("/account") and "/sign-" not in parsed.path)
        or bool(re.search(r"管理您的账户|欢迎|Manage your account|Welcome", body, re.I))
    )
    signup_form = bool(re.search(r"完成注册|Complete your sign up", body, re.I))
    sso_present = False
    if trusted:
        # Scope cookies to this URL: a cookie left on another auth domain does
        # not establish a session on the destination page. Never log values.
        cookies = await page.context.cookies([url])
        for item in cookies:
            domain = str(item.get("domain") or "").lower().lstrip(".")
            if not domain or not any(domain == suffix or domain.endswith("." + suffix) for suffix in _COOKIE_SUFFIXES):
                continue
            if host != domain and not host.endswith("." + domain):
                continue
            if item.get("name") in {"sso", "sso-rw"} and str(item.get("value") or "").strip():
                sso_present = True
                break
    return {
        "trusted_destination": trusted,
        "grok_landing": grok_landing,
        "account_page": account_page and not signup_form,
        "sso_cookie_present": sso_present,
        "signup_form_present": signup_form,
    }


async def _wait_for_success(page: Any, timeout_s: int = 75) -> None:
    deadline = time.monotonic() + timeout_s
    last = ""
    state: dict[str, bool] = {}
    while time.monotonic() < deadline:
        last = await _body_text(page)
        state = await _signup_success_state(page, last)
        if state["sso_cookie_present"] and not state["signup_form_present"] and (state["grok_landing"] or state["account_page"]):
            return
        reason = await _submitted_email_error(page) or _classify_email_stage_error(last)
        if reason:
            raise XaiBrowserFlowError(reason, stage="signup")
        await _pause(page, 500)
    diagnostic = await _page_diagnostic(page, body=last)
    diagnostic.update(state)
    reason = _page_issue(diagnostic) or "account_page_missing"
    raise XaiBrowserFlowError(reason, stage="signup", retryable=True, diagnostic=diagnostic)


async def signup(session_id: str, *, email: str, password: str, code: str, given_name: str, family_name: str, turnstile_token: str = "") -> dict:
    del email, code
    current = await _session(session_id)
    async with current.lock:
        page = current.page
        fields = [
            (("input[name=givenName]", "input[name=firstName]", 'input[autocomplete="given-name"]'), given_name),
            (("input[name=familyName]", "input[name=lastName]", 'input[autocomplete="family-name"]'), family_name),
            (("input[name=password]", 'input[type=password]', 'input[autocomplete="new-password"]'), password),
        ]
        for selectors, value in fields:
            if not await _fill_first(page, selectors, value):
                raise XaiBrowserFlowError("signup_profile_field_missing", stage="signup", retryable=True)
        await _set_native_input_value(page, 'input[name=password], input[type=password], input[autocomplete="new-password"]', password)
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
