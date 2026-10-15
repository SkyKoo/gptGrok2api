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
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/browserprofile"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func seedValidation(t *testing.T, s *Server) map[string]any {
	t.Helper()
	state := map[string]any{"id": "validation-generation", "progress_id": "login-task", "attempts": 0, "started_at": time.Now().UTC().Format(time.RFC3339Nano)}
	fields := validationFailureFields(&provider.AccountEndpointError{Stage: "quota", Endpoint: "/backend-api/conversation/init", Status: 503, Kind: "upstream_unavailable", Message: "busy", Retryable: true}, state, time.Now().Add(-time.Minute))
	for k, v := range map[string]any{"access_token": "saved-token", "email": "saved@example.test", "user_id": "user-saved", "source_type": "chatgpt_web", "browser_profile": browserprofile.Default(), "quota": 9, "enabled": true, "auto_relogin_next_attempt_at": time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339), "fp": map[string]any{"oai-device-id": "saved-device", "user-agent": browserprofile.Default().UserAgent}} {
		fields[k] = v
	}
	if _, _, _, err := s.store.AddAccounts(nil, []map[string]any{fields}); err != nil {
		t.Fatal(err)
	}
	row, _ := s.accountByRef("user-saved")
	return row
}

func validationUpstream(t *testing.T, status *atomic.Int32, requests *atomic.Int32, hook func()) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer saved-token" || r.Header.Get("Oai-Device-Id") != "saved-device" || r.Header.Get("User-Agent") != browserprofile.Default().UserAgent {
			t.Error("did not reuse saved token/environment")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			if hook != nil {
				hook()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "saved@example.test", "id": "user-saved"})
		case "/backend-api/conversation/init":
			if code := int(status.Load()); code != 200 {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"message":"backend temporarily unavailable","access_token":"saved-token"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 25}, map[string]any{"feature_name": "reason", "remaining": 50}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "free"}}}})
		default:
			t.Errorf("validation must not send login/OTP/OAuth or generation requests: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
}

func assertValidationBlocked(t *testing.T, s *Server) {
	t.Helper()
	row, _ := s.accountByRef("user-saved")
	if row["relogin_blocked"] != true || accountStatusCategory(row) != "abnormal" {
		t.Fatal("pending validation account became available")
	}
	for _, kind := range []string{"chat", "image"} {
		lease, err := s.accountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: kind})
		if !errors.Is(err, accounts.ErrUnavailable) {
			s.accountPool.Release(lease)
			t.Fatalf("scheduled blocked %s account", kind)
		}
	}
}

func TestValidationRetryPersistsReusesTokenAndRestoresScheduling(t *testing.T) {
	var status, requests atomic.Int32
	status.Store(503)
	upstream := validationUpstream(t, &status, &requests, nil)
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.OpenAIBaseURL = upstream.URL
	s := New(cfg)
	seedValidation(t, s)
	assertValidationBlocked(t, s)
	s.refreshDueCapabilities() // Must bypass the unrelated 30-minute login cooldown.
	row, _ := s.accountByRef("user-saved")
	state := mapValue(row["relogin_validation"])
	if intValue(state["attempts"]) != 1 || state["pending"] != true || requests.Load() != 2 {
		t.Fatal("did not run one full validation attempt")
	}
	if intValue(row["quota"]) != 9 || accountToken(row) != "saved-token" {
		t.Fatal("failed validation overwrote credentials/quota")
	}
	diagnostic := mapValue(row["last_account_validation_error"])
	if intValue(diagnostic["status"]) != 503 || stringValue(diagnostic["endpoint"]) != "/backend-api/conversation/init" || strings.Contains(stringValue(row["quota_refresh_error"]), "saved-token") {
		t.Fatal("missing or unsafe diagnostic")
	}
	next, _ := time.Parse(time.RFC3339Nano, stringValue(state["next_attempt_at"]))
	if next.Before(time.Now().Add(110 * time.Second)) {
		t.Fatal("missing backoff")
	}
	s = New(cfg) // Reload from disk, no login history or runtime task required.
	row, _ = s.accountByRef("user-saved")
	before := requests.Load()
	if s.retryAccountValidation(row, time.Now()) || requests.Load() != before {
		t.Fatal("restart bypassed retry deadline")
	}
	response := adminRequest(s.Handler(), http.MethodGet, "/api/accounts/relogin", nil)
	if !strings.Contains(response.Body.String(), "next_validation_at") || !strings.Contains(response.Body.String(), "quota") || strings.Contains(response.Body.String(), "saved-token") {
		t.Fatal("pending state not safely visible after restart")
	}
	status.Store(200)
	if !s.retryAccountValidation(row, next.Add(time.Second)) {
		t.Fatal("retry did not execute")
	}
	row, _ = s.accountByRef("user-saved")
	if row["relogin_blocked"] == true || row["relogin_validation"] != nil || intValue(row["quota"]) != 25 || row["quota_refresh_needed"] != false || accountStatusCategory(row) != "normal" {
		t.Fatal("full validation did not restore account")
	}
	if requests.Load() != 5 {
		t.Fatal("unexpected upstream attempts")
	}
	s = New(cfg)
	row, _ = s.accountByRef("user-saved")
	if row["relogin_blocked"] == true || s.retryAccountValidation(row, time.Now().Add(time.Hour)) {
		t.Fatal("success not persisted")
	}
	lease, err := s.accountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: "image"})
	if err != nil {
		t.Fatalf("verified account still unavailable: %v", err)
	}
	s.accountPool.Release(lease)
}

func TestValidationRetryStopsAfterBudgetOrPermanentError(t *testing.T) {
	for _, code := range []int{503, 401, 403} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var status, requests atomic.Int32
			status.Store(int32(code))
			upstream := validationUpstream(t, &status, &requests, nil)
			defer upstream.Close()
			cfg := adminTestConfig(t.TempDir())
			cfg.OpenAIBaseURL = upstream.URL
			s := New(cfg)
			row := seedValidation(t, s)
			want := 3
			if code != 503 {
				want = 1
			}
			for i := 0; i < want; i++ {
				if !s.retryAccountValidation(row, time.Now().Add(time.Hour)) {
					t.Fatal("missing attempt")
				}
				s = New(cfg)
				row, _ = s.accountByRef("user-saved")
			}
			if s.retryAccountValidation(row, time.Now().Add(24*time.Hour)) || mapValue(row["relogin_validation"])["pending"] == true {
				t.Fatal("exceeded retry budget or retried permanent failure")
			}
			if requests.Load() != int32(want*2) {
				t.Fatal("unexpected requests")
			}
			assertValidationBlocked(t, s)
		})
	}
}

func TestValidationConcurrentChangesArePreserved(t *testing.T) {
	for _, change := range []string{"delete", "rotate", "verified", "disabled"} {
		t.Run(change, func(t *testing.T) {
			var status, requests atomic.Int32
			status.Store(503)
			if change == "disabled" {
				status.Store(200)
			}
			var s *Server
			upstream := validationUpstream(t, &status, &requests, func() {
				switch change {
				case "delete":
					_, _, _ = s.store.DeleteAccounts([]string{"saved-token"})
				case "rotate":
					_, _, _ = s.store.RotateAccountTokens("saved-token", "operator-token", "", "", map[string]any{"status": "正常"})
				case "verified":
					_, _, _ = s.store.RotateAccountTokens("saved-token", "saved-token", "", "", map[string]any{"status": "正常", "quota": 31})
				case "disabled":
					_, _, _ = s.store.UpdateAccount("saved-token", map[string]any{"enabled": false})
				}
			})
			defer upstream.Close()
			cfg := adminTestConfig(t.TempDir())
			cfg.OpenAIBaseURL = upstream.URL
			s = New(cfg)
			row := seedValidation(t, s)
			s.retryAccountValidation(row, time.Now())
			row, err := s.accountByRef("user-saved")
			if change == "delete" {
				if err == nil {
					t.Fatal("resurrected account")
				}
				return
			}
			if change == "rotate" && accountToken(row) != "operator-token" {
				t.Fatal("overwrote newer token")
			}
			if (change == "rotate" || change == "verified") && row["relogin_blocked"] == true {
				t.Fatal("stale failure reblocked verified account")
			}
			if change == "verified" && intValue(row["quota"]) != 31 {
				t.Fatal("overwrote newer quota")
			}
			if change == "disabled" && row["enabled"] != false {
				t.Fatal("enabled account without authorization")
			}
		})
	}
}

func TestValidationGatesAndQuotaOnlyCannotUnblock(t *testing.T) {
	var status, requests atomic.Int32
	status.Store(200)
	upstream := validationUpstream(t, &status, &requests, nil)
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.OpenAIBaseURL = upstream.URL
	s := New(cfg)
	row := seedValidation(t, s)
	if err := s.refreshAccountQuotas(context.Background(), accounts.Account{Token: accountToken(row), Fields: row}); err != nil {
		t.Fatal(err)
	}
	assertValidationBlocked(t, s)
	before := requests.Load()
	for _, gate := range []string{"disabled", "inflight", "maintenance"} {
		switch gate {
		case "disabled":
			_, _, _ = s.store.UpdateAccount("saved-token", map[string]any{"enabled": false})
		case "inflight":
			s.reloginProgress["busy"] = &accountReloginProgress{ProgressID: "busy", AccountRef: "user-saved"}
		case "maintenance":
			if err := os.WriteFile(filepath.Join(cfg.DataDir, "cfm-maintenance"), []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if s.retryAccountValidation(row, time.Now()) {
			t.Fatalf("ignored %s gate", gate)
		}
		_, _, _ = s.store.UpdateAccount("saved-token", map[string]any{"enabled": true})
		delete(s.reloginProgress, "busy")
		_ = os.Remove(filepath.Join(cfg.DataDir, "cfm-maintenance"))
	}
	if requests.Load() != before {
		t.Fatal("gate sent upstream request")
	}
	// A manual full refresh can complete recovery without waiting for retry time.
	s.refreshOneAccount(context.Background(), "manual", "user-saved")
	row, _ = s.accountByRef("user-saved")
	if row["relogin_blocked"] == true || row["relogin_validation"] != nil {
		t.Fatal("manual full validation did not finish recovery")
	}
}

func TestValidationRequiresIdentityAndQuotaSnapshot(t *testing.T) {
	for _, scenario := range []string{"identity", "missing_quotas"} {
		t.Run(scenario, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/backend-api/me":
					id := "user-saved"
					if scenario == "identity" {
						id = "another-user"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"email": "saved@example.test", "id": id})
				case "/backend-api/conversation/init":
					_, _ = w.Write([]byte(`{}`))
				default:
					_, _ = w.Write([]byte(`{"accounts":{"default":{"account":{"plan_type":"free"}}}}`))
				}
			}))
			defer upstream.Close()
			cfg := adminTestConfig(t.TempDir())
			cfg.OpenAIBaseURL = upstream.URL
			s := New(cfg)
			row := seedValidation(t, s)
			s.retryAccountValidation(row, time.Now())
			assertValidationBlocked(t, s)
			row, _ = s.accountByRef("user-saved")
			kind := stringValue(mapValue(row["last_account_validation_error"])["kind"])
			if scenario == "identity" && kind != "account_identity_mismatch" || scenario == "missing_quotas" && kind != "quota_snapshot_missing" {
				t.Fatalf("wrong diagnostic %s", kind)
			}
		})
	}
}

func TestValidationDeduplicatesConcurrentLoginAndProbes(t *testing.T) {
	var status, requests atomic.Int32
	status.Store(200)
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := validationUpstream(t, &status, &requests, func() { close(entered); <-release })
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.OpenAIBaseURL = upstream.URL
	s := New(cfg)
	row := seedValidation(t, s)
	finished := make(chan bool, 1)
	go func() { finished <- s.retryAccountValidation(row, time.Now()) }()
	<-entered
	duplicate := s.retryAccountValidation(row, time.Now().Add(time.Hour))
	joined, err := s.startAccountRelogin(row, false)
	close(release)
	if !<-finished || duplicate || err != nil || !joined.ValidationOnly {
		t.Fatal("same account launched duplicate validation/login")
	}
	if requests.Load() != 3 {
		t.Fatal("duplicate requests reached upstream")
	}
}

func TestValidationReservationFailureDoesNotSendRequest(t *testing.T) {
	var status, requests atomic.Int32
	status.Store(200)
	upstream := validationUpstream(t, &status, &requests, nil)
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.OpenAIBaseURL = upstream.URL
	s := New(cfg)
	row := seedValidation(t, s)
	// Replace only the temporary test file destination with a directory so atomic
	// rename fails. An unpersisted attempt must never reach the upstream.
	if err := os.Rename(cfg.AccountsPath, cfg.AccountsPath+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.AccountsPath, 0700); err != nil {
		t.Fatal(err)
	}
	if s.retryAccountValidation(row, time.Now()) || requests.Load() != 0 {
		t.Fatal("sent request despite reservation failure")
	}
	row, _ = s.accountByRef("user-saved")
	if intValue(mapValue(row["relogin_validation"])["attempts"]) != 0 {
		t.Fatal("failed reservation mutated runtime state")
	}
}

func TestValidationRetryAfterAndCrashBudget(t *testing.T) {
	now := time.Now()
	fields := validationFailureFields(&provider.AccountEndpointError{Stage: "quota", Status: 429, Retryable: true, RetryAfterSeconds: 900}, map[string]any{"attempts": 0}, now)
	next, _ := time.Parse(time.RFC3339Nano, stringValue(mapValue(fields["relogin_validation"])["next_attempt_at"]))
	if next.Sub(now) != 15*time.Minute {
		t.Fatal("ignored Retry-After")
	}
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	row := seedValidation(t, s)
	state := cloneMap(mapValue(row["relogin_validation"]))
	state["attempts"] = 3
	_, _, _ = s.store.UpdateAccount("saved-token", map[string]any{"relogin_validation": state})
	s = New(cfg)
	row, _ = s.accountByRef("user-saved")
	if s.retryAccountValidation(row, now) {
		t.Fatal("restart ran a fourth retry")
	}
	row, _ = s.accountByRef("user-saved")
	if mapValue(row["relogin_validation"])["pending"] == true {
		t.Fatal("exhausted job remained pending")
	}
}
