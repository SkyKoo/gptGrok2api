package accounts

import (
	"encoding/json"
	"time"
)

const GrokMediaMaxAge = 5 * time.Minute
const GrokMediaSettleDelay = 30 * time.Second

type grokMediaSnapshot struct {
	Status    string `json:"status"`
	CheckedAt string `json:"checked_at"`
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

// The supported routes share the observed Pro image/edit and 720p video balances.
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

// GrokMediaRefreshDue is shared by the scheduler and request preflight. Missing
// observations are probed even for unused accounts; ended requests wait for billing.
func GrokMediaRefreshDue(row map[string]any, now time.Time) bool {
	if next, e := time.Parse(time.RFC3339Nano, stringValue(row["media_quota_not_before"])); e == nil && now.Before(next) {
		return false
	}
	if last, e := time.Parse(time.RFC3339Nano, stringValue(row["media_quota_last_attempt_at"])); e == nil && now.Sub(last) < 30*time.Second {
		return false
	}
	if !grokMediaReady(row, now) || row["quota_refresh_needed"] == true {
		return true
	}
	for _, q := range GrokMediaQuotas(row) {
		if q.Remaining != nil && *q.Remaining == 0 {
			if reset, e := time.Parse(time.RFC3339Nano, q.ResetAt); e == nil && !now.Before(reset) {
				return true
			}
		}
	}
	return false
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
		updates["quota_pending"] = map[string]int{}
		updates["media_quota_refresh_needed"] = false
		updates["quota_refresh_needed"] = false
		updates["media_quota_not_before"] = nil
	} else {
		updates["media_quota_refresh_needed"] = true
	}
	if _, _, err = p.repository.UpdateAccount(current.Token, updates); err != nil {
		return err
	}
	return fetchErr
}
