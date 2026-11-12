"""Provider-specific Node handle governed by the same browser lifecycle queue."""
from __future__ import annotations
import asyncio
import json
import os
import signal
import time
import uuid
from pydantic import BaseModel, Field
from browser_sessions import queue, BrowserQueueError, BrowserStartupError

class AdobeCommandRequest(BaseModel):
    browser_lease: str = Field(min_length=32, max_length=64, pattern=r"^[a-zA-Z0-9_-]+$")
    action: str = Field(default="run", pattern=r"^(run|verify|refresh)$")
    code: str = Field(default="", max_length=6, pattern=r"^(|[0-9]{6})$")

class AdobeProcess:
    def __init__(self, process):
        self.process = process
        self.lock = asyncio.Lock()
        self.tainted = False

    async def command(self, payload):
        async with self.lock:
            if self.tainted or self.process.returncode is not None:
                raise RuntimeError("adobe_process_unavailable")
            identifier = uuid.uuid4().hex
            self.process.stdin.write((json.dumps({**payload,"id":identifier})+"\n").encode())
            await self.process.stdin.drain()
            try:
                async with asyncio.timeout(150):
                    line = await self.process.stdout.readline()
                response = json.loads(line)
                if response.get("id") != identifier:
                    raise RuntimeError("adobe_response_mismatch")
                if not response.get("ok"):
                    return {"stage":"needs_manual", "code":response.get("code", "adobe_browser_failed"), **({"diagnostics":response["diagnostics"]} if "diagnostics" in response else {})}
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

async def launch_adobe(**options):
    process = await asyncio.create_subprocess_exec("node", "/app/adobe/worker-rpc.mjs",
        stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.DEVNULL, start_new_session=True, limit=2*1024*1024)
    handle = AdobeProcess(process)
    try:
        result = await handle.command({"action":"init", **options})
        if result.get("stage") != "ready":
            raise BrowserStartupError(result.get("code"))
        return handle
    except BaseException:
        await handle.close()
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
