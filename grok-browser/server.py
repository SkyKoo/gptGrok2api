"""CFM shared browser worker for Grok and Adobe.

CFM owns registration state, mailbox polling and account storage, while this
process owns the persistent Chromium page required by xAI's native signup
flow. Castle, Turnstile, page state and final SSO are kept in that context.
"""
from __future__ import annotations

import asyncio
import base64
import json
import logging
import os
import uuid
from datetime import datetime, timedelta, timezone
from typing import Any
from urllib.parse import urlparse

from fastapi import FastAPI, Header, HTTPException
from pydantic import BaseModel, Field

import cloakbrowser
from browser_sessions import queue, launch_browser, BrowserQueueError
from adobe_flow import launch_adobe, adobe_command, AdobeCommandRequest
from contextlib import asynccontextmanager

from common.browser import browser_context_kwargs, browser_kwargs, InvalidBrowserProxy, is_proxy_configuration_error
from signup_errors import signup_error_status
from oauth_device import authorize_device, validate_device_url
from turnstile.solve import solve_turnstile_realpage_session
from xai_flow import (
    XaiBrowserFlowError,
    close_session as close_xai_session,
    cleanup_expired_sessions,
    send_email as xai_send_email,
    signup as xai_signup,
    start_session as xai_start_session,
    verify_email as xai_verify_email,
)

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
log = logging.getLogger("cfm-browser-worker")

async def _cleanup_sessions():
    while True:
        await asyncio.sleep(15)
        await cleanup_expired_sessions()
        for key, session in list(_sessions.items()):
            if session["expires_at"] < asyncio.get_running_loop().time():
                try: await _close_session(key)
                except Exception: pass


@asynccontextmanager
async def lifespan(_app):
    queue.capacity = max(1, int(os.getenv("CFM_BROWSER_WORKER_CONCURRENCY", os.getenv("GROK_BROWSER_CONCURRENCY", "1"))))
    queue.start()
    cleanup = asyncio.create_task(_cleanup_sessions())
    try:
        yield
    finally:
        cleanup.cancel()
        await asyncio.gather(cleanup, return_exceptions=True)
        await queue.shutdown()


app = FastAPI(title="CFM Browser Worker", version="1.0.0", lifespan=lifespan)
_auth_token = os.getenv("CFM_BROWSER_WORKER_TOKEN", os.getenv("GROK_BROWSER_TOKEN", "")).strip()
_allowed_hosts = {"accounts.x.ai", "grok.com", "www.grok.com"}
_sessions: dict[str, dict] = {}
_sessions_lock = asyncio.Lock()


class SessionRequest(BaseModel):
    sitekey: str = Field(min_length=8, max_length=256)
    url: str = Field(min_length=8, max_length=2048)
    proxy: str = Field(default="", max_length=2048)
    timezone: str = Field(default="", max_length=128)
    locale: str = Field(default="", max_length=64)
    browser_profile: dict[str, Any] = Field(default_factory=dict)
    timeout_s: int = Field(default=180, ge=30, le=900)


class SessionCloseRequest(BaseModel):
    session_id: str = Field(min_length=8, max_length=128)


class NativeStartRequest(BaseModel):
    browser_lease: str = Field(default="", max_length=64)
    url: str = Field(min_length=8, max_length=2048)
    proxy: str = Field(default="", max_length=2048)
    timezone: str = Field(default="", max_length=128)
    locale: str = Field(default="", max_length=64)
    browser_profile: dict[str, Any] = Field(default_factory=dict)
    timeout_s: int = Field(default=180, ge=30, le=900)


class NativeEmailRequest(BaseModel):
    session_id: str = Field(min_length=8, max_length=128)
    email: str = Field(min_length=3, max_length=320)


class NativeVerifyRequest(NativeEmailRequest):
    code: str = Field(min_length=4, max_length=32)


class NativeSignupRequest(NativeVerifyRequest):
    password: str = Field(min_length=8, max_length=256)
    given_name: str = Field(min_length=1, max_length=128)
    family_name: str = Field(min_length=1, max_length=128)
    turnstile_token: str = Field(default="", max_length=8192)


class MaterializeRequest(BaseModel):
    wrapper: str = Field(min_length=40, max_length=8192)
    proxy: str = Field(default="", max_length=2048)
    cookies: list[dict] = Field(default_factory=list, max_length=128)


class OAuthDeviceRequest(BaseModel):
    browser_lease: str = Field(default="", max_length=64)
    verification_uri_complete: str = Field(min_length=8, max_length=2048)
    user_code: str = Field(min_length=8, max_length=9)
    sso: str = Field(min_length=1, max_length=8192)
    sso_rw: str = Field(default="", max_length=8192)
    expected_email: str = Field(min_length=3, max_length=320)
    proxy: str = Field(default="", max_length=2048)
    timeout_s: int = Field(default=120, ge=30, le=120)


@app.post("/v1/oauth/authorize-device")
async def oauth_authorize_device(req: OAuthDeviceRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        validate_device_url(req.verification_uri_complete, req.user_code)
    except ValueError:
        raise HTTPException(status_code=400, detail="oauth_verification_url_invalid")
    # Bound queue waiting as well as execution. No request body or browser
    # exception is logged, because both may include session credentials.
    try:
        async with asyncio.timeout(150):
            browser = queue.claim(req.browser_lease) if req.browser_lease else None
            return await authorize_device(**req.model_dump(exclude={"proxy", "browser_lease"}), proxy=_normalize_proxy(req.proxy), browser=browser)
    except BrowserQueueError:
        return {"status":"failed", "code":"oauth_browser_unavailable"}
    except TimeoutError:
        return {"status":"uncertain", "code":"oauth_worker_result_unknown"}


def _authorized(authorization: str | None) -> bool:
    if not _auth_token:
        return True
    return authorization == f"Bearer {_auth_token}"


def _normalize_proxy(raw: str) -> str:
    value = (raw or "").strip()
    return "" if value.lower() in ("direct", "none", "null") else value


def _profile_for_worker(value: Any) -> dict[str, Any]:
    """Keep only the non-secret browser environment fields used by the Worker."""
    if not isinstance(value, dict):
        return {}
    allowed = {
        "user_agent", "navigator_language", "timezone_iana", "screen_width",
        "screen_height", "inner_width", "inner_height", "device_pixel_ratio",
        "browser_family", "browser_os", "chrome_major", "tls_profile",
    }
    output: dict[str, Any] = {}
    for key in allowed:
        item = value.get(key)
        if isinstance(item, (str, int, float)) and str(item).strip():
            output[key] = item
    return output


def _validate_url(raw: str) -> str:
    parsed = urlparse(raw)
    if parsed.scheme != "https" or parsed.hostname not in _allowed_hosts:
        raise HTTPException(status_code=400, detail="url must be a trusted Grok HTTPS origin")
    return raw


def _cookies_for_cfm(items: list[dict]) -> list[dict]:
    """Drop browser-only fields and normalize Playwright's cookie shape."""
    output: list[dict] = []
    for item in items or []:
        name = str(item.get("name") or "").strip()
        value = str(item.get("value") or "")
        if not name or len(name) > 256 or len(value) > 16384:
            continue
        domain = str(item.get("domain") or "").strip().lower().lstrip(".")
        # The Go protocol client replays this snapshot to accounts.x.ai. Do
        # not move Cloudflare iframe or unrelated grok.com cookies onto that
        # origin: doing so changes the anti-abuse context and can invalidate
        # an otherwise valid Turnstile token.
        if domain and not (domain == "accounts.x.ai" or domain.endswith(".x.ai")):
            continue
        row = {
            "name": name,
            "value": value,
            "domain": str(item.get("domain") or ""),
            "path": str(item.get("path") or "/"),
            "secure": bool(item.get("secure")),
            "http_only": bool(item.get("httpOnly") or item.get("http_only")),
        }
        same_site = str(item.get("sameSite") or item.get("same_site") or "").lower()
        row["same_site"] = {"strict": 2, "lax": 1, "none": 0}.get(same_site, 0)
        output.append(row)
    return output[:128]



def _decode_wrapper_success_url(wrapper: str) -> str:
    parts = wrapper.split(".")
    if len(parts) != 3:
        return ""
    try:
        padded = parts[1] + "=" * ((4 - len(parts[1]) % 4) % 4)
        payload = json.loads(base64.urlsafe_b64decode(padded).decode("utf-8"))
    except Exception:
        return ""
    config = payload.get("config") if isinstance(payload, dict) else None
    candidate = config.get("success_url") if isinstance(config, dict) else None
    if not candidate:
        candidate = payload.get("success_url") if isinstance(payload, dict) else None
    if not isinstance(candidate, str):
        return ""
    try:
        parsed = urlparse(candidate)
        if parsed.scheme == "https" and parsed.hostname in _allowed_hosts:
            return candidate
    except Exception:
        pass
    return ""


def _is_wrapper(token: str) -> bool:
    return bool(_decode_wrapper_success_url(token))


def _is_session(token: str) -> bool:
    return bool(token and token.startswith("eyJ") and token.count(".") == 2 and not _is_wrapper(token))


def _worker_cookie_rows(items: list[dict]) -> list[dict]:
    rows = []
    for item in items or []:
        name = str(item.get("name") or "").strip()
        value = str(item.get("value") or "")
        if not name or len(name) > 256 or len(value) > 16384:
            continue
        domain = str(item.get("domain") or "")
        if domain and not (domain.endswith(".x.ai") or domain.endswith(".grok.com") or domain in _allowed_hosts):
            continue
        rows.append({"name": name, "value": value, "domain": domain or "accounts.x.ai", "path": str(item.get("path") or "/"), "secure": True})
    return rows[:128]


async def _close_session(session_id: str) -> None:
    async with _sessions_lock:
        session = _sessions.get(session_id)
    if session:
        await session["browser"].close()
        async with _sessions_lock:
            _sessions.pop(session_id, None)


@app.post("/v1/session/close")
async def session_close(req: SessionCloseRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        await close_xai_session(req.session_id)
        await _close_session(req.session_id)
    except BrowserQueueError:
        raise HTTPException(status_code=503, detail="browser_close_pending")
    return {"ok": True}


def _native_error(exc: XaiBrowserFlowError) -> HTTPException:
    status = signup_error_status(str(exc), exc.retryable)
    # Return only bounded page metadata. Never return page HTML, cookies,
    # challenge tokens, form values or mailbox content to the caller.
    diagnostic = exc.diagnostic if isinstance(exc.diagnostic, dict) else {}
    safe_diagnostic = {
        key: diagnostic[key]
        for key in (
            "url",
            "title",
            "http_status",
            "cloudflare_challenge",
            "proxy_connection_failed",
            "reason",
            "upstream_status",
            "upstream_code",
            "upstream_method",
            "upstream_path",
            "visible_controls",
            "network_failures",
            "page_scripts",
            "email_entry",
            "turnstile_clicks",
            "turnstile_frames",
            "turnstile_visible_frames",
            "turnstile_widgets",
            "turnstile_ready",
            "turnstile_callback_seen",
            "turnstile_token_present",
            "trusted_destination",
            "grok_landing",
            "account_page",
            "sso_cookie_present",
            "signup_form_present",
        )
        if key in diagnostic
    }
    detail = {"stage": exc.stage, "error": str(exc)[:300]}
    if safe_diagnostic:
        detail["diagnostic"] = safe_diagnostic
    return HTTPException(status_code=status, detail=detail)


@app.post("/v1/session/start")
async def native_session_start(req: NativeStartRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        browser = queue.claim(req.browser_lease) if req.browser_lease else None
        return await xai_start_session(
            signup_url=_validate_url(req.url),
            proxy=_normalize_proxy(req.proxy),
            timezone=req.timezone.strip(),
            locale=req.locale.strip(),
            profile=_profile_for_worker(req.browser_profile),
            timeout_s=req.timeout_s, browser=browser,
        )
    except BrowserQueueError as exc:
        raise HTTPException(status_code=409, detail={"stage":"waiting_browser", "error":str(exc)})
    except XaiBrowserFlowError as exc:
        raise _native_error(exc)
    except asyncio.TimeoutError:
        raise HTTPException(status_code=504, detail={"stage": "browser_start", "error": "browser start timeout"})
    except Exception as exc:
        if is_proxy_configuration_error(exc):
            log.warning("native signup rejected invalid proxy configuration")
            raise HTTPException(status_code=422, detail={"stage": "browser_start", "error": "proxy_configuration_invalid"}) from None
        log.exception("native signup session start failed")
        raise HTTPException(status_code=502, detail={"stage": "browser_start", "error": "browser start failed"})


@app.post("/v1/session/send-email")
async def native_session_send_email(req: NativeEmailRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        return await xai_send_email(req.session_id, req.email)
    except XaiBrowserFlowError as exc:
        # Keep the task journal useful without logging email addresses,
        # cookies, page HTML or challenge values.
        log.warning("native signup email rejected stage=%s reason=%s retryable=%s diagnostic=%s", exc.stage, str(exc)[:120], exc.retryable, exc.diagnostic)
        raise _native_error(exc)
    except Exception:
        log.exception("native signup email stage failed session=%s", req.session_id)
        raise HTTPException(status_code=502, detail={"stage": "send_code", "error": "browser email stage failed"})


@app.post("/v1/session/verify-email")
async def native_session_verify_email(req: NativeVerifyRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        return await xai_verify_email(req.session_id, req.email, req.code)
    except XaiBrowserFlowError as exc:
        log.warning("native signup verify rejected stage=%s reason=%s retryable=%s diagnostic=%s", exc.stage, str(exc)[:120], exc.retryable, exc.diagnostic)
        raise _native_error(exc)
    except Exception:
        log.exception("native signup verify stage failed session=%s", req.session_id)
        raise HTTPException(status_code=502, detail={"stage": "validate_code", "error": "browser verify stage failed"})


@app.post("/v1/session/signup")
async def native_session_signup(req: NativeSignupRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        return await xai_signup(
            req.session_id,
            email=req.email,
            password=req.password,
            code=req.code,
            given_name=req.given_name,
            family_name=req.family_name,
            turnstile_token=req.turnstile_token,
        )
    except XaiBrowserFlowError as exc:
        log.warning("native signup final rejected stage=%s reason=%s retryable=%s diagnostic=%s", exc.stage, str(exc)[:120], exc.retryable, exc.diagnostic)
        raise _native_error(exc)
    except Exception:
        log.exception("native signup final stage failed session=%s", req.session_id)
        raise HTTPException(status_code=502, detail={"stage": "signup", "error": "browser signup stage failed"})


async def _materialize_in_browser(wrapper: str, proxy: str, cookies: list[dict]) -> tuple[str, list[dict], str]:
    if _is_session(wrapper):
        return wrapper, cookies, ""
    urls = []
    success_url = _decode_wrapper_success_url(wrapper)
    if success_url:
        urls.append(success_url)
    urls.extend(["https://accounts.x.ai/", "https://grok.com/"])
    async with await launch_browser(cloakbrowser.launch_async, **browser_kwargs("TURNSTILE", proxy or None)) as browser:
        context = await browser.new_context(**browser_context_kwargs())
        page = await context.new_page()
        try:
            rows = _worker_cookie_rows(cookies)
            for domain in (".x.ai", ".grok.com"):
                rows.append({"name": "sso", "value": wrapper, "domain": domain, "path": "/", "secure": True})
                rows.append({"name": "sso-rw", "value": wrapper, "domain": domain, "path": "/", "secure": True})
            await page.context.add_cookies(rows)
            for url in urls:
                try:
                    await page.goto(url, wait_until="domcontentloaded", timeout=45000)
                except Exception:
                    continue
                await asyncio.sleep(1.2)
                for _ in range(12):
                    current = await page.context.cookies()
                    for item in current:
                        if item.get("name") in ("sso", "sso-rw") and _is_session(str(item.get("value") or "")):
                            ua = await page.evaluate("() => navigator.userAgent")
                            return str(item["value"]), _cookies_for_cfm(current), str(ua or "")
                    await asyncio.sleep(0.5)
            current = await page.context.cookies()
            ua = await page.evaluate("() => navigator.userAgent")
            return "", _cookies_for_cfm(current), str(ua or "")
        finally:
            await browser.close()


@app.get("/health")
async def health() -> dict:
    return {"status": "ok", "service": "cfm-browser-worker", "capacity": queue.available}


@app.post("/v1/sso/materialize")
async def materialize_sso(req: MaterializeRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    session_id = str(uuid.uuid4())
    try:
        token, cookies, user_agent = await _materialize_in_browser(req.wrapper.strip(), _normalize_proxy(req.proxy), req.cookies)
    except Exception:
        log.warning("sso materialize failed session=%s", session_id)
        raise HTTPException(status_code=502, detail="browser sso materialize failed")
    if not token:
        raise HTTPException(status_code=502, detail="browser worker did not receive a session cookie")
    expires_at = datetime.now(timezone.utc) + timedelta(seconds=300)
    return {"ok": True, "session_id": session_id, "token": token, "cookies": cookies, "user_agent": user_agent, "expires_at": expires_at.isoformat().replace("+00:00", "Z")}


@app.post("/v1/turnstile/session")
async def turnstile_session(req: SessionRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    page_url = _validate_url(req.url)
    session_id = str(uuid.uuid4())
    log.info("turnstile start session=%s host=%s", session_id, urlparse(page_url).hostname)
    live_session = None
    try:
        # xAI validates the Turnstile token against the live signup page.
        # There is deliberately no route fixture or synthetic fallback:
        # returning a token that was not minted by the real widget only
        # creates a later, opaque anti-abuse failure.
        browser, page, result = await solve_turnstile_realpage_session(
            url=page_url,
            sitekey=req.sitekey,
            timeout_s=req.timeout_s,
            proxy=_normalize_proxy(req.proxy) or None,
            timezone=req.timezone.strip() or None,
            locale=req.locale.strip() or None,
            profile=_profile_for_worker(req.browser_profile),
        )
        live_session = {"browser": browser, "context": page.context, "page": page,
                        "expires_at": asyncio.get_running_loop().time() + max(900, req.timeout_s)}
    except asyncio.TimeoutError:
        raise HTTPException(status_code=504, detail="browser turnstile timeout")
    except Exception as exc:
        log.warning("turnstile failed session=%s error=%s", session_id, type(exc).__name__)
        raise HTTPException(status_code=502, detail="browser turnstile failed")
    token = str(result.get("token") or "").strip()
    if not token:
        if live_session is not None:
            await live_session["browser"].close()
        raise HTTPException(status_code=502, detail="browser worker did not receive a token")
    if live_session is not None:
        async with _sessions_lock:
            _sessions[session_id] = live_session
    user_agent = str(result.get("user_agent") or "")
    expires_at = datetime.now(timezone.utc) + timedelta(seconds=300)
    log.info("turnstile success session=%s elapsed=%s", session_id, result.get("elapsed"))
    return {
        "ok": True,
        "session_id": session_id if live_session is not None else "",
        "token": token,
        "cookies": _cookies_for_cfm(result.get("cookies") or []),
        "user_agent": user_agent,
        "expires_at": expires_at.isoformat().replace("+00:00", "Z"),
    }


class BrowserReservationRequest(BaseModel):
    provider: str = Field(default="grok", pattern=r"^(grok|adobe)$")
    profile_id: str = Field(default="", max_length=80, pattern=r"^[a-zA-Z0-9_-]*$")
    email: str = Field(default="", max_length=320)
    adobe_profile: dict[str, Any] = Field(default_factory=dict)
    adobe_mode: str = Field(default="registration", pattern=r"^(registration|refresh|resume)$")
    adobe_require_new: bool = False
    adobe_recovery_id: str = Field(default="", max_length=80, pattern=r"^[a-zA-Z0-9_-]*$")
    account_id: str = Field(default="", max_length=256)
    password: str = Field(default="", max_length=256)
    id: str = Field(min_length=32, max_length=64, pattern=r"^[a-zA-Z0-9_-]+$")
    proxy: str = Field(default="", max_length=2048)
    timezone: str = Field(default="", max_length=128)
    locale: str = Field(default="", max_length=64)
    browser_profile: dict[str, Any] = Field(default_factory=dict)
    daily_direct: bool = False


@app.post("/v1/browser/reservations")
async def reserve_browser(req: BrowserReservationRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        proxy = _normalize_proxy(req.proxy)
        if req.provider == "adobe":
            if not req.profile_id or not req.email:
                raise HTTPException(status_code=422, detail="adobe_identity_required")
            if req.adobe_mode == "resume" and not req.adobe_recovery_id:
                raise HTTPException(status_code=422, detail="adobe_recovery_id_required")
            if req.adobe_require_new and req.adobe_mode != "registration":
                raise HTTPException(status_code=422, detail="adobe_profile_mode_invalid")
            return queue.enqueue(req.id, {"profile_id":req.profile_id, "email":req.email,
                "profile":req.adobe_profile, "proxy":proxy, "mode":req.adobe_mode, "require_new":req.adobe_require_new,
                "recovery_id":req.adobe_recovery_id, "account_id":req.account_id, "password":req.password}, launch_adobe,
                startup_progress=True)
        options = browser_kwargs("TURNSTILE", "direct" if req.daily_direct and not proxy else proxy or None,
            timezone=req.timezone or None, locale=req.locale or None, profile=_profile_for_worker(req.browser_profile))
        if req.daily_direct and not proxy:
            options.pop("proxy", None)
        return queue.enqueue(req.id, options, cloakbrowser.launch_async)
    except InvalidBrowserProxy:
        raise HTTPException(status_code=422, detail={"stage": "browser_start", "error": "proxy_configuration_invalid"}) from None
    except BrowserQueueError as exc:
        raise HTTPException(status_code=503, detail=str(exc))


@app.get("/v1/browser/reservations/{key}")
async def browser_reservation_status(key: str, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        return queue.status(key)
    except BrowserQueueError:
        raise HTTPException(status_code=404, detail="browser_reservation_expired")


@app.delete("/v1/browser/reservations/{key}")
async def release_browser_reservation(key: str, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        await queue.close(key)
    except BrowserQueueError:
        # Accepted cleanup; the slot remains occupied until closure completes.
        return {"status": "closing"}
    return {"status": "released"}


@app.post("/v1/adobe/run")
async def run_adobe(req: AdobeCommandRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    try:
        return await adobe_command(req)
    except BrowserQueueError:
        raise HTTPException(status_code=409, detail="adobe_session_unavailable") from None
    except (TimeoutError, RuntimeError):
        # A lost reply is never retried; the durable Node journal owns its outcome.
        return {"stage":"outcome_unknown", "code":"adobe_worker_result_unknown", "submission_started":True,
                "creation_outcome":"unknown"}
