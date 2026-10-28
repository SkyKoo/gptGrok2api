"""Run the real Grok signup page and return its Turnstile context.

The worker deliberately has no synthetic route fixture or generic captcha
solver endpoint. It navigates the target page in Chromium, renders the
official widget, and returns the short-lived token with the issuing context.
"""
import asyncio
import logging
import time

import cloakbrowser

from common.browser import browser_kwargs, run_pre_actions, run_post_fetch

log = logging.getLogger("grok-browser-turnstile")
_solve_lock = asyncio.Lock()

def _browser_kwargs(proxy: str = None, timezone: str = None, locale: str = None, profile: dict = None) -> dict:
    return browser_kwargs("TURNSTILE", proxy, timezone=timezone, locale=locale, profile=profile)


# ── Real-page solver ────────────────────────────────────────────────

# Sitekey is passed as the evaluate() arg `k` — never interpolated into JS source.
_WIDGET_INJECT_JS = (
    "async (k) => {"
    "  let d = document.querySelector('[data-cfm-grok-turnstile]');"
    "  if (!d) {"
    "    d = document.createElement('div');"
    "    d.setAttribute('data-cfm-grok-turnstile', '1');"
    "    d.style.cssText = 'min-height:64px;position:fixed;left:20px;top:20px;z-index:2147483647';"
    "    document.body.prepend(d);"
    "  }"
    "  for (let i = 0; i < 100 && !window.turnstile?.render; i++) {"
    "    await new Promise(resolve => setTimeout(resolve, 100));"
    "  }"
    "  if (!window.turnstile?.render) {"
    "    await new Promise((resolve, reject) => {"
    "      const callback = '__cfmGrokTurnstileReady';"
    "      window[callback] = resolve;"
    "      const script = document.createElement('script');"
    "      script.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit&onload=' + callback;"
    "      script.async = true;"
    "      script.defer = true;"
    "      script.onerror = () => reject(new Error('Turnstile script failed to load'));"
    "      document.head.appendChild(script);"
    "      setTimeout(() => reject(new Error('Turnstile script load timeout')), 15000);"
    "    });"
    "  }"
    "  window.__cfmGrokTurnstileToken = '';"
    "  window.turnstile.render(d, {"
    "    sitekey: k,"
    "    size: 'flexible',"
    "    callback: token => { window.__cfmGrokTurnstileToken = token; }"
    "  });"
    "  return true;"
    "}"
)


async def _inject_turnstile_widget(page, sitekey: str) -> None:
    """Inject a .cf-turnstile widget with the sitekey passed as data (evaluate arg)."""
    await page.evaluate(_WIDGET_INJECT_JS, sitekey)


async def _human_click_iframe(page, fr) -> bool:
    """Click the Turnstile checkbox via humanized page-level mouse movement.

    CloakBrowser's humanizer hooks page.mouse.click (B-spline paths + overshoot) but
    NOT frame.click, so fr.click() inside the cross-origin iframe sends a robotic instant
    click. Instead we resolve the iframe's page-absolute box and click at the checkbox
    offset (left edge + 30px, vertical centre) via the humanized page.mouse.
    """
    try:
        el = await fr.frame_element()
        box = await el.bounding_box()
    except Exception:
        return False
    if not box or box["width"] < 20:
        return False
    x = box["x"] + 30
    y = box["y"] + box["height"] / 2
    await page.mouse.click(x, y)  # humanized (B-spline) — page-level, not frame
    return True


async def _click_turnstile_checkbox(page, attempts: int = 25) -> bool:
    """Click the checkbox inside the cross-origin Cloudflare iframe.

    Prefers a humanized page-level mouse click on the iframe's box; falls back to
    a frame-level selector click (not humanized) if the box can't be resolved.
    """
    for _ in range(attempts):
        # Some Chromium builds expose the challenge frame before its frame URL
        # is populated. Locate the iframe first, then fall back to frame URLs.
        try:
            iframe = page.locator('iframe[src*="challenges.cloudflare.com"]').first
            if await iframe.is_visible():
                box = await iframe.bounding_box()
                if box and box["width"] >= 20 and box["height"] >= 20:
                    await page.mouse.click(box["x"] + 30, box["y"] + box["height"] / 2)
                    return True
        except Exception:
            pass
        for fr in page.frames:
            if "challenges.cloudflare.com" in (fr.url or ""):
                if await _human_click_iframe(page, fr):
                    return True
                for sel in ("input[type=checkbox]", "label", "body"):
                    try:
                        await fr.click(sel, timeout=2000)
                        return True
                    except Exception:
                        continue
        await asyncio.sleep(1)
    return False


async def solve_turnstile_realpage(url: str, sitekey: str = None,
                                   timeout_s: int = 60,
                                   pre_actions: list = None,
                                   post_fetch: list = None,
                                   proxy: str = None) -> dict:
    """Navigate a real page, execute pre_actions, click the CF Turnstile checkbox,
    return the token and browser cookies.

    pre_actions — optional list of steps before Turnstile appears:
      [{"type": "click", "selector": "text=Continue with Email"},
       {"type": "fill", "selector": "input[type=email]", "value": "user@example.com"},
       {"type": "click", "selector": "button[type=submit]"}]

    post_fetch — optional list of API calls to make from the SAME browser session
    after solving (keeps cookies/session for endpoints that require same-origin):
      [{"url": "https://app.kilo.ai/api/auth/verify-turnstile", "method": "POST", "body": {"token": "__TOKEN__"}},
       {"url": "https://app.kilo.ai/api/auth/magic-link", "method": "POST", "body": {"email": "user@example.com", "callbackUrl": "/"}}]

    Use __TOKEN__ placeholder in body to inject the solved Turnstile token.

    Selector formats supported: CSS, XPath (//), text=, regex=, role=
    """
    t0 = time.monotonic()

    async with _solve_lock:
        async with await cloakbrowser.launch_async(**_browser_kwargs(proxy)) as browser:
            page = await browser.new_page()
            try:
                await page.goto(url, wait_until="domcontentloaded", timeout=45000)

                if pre_actions:
                    await run_pre_actions(page, pre_actions)
                    await asyncio.sleep(2)

                # Inject sitekey widget if given (override page's own).
                if sitekey:
                    await _inject_turnstile_widget(page, sitekey)
                    await asyncio.sleep(3)

                clicked = await _click_turnstile_checkbox(page)
                log.info("Real-page checkbox clicked=%s", clicked)

                # Harvest token, bounded by timeout_s.
                token = ""
                deadline = time.monotonic() + timeout_s
                while time.monotonic() < deadline:
                    try:
                        token = await page.evaluate(
                        "() => window.__cfmGrokTurnstileToken || "
                            "document.querySelector('[name=cf-turnstile-response]')?.value || ''")
                    except Exception:
                        token = ""
                    if token:
                        break
                    await asyncio.sleep(1)

                cookies = await page.context.cookies()
                try:
                    user_agent = await page.evaluate("() => navigator.userAgent")
                except Exception:
                    user_agent = ""
                result = {"token": token,
                          "verify_success": bool(token),
                          "cookies": cookies,
                          "user_agent": str(user_agent or ""),
                          "method": "real-page",
                          "elapsed": round(time.monotonic() - t0, 1)}

                # Post_fetch from the same session (parameterized — injection-safe).
                if post_fetch and token:
                    result["post_fetch"] = await run_post_fetch(page, post_fetch, token)

                return result
            finally:
                await page.close()


async def solve_turnstile_realpage_session(
    url: str,
    sitekey: str,
    timeout_s: int = 60,
    proxy: str = None,
    timezone: str = None,
    locale: str = None,
    profile: dict = None,
):
    """Real-page solver variant that keeps Chromium alive for same-origin auth."""
    browser = await cloakbrowser.launch_async(**_browser_kwargs(proxy, timezone, locale, profile))
    page = await browser.new_page()
    try:
        await page.goto(url, wait_until="domcontentloaded", timeout=45000)
        if sitekey:
            await _inject_turnstile_widget(page, sitekey)
            await asyncio.sleep(3)
        clicked = await _click_turnstile_checkbox(page)
        log.info("Real-page session checkbox clicked=%s", clicked)
        token = ""
        deadline = time.monotonic() + timeout_s
        while time.monotonic() < deadline:
            try:
                token = await page.evaluate(
                    "() => window.__cfmGrokTurnstileToken || "
                    "document.querySelector('[name=cf-turnstile-response]')?.value || ''")
            except Exception:
                token = ""
            if token:
                break
            await asyncio.sleep(1)
        cookies = await page.context.cookies()
        try:
            user_agent = await page.evaluate("() => navigator.userAgent")
        except Exception:
            user_agent = ""
        return browser, page, {"token": token, "cookies": cookies,
                               "user_agent": str(user_agent or ""),
                               "method": "real-page-session"}
    except Exception:
        await page.close()
        await browser.close()
        raise
