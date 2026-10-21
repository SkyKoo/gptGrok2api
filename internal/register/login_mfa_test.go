package register

import (
	"context"
	"encoding/json"
	fhttp "github.com/bogdanfinn/fhttp"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginMFAContinuationContracts(t *testing.T) {
	for _, scenario := range []string{"json", "nested", "html_route", "redirect", "missing_secret", "invalid_secret", "missing_factor", "untrusted", "wrong_state", "rejected", "repeated", "missing_continuation"} {
		t.Run(scenario, func(t *testing.T) {
			issued, verified, callback := 0, 0, 0
			var base string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				write := func(v any) { json.NewEncoder(w).Encode(v) }
				switch r.URL.Path {
				case "/intermediate":
					http.Redirect(w, r, "/mfa-challenge/factor-1", 302)
				case "/api/accounts/mfa/issue_challenge":
					issued++
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if r.Method != "POST" || body["id"] != "factor-1" || body["type"] != "totp" || body["force_fresh_challenge"] != false {
						t.Error("invalid challenge request")
					}
					http.SetCookie(w, &http.Cookie{Name: "challenge", Value: "bound", Path: "/"})
					write(map[string]any{})
				case "/api/accounts/mfa/verify":
					verified++
					var body map[string]string
					json.NewDecoder(r.Body).Decode(&body)
					want, _ := TOTPCode("JBSWY3DPEHPK3PXP", time.Now())
					prev, _ := TOTPCode("JBSWY3DPEHPK3PXP", time.Now().Add(-30*time.Second))
					if body["id"] != "factor-1" || body["type"] != "totp" || (body["code"] != want && body["code"] != prev) {
						t.Error("invalid verification request")
					}
					if _, err := r.Cookie("challenge"); err != nil {
						t.Error("challenge cookies lost")
					}
					if scenario == "rejected" {
						http.Error(w, "do not expose raw upstream", 400)
						return
					}
					if scenario == "repeated" {
						write(map[string]any{"page": map[string]any{"type": "mfa_challenge"}, "factor_id": "factor-1"})
						return
					}
					if scenario == "missing_continuation" {
						write(map[string]any{})
						return
					}
					state := "state-1"
					if scenario == "wrong_state" {
						state = "wrong"
					}
					write(map[string]any{"continue_url": base + "/api/auth/callback/openai?state=" + state + "&code=private"})
				case "/api/auth/callback/openai":
					callback++
					write(map[string]any{})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			base = upstream.URL
			flow, err := NewWebRegistrar(base, base, "direct")
			if err != nil {
				t.Fatal(err)
			}
			defer flow.Close()
			flow.state = "state-1"
			flow.SetLoginTOTPSecret("JBSWY3DPEHPK3PXP")
			data := map[string]any{"page": map[string]any{"type": "mfa_challenge"}, "factor_id": "factor-1"}
			switch scenario {
			case "nested":
				data = map[string]any{"page": map[string]any{"type": "mfa_challenge", "payload": map[string]any{"id": "factor-1"}}}
			case "html_route":
				data = map[string]any{"continue_url": "/mfa-challenge/factor-1"}
			case "redirect":
				data = map[string]any{"continue_url": "/intermediate"}
			case "missing_secret":
				flow.SetLoginTOTPSecret("")
			case "invalid_secret":
				flow.SetLoginTOTPSecret("bad-secret!")
			case "missing_factor":
				delete(data, "factor_id")
			case "untrusted":
				data["continue_url"] = "https://untrusted.example.test/mfa-challenge/factor-1"
			}
			err = flow.continueAfterOTP(context.Background(), webReply{data: data, headers: fhttp.Header{}}, "fixture@example.test", true)
			success := scenario == "json" || scenario == "nested" || scenario == "html_route" || scenario == "redirect"
			if success {
				if err != nil || issued != 1 || verified != 1 || callback != 1 {
					t.Fatalf("continuation failed: %v calls %d/%d/%d", err, issued, verified, callback)
				}
			} else {
				if err == nil || callback != 0 {
					t.Fatalf("invalid challenge accepted: %v", err)
				}
				if strings.Contains(err.Error(), "JBSWY3DPEHPK3PXP") || strings.Contains(err.Error(), "do not expose") {
					t.Fatal("private data leaked in failure")
				}
				noRequest := scenario == "missing_secret" || scenario == "invalid_secret" || scenario == "missing_factor" || scenario == "untrusted"
				if noRequest && (issued != 0 || verified != 0) {
					t.Fatal("invalid challenge caused verification request")
				}
				if issued > 1 || verified > 1 {
					t.Fatal("authenticator code retried")
				}
			}
		})
	}
}
