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


def result(status: str, code: str = "") -> dict:
    return {"status": status, "code": code}


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
    deadline = time.monotonic() + timeout_s
    code_submitted = consent_submitted = False
    while time.monotonic() < deadline:
        if not trusted_url(page.url):
            return result("uncertain" if consent_submitted else "failed", "oauth_consent_unrecognized")
        path = urlsplit(page.url).path.rstrip("/")
        if path in ("/sign-in", "/sign-up", "/sso-select-account"):
            return result("manual_action_required", "oauth_login_required")
        if path.startswith("/mfa/") or path in ("/one-time-code", "/verify-email"):
            return result("manual_action_required", "oauth_verification_required")
        if path == "/oauth2/device/done":
            return result("approved")
        body = await page.locator("body").inner_text(timeout=5000)
        title = await page.title()
        if re.search(r"just a moment|verify you are human|attention required|验证您是人类|请完成人机验证", title + "\n" + body, re.I):
            return result("manual_action_required", "oauth_verification_required")
        if path == "/oauth2/device" and not code_submitted:
            # This target/method/name were verified on xAI's public DeviceCodeForm.
            form = page.locator('form[action="https://auth.x.ai/oauth2/device/verify"]')
            if await form.count() == 1:
                if (await form.get_attribute("method") or "").lower() != "post":
                    return result("failed", "oauth_consent_unrecognized")
                field = form.locator('input[name="user_code"]')
                if await field.count() != 1 or (await field.input_value()).replace("-", "") != user_code.replace("-", ""):
                    return result("failed", "oauth_verification_url_invalid")
                button = await _visible_button(form, _CONTINUE)
                if button:
                    code_submitted = True
                    await button.click(timeout=10000)
        elif path == "/oauth2/device/consent" and not consent_submitted:
            # Fresh cookie context belongs to the selected account. If an account
            # email is displayed, reject a different identity before confirmation.
            emails = {value.lower() for value in _EMAIL.findall(body)}
            if emails and expected_email.lower() not in emails:
                return result("failed", "oauth_identity_mismatch")
            if "grok build" not in body.lower() and "grok cli" not in body.lower():
                return result("failed", "oauth_consent_unrecognized")
            button = await _visible_button(page, _ALLOW)
            if button:
                consent_submitted = True
                try:
                    await button.click(timeout=15000)
                except Exception:
                    # The click may already have reached xAI. The Go caller will
                    # poll this device grant; never click Allow a second time.
                    return result("uncertain", "oauth_worker_result_unknown")
        await asyncio.sleep(0.4)
    return result("uncertain" if consent_submitted else "manual_action_required", "oauth_worker_result_unknown" if consent_submitted else "oauth_consent_unrecognized")


async def authorize_device(*, verification_uri_complete: str, user_code: str, sso: str, sso_rw: str = "", expected_email: str, proxy: str = "", timeout_s: int = 120) -> dict:
    validate_device_url(verification_uri_complete, user_code)
    if not sso.strip() or not expected_email.strip():
        return result("failed", "oauth_session_required")
    import cloakbrowser
    browser = context = None
    try:
        launch_options = browser_kwargs("TURNSTILE", proxy=proxy)
        # Empty here explicitly means daily direct, not the registration worker's
        # optional environment proxy. Keep the two routes independent.
        if not proxy.strip():
            launch_options.pop("proxy", None)
        browser = await cloakbrowser.launch_async(**launch_options)
        context = await browser.new_context(**native_browser_context_kwargs())
        # Host-scoped cookies avoid sending the SSO to unrelated x.ai subdomains.
        cookies = [{"name": name, "value": value, "url": "https://" + host, "secure": True, "httpOnly": True, "sameSite": "Lax"}
                   for host in _TRUSTED for name, value in (("sso", sso), ("sso-rw", sso_rw or sso))]
        await context.add_cookies(cookies)
        page = await context.new_page()

        async def navigation_guard(route):
            request = route.request
            if request.is_navigation_request() and request.frame == page.main_frame and not trusted_url(request.url):
                await route.abort()
            else:
                await route.continue_()
        await context.route("**/*", navigation_guard)
        async with asyncio.timeout(timeout_s):
            await page.goto(verification_uri_complete, wait_until="domcontentloaded", timeout=40000)
            return await confirm_on_page(page, user_code=user_code, expected_email=expected_email, timeout_s=timeout_s)
    except Exception:
        # Browser exceptions can contain URLs, codes and form values. Return only
        # a fixed error code and let the caller inspect the existing grant.
        return result("uncertain", "oauth_worker_result_unknown")
    finally:
        if context is not None:
            try: await context.close()
            except Exception: pass
        if browser is not None:
            try: await browser.close()
            except Exception: pass
