"""FIFO browser lifecycle ownership; credentials never enter queue status.

A reservation prepares Chromium before OAuth requests a device code. All browser
entry points use this owner, including legacy persistent Turnstile sessions.
"""
from __future__ import annotations

import asyncio
import logging
import time
import uuid
from dataclasses import dataclass, field
from typing import Any

log = logging.getLogger("grok-browser.queue")


class BrowserQueueError(RuntimeError):
    pass


@dataclass
class Entry:
    key: str
    options: dict
    launcher: Any
    state: str = "waiting_browser"
    touched: float = field(default_factory=time.monotonic)
    browser: Any = None
    task: Any = None
    close_task: Any = None
    closing: bool = False
    retry_at: float = 0
    error: str = ""


class ManagedBrowser:
    def __init__(self, queue, entry):
        self.queue, self.entry = queue, entry

    def __getattr__(self, name):
        return getattr(self.entry.browser, name)

    async def __aenter__(self):
        return self

    async def __aexit__(self, *_):
        await self.close()

    async def close(self):
        await self.queue.close(self.entry.key)


class BrowserQueue:
    def __init__(self, capacity=1, *, ready_ttl=45, active_ttl=1200, close_timeout=8, license_cooldown=240):
        self.capacity = max(1, capacity)
        self.ready_ttl, self.active_ttl = ready_ttl, active_ttl
        self.close_timeout, self.license_cooldown = close_timeout, license_cooldown
        self.entries = {}
        self.cooldown_until = 0
        self.runner = None
        self.stopping = False

    def start(self):
        if self.runner is None or self.runner.done():
            self.runner = asyncio.create_task(self._run())

    def enqueue(self, key, options, launcher):
        if self.stopping:
            raise BrowserQueueError("browser_worker_stopping")
        existing = self.entries.get(key)
        if existing is not None:
            # A retry of queue admission cannot create a second browser.
            return self.status(key)
        if len(self.entries) >= 128:
            raise BrowserQueueError("browser_queue_full")
        self.entries[key] = Entry(key, dict(options), launcher)
        self.start()
        return self.status(key)

    def status(self, key):
        entry = self.entries.get(key)
        if entry is None:
            raise BrowserQueueError("browser_reservation_expired")
        if entry.state in ("waiting_browser", "starting_browser", "ready") and not entry.closing:
            entry.touched = time.monotonic()
        state = "closing" if entry.closing else entry.state
        return {"id": key, "status": state, "code": entry.error,
                "message": "等待浏览器会话释放" if state == "waiting_browser" else ""}

    def claim(self, key):
        entry = self.entries.get(key)
        if entry is None or entry.state != "ready" or entry.closing:
            raise BrowserQueueError("browser_reservation_not_ready")
        entry.state, entry.touched = "active", time.monotonic()
        return ManagedBrowser(self, entry)

    @property
    def available(self):
        if time.monotonic() < self.cooldown_until:
            return 0
        return max(0, self.capacity - sum(e.state in ("starting_browser", "ready", "active") or e.closing for e in self.entries.values()))

    async def _launch(self, entry):
        try:
            entry.browser = await entry.launcher(**entry.options)
            entry.state = "ready"
            entry.touched = time.monotonic()
        except Exception as exc:
            # Only this explicit pre-launch license refusal can be retried.
            # Never retry navigation, signup, consent or a failed model call.
            if type(exc).__name__ == "CloakBrowserLicenseError" and "session limit reached" in str(exc).lower():
                self.cooldown_until = time.monotonic() + self.license_cooldown
                entry.state, entry.error = "waiting_browser", "browser_license_busy"
                log.warning("browser license busy; waiting for session release")
            else:
                entry.state, entry.error = "failed", "browser_start_failed"
                log.warning("browser start failed type=%s", type(exc).__name__)
        finally:
            entry.options.clear()  # drop proxy credentials as soon as no retry is needed
            # License refusal needs the same environment for its later launch.
            # _run retains a separate copy only for this pre-launch retry.
            entry.task = None

    async def _close(self, entry):
        try:
            await entry.browser.close()
        except BaseException:
            entry.retry_at = time.monotonic() + 10
            log.warning("browser close unconfirmed; capacity remains occupied")
            raise
        else:
            self.entries.pop(entry.key, None)
            log.info("browser closed; capacity released")

    async def close(self, key):
        entry = self.entries.get(key)
        if entry is None:
            return
        entry.closing = True
        if entry.task is not None:
            # Launch may already have started a process. Keep its result tracked,
            # then close it; cancelling the waiter cannot orphan the process.
            raise BrowserQueueError("browser_close_pending")
        if entry.browser is None:
            self.entries.pop(key, None)
            return
        if entry.close_task is None or (entry.close_task.done() and time.monotonic() >= entry.retry_at):
            entry.close_task = asyncio.create_task(self._close(entry))
        done, _ = await asyncio.wait({entry.close_task}, timeout=self.close_timeout)
        if not done:
            raise BrowserQueueError("browser_close_pending")
        try:
            entry.close_task.result()
        except BaseException:
            raise BrowserQueueError("browser_close_pending") from None

    async def _run(self):
        while not self.stopping:
            now = time.monotonic()
            for entry in list(self.entries.values()):
                ttl = self.active_ttl if entry.state == "active" else self.ready_ttl
                if now - entry.touched >= ttl:
                    entry.closing = True
                if entry.closing and entry.task is None:
                    if entry.browser is None:
                        self.entries.pop(entry.key, None)
                    elif entry.close_task is None or (entry.close_task.done() and now >= entry.retry_at):
                        entry.close_task = asyncio.create_task(self._close(entry))
                if entry.close_task is not None and entry.close_task.done():
                    # Retrieve exceptions even when the HTTP client disconnected.
                    try: entry.close_task.result()
                    except BaseException: pass
            if now >= self.cooldown_until:
                for entry in list(self.entries.values()):
                    if not self.available:
                        break
                    if entry.state == "waiting_browser" and not entry.closing:
                        entry.state, entry.error = "starting_browser", ""
                        entry.task = asyncio.create_task(self._launch_with_options(entry))
            await asyncio.sleep(0.2)

    async def _launch_with_options(self, entry):
        options = dict(entry.options)
        await self._launch(entry)
        if entry.state == "waiting_browser" and not entry.closing:
            entry.options = options
        if self.stopping or entry.closing:
            try: await self.close(entry.key)
            except BrowserQueueError: pass

    async def shutdown(self):
        self.stopping = True
        if self.runner is not None:
            await self.runner
        # Closing each entry remains bounded, including a stuck close callback.
        results = await asyncio.gather(*(self.close(key) for key in list(self.entries)), return_exceptions=True)
        pending = {e.task for e in self.entries.values() if e.task is not None}
        pending.update(e.close_task for e in self.entries.values() if e.close_task is not None)
        if pending:
            await asyncio.wait(pending, timeout=self.close_timeout)
        if self.entries:
            log.warning("worker shutting down with browser closure unconfirmed count=%d", len(self.entries))
        return not self.entries and not any(isinstance(r, BaseException) for r in results)


queue = BrowserQueue()


async def launch_browser(launcher, **options):
    """Legacy callers join the same queue and keep ownership through close()."""
    key = uuid.uuid4().hex
    queue.enqueue(key, options, launcher)
    try:
        async with asyncio.timeout(480):
            while True:
                state = queue.status(key)
                if state["status"] == "ready":
                    return queue.claim(key)
                if state["status"] == "failed":
                    raise BrowserQueueError(state["code"])
                await asyncio.sleep(0.5)
    except BaseException:
        try: await queue.close(key)
        except BrowserQueueError: pass
        raise
