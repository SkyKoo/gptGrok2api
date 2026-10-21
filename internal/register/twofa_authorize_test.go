package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reauthentication currently uses /oauth/authorize; older sessions can return
// /api/accounts/authorize. Both must retain the same origin and state checks.
func TestBeginTwoFAAuthorizeRoutes(t *testing.T) {
	for _, scenario := range []struct {
		name, path, origin, state string
		success                   bool
	}{
		{"current_reauth", "/oauth/authorize", "", "fixture-state", true},
		{"legacy_reauth", "/api/accounts/authorize", "", "fixture-state", true},
		{"missing_state", "/oauth/authorize", "", "", false},
		{"untrusted_origin", "/oauth/authorize", "https://untrusted.example.invalid", "fixture-state", false},
		{"unsupported_path", "/other/authorize", "", "fixture-state", false},
		{"credentials_in_url", "/oauth/authorize", "http://user:password@localhost", "fixture-state", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var base string
			navigations, sends := 0, 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/auth/csrf":
					json.NewEncoder(w).Encode(map[string]string{"csrfToken": "fixture-csrf"})
				case "/api/auth/signin/openai":
					_ = r.ParseForm()
					if r.URL.Query().Get("reauth") != "password" || r.Form.Get("csrfToken") != "fixture-csrf" || r.Form.Get("callbackUrl") != base+"/?action=enable&factor=totp" {
						t.Error("reauth request changed")
					}
					origin := base
					if scenario.origin != "" {
						origin = scenario.origin
					}
					json.NewEncoder(w).Encode(map[string]string{"url": origin + scenario.path + "?state=" + scenario.state})
				case "/api/accounts/authorize", "/oauth/authorize":
					navigations++
					if r.URL.Query().Get("state") != "fixture-state" {
						t.Error("lost OAuth state")
					}
					http.Redirect(w, r, "/email-verification", 302)
				case "/email-verification":
					w.Write([]byte("email verification"))
				case "/api/accounts/email-otp/send":
					sends++
					w.Write([]byte("email verification"))
				default:
					t.Error("unexpected request")
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			base = upstream.URL
			flow, err := NewWebRegistrar(base, base, "direct")
			if err != nil {
				t.Fatal(err)
			}
			defer flow.Close()
			flow.resolveTimezone = nil
			flow.SetDeviceID("fixture-device")
			after, err := flow.BeginTwoFA(context.Background(), "fixture@example.invalid")
			if scenario.success {
				if err != nil || after.IsZero() || navigations != 1 || sends != 1 || flow.state != "fixture-state" {
					t.Fatalf("valid reauth rejected: %v, navigations=%d sends=%d", err, navigations, sends)
				}
			} else if err == nil || !strings.Contains(err.Error(), "invalid_authorize_url") || navigations != 0 || sends != 0 {
				t.Fatalf("unsafe URL accepted: %v, navigations=%d sends=%d", err, navigations, sends)
			}
		})
	}
}
