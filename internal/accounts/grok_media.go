package accounts

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/auucoder/gptgrok2api-go/internal/store"
	"strings"
	"time"
)

const GrokMediaMaxAge = 5 * time.Minute
const GrokMediaSettleDelay = 30 * time.Second

type grokMediaSnapshot struct {
	Status    string `json:"status"`
	Error     string `json:"error"`
	CheckedAt string `json:"checked_at"`
	RetryAt   string `json:"retry_at"`
	Quotas    map[string]struct {
		Available *bool  `json:"available"`
		Remaining *int   `json:"remaining"`
		ResetAt   string `json:"next_available_at"`
	} `json:"quotas"`
}

func mediaSnapshot(row map[string]any) grokMediaSnapshot {
	var value grokMediaSnapshot
	raw, _ := json.Marshal(row["media_quota"])
	_ = json.Unmarshal(raw, &value)
	return value
}
func isMediaIntent(i Intent) bool { return i.Kind == "image" || i.Kind == "video" }

// Pro image/edit share imagePro; observed 480p/720p video routes share video720p.
// Unknown counts never become zero or an invented unlimited quota.
func GrokMediaQuotas(row map[string]any) map[string]FeatureQuota {
	result := map[string]FeatureQuota{}
	snapshot := mediaSnapshot(row)
	if snapshot.Status != "success" {
		return result
	}
	for group, key := range map[string]string{"image_gen": "imagePro", "video_gen": "video720p"} {
		value, ok := snapshot.Quotas[key]
		if !ok {
			continue
		}
		quota := FeatureQuota{ResetAt: value.ResetAt}
		if value.Available != nil && !*value.Available {
			zero := 0
			quota.Remaining = &zero
		} else if value.Remaining != nil && *value.Remaining >= 0 {
			// Grok uses available + no reset + zero as an unspecified-count sentinel.
			if !(*value.Remaining == 0 && value.Available != nil && *value.Available && value.ResetAt == "") {
				quota.Remaining = value.Remaining
			}
		}
		result[group] = quota
	}
	return result
}
func grokMediaReady(row map[string]any, now time.Time) bool {
	snapshot := mediaSnapshot(row)
	observed, err := time.Parse(time.RFC3339Nano, snapshot.CheckedAt)
	return snapshot.Status == "success" && err == nil && !observed.After(now.Add(time.Minute)) && now.Sub(observed) <= GrokMediaMaxAge && row["media_quota_refresh_needed"] != true
}

// GrokMediaCheckPlan separates low-frequency idle polling from fresh-before-use
// checks. A reset time schedules an observation; it never creates local credits.
func GrokMediaCheckPlan(row map[string]any, now time.Time, onDemand bool) QuotaPlan {
	snapshot := mediaSnapshot(row)
	observed, _ := time.Parse(time.RFC3339Nano, snapshot.CheckedAt)
	used := timestamp(row, "quota_last_used_at")
	plan := QuotaPlan{At: observed, Reason: "unknown"}
	if snapshot.Status == "success" && !observed.IsZero() {
		interval := time.Hour
		if onDemand || (!used.IsZero() && now.Sub(used) < 30*time.Minute) {
			interval = GrokMediaMaxAge
		}
		quotas := GrokMediaQuotas(row)
		lastFeature := map[string]string{"image": "image_gen", "video": "video_gen"}[stringValue(row["quota_last_intent"])]
		if !onDemand && quotas[lastFeature].Remaining != nil && *quotas[lastFeature].Remaining == 0 {
			interval = time.Hour
		}
		plan = QuotaPlan{At: observed.Add(interval), Reason: "periodic"}
		for _, q := range quotas {
			if q.Remaining == nil || *q.Remaining != 0 {
				continue
			}
			if reset, err := time.Parse(time.RFC3339Nano, q.ResetAt); err == nil {
				at := reset.Add(5 * time.Second)
				if !observed.Before(reset) {
					at = observed.Add(GrokMediaMaxAge)
				}
				if at.Before(plan.At) {
					plan = QuotaPlan{At: at, Reason: "reset_due"}
				}
			}
		}
		for _, n := range pendingQuotas(row["quota_pending"]) {
			if n > 0 && observed.Add(GrokMediaMaxAge).Before(plan.At) {
				plan = QuotaPlan{At: observed.Add(GrokMediaMaxAge), Reason: "reconcile"}
			}
		}
	}
	if row["media_quota_refresh_needed"] == true || row["quota_refresh_needed"] == true {
		plan = QuotaPlan{At: used, Reason: "reconcile"}
	}
	if snapshot.Status == "failed" {
		delay := time.Minute * time.Duration(1<<min(max(intValue(row["media_quota_failure_count"])-1, 0), 5))
		if delay > 30*time.Minute {
			delay = 30 * time.Minute
		}
		switch strings.TrimSpace(snapshot.Error) {
		case "HTTP 401", "HTTP 403", "HTTP 429":
			if delay < 15*time.Minute {
				delay = 15 * time.Minute
			}
		}
		attempted := timestamp(row, "media_quota_last_attempt_at")
		if observed.After(attempted) {
			attempted = observed
		}
		plan = QuotaPlan{At: attempted.Add(delay), Reason: "backoff"}
		if retry, err := time.Parse(time.RFC3339Nano, snapshot.RetryAt); err == nil && retry.After(plan.At) {
			plan.At = retry
		}
	}
	if last := timestamp(row, "media_quota_last_attempt_at"); !last.IsZero() && plan.At.Before(last.Add(30*time.Second)) {
		plan.At = last.Add(30 * time.Second)
	}
	if until := timestamp(row, "media_quota_not_before"); until.After(plan.At) {
		plan = QuotaPlan{At: until, Reason: "settlement"}
	}
	return plan
}

func GrokMediaRefreshDue(row map[string]any, now time.Time) bool {
	return !GrokMediaCheckPlan(row, now, true).At.After(now)
}

func grokMediaHistory(row map[string]any, quotas map[string]FeatureQuota, now time.Time) []QuotaEvent {
	history := []QuotaEvent{}
	raw, _ := json.Marshal(row["media_quota_history"])
	_ = json.Unmarshal(raw, &history)
	previous := GrokMediaQuotas(row)
	if mediaSnapshot(row).Status != "success" {
		previous = GrokMediaQuotas(map[string]any{"media_quota": row["media_quota_last_success"]})
	}
	changes := map[string]QuotaChange{}
	for k, q := range quotas {
		before := previous[k]
		if before.Remaining != nil && q.Remaining != nil && *before.Remaining != *q.Remaining {
			changes[k] = QuotaChange{Before: before.Remaining, After: q.Remaining}
		}
	}
	if len(changes) > 0 {
		history = append(history, QuotaEvent{At: now.UTC().Format(time.RFC3339), Source: "grok_media", Changes: changes})
	}
	if len(history) > 20 {
		history = history[len(history)-20:]
	}
	return history
}

// RefreshGrokMedia gates only this account; live generations and settlement must
// finish before a fresh observation can clear their durable reservations.
func (p *Pool) RefreshGrokMedia(a Account, fetch func(Account) (map[string]any, error)) error {
	p.mu.Lock()
	id := Identity(a)
	current, err := p.currentAccountLocked(a)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	if p.refreshing[id] || p.capabilityActive[id] > 0 {
		p.mu.Unlock()
		return ErrRefreshDeferred
	}
	if next, e := time.Parse(time.RFC3339Nano, stringValue(current.Fields["media_quota_not_before"])); e == nil && time.Now().Before(next) {
		p.mu.Unlock()
		return ErrRefreshDeferred
	}
	p.refreshing[id] = true
	p.mu.Unlock()
	fields, fetchErr := fetch(current)
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.signalLocked()
	defer delete(p.refreshing, id)
	latest, err := p.currentAccountLocked(current)
	if err != nil {
		return err
	}
	if latest.Token != current.Token {
		return ErrRefreshDeferred
	}
	updates := map[string]any{"media_quota_last_attempt_at": time.Now().UTC().Format(time.RFC3339Nano)}
	// Failed snapshots deliberately replace stale positive media balances.
	for k, v := range fields {
		updates[k] = v
	}
	if fetchErr == nil {
		quotas := GrokMediaQuotas(fields)
		pending := pendingQuotas(latest.Fields["quota_pending"])
		blocks := map[string]any{}
		raw, _ := json.Marshal(latest.Fields["capability_cooldowns"])
		_ = json.Unmarshal(raw, &blocks)
		if blocks == nil {
			blocks = map[string]any{}
		}
		for feature, quota := range quotas {
			if quota.Remaining != nil {
				delete(pending, feature)
				if *quota.Remaining > 0 {
					delete(blocks, feature)
				}
			}
		}
		updates["quota_pending"] = pending
		updates["capability_cooldowns"] = blocks
		updates["media_quota_last_success"] = fields["media_quota"]
		updates["media_quota_history"] = grokMediaHistory(latest.Fields, quotas, time.Now())
		updates["media_quota_failure_count"] = 0
		updates["media_quota_refresh_needed"] = false
		updates["quota_refresh_needed"] = false
		updates["media_quota_not_before"] = nil
	} else {
		updates["media_quota_failure_count"] = min(intValue(latest.Fields["media_quota_failure_count"])+1, 16)
		if mediaSnapshot(latest.Fields).Status == "success" {
			updates["media_quota_last_success"] = latest.Fields["media_quota"]
		}
		updates["media_quota_refresh_needed"] = true
	}
	if _, _, err = p.repository.UpdateAccountIf(current.Token, updates, func(row map[string]any) bool {
		return reflect.DeepEqual(row["media_quota"], current.Fields["media_quota"]) &&
			reflect.DeepEqual(row["enabled"], current.Fields["enabled"]) &&
			stringValue(row["status"]) == stringValue(current.Fields["status"]) &&
			stringValue(row["last_error_at"]) == stringValue(current.Fields["last_error_at"])
	}); err != nil {
		if errors.Is(err, store.ErrAccountChanged) {
			return ErrRefreshDeferred
		}
		return err
	}
	return fetchErr
}

// GrokMediaAvailability uses the same predicate as leasing; this is a snapshot,
// not a reservation. Pending usage is deducted before advertising availability.
func (p *Pool) GrokMediaAvailability(row map[string]any, now time.Time) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := map[string]any{}
	a, found := normalize(row)
	for kind, feature := range map[string]string{"image": "image_gen", "video": "video_gen"} {
		model := "grok-imagine-image-pro"
		if kind == "video" {
			model = "grok-imagine-video"
		}
		intent := Intent{Kind: kind, Model: model}
		eligible := found && p.availableForIntent(a, []string{a.Pool}, now, intent)
		reason := "ready"
		q := GrokMediaQuotas(row)[feature]
		switch {
		case !boolValue(row["enabled"], true) || stringValue(row["status"]) == "disabled":
			reason = "disabled"
		case !found || IsAbnormal(row) || stringValue(row["status"]) == "invalid" || stringValue(row["status"]) == "expired":
			reason = "auth_invalid"
		case !p.available(a, []string{a.Pool}, now):
			reason = "account_unavailable"
		case mediaSnapshot(row).Status != "success":
			reason = "quota_unknown"
		case row["media_quota_refresh_needed"] == true:
			reason = "settlement"
		case !grokMediaReady(row, now):
			reason = "quota_stale"
		case q.Remaining == nil:
			reason = "quota_unknown"
		case *q.Remaining == 0:
			reason = "exhausted"
		case *q.Remaining-pendingQuotas(row["quota_pending"])[feature] < intent.Costs()[feature]:
			reason = "reserved"
		case !eligible:
			reason = "cooling"
		}
		result[kind] = map[string]any{"eligible": eligible, "reason": reason}
	}
	return result
}

// HasGrokMediaCapacity avoids polling other idle accounts while a suitable fresh
// account already exists. Leasing still performs the final atomic reservation.
func (p *Pool) HasGrokMediaCapacity(intent Intent) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	rows, _, err := p.repository.AccountSnapshot()
	if err != nil {
		return false
	}
	for _, row := range rows {
		a, ok := normalize(row)
		if ok && !p.refreshing[Identity(a)] && p.capabilityActive[Identity(a)] == 0 && p.availableForIntent(a, []string{a.Pool}, time.Now(), intent) {
			return true
		}
	}
	return false
}
