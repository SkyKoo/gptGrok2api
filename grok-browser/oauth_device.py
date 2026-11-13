"""Complete a CFM-created xAI device grant in an isolated server-side session.

No account signup, password entry, CAPTCHA solving or consent replay is performed.
The caller exchanges the device code and checks token identity before persistence.
"""
from __future__ import annotations

import asyncio
import re
import time
from urllib.parse import parse_qs, urlsplit

from common.browser import browser_kwargs, native_browser_context_kwargs

_TRUSTED = {"accounts.x.ai", "auth.x.ai"}
_CONTINUE = re.compile(r"^(Continue|继续)$", re.I)
_ALLOW = re.compile(r"^(Allow|Authorize|Approve|Allow access|授权|允许|确认授权|允许访问)$", re.I)
_EMAIL = re.compile(r"[A-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Z0-9-]+(?:\.[A-Z0-9-]+)+", re.I)
_CONSENT_CALLBACK_GRACE_SECONDS = 15
_NETWORK_CODE = re.compile(r"\b(ERR_[A-Z0-9_]+|TIMEOUT|TIMED_OUT|CANCELLED)\b")

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


async def _visible_button(page, name):
    matches = page.get_by_role("button", name=name)
    candidates = []
    for index in range(await matches.count()):
        item = matches.nth(index)
        if await item.is_visible() and await item.is_enabled():
            candidates.append(item)
    return candidates[0] if len(candidates) == 1 else None


async def confirm_on_page(page, *, user_code: str, expected_email: str, timeout_s: float) -> dict:
    """Allow only the known device form and one explicit consent on its route."""
    started = time.monotonic()
    deadline = started + timeout_s
    code_submitted = consent_submitted = False
    code_form_seen = consent_page_seen = consent_button_seen = False
    while time.monotonic() < deadline:
        if not trusted_url(page.url):
            return result("uncertain" if consent_submitted else "failed", "oauth_consent_unrecognized",
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
            body = await page.locator("body").inner_text(timeout=5000)
            title = await page.title()
        except asyncio.TimeoutError:
            return result("uncertain" if consent_submitted else "failed", "oauth_page_observation_timeout",
                          **_diagnostic(stage="page_observation", detail="body_or_title_timeout", page=page.url,
                                        elapsed_ms=(time.monotonic() - started) * 1000,
                                        consent_submitted=consent_submitted, code_form_seen=code_form_seen,
                                        consent_page_seen=consent_page_seen, consent_button_seen=consent_button_seen))
        except Exception as exc:
            if _is_timeout_error(exc):
                return result("uncertain" if consent_submitted else "failed", "oauth_page_observation_timeout",
                              **_diagnostic(stage="page_observation", detail="body_or_title_timeout", page=page.url,
                                            elapsed_ms=(time.monotonic() - started) * 1000,
                                            consent_submitted=consent_submitted, code_form_seen=code_form_seen,
                                            consent_page_seen=consent_page_seen, consent_button_seen=consent_button_seen))
            return result("uncertain" if consent_submitted else "failed", "oauth_page_observation_failed",
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
            form = page.locator('form[action="https://auth.x.ai/oauth2/device/verify"]')
            if await form.count() == 1:
                code_form_seen = True
                if (await form.get_attribute("method") or "").lower() != "post":
                    return result("failed", "oauth_consent_unrecognized",
                                  **_diagnostic(stage="device_code_form", detail="invalid_form_method", page=page.url,
                                                elapsed_ms=(time.monotonic() - started) * 1000,
                                                consent_submitted=consent_submitted, code_form_seen=code_form_seen))
                field = form.locator('input[name="user_code"]')
                if await field.count() != 1 or (await field.input_value()).replace("-", "") != user_code.replace("-", ""):
                    return result("failed", "oauth_verification_url_invalid",
                                  **_diagnostic(stage="device_code_form", detail="user_code_mismatch", page=page.url,
                                                elapsed_ms=(time.monotonic() - started) * 1000,
                                                consent_submitted=consent_submitted, code_form_seen=code_form_seen))
                button = await _visible_button(form, _CONTINUE)
                if button:
                    code_submitted = True
                    try:
                        await button.click(timeout=10000)
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
            button = await _visible_button(page, _ALLOW)
            if button:
                consent_button_seen = True
                consent_submitted = True
                try:
                    await button.click(timeout=15000)
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
        await asyncio.sleep(0.4)
    return result("uncertain" if consent_submitted else "manual_action_required",
                  "oauth_callback_timeout" if consent_submitted else "oauth_consent_unrecognized",
                  **_diagnostic(stage="consent_callback" if consent_submitted else "consent_page",
                                detail="callback_wait_timeout" if consent_submitted else "consent_button_missing",
                                page=page.url, elapsed_ms=(time.monotonic() - started) * 1000,
                                consent_submitted=consent_submitted, code_form_seen=code_form_seen,
                                consent_page_seen=consent_page_seen, consent_button_seen=consent_button_seen))


async def authorize_device(*, verification_uri_complete: str, user_code: str, sso: str, sso_rw: str = "", expected_email: str, proxy: str = "", timeout_s: int = 120, browser=None) -> dict:
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
            browser = await launch_browser(cloakbrowser.launch_async, **launch_options)
        stage = "browser_context"
        context = await browser.new_context(**native_browser_context_kwargs())
        # Host-scoped cookies avoid sending the SSO to unrelated x.ai subdomains.
        cookies = [{"name": name, "value": value, "url": "https://" + host, "secure": True, "httpOnly": True, "sameSite": "Lax"}
                   for host in _TRUSTED for name, value in (("sso", sso), ("sso-rw", sso_rw or sso))]
        stage = "browser_cookies"
        await context.add_cookies(cookies)
        page = await context.new_page()
        stage = "browser_routing"

        async def navigation_guard(route):
            request = route.request
            if request.is_navigation_request() and request.frame == page.main_frame and not trusted_url(request.url):
                await route.abort()
            else:
                await route.continue_()
        await context.route("**/*", navigation_guard)
        async with asyncio.timeout(timeout_s):
            stage = "page_navigation"
            if hasattr(page, "on"):
                page.on("requestfailed", request_failed)
            try:
                await page.goto(verification_uri_complete, wait_until="domcontentloaded", timeout=40000)
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
            outcome = await confirm_on_page(page, user_code=user_code, expected_email=expected_email, timeout_s=timeout_s)
            outcome.update(network_diagnostic())
            return outcome
    except asyncio.TimeoutError:
        return result("uncertain", "oauth_authorization_timeout",
                      **_diagnostic(stage=stage, detail="authorization_timeout", elapsed_ms=(time.monotonic() - started) * 1000,
                                    **network_diagnostic()))
    except Exception:
        # Browser exceptions can contain URLs, codes and form values. Return only
        # a fixed error code and bounded stage metadata; let the caller inspect
        # the existing grant rather than replaying consent.
        return result("uncertain", "oauth_worker_result_unknown",
                      **_diagnostic(stage=stage, detail="browser_exception", elapsed_ms=(time.monotonic() - started) * 1000,
                                    **network_diagnostic()))
    finally:
        if browser is not None:
            # A close failure must not replay consent or overwrite its outcome.
            # The queue keeps ownership until its background cleanup succeeds.
            try: await browser.close()
            except Exception: pass
