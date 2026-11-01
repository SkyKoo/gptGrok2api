"""Passive, bounded network evidence without URLs, headers, bodies or secrets."""
import time
from collections import deque
from urllib.parse import urlparse

_TARGETS = {"accounts.x.ai", "auth.x.ai", "challenges.cloudflare.com", "brunhild.challenges.cloudflare.com"}
_ERRORS = {
    "net::ERR_TUNNEL_CONNECTION_FAILED": "proxy_tunnel_failed",
    "net::ERR_PROXY_CONNECTION_FAILED": "proxy_connection_failed",
    "net::ERR_PROXY_AUTH_UNSUPPORTED": "proxy_auth_failed",
    "net::ERR_TIMED_OUT": "network_timeout",
    "net::ERR_CONNECTION_TIMED_OUT": "network_timeout",
    "net::ERR_NAME_NOT_RESOLVED": "dns_failed",
    "net::ERR_CONNECTION_RESET": "connection_reset",
    "net::ERR_CONNECTION_CLOSED": "connection_closed",
    "net::ERR_CERT_AUTHORITY_INVALID": "tls_failed",
    "net::ERR_SSL_PROTOCOL_ERROR": "tls_failed",
}


class NetworkDiagnostics:
    def __init__(self):
        self._events = deque(maxlen=8)
        self._scripts = {}
        self._script_completed = 0
        self._script_failed = 0
        self._script_slowest_ms = 0

    def started(self, request):
        # Only first-party scripts matter for the signup page's event binding.
        # Store request identity/timing, never paths, query strings or headers.
        try:
            host = (urlparse(str(request.url)).hostname or "").lower()
            if host in {"accounts.x.ai", "auth.x.ai"} and request.resource_type == "script" and len(self._scripts) < 256:
                self._scripts[id(request)] = time.monotonic()
        except Exception:
            return

    def finished(self, request):
        started = self._scripts.pop(id(request), None)
        if started is not None:
            self._script_completed += 1
            self._script_slowest_ms = max(self._script_slowest_ms, int((time.monotonic() - started) * 1000))

    def loading_snapshot(self):
        now = time.monotonic()
        return {
            "pending_scripts": len(self._scripts),
            "oldest_pending_ms": max((int((now - start) * 1000) for start in self._scripts.values()), default=0),
            "completed_scripts": self._script_completed,
            "failed_scripts": self._script_failed,
            "slowest_script_ms": self._script_slowest_ms,
        }

    def failed(self, request):
        if self._scripts.pop(id(request), None) is not None:
            self._script_failed += 1
        try:
            host = (urlparse(str(request.url)).hostname or "").lower()
            if host not in _TARGETS:
                return
            # Unknown messages may contain credentials or URLs; persist only a category.
            failure = str(request.failure or "")
            if failure == "net::ERR_ABORTED":
                return
            self._events.append({"host": host, "kind": _ERRORS.get(failure, "network_failed"), "status": 0})
        except Exception:
            return

    def response(self, response):
        try:
            host = (urlparse(str(response.url)).hostname or "").lower()
            status = int(response.status)
            if host in _TARGETS and 400 <= status <= 599:
                self._events.append({"host": host, "kind": "http_error", "status": status})
        except Exception:
            return

    def snapshot(self):
        return [dict(event) for event in self._events]
