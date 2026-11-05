package accounts

import (
	"errors"
	"testing"
	"time"
)

func TestQuotaScheduleUsesResetBackoffAndOtherFeatures(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	stamp := func(t time.Time) string { return t.Format(time.RFC3339) }
	row := map[string]any{"quota_observed_at": stamp(now.Add(-6 * time.Minute)), "quota_last_used_at": stamp(now.Add(-10 * time.Minute)), "quota_last_intent": "image", "capability_quotas": map[string]any{"image_gen": map[string]any{"remaining": 0, "reset_at": stamp(now.Add(10 * time.Hour))}, "reason": map[string]any{"remaining": 10}}}
	if plan := QuotaCheckPlan(row, now); !plan.At.Equal(now.Add(54 * time.Minute)) {
		t.Fatalf("exhausted idle account polled early: %+v", plan)
	}
	row["capability_quotas"].(map[string]any)["reason"] = map[string]any{"remaining": 0, "reset_at": stamp(now.Add(time.Minute))}
	if plan := QuotaCheckPlan(row, now); plan.Reason != "reset_due" || !plan.At.Equal(now.Add(65*time.Second)) {
		t.Fatalf("image reset suppressed another feature: %+v", plan)
	}
	row["quota_pending"] = map[string]int{"reason": 1}
	row["quota_refresh_needed"] = true
	if plan := QuotaCheckPlan(row, now); plan.Reason != "reconcile" || plan.At.After(now) {
		t.Fatalf("pending usage delayed: %+v", plan)
	}
	row["quota_retry_at"] = stamp(now.Add(time.Hour))
	if plan := QuotaCheckPlan(row, now); plan.Reason != "backoff" || !plan.At.Equal(now.Add(time.Hour)) {
		t.Fatalf("retry-after ignored: %+v", plan)
	}
	// Failed checks retry even if the last successful snapshot is still fresh.
	row["quota_observed_at"] = stamp(now)
	row["quota_retry_at"] = stamp(now.Add(time.Minute))
	row["quota_pending"] = map[string]int{}
	row["quota_refresh_needed"] = false
	row["capability_quotas"] = map[string]any{"image_gen": map[string]any{"remaining": 5}}
	if plan := QuotaCheckPlan(row, now); !plan.At.Equal(now.Add(time.Minute)) {
		t.Fatalf("failed fresh snapshot postponed retry: %+v", plan)
	}
	row["quota_observed_at"] = stamp(now.Add(-6 * time.Minute))
	row["quota_pending"] = map[string]int{"reason": 1}
	delete(row, "quota_retry_at")
	row["quota_refresh_needed"] = false
	row["capability_quotas"] = map[string]any{}
	if plan := QuotaCheckPlan(row, now); !plan.At.Equal(now.Add(-time.Minute)) {
		t.Fatalf("unknown pending should wait for another bounded check: %+v", plan)
	}
}

func TestQuotaFailureBackoffPersistsAndSuccessClears(t *testing.T) {
	p, s := quotaPool(t, 10, 5, "正常")
	rows, _, _ := s.AccountSnapshot()
	a, _ := normalize(rows[0])
	err := p.RefreshCapabilities(a, func(Account) (map[string]any, error) {
		return nil, &QuotaRefreshError{Err: errors.New("secret must not persist"), Kind: "rate_limit", RetryAfter: time.Hour}
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	rows, _, _ = s.AccountSnapshot()
	row := rows[0]
	retry := timestamp(row, "quota_retry_at")
	if time.Until(retry) < 59*time.Minute || intValue(row["quota_failure_count"]) != 1 || intValue(row["quota"]) != 5 {
		t.Fatal("failure lost prior quota or retry-after")
	}
	if stringValue(row["quota_refresh_error"]) == "secret must not persist" {
		t.Fatal("raw error persisted")
	}
	now := time.Now()
	if plan := QuotaCheckPlan(row, now); plan.Reason != "backoff" || plan.At.Before(retry) {
		t.Fatal("restart plan bypasses retry")
	}
	for i := 1; i <= 8; i++ {
		u := quotaFailureUpdates(map[string]any{"quota_failure_count": i - 1}, errors.New("offline"), now)
		d := timestamp(u, "quota_retry_at").Sub(now)
		if d < 29*time.Second || d > 30*time.Minute {
			t.Fatalf("invalid backoff %v", d)
		}
	}
	if err := p.RefreshCapabilities(a, func(Account) (map[string]any, error) {
		return map[string]any{"quota": 4, "capability_quotas": map[string]any{"image_gen": map[string]any{"remaining": 4}, "reason": map[string]any{"remaining": 10}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, _, _ = s.AccountSnapshot()
	if rows[0]["quota_retry_at"] != nil || intValue(rows[0]["quota_failure_count"]) != 0 {
		t.Fatal("successful observation retained backoff")
	}
}

func TestQuotaSnapshotCannotOverwriteNewerObservation(t *testing.T) {
	p, s := quotaPool(t, 10, 5, "正常")
	rows, _, _ := s.AccountSnapshot()
	a, _ := normalize(rows[0])
	err := p.RefreshCapabilities(a, func(Account) (map[string]any, error) {
		_, _, e := s.UpdateAccount(a.Token, map[string]any{"quota": 3, "capability_quotas": map[string]any{"image_gen": map[string]any{"remaining": 3}}})
		if e != nil {
			t.Fatal(e)
		}
		return map[string]any{"quota": 4, "capability_quotas": map[string]any{"image_gen": map[string]any{"remaining": 4}}}, nil
	})
	if !errors.Is(err, ErrRefreshDeferred) {
		t.Fatalf("stale result committed: %v", err)
	}
	rows, _, _ = s.AccountSnapshot()
	if intValue(rows[0]["quota"]) != 3 {
		t.Fatal("new snapshot overwritten")
	}
}

func TestQuotaHistoryBoundedAndUnknownDoesNotSettlePending(t *testing.T) {
	p, s := quotaPool(t, 10, 5, "正常")
	rows, _, _ := s.AccountSnapshot()
	a, _ := normalize(rows[0])
	_, _, _ = s.UpdateAccount(a.Token, map[string]any{"quota_pending": map[string]int{"file_upload": 1}})
	for i := 0; i < 25; i++ {
		err := p.RefreshCapabilities(a, func(Account) (map[string]any, error) {
			return map[string]any{"capability_quotas": map[string]any{"image_gen": map[string]any{"remaining": i % 2}, "file_upload": map[string]any{"remaining": nil}}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, _, _ = s.AccountSnapshot()
	row := rows[0]
	if pendingQuotas(row["quota_pending"])["file_upload"] != 1 {
		t.Fatal("unknown snapshot refunded usage")
	}
	h := quotaHistory(row, DecodeQuotas(row["capability_quotas"]), "quota", time.Now())
	if len(h) != 20 {
		t.Fatalf("history length %d", len(h))
	}
	if h[19].Changes["image_gen"].Before == nil || h[19].Source != "quota" {
		t.Fatal("history missing provenance")
	}
}
