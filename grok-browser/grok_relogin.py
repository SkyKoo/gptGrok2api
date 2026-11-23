"""Sign into an existing Grok account in a fresh, reserved browser context."""
import asyncio
import re
from urllib.parse import urlsplit

from common.browser import native_browser_context_kwargs


SIGN_IN = "https://accounts.x.ai/sign-in?redirect=grok-com"
AUTH_HOSTS = {"accounts.x.ai", "auth.x.ai"}
APP_HOSTS = {"grok.com", "www.grok.com"}
EMAIL = 'input[type="email"], input[autocomplete="username"], input[name="email"]'
PASSWORD = 'input[type="password"]:not([autocomplete="new-password"])'
VERIFICATION = ('input[autocomplete="one-time-code"], input[name="otp"], input[name="code"], '
                'input[type="tel"], iframe[src*="challenges.cloudflare.com"]')
EMAIL_ENTRY = re.compile(r"^(?:log ?in|sign ?in|continue) with email$|^使用邮箱(?:登录|继续)$", re.I)
SUBMIT = re.compile(r"^(?:log ?in|sign ?in|continue|next|登录|继续|下一步)$", re.I)


def _trusted(page, hosts):
    url = urlsplit(page.url)
    return (url.scheme == "https" and url.hostname in hosts and url.port in (None, 443)
            and not url.username and not url.password)


def _on_sign_in(page):
    return _trusted(page, AUTH_HOSTS) and not re.search(r"(?:sign-up|signup|register|reset|recover)", urlsplit(page.url).path, re.I)


async def _single_visible(locator):
    matches = [item for item in await locator.all() if await item.is_visible()]
    return matches[0] if len(matches) == 1 else None


async def _submit(page):
    button = await _single_visible(page.get_by_role("button", name=SUBMIT))
    if button is None or not _on_sign_in(page):
        return False
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


async def _sign_in(page, email, password):
    await page.goto(SIGN_IN, wait_until="domcontentloaded", timeout=25000)
    entry_clicked = email_submitted = password_submitted = False
    while True:
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
                await email_input.fill(email, timeout=5000)
            if not _on_sign_in(page):
                return {"code": "login_page_unrecognized"}
            await password_input.fill(password, timeout=5000)
            if not await _submit(page):
                return {"code": "login_page_unrecognized"}
            password_submitted = True
        elif email_input is not None and not email_submitted and not password_submitted:
            if not _on_sign_in(page):
                return {"code": "login_page_unrecognized"}
            await email_input.fill(email, timeout=5000)
            if not await _submit(page):
                return {"code": "login_page_unrecognized"}
            email_submitted = True
        elif not entry_clicked and not email_submitted and not password_submitted:
            entry = await _single_visible(page.get_by_role("button", name=EMAIL_ENTRY))
            if entry is None:
                entry = await _single_visible(page.get_by_role("link", name=EMAIL_ENTRY))
            if entry is not None and _on_sign_in(page):
                await entry.click(timeout=5000)
                entry_clicked = True
        await asyncio.sleep(.25)


async def relogin(browser, email, password):
    context = None
    try:
        async with asyncio.timeout(60):
            context = await browser.new_context(**native_browser_context_kwargs())
            return await _sign_in(await context.new_page(), email, password)
    except TimeoutError:
        return {"code": "login_timeout"}
    except Exception:
        return {"code": "login_failed"}
    finally:
        if context is not None:
            try:
                async with asyncio.timeout(5):
                    await context.close()
            except Exception:
                pass  # The reservation still owns final browser cleanup.
