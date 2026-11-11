package adobe

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPublicSeparatesVerifiedAccountFromQuotaFreshness(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	stale := now.Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)
	amount, zero, cost := float64(3), float64(0), float64(1)
	for _, tc := range []struct {
		name                                  string
		change                                func(*Account)
		wantStatus, wantQuota, wantGeneration string
	}{
		{name: "fresh verified account", wantStatus: "active", wantQuota: "known", wantGeneration: "known"},
		{name: "idle verified account", change: func(a *Account) { a.Quota.CheckedAt = stale; a.Quota.Generation.CheckedAt = stale }, wantStatus: "active", wantQuota: "stale", wantGeneration: "stale"},
		{name: "explicit stale after settlement", change: func(a *Account) { a.Quota.Status = "stale"; a.Quota.Generation.Status = "stale" }, wantStatus: "active", wantQuota: "stale", wantGeneration: "stale"},
		{name: "fresh credits stale generations", change: func(a *Account) { a.Quota.Generation.CheckedAt = stale }, wantStatus: "active", wantQuota: "known", wantGeneration: "stale"},
		{name: "fresh confirmed exhausted", change: func(a *Account) { a.Quota.Generation.Pools[0].Available = &zero }, wantStatus: "exhausted", wantQuota: "known", wantGeneration: "known"},
		{name: "old zero is not current exhaustion", change: func(a *Account) {
			a.Quota.Generation.Pools[0].Available = &zero
			a.Quota.CheckedAt = stale
			a.Quota.Generation.CheckedAt = stale
		}, wantStatus: "active", wantQuota: "stale", wantGeneration: "stale"},
		{name: "never checked account", change: func(a *Account) {
			a.Status = "pending"
			a.Quota.Status = "unknown"
			a.Quota.Generation.Status = "unknown"
		}, wantStatus: "pending", wantQuota: "unknown", wantGeneration: "unknown"},
		{name: "pending account cannot become verified from stale data", change: func(a *Account) { a.Status = "pending"; a.Quota.CheckedAt = stale }, wantStatus: "pending", wantQuota: "stale", wantGeneration: "stale"},
		{name: "failed quota query keeps check required", change: func(a *Account) {
			a.Quota.Status = "error"
			a.Quota.Error = "quota: rate_limit_exceeded"
			a.LastError = a.Quota.Error
		}, wantStatus: "pending", wantQuota: "error", wantGeneration: "stale"},
		{name: "invalid credentials take precedence", change: func(a *Account) { a.Status = "invalid"; a.Quota.CheckedAt = stale }, wantStatus: "invalid", wantQuota: "stale", wantGeneration: "stale"},
		{name: "expired credentials take precedence", change: func(a *Account) { a.ExpiresAt = now.Add(-time.Minute).Unix(); a.Quota.CheckedAt = stale }, wantStatus: "expired", wantQuota: "stale", wantGeneration: "stale"},
		{name: "disabled takes precedence", change: func(a *Account) { a.Enabled = false; a.Quota.CheckedAt = stale }, wantStatus: "disabled", wantQuota: "stale", wantGeneration: "stale"},
		{name: "unknown submission stays held", change: func(a *Account) {
			a.Status = "submission_unknown"
			a.InFlight = "existing-job"
			a.Quota.CheckedAt = stale
		}, wantStatus: "submission_unknown", wantQuota: "stale", wantGeneration: "stale"},
		{name: "busy takes precedence", change: func(a *Account) { a.InFlight = "existing-job"; a.Quota.CheckedAt = stale }, wantStatus: "busy", wantQuota: "stale", wantGeneration: "stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Account{ID: "adobe:fixture", Enabled: true, Status: "active", ExpiresAt: now.Add(time.Hour).Unix(), Quota: Quota{Status: "known", Available: &amount, CheckedAt: fresh, LastSuccessAt: fresh, Generation: &GenerationEntitlements{Status: "known", CheckedAt: fresh, Models: []ModelEntitlement{{Model: "gemini-3.1-flash-image", Authorized: true, PoolID: "daily-images", Cost: &cost}}, Pools: []GenerationPool{{ID: "daily-images", Available: &amount, Total: &amount}}}}}
			if tc.change != nil {
				tc.change(&a)
			}
			before, _ := json.Marshal(a)
			public := Public(a)
			q := public["quota"].(Quota)
			if public["status"] != tc.wantStatus || q.Status != tc.wantQuota || q.Generation.Status != tc.wantGeneration {
				t.Fatalf("got status=%v quota=%s generation=%s, want %s/%s/%s", public["status"], q.Status, q.Generation.Status, tc.wantStatus, tc.wantQuota, tc.wantGeneration)
			}
			if q.Available == nil || *q.Available != *a.Quota.Available || q.CheckedAt != a.Quota.CheckedAt || q.LastSuccessAt != a.Quota.LastSuccessAt || q.Generation.CheckedAt != a.Quota.Generation.CheckedAt || q.Generation.Pools[0].Available == nil || *q.Generation.Pools[0].Available != *a.Quota.Generation.Pools[0].Available || public["last_error"] != a.LastError {
				t.Fatal("public projection lost observed counters, timestamps, or error")
			}
			after, _ := json.Marshal(a)
			if string(before) != string(after) {
				t.Fatal("public projection modified stored account")
			}
		})
	}
}
