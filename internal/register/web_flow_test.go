package register

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This fixture models the Turb protocol ordering without contacting OpenAI.
// The first OTP is triggered by the authorize redirect; email-otp/send is only
// used after a wait timeout. The profile branch also covers the protocol-only
// about-you submission used for new accounts.
func TestWebRegistrationTurbProtocol(t *testing.T) {
	for _, scenario := range []string{"success", "oauth_authorize", "existing_success", "resend", "profile_required", "existing_profile_required", "wrong_state"} {
		t.Run(scenario, func(t *testing.T) {
			var base string
			authorizePath := "/api/accounts/authorize"
			if scenario == "oauth_authorize" {
				authorizePath = "/oauth/authorize"
			}
			var timezoneCalls atomic.Int32
			savedProfile := browserProfilePool[4]
			phoenix, err := time.LoadLocation("America/Phoenix")
			if err != nil {
				t.Fatal(err)
			}
			providers, anonymousSession, csrf, signin, authorize, resend, validate, callback := 0, 0, 0, 0, 0, 0, 0, 0
			aboutYou, sentinel, createProfile := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if timezoneCalls.Load() != 1 {
					t.Error("auth started before timezone resolution or resolved repeatedly")
				}
				if r.Header.Get("User-Agent") != savedProfile.UserAgent {
					t.Error("stable browser identity changed")
				}
				write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
				switch r.URL.Path {
				case "/api/auth/providers":
					providers++
					write(map[string]any{"openai": map[string]string{"id": "openai"}})
				case "/api/auth/session":
					if _, err := r.Cookie("web-session"); err != nil {
						anonymousSession++
						write(map[string]any{})
					} else {
						write(map[string]any{"accessToken": "secret-account-token", "user": map[string]string{"email": "alias@example.test"}})
					}
				case "/api/auth/csrf":
					csrf++
					write(map[string]string{"csrfToken": "csrf"})
				case "/api/auth/signin/openai":
					signin++
					if r.URL.Query().Get("login_hint") != "alias@example.test" || r.URL.Query().Get("screen_hint") != "login_or_signup" || r.URL.Query().Get("ext-oai-did") == "" || r.URL.Query().Get("auth_session_logging_id") == "" {
						t.Error("Turb signin query is incomplete")
					}
					if err := r.ParseForm(); err != nil || r.Form.Get("csrfToken") != "csrf" || r.Form.Get("callbackUrl") != "/" {
						t.Error("Turb signin form is incomplete")
					}
					write(map[string]string{"url": base + authorizePath + "?device_id=did&state=expected-state"})
				case "/api/accounts/authorize", "/oauth/authorize":
					authorize++
					if c, err := r.Cookie("oai-did"); err != nil || c.Value != "did" {
						t.Error("device cookie missing")
					}
					http.Redirect(w, r, "/email-verification", http.StatusFound)
				case "/email-verification":
					_, _ = w.Write([]byte("verification page"))
				case "/api/accounts/email-otp/send":
					resend++
					_, _ = w.Write([]byte("verification page"))
				case "/api/accounts/email-otp/validate":
					validate++
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["code"] != "987654" {
						t.Error("invalid OTP")
					}
					if scenario == "profile_required" || scenario == "existing_profile_required" {
						write(map[string]any{"page": map[string]string{"type": "about_you"}})
						return
					}
					nextState := "expected-state"
					if scenario == "wrong_state" {
						nextState = "wrong-state"
					}
					write(map[string]string{"continue_url": base + "/api/auth/callback/openai?state=" + nextState + "&code=private"})
				case "/about-you":
					aboutYou++
					_, _ = w.Write([]byte("about-you"))
				case "/backend-api/sentinel/frame.html":
					sentinel++
					_, _ = w.Write([]byte("sentinel-frame"))
				case "/backend-api/sentinel/req":
					sentinel++
					var challenge map[string]string
					if err := json.NewDecoder(r.Body).Decode(&challenge); err != nil {
						t.Error(err)
					}
					raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(challenge["p"], "gAAAAAC"))
					var proof []any
					if err != nil || json.Unmarshal(raw, &proof) != nil || len(proof) < 2 {
						t.Error("invalid challenge proof")
					} else {
						date, err := time.Parse("Mon Jan 02 2006 15:04:05 GMT-0700 (MST)", stringValue(proof[1]))
						_, offset := date.Zone()
						if err != nil || offset != -7*3600 || time.Since(date) > 5*time.Second {
							t.Errorf("challenge date not aligned to Phoenix: %v", proof[1])
						}
					}
					if r.Method != http.MethodPost {
						t.Error("sentinel request must be POST")
					}
					http.SetCookie(w, &http.Cookie{Name: "oai-sc", Value: "challenge-session", Path: "/"})
					write(map[string]string{"token": "challenge-token"})
				case "/api/accounts/create_account":
					createProfile++
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["name"] != "Alias User" || body["birthdate"] == "" {
						t.Errorf("unexpected generated profile: %#v", body)
					}
					write(map[string]string{"continue_url": base + "/api/auth/callback/openai?state=expected-state&code=private"})
				case "/api/auth/callback/openai":
					callback++
					if r.URL.Query().Get("state") != "expected-state" {
						t.Error("callback state was not checked before request")
					}
					http.SetCookie(w, &http.Cookie{Name: "web-session", Value: "session", Path: "/"})
					http.Redirect(w, r, "/", http.StatusFound)
				case "/":
					_, _ = w.Write([]byte("home"))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base = server.URL
			flow, err := NewWebRegistrar(base, base, "direct", savedProfile)
			if err != nil {
				t.Fatal(err)
			}
			defer flow.Close()
			flow.sentinel = base
			flow.resolveTimezone = func(ctx context.Context, origin, proxy string) *time.Location {
				if origin != base || proxy != "direct" {
					t.Error("timezone uses another egress")
				}
				timezoneCalls.Add(1)
				return phoenix
			}
			if err := flow.SetDeviceID("did"); err != nil {
				t.Fatal(err)
			}
			stages := []string{}
			waitCalls := 0
			login := flow.Register
			if scenario == "existing_profile_required" || scenario == "existing_success" {
				login = flow.LoginExisting
			}
			account, err := login(context.Background(), Mailbox{Email: "alias@example.test"}, FreeConfig{MailTimeout: 20 * time.Millisecond}, func(ctx context.Context, after time.Time) (string, error) {
				waitCalls++
				if scenario == "resend" && waitCalls == 1 {
					return "", context.DeadlineExceeded
				}
				return "987654", nil
			}, func(stage string) error { stages = append(stages, stage); return nil })

			if providers != 1 || anonymousSession != 1 || csrf != 1 || signin != 1 || authorize != 1 {
				t.Errorf("initialization counts providers=%d session=%d csrf=%d signin=%d authorize=%d", providers, anonymousSession, csrf, signin, authorize)
			}
			if scenario == "success" || scenario == "oauth_authorize" || scenario == "existing_success" || scenario == "resend" || scenario == "profile_required" {
				if err != nil || account["access_token"] != "secret-account-token" {
					t.Fatalf("unexpected result account=%#v err=%v", account, err)
				}
				rawCookies, _ := json.Marshal(account["web_cookies"])
				if !strings.Contains(string(rawCookies), "web-session") {
					t.Fatal("successful registration omitted private cookies")
				}
				if stringValue(account["web_cookies_token_hash"]) == "" {
					t.Fatal("successful registration omitted cookie credential binding")
				}
				wantProfile := savedProfile
				wantProfile.setTimezone(phoenix, time.Now())
				if got, ok := ParseBrowserProfile(account["browser_profile"]); !ok || !reflect.DeepEqual(got, wantProfile) {
					t.Error("returned profile lost resolved timezone or changed stable fields")
				}
				if fp := object(account["fp"]); fp["timezone"] != "America/Phoenix" || fp["oai-device-id"] != "did" {
					t.Error("returned fingerprint lost resolved timezone or device identity")
				}
				wantResend := 0
				wantStages := "authorize,wait_code,validate_code,session"
				if scenario == "resend" {
					wantResend = 1
					wantStages = "authorize,wait_code,send_code,wait_code,validate_code,session"
				}
				if resend != wantResend || validate != 1 || callback != 1 {
					t.Errorf("counts resend=%d validate=%d callback=%d", resend, validate, callback)
				}
				if scenario == "profile_required" && (aboutYou != 1 || sentinel != 2 || createProfile != 1) {
					t.Errorf("profile counts about_you=%d sentinel=%d create_profile=%d", aboutYou, sentinel, createProfile)
				}
				if strings.Join(stages, ",") != wantStages {
					t.Errorf("stages=%s want=%s", strings.Join(stages, ","), wantStages)
				}
			} else {
				if err == nil || account != nil {
					t.Fatal("accepted invalid registration")
				}
				if (scenario == "profile_required" || scenario == "existing_profile_required") && !strings.Contains(err.Error(), "profile_completion_required") {
					t.Fatalf("wrong profile error: %v", err)
				}
				if scenario == "wrong_state" && !strings.Contains(err.Error(), "callback_state_mismatch") {
					t.Fatalf("wrong state error: %v", err)
				}
			}
			if scenario == "existing_profile_required" && (aboutYou != 0 || sentinel != 0 || createProfile != 0 || callback != 0) {
				t.Errorf("existing login attempted profile completion: about_you=%d sentinel=%d create_profile=%d callback=%d", aboutYou, sentinel, createProfile, callback)
			}
			if scenario != "resend" && resend != 0 {
				t.Errorf("initial protocol unexpectedly called send endpoint: %d", resend)
			}
		})
	}
}

func TestRegistrationHashMatchesLegacyVectors(t *testing.T) {
	for input, want := range map[string]string{"": "ab3e7c0b", "seedabc": "1d8215f2", "中文": "39d7c2ee"} {
		if got := legacyRegistrationHash(input); got != want {
			t.Fatalf("%q: %s != %s", input, got, want)
		}
	}
}
