"""Provider-specific Node handle governed by the same browser lifecycle queue."""
from __future__ import annotations
import asyncio
import json
import os
import signal
import time
import uuid
from pydantic import BaseModel, Field
from browser_sessions import queue, BrowserQueueError, BrowserStartupError, BrowserStartupCleanupPending, startup_progress_event

def creation_result(response):
    outcome = response.get("creation_outcome")
    if not isinstance(outcome, str) or outcome not in {"not_submitted", "rejected", "accepted", "unknown"}:
        return {"creation_outcome": "unknown"}
    if outcome == "rejected":
        if response.get("creation_error_code") != "captcha_required" or type(response.get("creation_http_status")) is not int or response["creation_http_status"] != 400:
            return {"creation_outcome": "unknown"}
        return {"creation_outcome": outcome, "creation_error_code": "captcha_required", "creation_http_status": 400}
    return {"creation_outcome": outcome}

class AdobeCommandRequest(BaseModel):
    browser_lease: str = Field(min_length=32, max_length=64, pattern=r"^[a-zA-Z0-9_-]+$")
    action: str = Field(default="run", pattern=r"^(prepare|run|verify|refresh|resume|media)$")
    code: str = Field(default="", max_length=6, pattern=r"^(|[0-9]{6})$")

class AdobeProcess:
    def __init__(self, process):
        self.process = process
        self.lock = asyncio.Lock()
        self.tainted = False

    async def command(self, payload, *, startup_progress=None):
        async with self.lock:
            if self.tainted or self.process.returncode is not None:
                raise RuntimeError("adobe_process_unavailable")
            identifier = uuid.uuid4().hex
            try:
                # One deadline covers all progress frames and the final reply.
                # Progress is observation only; it must not extend an init or
                # cause the business command to be sent again.
                async with asyncio.timeout(150):
                    self.process.stdin.write((json.dumps({**payload,"id":identifier})+"\n").encode())
                    await self.process.stdin.drain()
                    previous_sequence = 0
                    while True:
                        line = await self.process.stdout.readline()
                        response = json.loads(line)
                        if not isinstance(response, dict) or response.get("id") != identifier:
                            raise RuntimeError("adobe_response_mismatch")
                        if "event" in response:
                            if response["event"] != "startup_progress" or payload.get("action") != "init":
                                raise RuntimeError("adobe_unexpected_event")
                            event = startup_progress_event(response.get("progress"))
                            if event is not None and event["sequence"] > previous_sequence:
                                previous_sequence = event["sequence"]
                                if startup_progress is not None:
                                    startup_progress(event)
                            continue
                        if not response.get("ok"):
                            return {"stage":"needs_manual", "code":response.get("code", "adobe_browser_failed"),
                                **creation_result(response),
                                **({"submission_started":response["submission_started"]} if isinstance(response.get("submission_started"), bool) else {}),
                                **({"diagnostics":response["diagnostics"]} if "diagnostics" in response else {})}
                        return response["result"]
            except BaseException:
                # Never send another business command after a lost response.
                self.tainted = True
                raise

    async def close(self):
        try:
            if not self.tainted:
                async with asyncio.timeout(5):
                    await self.command({"action":"close"})
                    await self.process.wait()
                    return
        except BaseException:
            pass
        # Child browser processes share this isolated process group. Do not
        # release queue capacity until the whole group has been terminated.
        try: os.killpg(self.process.pid, signal.SIGKILL)
        except ProcessLookupError: pass
        await self.process.wait()
        for _ in range(50):
            try: os.killpg(self.process.pid, 0)
            except ProcessLookupError: return
            await asyncio.sleep(.1)
        raise RuntimeError("adobe_browser_close_unconfirmed")

async def launch_adobe(*, startup_progress=None, **options):
    process = await asyncio.create_subprocess_exec("node", "/app/adobe/worker-rpc.mjs",
        stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.DEVNULL, start_new_session=True, limit=2*1024*1024)
    handle = AdobeProcess(process)
    try:
        result = await handle.command({"action":"init", **options}, startup_progress=startup_progress)
        if result.get("stage") != "ready":
            raise BrowserStartupError(result.get("code"))
        return handle
    except BaseException as error:
        try:
            await handle.close()
        except BaseException:
            # Transfer ownership even when initialization failed. Dropping the
            # handle here would free the queue slot with a process still alive.
            code = error.code if isinstance(error, BrowserStartupError) else None
            raise BrowserStartupCleanupPending(handle, code) from None
        raise

async def adobe_command(request):
    entry = queue.entries.get(request.browser_lease)
    if entry is None or entry.closing or not isinstance(entry.browser, AdobeProcess):
        raise BrowserQueueError("adobe_session_unavailable")
    if entry.state == "ready":
        queue.claim(request.browser_lease)
    elif entry.state != "active":
        raise BrowserQueueError("adobe_session_unavailable")
    entry.touched = time.monotonic()
    return await entry.browser.command({"action":request.action,"code":request.code})
