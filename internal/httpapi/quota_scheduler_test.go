package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func auditQuotaServer(t *testing.T) (*Server, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/conversation/init" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 5}, map[string]any{"feature_name": "reason", "remaining": 10}}})
	}))
	t.Cleanup(upstream.Close)
	cfg := adminTestConfig(t.TempDir())
	cfg.OpenAIBaseURL = upstream.URL
	return New(cfg), hits
}
func auditQuotaRow(token string) map[string]any {
	return map[string]any{"access_token": token, "user_id": token, "source_type": "chatgpt_web", "type": "free", "status": "正常", "enabled": true, "quota": 10, "quota_last_used_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "quota_observed_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "capability_quotas": map[string]any{"reason": map[string]any{"remaining": 10}, "image_gen": map[string]any{"remaining": 10}}}
}
func TestQuotaSchedulerIdleControl(t *testing.T) {
	s, hits := auditQuotaServer(t)
	if e := s.store.SaveAccounts([]map[string]any{auditQuotaRow("idle")}); e != nil {
		t.Fatal(e)
	}
	s.refreshDueCapabilities()
	if hits.Load() != 1 {
		t.Fatalf("control expected 1 quota query; got %d", hits.Load())
	}
}
func TestQuotaSchedulerBusyHeadDoesNotStarveTail(t *testing.T) {
	s, hits := auditQuotaServer(t)
	rows := []map[string]any{}
	for i := 0; i < 5; i++ {
		rows = append(rows, auditQuotaRow(fmt.Sprint("token-", i)))
	}
	if e := s.store.SaveAccounts(rows); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 4; i++ {
		token := fmt.Sprint("token-", i)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		l, e := s.accountPool.ReserveIntent(ctx, []string{"basic"}, nil, func(a accounts.Account) bool { return a.Token == token }, 0, accounts.Intent{Kind: "chat"})
		cancel()
		if e != nil {
			t.Fatal(e)
		}
		defer s.accountPool.Release(l)
	}
	for i := 0; i < 3; i++ {
		s.refreshDueCapabilities()
	}
	if hits.Load() == 0 {
		t.Fatal("idle fifth account never queried after three scans: four busy accounts consume all refresh slots")
	}
}
func TestQuotaSchedulerExpiredResetWithoutLocalUsage(t *testing.T) {
	s, hits := auditQuotaServer(t)
	row := auditQuotaRow("imported-exhausted")
	delete(row, "quota_last_used_at")
	row["status"] = "限流"
	row["quota"] = 0
	row["quota_refresh_needed"] = false
	row["capability_quotas"] = map[string]any{"image_gen": map[string]any{"remaining": 0, "reset_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}
	if e := s.store.SaveAccounts([]map[string]any{row}); e != nil {
		t.Fatal(e)
	}
	s.refreshDueCapabilities()
	if hits.Load() != 1 {
		t.Fatalf("expired zero quota should be checked without prior local use; got %d queries", hits.Load())
	}
}
func TestQuotaSurvivalReconcilesIdlePending(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			fmt.Fprint(w, `{"email":"audit@example.test","id":"survival-audit"}`)
		case "/backend-api/conversation/init":
			fmt.Fprint(w, `{"limits_progress":[{"feature_name":"image_gen","remaining":5},{"feature_name":"reason","remaining":10}]}`)
		case "/backend-api/accounts/check/v4-2023-04-27":
			fmt.Fprint(w, `{"accounts":{"default":{"account":{"plan_type":"free"}}}}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.OpenAIBaseURL = upstream.URL
	s := New(cfg)
	row := auditQuotaRow("survival-audit")
	row["email"] = "audit@example.test"
	if e := s.store.SaveAccounts([]map[string]any{row}); e != nil {
		t.Fatal(e)
	}
	l, e := s.accountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: "image"})
	if e != nil {
		t.Fatal(e)
	}
	s.accountPool.MarkSent(l, "reason")
	s.accountPool.MarkSent(l, "image_gen")
	s.accountPool.Release(l)
	rows, _, e := s.store.AccountSnapshot()
	if e != nil {
		t.Fatal(e)
	}
	if got := s.survivalProbe(rows[0], false); got != "free" {
		t.Fatalf("survival failed: %s", got)
	}
	rows, _, _ = s.store.AccountSnapshot()
	b, _ := json.Marshal(rows[0]["quota_pending"])
	var pending map[string]int
	json.Unmarshal(b, &pending)
	t.Logf("fresh remaining=%v, pending=%d, refresh_needed=%v", rows[0]["quota"], pending["image_gen"], rows[0]["quota_refresh_needed"])
	if pending["image_gen"] != 0 && rows[0]["quota_refresh_needed"] != true {
		t.Fatal("survival saved fresh balance but retained already-accounted pending usage and cleared refresh flag")
	}
}

func TestQuotaSurvivalDefersActiveRequestsAndFreshBackoff(t *testing.T) {
	s, hits := auditQuotaServer(t)
	row := auditQuotaRow("deferred-survival")
	if err := s.store.SaveAccounts([]map[string]any{row}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.accountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: "image"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.survivalProbe(row, false); got != "deferred" || hits.Load() != 0 {
		t.Fatal("survival queried an active account")
	}
	s.accountPool.Release(lease)
	_, _, err = s.store.UpdateAccount("deferred-survival", map[string]any{"quota_retry_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	// The caller's row predates the failure. Check the current row inside the gate.
	if got := s.survivalProbe(row, false); got != "deferred" || hits.Load() != 0 {
		t.Fatal("stale survival snapshot bypassed retry-after")
	}
}

func TestQuotaScheduleProjectionDoesNotExposeCredentials(t *testing.T) {
	row := auditQuotaRow("PRIVATE-QUOTA-TOKEN")
	row["quota_retry_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	got := accountForAPI(row)
	if got["quota_check_reason"] != "backoff" || got["quota_next_check_at"] == nil {
		t.Fatal("missing check plan")
	}
	if _, ok := got["access_token"]; ok {
		t.Fatal("credential exposed")
	}
	row["enabled"] = false
	got = accountForAPI(row)
	if got["quota_check_reason"] != "disabled" || got["quota_next_check_at"] != nil {
		t.Fatal("disabled account still scheduled")
	}
}

func TestQuotaSurvivalHonorsDisabledCredentialRefresh(t *testing.T) {
	var oauthCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			oauthCalls.Add(1)
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/backend-api/me" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		t.Errorf("unexpected endpoint %s", r.URL.Path)
		w.WriteHeader(404)
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.OpenAIBaseURL = upstream.URL
	cfg.OpenAIOAuthURL = upstream.URL + "/oauth/token"
	s := New(cfg)
	row := auditQuotaRow("no-refresh")
	row["source_type"] = "oauth"
	row["refresh_token"] = "private-refresh-token"
	if e := s.store.SaveAccounts([]map[string]any{row}); e != nil {
		t.Fatal(e)
	}
	if got := s.survivalProbe(row, false); got != "token_dead" {
		t.Fatalf("expected token failure, got %s", got)
	}
	if oauthCalls.Load() != 0 {
		t.Fatal("disabled credential refresh called OAuth")
	}
	rows, _, _ := s.store.AccountSnapshot()
	if rows[0]["refresh_token"] != "private-refresh-token" {
		t.Fatal("setting erased stored refresh token")
	}
}

func TestQuotaSchedulerHonorsUpstreamRetryAfter(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/backend-api/conversation/init" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.OpenAIBaseURL = upstream.URL
	s := New(cfg)
	if e := s.store.SaveAccounts([]map[string]any{auditQuotaRow("retry-after-account")}); e != nil {
		t.Fatal(e)
	}
	s.refreshDueCapabilities()
	s.refreshDueCapabilities()
	rows, _, _ := s.store.AccountSnapshot()
	retry, e := time.Parse(time.RFC3339, stringValue(rows[0]["quota_retry_at"]))
	if e != nil || time.Until(retry) < 9*time.Minute {
		t.Fatalf("Retry-After lost: %v", rows[0]["quota_retry_at"])
	}
	if hits.Load() != 1 || intValue(rows[0]["quota"]) != 10 || rows[0]["quota_last_failure_kind"] != "rate_limit" {
		t.Fatal("rate limit retried or old balance changed")
	}
}
