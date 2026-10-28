"""CFM-owned Grok browser worker.

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

from common.browser import browser_kwargs
from turnstile.solve import solve_turnstile_realpage_session
from xai_flow import (
    XaiBrowserFlowError,
    close_session as close_xai_session,
    send_email as xai_send_email,
    signup as xai_signup,
    start_session as xai_start_session,
    verify_email as xai_verify_email,
)

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
log = logging.getLogger("grok-browser")

app = FastAPI(title="CFM Grok Browser Worker", version="1.0.0")
_slots = asyncio.Semaphore(max(1, int(os.getenv("GROK_BROWSER_CONCURRENCY", "1"))))
_auth_token = os.getenv("GROK_BROWSER_TOKEN", "").strip()
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
        session = _sessions.pop(session_id, None)
    if not session:
        return
    try:
        await session["page"].close()
    finally:
        await session["browser"].close()


@app.post("/v1/session/close")
async def session_close(req: SessionCloseRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    await close_xai_session(req.session_id)
    await _close_session(req.session_id)
    return {"ok": True}


def _native_error(exc: XaiBrowserFlowError) -> HTTPException:
    status = 504 if exc.retryable else 422
    # Return only bounded page metadata. Never return page HTML, cookies,
    # challenge tokens, form values or mailbox content to the caller.
    diagnostic = exc.diagnostic if isinstance(exc.diagnostic, dict) else {}
    safe_diagnostic = {
        key: diagnostic[key]
        for key in ("url", "title", "http_status", "cloudflare_challenge", "proxy_connection_failed", "reason")
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
        async with _slots:
            return await xai_start_session(
                signup_url=_validate_url(req.url),
                proxy=_normalize_proxy(req.proxy),
                timezone=req.timezone.strip(),
                locale=req.locale.strip(),
                profile=_profile_for_worker(req.browser_profile),
                timeout_s=req.timeout_s,
            )
    except XaiBrowserFlowError as exc:
        raise _native_error(exc)
    except asyncio.TimeoutError:
        raise HTTPException(status_code=504, detail={"stage": "browser_start", "error": "browser start timeout"})
    except Exception:
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
    async with await cloakbrowser.launch_async(**browser_kwargs("TURNSTILE", proxy or None)) as browser:
        page = await browser.new_page()
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
            await page.close()


@app.get("/health")
async def health() -> dict:
    return {"status": "ok", "service": "grok-browser", "capacity": _slots._value}


@app.post("/v1/sso/materialize")
async def materialize_sso(req: MaterializeRequest, authorization: str | None = Header(default=None)) -> dict:
    if not _authorized(authorization):
        raise HTTPException(status_code=401, detail="invalid worker token")
    session_id = str(uuid.uuid4())
    async with _slots:
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
    async with _slots:
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
            live_session = {"browser": browser, "page": page,
                            "expires_at": asyncio.get_running_loop().time() + max(900, req.timeout_s)}
        except asyncio.TimeoutError:
            raise HTTPException(status_code=504, detail="browser turnstile timeout")
        except Exception as exc:
            log.warning("turnstile failed session=%s error=%s", session_id, type(exc).__name__)
            raise HTTPException(status_code=502, detail="browser turnstile failed")
    token = str(result.get("token") or "").strip()
    if not token:
        if live_session is not None:
            try:
                await live_session["page"].close()
            finally:
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
