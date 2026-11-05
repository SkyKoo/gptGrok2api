package accounts

import (
	"encoding/json"
	"errors"
	"time"
)

// QuotaPlan is a query schedule, never a promise that upstream quota has reset.
type QuotaPlan struct {
	At     time.Time
	Reason string
}

// QuotaCheckPlan considers every quota group returned by conversation/init.
// An image reset must not suppress a pending chat/upload reconciliation.
func QuotaCheckPlan(row map[string]any, now time.Time) QuotaPlan {
	observed := timestamp(row, "quota_observed_at")
	used := timestamp(row, "quota_last_used_at")
	plan := QuotaPlan{At: observed, Reason: "unknown"}
	pending := false
	for _, n := range pendingQuotas(row["quota_pending"]) {
		pending = pending || n > 0
	}
	if !observed.IsZero() {
		interval := time.Hour
		if !used.IsZero() && now.Sub(used) < 30*time.Minute {
			interval = 5 * time.Minute
		}
		image := DecodeQuotas(row["capability_quotas"])["image_gen"]
		// The last image request exhausted its lane. Until another feature is used,
		// there is no reason to poll this idle account every five minutes.
		if image.Remaining != nil && *image.Remaining == 0 && stringValue(row["quota_last_intent"]) == "image" {
			interval = time.Hour
		}
		plan = QuotaPlan{At: observed.Add(interval), Reason: "periodic"}
	}
	if pending || row["quota_refresh_needed"] == true {
		plan = QuotaPlan{At: used, Reason: "reconcile"}
		if pending && row["quota_refresh_needed"] != true && !observed.IsZero() {
			// An upstream snapshot with an unknown feature could not settle this use.
			plan.At = observed.Add(5 * time.Minute)
		}
	}
	for _, q := range DecodeQuotas(row["capability_quotas"]) {
		if q.Remaining == nil || *q.Remaining != 0 {
			continue
		}
		reset, err := time.Parse(time.RFC3339, q.ResetAt)
		if err != nil {
			continue
		}
		at := reset.Add(5 * time.Second)
		// Upstream may keep returning an expired reset while still exhausted.
		if !observed.Before(reset) {
			at = observed.Add(5 * time.Minute)
		}
		if plan.At.IsZero() || !at.Before(plan.At) {
			continue
		}
		plan = QuotaPlan{At: at, Reason: "reset_due"}
	}
	if retry := timestamp(row, "quota_retry_at"); !retry.IsZero() {
		plan = QuotaPlan{At: retry, Reason: "backoff"}
	}
	floor := timestamp(row, "quota_last_attempt_at").Add(30 * time.Second)
	if !timestamp(row, "quota_last_attempt_at").IsZero() && plan.At.Before(floor) {
		plan.At = floor
	}
	if deferred := timestamp(row, "quota_deferred_until"); !deferred.IsZero() && plan.At.Before(deferred) {
		plan = QuotaPlan{At: deferred, Reason: "busy"}
	}
	return plan
}

// QuotaRefreshError retains the original error for auth recovery while exposing
// only bounded diagnostic categories to persistence/UI.
type QuotaRefreshError struct {
	Err        error
	Kind       string
	RetryAfter time.Duration
}

func (e *QuotaRefreshError) Error() string { return e.Err.Error() }
func (e *QuotaRefreshError) Unwrap() error { return e.Err }

func quotaFailureUpdates(row map[string]any, err error, now time.Time) map[string]any {
	failures := min(max(intValue(row["quota_failure_count"]), 0)+1, 16)
	delay := 30 * time.Second * time.Duration(1<<min(failures-1, 6))
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	kind := "upstream_error"
	var detail *QuotaRefreshError
	if errors.As(err, &detail) {
		switch detail.Kind {
		case "auth", "rate_limit", "network", "upstream", "invalid_response":
			kind = detail.Kind
		}
		if detail.RetryAfter > delay {
			delay = detail.RetryAfter
		}
		if kind == "auth" && delay < 30*time.Minute {
			delay = 30 * time.Minute
		}
	}
	return map[string]any{"quota_failure_count": failures, "quota_retry_at": now.Add(delay).UTC().Format(time.RFC3339), "quota_last_failure_kind": kind, "quota_refresh_error": "upstream quota refresh failed; previous snapshot retained"}
}

type QuotaChange struct {
	Before *int `json:"before"`
	After  *int `json:"after"`
}
type QuotaEvent struct {
	At      string                 `json:"at"`
	Source  string                 `json:"source"`
	Changes map[string]QuotaChange `json:"changes"`
}

func quotaHistory(row map[string]any, next map[string]FeatureQuota, source string, now time.Time) []QuotaEvent {
	history := []QuotaEvent{}
	raw, _ := json.Marshal(row["quota_history"])
	_ = json.Unmarshal(raw, &history)
	changes := map[string]QuotaChange{}
	previous := accountQuotas(Account{Fields: row})
	for k, q := range next {
		before := previous[k]
		// Initial observations and missing values are not evidence of quota recovery.
		if before.Remaining != nil && q.Remaining != nil && *before.Remaining != *q.Remaining {
			changes[k] = QuotaChange{Before: before.Remaining, After: q.Remaining}
		}
	}
	if len(changes) > 0 {
		history = append(history, QuotaEvent{At: now.UTC().Format(time.RFC3339), Source: source, Changes: changes})
	}
	if len(history) > 20 {
		history = history[len(history)-20:]
	}
	return history
}
