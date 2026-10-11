package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func waitRecovery(t *testing.T, s *Server) accountReloginProgress {
	t.Helper()
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		s.reloginMu.RLock()
		for _, p := range s.reloginProgress {
			if p.Done {
				result := reloginSummary(p)
				s.reloginMu.RUnlock()
				return result
			}
		}
		s.reloginMu.RUnlock()
	}
	t.Fatal("recovery did not complete")
	return accountReloginProgress{}
}

// Full OTP/session and subsequent Web account probe, all against one local mock.
func TestAutoReloginRecoveryRefreshesAccount(t *testing.T) {
	for _, scenario := range []string{"quotas", "manual_refresh", "manual_inflight", "survival", "login_failure", "quota_failure", "missing_limits", "disabled_during_login", "identity_mismatch", "new_token_rejected", "credentials_changed"} {
		t.Run(scenario, func(t *testing.T) {
			profile := registerruntime.NewBrowserProfile()
			const email = "recovery@example.test"
			const device = "persisted-device-42"
			var logins, probes atomic.Int32
			arrived, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var base string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
				if strings.HasPrefix(r.URL.Path, "/backend-api/") {
					if r.Header.Get("Authorization") != "Bearer new-private-token" {
						w.WriteHeader(401)
						return
					}
					probes.Add(1)
					if scenario == "new_token_rejected" {
						w.WriteHeader(401)
						return
					}
					switch r.URL.Path {
					case "/backend-api/me":
						id := "user-recovery"
						if scenario == "identity_mismatch" {
							id = "wrong-user"
						}
						write(map[string]any{"email": email, "id": id})
					case "/backend-api/conversation/init":
						if scenario == "quota_failure" {
							w.WriteHeader(503)
							return
						}
						if scenario == "missing_limits" {
							write(map[string]any{"default_model_slug": "auto"})
							return
						}
						write(map[string]any{"default_model_slug": "auto", "limits_progress": []any{
							map[string]any{"feature_name": "image_gen", "remaining": 23},
							map[string]any{"feature_name": "reason", "remaining": 49},
						}})
					case "/backend-api/accounts/check/v4-2023-04-27":
						write(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "free"}}}})
					default:
						t.Errorf("unexpected account endpoint: %s", r.URL.Path)
						http.NotFound(w, r)
					}
					return
				}
				switch r.URL.Path {
				case "/api/auth/login":
					logins.Add(1)
					close(arrived)
					<-release
					if scenario == "login_failure" {
						w.WriteHeader(401)
						write(map[string]any{"success": false})
						return
					}
					write(map[string]any{"success": true, "data": map[string]string{"csrf_token": "csrf-hme"}})
				case "/api/inbox":
					if r.URL.Query().Get("alias") != email {
						t.Error("wrong mailbox")
					}
					write(map[string]any{"success": true, "data": map[string]any{"messages": []any{map[string]string{"From": "noreply@openai.com", "To": email, "Date": time.Now().UTC().Format(time.RFC3339), "Subject": "Code 987654"}}}})
				case "/api/auth/providers":
					if r.Header.Get("User-Agent") != profile.UserAgent || r.Header.Get("Sec-Ch-Ua") != profile.SecCHUA || r.Header.Get("Oai-Device-Id") != device {
						t.Error("did not reuse bound browser environment")
					}
					write(map[string]any{})
				case "/api/auth/session":
					if _, err := r.Cookie("web-session"); err != nil {
						write(map[string]any{})
					} else {
						write(map[string]any{"accessToken": "new-private-token", "user": map[string]string{"email": email, "id": "user-recovery"}})
					}
				case "/api/auth/csrf":
					write(map[string]string{"csrfToken": "fresh-csrf"})
				case "/api/auth/signin/openai":
					if r.URL.Query().Get("ext-oai-did") != device {
						t.Error("device changed")
					}
					write(map[string]string{"url": base + "/api/accounts/authorize?device_id=" + device + "&state=fresh"})
				case "/api/accounts/authorize":
					http.Redirect(w, r, "/email-verification", 302)
				case "/email-verification", "/":
					_, _ = w.Write([]byte("page"))
				case "/api/accounts/email-otp/validate":
					write(map[string]string{"continue_url": base + "/api/auth/callback/openai?state=fresh&code=fresh"})
				case "/api/auth/callback/openai":
					http.SetCookie(w, &http.Cookie{Name: "web-session", Value: "new", Path: "/"})
					http.Redirect(w, r, "/", 302)
				default:
					t.Errorf("unexpected request (no registration, alias creation or OAuth refresh allowed): %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			defer unblock()
			base = upstream.URL
			cfg := adminTestConfig(t.TempDir())
			cfg.OpenAIBaseURL = base
			cfg.OpenAIAuthBaseURL = base
			s := New(cfg)
			_, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"access_token": "old-private-token", "email": email, "user_id": "user-recovery", "source_type": "chatgpt_web", "enabled": true, "status": "正常", "quota": 9, "browser_profile": profile, "fp": map[string]any{"oai-device-id": device}, "quota_pending": map[string]int{"image_gen": 2}, "quota_refresh_error": "old-error"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.registerStore.Update(map[string]any{"target": "openai", "mode": "total", "total": 1, "threads": 1, "proxy": "direct", "mail": map[string]any{"providers": []any{map[string]any{"id": "mock", "type": "icloud_hme", "api_base": base, "admin_password": "private-password", "account_id": "acc-mock", "enable": true}}}})
			if err != nil {
				t.Fatal(err)
			}
			original, _ := s.accountByRef("user-recovery")
			switch scenario {
			case "manual_inflight":
				if _, e := s.startAccountRelogin(original, false); e != nil {
					t.Fatal(e)
				}
			case "manual_refresh":
				s.refreshOneAccount(context.Background(), "test-refresh", "user-recovery")
			case "survival":
				s.survivalProbe(original, false)
			default:
				err = s.refreshAccountQuotas(context.Background(), accounts.Account{Token: accountToken(original), Fields: original})
				if !errors.Is(err, provider.ErrInvalidAccessToken) {
					t.Fatalf("unexpected quota failure: %v", err)
				}
			}
			select {
			case <-arrived:
			case <-time.After(3 * time.Second):
				t.Fatal("recovery not triggered")
			}
			// Duplicate background errors and manual clicks must join the same task.
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); s.recoverInvalidAccount(original, provider.ErrInvalidAccessToken) }()
			}
			wg.Wait()
			joined, err := s.startAccountRelogin(original, false)
			if err != nil || joined.Automatic != (scenario != "manual_inflight") {
				t.Fatalf("manual start did not join automatic task: %v", err)
			}
			rec := adminRequest(s.Handler(), http.MethodGet, "/api/accounts/relogin", nil)
			for _, secret := range []string{"private-token", "private-password", "browser_profile"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Fatal("progress leaked private state")
				}
			}
			if scenario == "disabled_during_login" {
				if _, _, e := s.store.UpdateAccount("old-private-token", map[string]any{"enabled": false}); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "credentials_changed" {
				_, _, err = s.store.RotateAccountTokens("old-private-token", "operator-new-token", "", "", map[string]any{"status": "正常"})
				if err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			p := waitRecovery(t, s)
			if logins.Load() != 1 {
				t.Fatalf("launched %d logins", logins.Load())
			}
			saved, _ := s.accountByRef("user-recovery")
			switch scenario {
			case "login_failure", "identity_mismatch", "new_token_rejected":
				wantStage := "refresh_account"
				if scenario == "login_failure" {
					wantStage = "mail"
				}
				if p.FailedStage != wantStage {
					t.Fatalf("unexpected failure stage: %+v", p)
				}
				if p.Error == "" || p.SessionUpdated || accountToken(saved) != "old-private-token" || intValue(saved["quota"]) != 9 {
					t.Fatal("failed login replaced old account")
				}
			case "credentials_changed":
				if p.Error == "" || accountToken(saved) != "operator-new-token" {
					t.Fatal("overwrote newer credentials")
				}
			case "quota_failure", "missing_limits":
				if p.Error == "" || !p.SessionUpdated || p.QuotasRefreshed || accountToken(saved) != "new-private-token" || intValue(saved["quota"]) != 9 || saved["quota_refresh_needed"] != true {
					t.Fatalf("partial success incorrectly handled: %+v", p)
				}
			default:
				if p.Error != "" || !p.QuotasRefreshed || !p.SessionUpdated {
					t.Fatalf("recovery failed: %+v", p)
				}
				if accountToken(saved) != "new-private-token" || intValue(saved["quota"]) != 23 || saved["status"] != "正常" || probes.Load() != 3 {
					t.Fatal("new token not used for full account refresh")
				}
				if scenario == "disabled_during_login" && saved["enabled"] != false {
					t.Fatal("re-enabled account during login")
				}
				quotas := accounts.DecodeQuotas(saved["capability_quotas"])
				if quotas["reason"].Remaining == nil || *quotas["reason"].Remaining != 49 || stringValue(saved["quota_refresh_error"]) != "" || saved["quota_refresh_needed"] != false {
					t.Fatal("quota snapshot/markers not updated")
				}
			}
			failed := scenario == "login_failure" || scenario == "identity_mismatch" || scenario == "new_token_rejected" || scenario == "quota_failure" || scenario == "missing_limits"
			if failed {
				if saved["status"] != "异常" || saved["relogin_blocked"] != true || accountStatusCategory(saved) != "abnormal" {
					t.Fatal("failed recovery did not stop scheduling")
				}
				reopened := New(cfg)
				persisted, _ := reopened.accountByRef("user-recovery")
				if persisted["relogin_blocked"] != true {
					t.Fatal("restart lost failed-login block")
				}
				for _, intent := range []accounts.Intent{{}, {Kind: "chat"}, {Kind: "image"}} {
					lease, e := reopened.accountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, intent)
					if !errors.Is(e, accounts.ErrUnavailable) {
						reopened.accountPool.Release(lease)
						t.Fatalf("failed recovery was scheduled for %q: %v", intent.Kind, e)
					}
				}
			} else if saved["relogin_blocked"] == true || stringValue(saved["last_relogin_error"]) != "" {
				t.Fatal("successful/newer credential incorrectly blocked")
			}
			// Delayed failures from the old credential cannot start another login or
			// mark the newly refreshed credential invalid. Cooldown survives restart.
			s.recoverInvalidAccount(original, provider.ErrInvalidAccessToken)
			restarted := New(cfg)
			restarted.recoverInvalidAccount(saved, provider.ErrInvalidAccessToken)
			if len(restarted.reloginProgress) != 0 {
				t.Fatal("restart bypassed persisted cooldown")
			}
			if scenario != "login_failure" && scenario != "identity_mismatch" && scenario != "new_token_rejected" {
				// Check the stale callback alone (the explicit current-token probe above
				// legitimately records auth failure in the restarted store).
				current, _ := s.accountByRef("user-recovery")
				if accountToken(current) != accountToken(saved) {
					t.Fatal("stale failure changed token")
				}
			}
		})
	}
}

func TestAutoReloginGatesAndConcurrency(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	profile := registerruntime.NewBrowserProfile()
	original := map[string]any{"access_token": "old", "user_id": "user-gate", "email": "gate@example.test", "enabled": true, "source_type": "chatgpt_web", "browser_profile": profile, "fp": map[string]any{"oai-device-id": "same-device"}}
	_, _, _, err := s.store.AddAccounts(nil, []map[string]any{original})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []error{nil, context.DeadlineExceeded, errors.New("HTTP 403 unauthorized"), errors.New("HTTP 429"), errors.New("HTTP 502")} {
		s.recoverInvalidAccount(original, e)
	}
	if len(s.reloginProgress) != 0 {
		t.Fatal("transient failure launched login")
	}
	for _, scenario := range []string{"disabled", "has_rt", "missing_profile", "missing_device", "bad_profile", "missing_email", "maintenance", "capacity"} {
		t.Run(scenario, func(t *testing.T) {
			item := cloneMap(original)
			switch scenario {
			case "disabled":
				item["enabled"] = false
			case "has_rt":
				item["refresh_token"] = "private-rt"
			case "missing_profile":
				item["browser_profile"] = nil
			case "missing_device":
				item["fp"] = nil
			case "bad_profile":
				item["browser_profile"] = map[string]any{"name": "unknown"}
			case "missing_email":
				item["email"] = ""
			case "maintenance":
				if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cfg.DataDir, "cfm-maintenance"), []byte("test"), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(filepath.Join(cfg.DataDir, "cfm-maintenance"))
			case "capacity":
				s.reloginProgress["a"] = &accountReloginProgress{AccountRef: "a", Automatic: true}
				s.reloginProgress["b"] = &accountReloginProgress{AccountRef: "b", Automatic: true}
				defer delete(s.reloginProgress, "a")
				defer delete(s.reloginProgress, "b")
			}
			item["refresh_token"] = firstNonEmpty(stringValue(item["refresh_token"]), "")
			_, _, err = s.store.UpdateAccount("old", item)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.startAccountRelogin(item, true); err == nil {
				t.Fatal("automatic gate bypassed")
			}
		})
	}
	// A deleted account is never recreated by a stale refresh failure.
	_, _, _ = s.store.DeleteAccounts([]string{"old"})
	s.recoverInvalidAccount(original, provider.ErrInvalidAccessToken)
	items, _ := s.store.AccountList()
	if len(items) != 0 {
		t.Fatal("recreated deleted account")
	}
}
