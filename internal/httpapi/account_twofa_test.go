package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func twoFARequest(t *testing.T, s *Server, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "/api/accounts/twofa", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func twoFATaskAt(t *testing.T, s *Server, want string) accountTwoFAProgress {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		rec := twoFARequest(t, s, http.MethodGet, nil)
		var body struct {
			Items []accountTwoFAProgress `json:"items"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		for _, p := range body.Items {
			if p.Stage == want || p.Status == want {
				return p
			}
		}
	}
	t.Fatalf("task did not reach %s", want)
	return accountTwoFAProgress{}
}

func TestAccountTwoFAFlowAndRecovery(t *testing.T) {
	for _, scenario := range []string{"success", "activate_timeout", "identity_mismatch", "state_mismatch", "pending_save_failure", "resume_pending", "cancel", "hme"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			accountPath := filepath.Join(root, "accounts.json")
			var base string
			var enrolls, activations atomic.Int32
			const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
			profile := registerruntime.NewBrowserProfile()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
				if r.URL.Path != "/api/auth/login" && r.URL.Path != "/api/inbox" && r.Header.Get("User-Agent") != profile.UserAgent {
					t.Error("lost saved browser profile")
				}
				switch r.URL.Path {
				case "/api/auth/login":
					write(map[string]any{"success": true, "data": map[string]string{"csrf_token": "hme-test-csrf"}})
				case "/api/inbox":
					write(map[string]any{"success": true, "data": map[string]any{"messages": []any{map[string]string{"From": "noreply@openai.com", "To": "alias@example.test", "Date": time.Now().UTC().Format(time.RFC3339), "Subject": "Code 123456"}}}})

				case "/api/auth/csrf":
					write(map[string]string{"csrfToken": "csrf-test"})
				case "/api/auth/signin/openai":
					_ = r.ParseForm()
					if r.URL.Query().Get("reauth") != "password" || r.URL.Query().Get("ext-oai-did") != "saved-device" || r.Form.Get("callbackUrl") != base+"/?action=enable&factor=totp" {
						t.Error("not using reauthentication")
					}
					write(map[string]string{"url": base + "/api/accounts/authorize?state=expected-state"})
				case "/api/accounts/authorize", "/api/accounts/email-otp/send":
					http.Redirect(w, r, "/email-verification", 302)
				case "/email-verification", "/":
					_, _ = w.Write([]byte("page"))
				case "/api/accounts/email-otp/validate":
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["code"] != "123456" {
						t.Error("wrong mailbox code")
					}
					state := "expected-state"
					if scenario == "state_mismatch" {
						state = "other"
					}
					write(map[string]string{"continue_url": base + "/api/auth/callback/openai?code=private-code&state=" + state})
				case "/api/auth/callback/openai":
					http.Redirect(w, r, "/", 302)
				case "/api/auth/session":
					email := "alias@example.test"
					if scenario == "identity_mismatch" {
						email = "other@example.test"
					}
					write(map[string]any{"accessToken": "fresh-private-token", "user": map[string]string{"email": email, "id": "user-1"}})
				case "/backend-api/accounts/mfa/enroll":
					enrolls.Add(1)
					if r.Header.Get("Authorization") != "Bearer fresh-private-token" {
						t.Error("not using fresh reauth token")
					}
					if scenario == "pending_save_failure" {
						if err := os.Rename(accountPath, accountPath+".before"); err != nil {
							t.Error(err)
						}
						if err := os.Mkdir(accountPath, 0700); err != nil {
							t.Error(err)
						}
					}
					write(map[string]string{"secret": secret, "session_id": "enroll-session"})
				case "/backend-api/accounts/mfa/user/activate_enrollment":
					activations.Add(1)
					raw, err := os.ReadFile(accountPath)
					if err != nil || !strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "twofa_pending") {
						t.Error("activation happened before durable secret backup")
					}
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					code, _ := registerruntime.TOTPCode(secret, time.Now())
					if body["code"] != code || body["session_id"] != "enroll-session" {
						t.Error("invalid TOTP activation body")
					}
					if scenario == "activate_timeout" {
						w.WriteHeader(504)
						write(map[string]bool{"success": false})
						return
					}
					write(map[string]bool{"success": true})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			base = upstream.URL
			cfg := testConfig()
			cfg.RootDir, cfg.DataDir = root, root
			cfg.AccountsPath = accountPath
			cfg.AuthKeysPath = filepath.Join(root, "keys.json")
			cfg.ConfigPath = filepath.Join(root, "config.json")
			cfg.RegisterPath = filepath.Join(root, "register.json")
			cfg.OpenAIBaseURL, cfg.OpenAIAuthBaseURL = base+"/backend-api", base
			s := New(cfg)
			original := map[string]any{"access_token": "original-private-token", "email": "alias@example.test", "user_id": "user-1", "source_type": "chatgpt_web", "enabled": false, "browser_profile": profile, "fp": map[string]any{"oai-device-id": "saved-device"}}
			if scenario == "resume_pending" {
				original["twofa_pending"] = map[string]any{"secret": secret, "session_id": "enroll-session"}
				original["twofa_setup"] = map[string]any{"progress_id": "previous-task", "status": "running"}
			}
			if scenario == "hme" {
				original["registration_mailbox"] = map[string]any{"provider": "icloud_hme", "email": "alias@example.test", "account_id": "hme-account"}
				if _, err := s.registerStore.Update(map[string]any{"target": "openai", "mode": "total", "total": 1, "threads": 1, "mail": map[string]any{"providers": []any{map[string]any{"type": "icloud_hme", "enable": true, "api_base": base, "admin_password": "fake-test-password", "account_id": "hme-account"}}}}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "hme" {
				if _, err := registerruntime.ParseFreeConfig(s.registerStore.Get()); err != nil {
					t.Fatalf("HME fixture config: %v", err)
				}
			}
			if err := s.store.SaveAccounts([]map[string]any{original}); err != nil {
				t.Fatal(err)
			}
			if scenario == "resume_pending" {
				rec := twoFARequest(t, s, "GET", nil)
				if !strings.Contains(rec.Body.String(), "interrupted") {
					t.Fatal("restart was not surfaced")
				}
			}
			unauth := httptest.NewRecorder()
			s.Handler().ServeHTTP(unauth, httptest.NewRequest("POST", "/api/accounts/twofa", nil))
			if unauth.Code != 401 {
				t.Fatal("missing admin authentication")
			}
			rec := twoFARequest(t, s, "POST", map[string]string{"account_ref": "user-1"})
			if rec.Code != 200 {
				t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
			}
			if scenario == "hme" {
				twoFATaskAt(t, s, "completed")
				rows, _ := s.store.AccountList()
				if rows[0]["two_factor_secret"] != secret {
					t.Fatal("HME did not complete setup")
				}
				return
			}
			p := twoFATaskAt(t, s, "waiting_email_code")
			dup := twoFARequest(t, s, "POST", map[string]string{"account_ref": "user-1"})
			if !strings.Contains(dup.Body.String(), p.ID) {
				t.Fatal("did not reuse active task")
			}
			// All authentication entry points reject work on an account being enrolled.
			if _, err := s.startAccountRelogin(original, false); err == nil {
				t.Fatal("relogin overlapped 2FA")
			}
			oauthReq := httptest.NewRequest("POST", "/api/accounts/codex/oauth/start", strings.NewReader(`{"account_ref":"user-1"}`))
			oauthReq.Header.Set("Authorization", "Bearer admin-secret")
			oauthRec := httptest.NewRecorder()
			s.Handler().ServeHTTP(oauthRec, oauthReq)
			if oauthRec.Code != 409 {
				t.Fatalf("OAuth overlap: %d %s", oauthRec.Code, oauthRec.Body.String())
			}
			action := "code"
			if scenario == "cancel" {
				action = "cancel"
			}
			rec = twoFARequest(t, s, "POST", map[string]string{"progress_id": p.ID, "action": action, "code": "123456"})
			if rec.Code != 200 {
				t.Fatalf("submit: %s", rec.Body.String())
			}
			if action == "code" {
				duplicate := twoFARequest(t, s, "POST", map[string]string{"progress_id": p.ID, "action": "code", "code": "123456"})
				if duplicate.Code != 409 {
					t.Fatal("accepted a duplicate email code")
				}
			}
			want := "failed"
			if scenario == "success" || scenario == "resume_pending" {
				want = "completed"
			}
			if scenario == "cancel" {
				want = "cancelled"
			}
			p = twoFATaskAt(t, s, want)
			rec = twoFARequest(t, s, "GET", nil)
			for _, private := range []string{secret, "original-private-token", "fresh-private-token", "123456", "enroll-session"} {
				if strings.Contains(rec.Body.String(), private) {
					t.Fatalf("progress leaked %s", private)
				}
			}
			rows, _ := s.store.AccountList()
			row := rows[0]
			public, _ := json.Marshal(accountForAPI(row))
			if strings.Contains(string(public), secret) || strings.Contains(string(public), "enroll-session") {
				t.Fatal("account list leaked pending secret")
			}
			if row["access_token"] != "original-private-token" || row["enabled"] != false {
				t.Fatal("2FA changed account session or scheduling")
			}
			switch scenario {
			case "success", "resume_pending":
				if row["two_factor_secret"] != secret || row["twofa_pending"] != nil {
					t.Fatal("secret was not promoted atomically")
				}
				if scenario == "resume_pending" && enrolls.Load() != 0 {
					t.Fatal("re-enrolled instead of resuming")
				}
				restarted := New(cfg)
				twoFATaskAt(t, restarted, "completed")
			case "activate_timeout":
				if stringValue(mapValue(row["twofa_pending"])["secret"]) != secret || stringValue(row["two_factor_secret"]) != "" || activations.Load() != 1 {
					t.Fatal("uncertain activation lost secret or retried")
				}
				restarted := New(cfg)
				rp := twoFATaskAt(t, restarted, "failed")
				if !rp.Pending {
					t.Fatal("restart lost pending flag")
				}
				// Recovery material is available only through the explicit
				// admin credential endpoint, separate from confirmed secrets.
				req := httptest.NewRequest("POST", "/api/accounts/token", strings.NewReader(`{"access_token":"user-1"}`))
				req.Header.Set("Authorization", "Bearer admin-secret")
				rec := httptest.NewRecorder()
				restarted.Handler().ServeHTTP(rec, req)
				var credentials map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &credentials)
				if credentials["two_factor_pending_secret"] != secret || credentials["two_factor_secret"] != "" {
					t.Fatal("pending recovery secret unavailable or conflated")
				}
				body, _ := json.Marshal(map[string]string{"access_token": "user-1", "two_factor_secret": secret})
				req = httptest.NewRequest("POST", "/api/accounts/update", strings.NewReader(string(body)))
				req.Header.Set("Authorization", "Bearer admin-secret")
				rec = httptest.NewRecorder()
				restarted.Handler().ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("manual recovery: %d", rec.Code)
				}
				rp = twoFATaskAt(t, restarted, "completed")
				if rp.Pending {
					t.Fatal("manual confirmation retained pending state")
				}

			case "identity_mismatch", "state_mismatch", "cancel":
				if enrolls.Load() != 0 || activations.Load() != 0 {
					t.Fatal("unsafe enrollment after rejected verification")
				}
			case "pending_save_failure":
				if activations.Load() != 0 {
					t.Fatal("activated after backup failure")
				}
			}
		})
	}
}

func TestTwoFARejectsConflictingAndAlreadyConfiguredAccounts(t *testing.T) {
	cfg := testConfig()
	cfg.RootDir = t.TempDir()
	cfg.DataDir = cfg.RootDir
	cfg.AccountsPath = filepath.Join(cfg.DataDir, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(cfg.DataDir, "keys.json")
	cfg.ConfigPath = filepath.Join(cfg.DataDir, "config.json")
	cfg.RegisterPath = filepath.Join(cfg.DataDir, "register.json")
	s := New(cfg)
	original := map[string]any{"access_token": "test-token", "user_id": "user-1", "email": "alias@example.test", "source_type": "chatgpt_web"}
	if err := s.store.SaveAccounts([]map[string]any{original}); err != nil {
		t.Fatal(err)
	}
	s.reloginProgress["login"] = &accountReloginProgress{AccountRef: "user-1"}
	if rec := twoFARequest(t, s, "POST", map[string]string{"account_ref": "user-1"}); rec.Code != 409 {
		t.Fatalf("relogin conflict: %d", rec.Code)
	}
	delete(s.reloginProgress, "login")
	s.codexProgress["oauth"] = &codexOAuthProgress{AccountRef: "user-1", Status: "running", StartedAt: time.Now().Format(time.RFC3339Nano)}
	if rec := twoFARequest(t, s, "POST", map[string]string{"account_ref": "user-1"}); rec.Code != 409 {
		t.Fatalf("OAuth conflict: %d", rec.Code)
	}
	delete(s.codexProgress, "oauth")
	_, _, _ = s.store.UpdateAccount("test-token", map[string]any{"two_factor_secret": "already-configured"})
	if rec := twoFARequest(t, s, "POST", map[string]string{"account_ref": "user-1"}); rec.Code != 409 {
		t.Fatalf("existing secret: %d", rec.Code)
	}
}
