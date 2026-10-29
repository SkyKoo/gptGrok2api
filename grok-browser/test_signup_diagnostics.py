import json
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
sys.path.insert(0, str(Path(__file__).resolve().parent))
from signup_errors import signup_error_status
from network_diagnostics import NetworkDiagnostics


class SignupDiagnosticsTests(unittest.TestCase):
    def test_explicit_upstream_rejection_is_not_a_timeout_even_for_old_retry_flag(self):
        for retryable in (True, False):
            self.assertEqual(signup_error_status("signup_unavailable", retryable), 422)
            self.assertEqual(signup_error_status("turnstile_incomplete", retryable), 422)
            self.assertEqual(signup_error_status("email_already_registered", retryable), 409)
            self.assertEqual(signup_error_status("signup_rate_limited", retryable), 429)
        self.assertEqual(signup_error_status("verification_page_missing", True), 504)
        self.assertEqual(signup_error_status("proxy_connection_failed", True), 502)

    def test_failure_diagnostics_are_bounded_and_do_not_copy_secrets(self):
        d = NetworkDiagnostics()
        for i in range(20):
            d.failed(SimpleNamespace(url="https://user:password@brunhild.challenges.cloudflare.com/path?token=SECRET", failure="net::ERR_TUNNEL_CONNECTION_FAILED"))
        self.assertEqual(len(d.snapshot()), 8)
        self.assertEqual(d.snapshot()[0], {"host": "brunhild.challenges.cloudflare.com", "kind": "proxy_tunnel_failed", "status": 0})
        text=json.dumps(d.snapshot())
        for secret in ("password", "SECRET", "/path", "token="):
            self.assertNotIn(secret, text)
        d.snapshot()[0]['host']='changed'
        self.assertNotEqual(d.snapshot()[0]['host'], 'changed')

    def test_ignores_ads_abort_success_and_lookalike_hosts(self):
        d=NetworkDiagnostics()
        for host in ("stapecdn.com", "challenges.cloudflare.com.evil.test"):
            d.failed(SimpleNamespace(url="https://"+host+"/", failure="net::ERR_TIMED_OUT"))
        d.failed(SimpleNamespace(url="https://accounts.x.ai/", failure="net::ERR_ABORTED"))
        d.response(SimpleNamespace(url="https://accounts.x.ai/",status=200))
        self.assertEqual(d.snapshot(), [])
        d.response(SimpleNamespace(url="https://accounts.x.ai/private?email=PRIVATE",status=400))
        self.assertEqual(d.snapshot(), [{"host":"accounts.x.ai","kind":"http_error","status":400}])

    def test_unknown_transport_error_never_leaks_exception_text(self):
        d=NetworkDiagnostics()
        d.failed(SimpleNamespace(url="https://accounts.x.ai/",failure="secret proxy password"))
        self.assertEqual(d.snapshot()[0]['kind'], 'network_failed')
        self.assertNotIn('secret',json.dumps(d.snapshot()))
