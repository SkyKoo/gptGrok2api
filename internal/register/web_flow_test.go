package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// This fixture checks the externally visible ordering/cookie contract against
// the historical web flow; it never calls ChatGPT or creates an actual account.
func TestWebRegistrationProtocol(t *testing.T) {
	for _, scenario := range []string{"success", "cookie_retained", "nested_callback", "otp_rejected", "foreign_callback", "wrong_state", "wrong_email", "missing_cookie", "existing_account", "challenge_403"} {
		t.Run(scenario, func(t *testing.T) {
			var base string
			csrfCount, sendCount, verifyCount, challengeCount := 0, 0, 0, 0
			var authorizeChallenge string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
				switch r.URL.Path {
				case "/api/auth/csrf":
					csrfCount++
					write(map[string]string{"csrfToken": "csrf"})
				case "/api/auth/signin/openai":
					if r.Header.Get("Origin") != base || r.Header.Get("x-openai-target-route") != "/api/auth/signin/{provider}" {
						t.Error("missing web client headers")
					}
					_ = r.ParseForm()
					if r.Form.Get("csrfToken") != "csrf" {
						t.Error("missing csrf")
					}
					write(map[string]string{"url": base + "/api/accounts/authorize?device_id=did&state=expected-state"})
				case "/api/accounts/authorize":
					if c, err := r.Cookie("oai-did"); err != nil || c.Value != "did" {
						t.Error("device cookie missing")
					}
					http.Redirect(w, r, "/log-in", 302)
				case "/log-in":
					http.SetCookie(w, &http.Cookie{Name: "auth-context", Value: "one", Path: "/"})
					_, _ = w.Write([]byte("login"))
				case "/backend-api/sentinel/req":
					challengeCount++
					if scenario == "challenge_403" {
						w.WriteHeader(403)
						_, _ = w.Write([]byte("secret upstream body"))
						return
					}
					if scenario != "missing_cookie" && !(scenario == "cookie_retained" && challengeCount == 2) {
						http.SetCookie(w, &http.Cookie{Name: "oai-sc", Value: "session-cookie", Path: "/"})
					}
					write(map[string]any{"token": "challenge-token", "proofofwork": map[string]any{"required": true, "seed": "seed", "difficulty": "f"}})
				case "/api/accounts/authorize/continue":
					if c, err := r.Cookie("auth-context"); err != nil || c.Value != "one" {
						t.Error("auth cookie lost")
					}
					authorizeChallenge = r.Header.Get("openai-sentinel-token")
					if authorizeChallenge == "" {
						t.Error("challenge missing")
					}
					page := "email_otp_verification"
					if scenario == "existing_account" {
						page = "password"
					}
					write(map[string]any{"page": map[string]string{"type": page}})
				case "/api/accounts/email-otp/send":
					sendCount++
					if challengeCount != 1 || r.Header.Get("openai-sentinel-token") != authorizeChallenge {
						t.Error("OTP challenge was regenerated")
					}
					write(map[string]any{"success": true})
				case "/api/accounts/email-otp/validate":
					verifyCount++
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["code"] != "987654" {
						t.Error("invalid OTP")
					}
					if scenario == "otp_rejected" {
						w.WriteHeader(400)
						write(map[string]string{"message": "SECRET invalid_state"})
						return
					}
					write(map[string]any{"continue_url": base + "/about-you"})
				case "/about-you":
					http.SetCookie(w, &http.Cookie{Name: "profile-context", Value: "ready", Path: "/"})
					_, _ = w.Write([]byte("profile"))
				case "/api/accounts/user/profile":
					if r.Header.Get("Origin") != base {
						t.Error("missing auth origin")
					}
					if c, err := r.Cookie("profile-context"); err != nil || c.Value != "ready" {
						t.Error("OTP continuation was not followed before profile")
					}
					callback := base + "/api/auth/callback/openai?state=expected-state&code=private"
					if scenario == "foreign_callback" {
						callback = "https://untrusted.invalid/api/auth/callback/openai"
					}
					if scenario == "wrong_state" {
						callback = base + "/api/auth/callback/openai?state=wrong"
					}
					if scenario == "nested_callback" {
						write(map[string]any{"page": map[string]any{"payload": map[string]string{"nextUrl": callback}}})
					} else {
						write(map[string]string{"continue_url": callback})
					}
				case "/api/auth/callback/openai":
					http.SetCookie(w, &http.Cookie{Name: "web-session", Value: "session", Path: "/"})
					http.Redirect(w, r, "/", 302)
				case "/":
					_, _ = w.Write([]byte("home"))
				case "/api/auth/session":
					if _, err := r.Cookie("web-session"); err != nil {
						t.Error("web cookie lost")
					}
					email := "alias@example.test"
					if scenario == "wrong_email" {
						email = "other@example.test"
					}
					write(map[string]any{"accessToken": "secret-account-token", "user": map[string]string{"email": email}})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base = server.URL
			flow, err := NewWebRegistrar(base, base, "direct")
			if err != nil {
				t.Fatal(err)
			}
			defer flow.Close()
			stages := []string{}
			account, err := flow.Register(context.Background(), Mailbox{Email: "alias@example.test"}, FreeConfig{Name: "Test User", Birthdate: "1990-01-01", MailTimeout: time.Second}, func(ctx context.Context, after time.Time) (string, error) {
				if sendCount != 1 || after.After(time.Now()) {
					t.Error("polled before send")
				}
				return "987654", nil
			}, func(s string) error { stages = append(stages, s); return nil })
			if csrfCount != 2 {
				t.Errorf("expected two initialization handshakes, got %d", csrfCount)
			}
			if scenario == "success" || scenario == "cookie_retained" || scenario == "nested_callback" {
				if err != nil {
					t.Fatal(err)
				}
				if account["access_token"] != "secret-account-token" || account["source_type"] != "chatgpt_web" || account["enabled"] != false {
					t.Fatalf("bad result %#v", account)
				}
				if sendCount != 1 || verifyCount != 1 || challengeCount != 2 {
					t.Errorf("counts send=%d verify=%d challenge=%d", sendCount, verifyCount, challengeCount)
				}
				if strings.Join(stages, ",") != "authorize,submit_email,send_code,wait_code,validate_code,create_profile,session" {
					t.Error("incorrect stage order")
				}
			} else {
				if err == nil || account != nil {
					t.Fatal("accepted failed registration")
				}
				if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "secret") {
					t.Fatal("leaked upstream response")
				}
			}
			if verifyCount > 1 {
				t.Fatal("one-time code was replayed")
			}
		})
	}
}

func TestRegistrationHashMatchesLegacyVectors(t *testing.T) {
	// Generated independently with the Python implementation retained in history.
	for input, want := range map[string]string{"": "ab3e7c0b", "seedabc": "1d8215f2", "中文": "39d7c2ee"} {
		if got := legacyRegistrationHash(input); got != want {
			t.Fatalf("%q: %s != %s", input, got, want)
		}
	}
}
