"""Complete a CFM-created xAI device grant in an isolated server-side session.

No account signup, password entry, CAPTCHA solving or consent replay is performed.
The caller exchanges the device code and checks token identity before persistence.
"""
from __future__ import annotations

import asyncio
import re
import time
from dataclasses import dataclass
from urllib.parse import parse_qs, urlsplit

from common.browser import browser_kwargs, native_browser_context_kwargs

_TRUSTED = {"accounts.x.ai", "auth.x.ai"}
_CONTINUE = re.compile(r"^(Continue|继续)$", re.I)
_ALLOW = re.compile(r"^(Allow|Authorize|Approve|Allow access|授权|允许|确认授权|允许访问)$", re.I)
_EMAIL = re.compile(r"[A-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Z0-9-]+(?:\.[A-Z0-9-]+)+", re.I)
_CONSENT_CALLBACK_GRACE_SECONDS = 15
_NETWORK_CODE = re.compile(r"\b(ERR_[A-Z0-9_]+|TIMEOUT|TIMED_OUT|CANCELLED)\b")
_OPERATIONS = frozenset({
    "browser_start", "browser_context", "browser_cookies", "browser_page", "browser_routing",
    "page_navigation", "page_url", "read_body", "read_title", "form_count", "form_method",
    "code_count", "code_value", "button_count", "button_visible", "button_enabled",
    "device_code_click", "consent_click", "wait_callback", "browser_claim", "unknown",
})
_EXCEPTION_TYPES = frozenset({"timeout", "navigation", "page_closed", "type_error", "attribute_error",
                              "value_error", "strict_mode", "browser_error", "unknown"})
_CONSENT_STATES = frozenset({"not_attempted", "attempted", "unknown"})


def safe_task_id(value) -> str:
    return value if isinstance(value, str) and re.fullmatch(r"task_[A-Za-z0-9_.-]{1,64}", value) else ""


@dataclass
class FlowDiagnostics:
    """单次请求的观察状态；提交前置记录，异常和取消不能抹去提交事实。"""
    operation: str = "browser_start"
    consent_state: str = "not_attempted"
    page: object = None
    exception_type: str = ""

    def fields(self, exception=None) -> dict:
        try:
            page = self.page.url if self.page is not None else ""
        except Exception:
            page = ""
        return {
            "operation": self.operation if self.operation in _OPERATIONS else "unknown",
            "consent_state": self.consent_state if self.consent_state in _CONSENT_STATES else "unknown",
            "consent_submitted": self.consent_state == "attempted",
            "page_path": _page_path(page),
            **({"exception_type": _exception_type(exception)} if exception is not None else
               {"exception_type": self.exception_type} if self.exception_type in _EXCEPTION_TYPES else {}),
        }


def _exception_type(exc: BaseException) -> str:
    """原始错误只用于内存分类，不返回原文、动态类名或异常栈。"""
    if _is_timeout_error(exc):
        return "timeout"
    for error, category in ((TypeError, "type_error"), (AttributeError, "attribute_error"), (ValueError, "value_error")):
        if isinstance(exc, error):
            return category
    try:
        message = str(exc).lower()
    except Exception:
        return "unknown"
    if type(exc).__name__ == "TargetClosedError" or any(value in message for value in (
            "target page, context or browser has been closed", "page has been closed", "browser has been closed")):
        return "page_closed"
    if any(value in message for value in (
            "execution context was destroyed", "cannot find context with specified id", "frame was detached")):
        return "navigation"
    if "strict mode violation" in message:
        return "strict_mode"
    if type(exc).__module__.startswith("playwright.") and type(exc).__name__ == "Error":
        return "browser_error"
    return "unknown"


async def _observe(state: FlowDiagnostics, operation: str, action):
    state.operation = operation
    try:
        return await action()
    except Exception as exc:
        state.exception_type = _exception_type(exc)
        raise


def observation_failure(state: FlowDiagnostics, exc: BaseException, elapsed_ms: float = 0) -> dict:
    """明确未点击授权时结束失败；已尝试或未知时仍由调用方核对原 grant。"""
    timeout = _is_timeout_error(exc)
    diagnostic = _diagnostic(stage="page_observation", detail="observation_timeout" if timeout else "observation_failed",
                             elapsed_ms=elapsed_ms)
    diagnostic.update(state.fields(exc))
    return result("failed" if state.consent_state == "not_attempted" else "uncertain",
                  "oauth_page_observation_timeout" if timeout else "oauth_page_observation_failed", **diagnostic)


_SAFE_PATHS = {
    "/oauth2/device",
    "/oauth2/device/consent",
    "/oauth2/device/done",
    "/sign-in",
    "/sign-up",
    "/sso-select-account",
    "/mfa/verify",
    "/one-time-code",
    "/verify-email",
}
_RESULT_STAGES = frozenset({
    "browser_start", "browser_context", "browser_cookies", "browser_routing", "browser_claim", "worker_timeout",
    "page_navigation", "page_observation", "device_code_form", "device_code_submit",
    "consent_page", "consent_submit", "consent_callback", "unknown",
})
_RESULT_DETAILS = frozenset({
    "untrusted_destination", "login_required", "verification_required", "callback_received",
    "body_or_title_timeout", "body_or_title_failed", "challenge_detected", "invalid_form_method",
    "user_code_mismatch", "continue_click_timeout", "continue_click_failed", "identity_mismatch",
    "product_marker_missing", "allow_click_timeout", "allow_click_failed", "callback_wait_timeout",
    "consent_button_missing", "navigation_timeout", "navigation_failed", "authorization_timeout",
    "browser_exception", "queue_claim_failed", "worker_deadline", "observation_timeout", "observation_failed", "unknown",
})
_RESULT_CODES = frozenset({
    "", "oauth_session_required", "oauth_browser_unavailable", "oauth_worker_result_unknown",
    "oauth_consent_unrecognized", "oauth_login_required", "oauth_verification_required",
    "oauth_verification_url_invalid", "oauth_page_observation_timeout", "oauth_page_observation_failed",
    "oauth_device_code_click_timeout", "oauth_device_code_click_failed", "oauth_identity_mismatch",
    "oauth_consent_click_timeout", "oauth_consent_click_failed", "oauth_callback_timeout",
    "oauth_page_navigation_timeout", "oauth_page_navigation_failed", "oauth_authorization_timeout",
})


def safe_device_result(value: dict) -> dict:
    """在 HTTP/日志边界再次投影白名单，避免内部异常对象或动态文本外泄。"""
    def allowed(key, choices, fallback):
        candidate = value.get(key)
        return candidate if isinstance(candidate, str) and candidate in choices else fallback

    def bounded(key, maximum):
        number = value.get(key, 0)
        return max(0, min(number, maximum)) if type(number) is int else 0

    consent_state = allowed("consent_state", _CONSENT_STATES, "unknown")
    if value.get("consent_submitted") is True:
        consent_state = "attempted"
    codes = value.get("network_error_codes", "")
    if (not isinstance(codes, str) or len(codes) > 256 or len(codes.split(",")) > 8
            or (codes and any(not re.fullmatch(r"ERR_[A-Z0-9_]+|TIMEOUT|TIMED_OUT|CANCELLED|unknown", part)
                              for part in codes.split(",")))):
        codes = ""
    return {
        "status": allowed("status", {"approved", "manual_action_required", "failed", "uncertain"}, "uncertain"),
        "code": allowed("code", _RESULT_CODES, "oauth_worker_result_unknown"),
        "stage": allowed("stage", _RESULT_STAGES, "unknown"),
        "detail": allowed("detail", _RESULT_DETAILS, "unknown"),
        "elapsed_ms": bounded("elapsed_ms", 150000),
        "consent_state": consent_state,
        "consent_submitted": consent_state == "attempted",
        "operation": allowed("operation", _OPERATIONS, "unknown"),
        "exception_type": allowed("exception_type", _EXCEPTION_TYPES | {""}, "unknown"),
        "page_path": allowed("page_path", _SAFE_PATHS | {"<other>", "<invalid>"}, "<other>"),
        "network_failures": bounded("network_failures", 32),
        "network_error_codes": codes,
        **{key: value[key] for key in ("code_form_seen", "consent_page_seen", "consent_button_seen")
           if type(value.get(key)) is bool},
    }


def _page_path(value: str) -> str:
    """Return a fixed allowlisted xAI path for diagnostics, without queries."""
    try:
        path = urlsplit(value).path.rstrip("/") or "/"
    except ValueError:
        return "<invalid>"
    return path if path in _SAFE_PATHS else "<other>"


def _diagnostic(*, stage: str, detail: str, page: str = "", elapsed_ms: int = 0,
                consent_submitted: bool = False, **extra) -> dict:
    """Build bounded, non-secret worker diagnostics for the CFM log."""
    value = {
        "stage": stage,
        "detail": detail,
        "elapsed_ms": max(0, min(int(elapsed_ms), 150000)),
        "consent_submitted": bool(consent_submitted),
    }
    if page:
        value["page_path"] = _page_path(page)
    for key in ("code_form_seen", "consent_page_seen", "consent_button_seen"):
        if key in extra:
            value[key] = bool(extra[key])
    if "network_failures" in extra:
        value["network_failures"] = max(0, min(int(extra["network_failures"]), 32))
    if "network_error_codes" in extra:
        codes = str(extra["network_error_codes"] or "")
        if re.fullmatch(r"(?:ERR_[A-Z0-9_]+|TIMEOUT|TIMED_OUT|CANCELLED|unknown)(?:,(?:ERR_[A-Z0-9_]+|TIMEOUT|TIMED_OUT|CANCELLED|unknown))*", codes):
            value["network_error_codes"] = codes[:256]
    return value


def result(status: str, code: str = "", **diagnostic) -> dict:
    value = {"status": status, "code": code}
    value.update({key: item for key, item in diagnostic.items() if item not in (None, "")})
    return value


def _is_timeout_error(exc: BaseException) -> bool:
    """Recognize asyncio and Playwright timeout errors without logging text."""
    return isinstance(exc, asyncio.TimeoutError) or type(exc).__name__ == "TimeoutError"


def trusted_url(value: str) -> bool:
    try:
        u = urlsplit(value)
        return u.scheme == "https" and u.hostname in _TRUSTED and u.port in (None, 443) and not u.username and not u.password
    except ValueError:
        return False


def validate_device_url(value: str, code: str) -> None:
    u = urlsplit(value)
    if not trusted_url(value) or u.netloc != "accounts.x.ai" or u.path != "/oauth2/device" or u.fragment or parse_qs(u.query, keep_blank_values=True) != {"user_code": [code]} or not re.fullmatch(r"[A-Z0-9]{4}-?[A-Z0-9]{4}", code):
        raise ValueError("oauth_verification_url_invalid")


async def _visible_button(page, name, state):
    state.operation = "button_count"
    matches = page.get_by_role("button", name=name)
    candidates = []
    for index in range(await _observe(state, "button_count", lambda: matches.count())):
        item = matches.nth(index)
        if (await _observe(state, "button_visible", lambda: item.is_visible())
                and await _observe(state, "button_enabled", lambda: item.is_enabled())):
            candidates.append(item)
    return candidates[0] if len(candidates) == 1 else None


async def confirm_on_page(page, *, user_code: str, expected_email: str, timeout_s: float,
                          diagnostics: FlowDiagnostics | None = None) -> dict:
    state = diagnostics if diagnostics is not None else FlowDiagnostics()
    state.page = page
    started = time.monotonic()
    try:
        outcome = await _confirm_on_page(page, user_code=user_code, expected_email=expected_email,
                                         timeout_s=timeout_s, state=state)
        outcome.update(state.fields())
        return outcome
    except Exception as exc:
        return observation_failure(state, exc, (time.monotonic() - started) * 1000)


async def _confirm_on_page(page, *, user_code: str, expected_email: str, timeout_s: float, state: FlowDiagnostics) -> dict:
    """Allow only the known device form and one explicit consent on its route."""
    started = time.monotonic()
    deadline = started + timeout_s
    code_submitted = consent_submitted = False
    code_form_seen = consent_page_seen = consent_button_seen = False
    while time.monotonic() < deadline:
        state.operation = "page_url"
        if not trusted_url(page.url):
            return result("failed" if state.consent_state == "not_attempted" else "uncertain", "oauth_consent_unrecognized",
                          **_diagnostic(stage="page_navigation", detail="untrusted_destination", page=page.url,
                                        elapsed_ms=(time.monotonic() - started) * 1000,
                                        consent_submitted=consent_submitted, code_form_seen=code_form_seen,
                                        consent_page_seen=consent_page_seen, consent_button_seen=consent_button_seen))
        path = urlsplit(page.url).path.rstrip("/")
        if path in ("/sign-in", "/sign-up", "/sso-select-account"):
            return result("manual_action_required", "oauth_login_required",
                          **_diagnostic(stage="page_navigation", detail="login_required", page=page.url,
                                        elapsed_ms=(time.monotonic() - started) * 1000,
                                        consent_submitted=consent_submitted))
        if path.startswith("/mfa/") or path in ("/one-time-code", "/verify-email"):
            return result("manual_action_required", "oauth_verification_required",
                          **_diagnostic(stage="page_navigation", detail="verification_required", page=page.url,
                                        elapsed_ms=(time.monotonic() - started) * 1000,
                                        consent_submitted=consent_submitted))
        if path == "/oauth2/device/done":
            return result("approved", **_diagnostic(stage="consent_callback", detail="callback_received", page=page.url,
                                                     elapsed_ms=(time.monotonic() - started) * 1000,
                                                     consent_submitted=consent_submitted,
                                                     code_form_seen=code_form_seen,
                                                     consent_page_seen=consent_page_seen,
                                                     consent_button_seen=consent_button_seen))
        try:
            state.operation = "read_body"
            body = await _observe(state, "read_body", lambda: page.locator("body").inner_text(timeout=5000))
            title = await _observe(state, "read_title", lambda: page.title())
        except asyncio.TimeoutError:
            return result("failed" if state.consent_state == "not_attempted" else "uncertain", "oauth_page_observation_timeout",
                          **_diagnostic(stage="page_observation", detail="body_or_title_timeout", page=page.url,
                                        elapsed_ms=(time.monotonic() - started) * 1000,
                                        consent_submitted=consent_submitted, code_form_seen=code_form_seen,
                                        consent_page_seen=consent_page_seen, consent_button_seen=consent_button_seen))
        except Exception as exc:
            if _is_timeout_error(exc):
                return result("failed" if state.consent_state == "not_attempted" else "uncertain", "oauth_page_observation_timeout",
                              **_diagnostic(stage="page_observation", detail="body_or_title_timeout", page=page.url,
                                            elapsed_ms=(time.monotonic() - started) * 1000,
                                            consent_submitted=consent_submitted, code_form_seen=code_form_seen,
                                            consent_page_seen=consent_page_seen, consent_button_seen=consent_button_seen))
            return result("failed" if state.consent_state == "not_attempted" else "uncertain", "oauth_page_observation_failed",
                          **_diagnostic(stage="page_observation", detail="body_or_title_failed", page=page.url,
                                        elapsed_ms=(time.monotonic() - started) * 1000,
                                        consent_submitted=consent_submitted, code_form_seen=code_form_seen,
                                        consent_page_seen=consent_page_seen, consent_button_seen=consent_button_seen))
        if re.search(r"just a moment|verify you are human|attention required|验证您是人类|请完成人机验证", title + "\n" + body, re.I):
            return result("manual_action_required", "oauth_verification_required",
                          **_diagnostic(stage="page_observation", detail="challenge_detected", page=page.url,
                                        elapsed_ms=(time.monotonic() - started) * 1000,
                                        consent_submitted=consent_submitted))
        if path == "/oauth2/device" and not code_submitted:
            # This target/method/name were verified on xAI's public DeviceCodeForm.
            state.operation = "form_count"
            form = page.locator('form[action="https://auth.x.ai/oauth2/device/verify"]')
            if await _observe(state, "form_count", lambda: form.count()) == 1:
                code_form_seen = True
                if (await _observe(state, "form_method", lambda: form.get_attribute("method")) or "").lower() != "post":
                    return result("failed", "oauth_consent_unrecognized",
                                  **_diagnostic(stage="device_code_form", detail="invalid_form_method", page=page.url,
                                                elapsed_ms=(time.monotonic() - started) * 1000,
                                                consent_submitted=consent_submitted, code_form_seen=code_form_seen))
                state.operation = "code_count"
                field = form.locator('input[name="user_code"]')
                if (await _observe(state, "code_count", lambda: field.count()) != 1
                        or (await _observe(state, "code_value", lambda: field.input_value())).replace("-", "") != user_code.replace("-", "")):
                    return result("failed", "oauth_verification_url_invalid",
                                  **_diagnostic(stage="device_code_form", detail="user_code_mismatch", page=page.url,
                                                elapsed_ms=(time.monotonic() - started) * 1000,
                                                consent_submitted=consent_submitted, code_form_seen=code_form_seen))
                button = await _visible_button(form, _CONTINUE, state)
                if button:
                    code_submitted = True
                    try:
                        await _observe(state, "device_code_click", lambda: button.click(timeout=10000))
                    except asyncio.TimeoutError:
                        return result("failed", "oauth_device_code_click_timeout",
                                      **_diagnostic(stage="device_code_submit", detail="continue_click_timeout", page=page.url,
                                                    elapsed_ms=(time.monotonic() - started) * 1000,
                                                    consent_submitted=consent_submitted, code_form_seen=code_form_seen))
                    except Exception as exc:
                        if _is_timeout_error(exc):
                            return result("failed", "oauth_device_code_click_timeout",
                                          **_diagnostic(stage="device_code_submit", detail="continue_click_timeout", page=page.url,
                                                        elapsed_ms=(time.monotonic() - started) * 1000,
                                                        consent_submitted=consent_submitted, code_form_seen=code_form_seen))
                        return result("failed", "oauth_device_code_click_failed",
                                      **_diagnostic(stage="device_code_submit", detail="continue_click_failed", page=page.url,
                                                    elapsed_ms=(time.monotonic() - started) * 1000,
                                                    consent_submitted=consent_submitted, code_form_seen=code_form_seen))
        elif path == "/oauth2/device/consent" and not consent_submitted:
            consent_page_seen = True
            # Fresh cookie context belongs to the selected account. If an account
            # email is displayed, reject a different identity before confirmation.
            emails = {value.lower() for value in _EMAIL.findall(body)}
            if emails and expected_email.lower() not in emails:
                return result("failed", "oauth_identity_mismatch",
                              **_diagnostic(stage="consent_page", detail="identity_mismatch", page=page.url,
                                            elapsed_ms=(time.monotonic() - started) * 1000,
                                            consent_submitted=consent_submitted, consent_page_seen=consent_page_seen))
            if "grok build" not in body.lower() and "grok cli" not in body.lower():
                return result("failed", "oauth_consent_unrecognized",
                              **_diagnostic(stage="consent_page", detail="product_marker_missing", page=page.url,
                                            elapsed_ms=(time.monotonic() - started) * 1000,
                                            consent_submitted=consent_submitted, consent_page_seen=consent_page_seen))
            button = await _visible_button(page, _ALLOW, state)
            if button:
                consent_button_seen = True
                consent_submitted = True
                state.consent_state = "attempted"
                try:
                    await _observe(state, "consent_click", lambda: button.click(timeout=15000))
                except asyncio.TimeoutError:
                    return result("uncertain", "oauth_consent_click_timeout",
                                  **_diagnostic(stage="consent_submit", detail="allow_click_timeout", page=page.url,
                                                elapsed_ms=(time.monotonic() - started) * 1000,
                                                consent_submitted=consent_submitted, consent_page_seen=consent_page_seen,
                                                consent_button_seen=consent_button_seen))
                except Exception as exc:
                    if _is_timeout_error(exc):
                        return result("uncertain", "oauth_consent_click_timeout",
                                      **_diagnostic(stage="consent_submit", detail="allow_click_timeout", page=page.url,
                                                    elapsed_ms=(time.monotonic() - started) * 1000,
                                                    consent_submitted=consent_submitted, consent_page_seen=consent_page_seen,
                                                    consent_button_seen=consent_button_seen))
                    # The click may already have reached xAI. The Go caller will
                    # poll this device grant; never click Allow a second time.
                    return result("uncertain", "oauth_consent_click_failed",
                                  **_diagnostic(stage="consent_submit", detail="allow_click_failed", page=page.url,
                                                elapsed_ms=(time.monotonic() - started) * 1000,
                                                consent_submitted=consent_submitted, consent_page_seen=consent_page_seen,
                                                consent_button_seen=consent_button_seen))
                # The click may already have reached xAI even when the browser
                # does not navigate to the done page. Stop holding the shared
                # browser after a short, bounded callback window and let CFM
                # poll the same device grant. Never click again.
                deadline = min(deadline, time.monotonic() + _CONSENT_CALLBACK_GRACE_SECONDS)
        state.operation = "wait_callback"
        await asyncio.sleep(0.4)
    return result("uncertain" if consent_submitted else "manual_action_required",
                  "oauth_callback_timeout" if consent_submitted else "oauth_consent_unrecognized",
                  **_diagnostic(stage="consent_callback" if consent_submitted else "consent_page",
                                detail="callback_wait_timeout" if consent_submitted else "consent_button_missing",
                                page=page.url, elapsed_ms=(time.monotonic() - started) * 1000,
                                consent_submitted=consent_submitted, code_form_seen=code_form_seen,
                                consent_page_seen=consent_page_seen, consent_button_seen=consent_button_seen))


async def authorize_device(*, verification_uri_complete: str, user_code: str, sso: str, sso_rw: str = "", expected_email: str,
                           proxy: str = "", timeout_s: int = 120, browser=None,
                           diagnostics: FlowDiagnostics | None = None) -> dict:
    state = diagnostics if diagnostics is not None else FlowDiagnostics()
    outcome = await _authorize_device(verification_uri_complete=verification_uri_complete, user_code=user_code,
                                     sso=sso, sso_rw=sso_rw, expected_email=expected_email, proxy=proxy,
                                     timeout_s=timeout_s, browser=browser, state=state)
    outcome.update(state.fields())
    return outcome


async def _authorize_device(*, verification_uri_complete, user_code, sso, sso_rw, expected_email,
                            proxy, timeout_s, browser, state):
    validate_device_url(verification_uri_complete, user_code)
    if not sso.strip() or not expected_email.strip():
        return result("failed", "oauth_session_required")
    from browser_sessions import launch_browser
    context = None
    started = time.monotonic()
    stage = "browser_start"
    network_errors: list[str] = []

    def request_failed(request) -> None:
        if len(network_errors) >= 8:
            return
        failure = getattr(request, "failure", None)
        try:
            failure = failure() if callable(failure) else failure
        except Exception:
            failure = None
        raw = ""
        if isinstance(failure, dict):
            raw = str(failure.get("errorText") or failure.get("error_text") or "")
        elif failure:
            raw = str(failure)
        match = _NETWORK_CODE.search(raw.upper())
        network_errors.append(match.group(1) if match else "unknown")

    def network_diagnostic() -> dict:
        codes = sorted(set(network_errors))
        return {"network_failures": len(network_errors), "network_error_codes": ",".join(codes)}

    try:
        launch_options = browser_kwargs("TURNSTILE", proxy=proxy)
        # Empty here explicitly means daily direct, not the registration worker's
        # optional environment proxy. Keep the two routes independent.
        if not proxy.strip():
            launch_options.pop("proxy", None)
        if browser is None:
            import cloakbrowser
            browser = await _observe(state, "browser_start", lambda: launch_browser(cloakbrowser.launch_async, **launch_options))
        stage = "browser_context"
        context = await _observe(state, "browser_context", lambda: browser.new_context(**native_browser_context_kwargs()))
        # Host-scoped cookies avoid sending the SSO to unrelated x.ai subdomains.
        cookies = [{"name": name, "value": value, "url": "https://" + host, "secure": True, "httpOnly": True, "sameSite": "Lax"}
                   for host in _TRUSTED for name, value in (("sso", sso), ("sso-rw", sso_rw or sso))]
        stage = "browser_cookies"
        await _observe(state, "browser_cookies", lambda: context.add_cookies(cookies))
        page = await _observe(state, "browser_page", lambda: context.new_page())
        state.page = page
        stage = "browser_routing"

        async def navigation_guard(route):
            request = route.request
            if request.is_navigation_request() and request.frame == page.main_frame and not trusted_url(request.url):
                await route.abort()
            else:
                await route.continue_()
        await _observe(state, "browser_routing", lambda: context.route("**/*", navigation_guard))
        async with asyncio.timeout(timeout_s):
            stage = "page_navigation"
            if hasattr(page, "on"):
                page.on("requestfailed", request_failed)
            try:
                await _observe(state, "page_navigation", lambda: page.goto(verification_uri_complete, wait_until="domcontentloaded", timeout=40000))
            except asyncio.TimeoutError:
                return result("failed", "oauth_page_navigation_timeout",
                              **_diagnostic(stage=stage, detail="navigation_timeout", page=page.url,
                                            elapsed_ms=(time.monotonic() - started) * 1000, **network_diagnostic()))
            except Exception as exc:
                if _is_timeout_error(exc):
                    return result("failed", "oauth_page_navigation_timeout",
                                  **_diagnostic(stage=stage, detail="navigation_timeout", page=page.url,
                                                elapsed_ms=(time.monotonic() - started) * 1000, **network_diagnostic()))
                return result("failed", "oauth_page_navigation_failed",
                              **_diagnostic(stage=stage, detail="navigation_failed", page=page.url,
                                            elapsed_ms=(time.monotonic() - started) * 1000, **network_diagnostic()))
            stage = "consent_page"
            outcome = await confirm_on_page(page, user_code=user_code, expected_email=expected_email, timeout_s=timeout_s,
                                            diagnostics=state)
            outcome.update(network_diagnostic())
            return outcome
    except asyncio.TimeoutError as exc:
        state.exception_type = _exception_type(exc)
        if stage == "consent_page":
            outcome = observation_failure(state, exc, (time.monotonic() - started) * 1000)
            outcome.update(network_diagnostic())
            return outcome
        return result("failed" if state.consent_state == "not_attempted" else "uncertain", "oauth_authorization_timeout",
                      **_diagnostic(stage=stage, detail="authorization_timeout", elapsed_ms=(time.monotonic() - started) * 1000,
                                    **network_diagnostic()))
    except Exception as exc:
        # Browser exceptions can contain URLs, codes and form values. Return only
        # a fixed error code and bounded stage metadata; let the caller inspect
        # the existing grant rather than replaying consent.
        state.exception_type = _exception_type(exc)
        return result("failed" if state.consent_state == "not_attempted" else "uncertain", "oauth_worker_result_unknown",
                      **_diagnostic(stage=stage, detail="browser_exception", elapsed_ms=(time.monotonic() - started) * 1000,
                                    **network_diagnostic()))
    finally:
        if browser is not None:
            # A close failure must not replay consent or overwrite its outcome.
            # The queue keeps ownership until its background cleanup succeeds.
            try: await browser.close()
            except Exception: pass
