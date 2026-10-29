"""Passive, bounded network evidence without URLs, headers, bodies or secrets."""
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

    def failed(self, request):
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
