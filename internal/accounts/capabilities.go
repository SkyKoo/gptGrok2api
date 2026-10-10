package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Intent describes the quota groups needed by one upstream attempt, not API billing.
// These are conservative reservations; only upstream observations are authoritative.
type Intent struct {
	Kind    string
	Model   string
	Uploads int
}

func (i Intent) Costs() map[string]int {
	c := map[string]int{"reason": 1}
	if i.Kind == "image" {
		c["image_gen"] = 1
	}
	if i.Uploads > 0 {
		c["file_upload"] = i.Uploads
	}
	return c
}

type FeatureQuota struct {
	Remaining *int   `json:"remaining"`
	ResetAt   string `json:"reset_at,omitempty"`
}

// Identity is persisted before the first reservation/refresh and survives credential rotation.
func Identity(a Account) string {
	if id := stringValue(a.Fields["cfm_account_id"]); id != "" {
		return id
	}
	source := firstString(a.Fields, "user_id", "email")
	if source == "" {
		source = a.Token
	}
	h := sha256.Sum256([]byte(source))
	return "acct-" + hex.EncodeToString(h[:16])
}
func DecodeQuotas(v any) map[string]FeatureQuota {
	var result map[string]FeatureQuota
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &result)
	if result == nil {
		result = map[string]FeatureQuota{}
	}
	return result
}
func pendingQuotas(v any) map[string]int {
	var result map[string]int
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &result)
	if result == nil {
		result = map[string]int{}
	}
	return result
}

// Quotas normalizes known features, retaining unknown quantities as nil, never zero.
func Quotas(raw any, observed time.Time) map[string]FeatureQuota {
	out := map[string]FeatureQuota{}
	for _, k := range []string{"reason", "image_gen", "file_upload", "paste_text_to_file", "deep_research"} {
		out[k] = FeatureQuota{}
	}
	var rows []map[string]any
	b, _ := json.Marshal(raw)
	_ = json.Unmarshal(b, &rows)
	for _, row := range rows {
		name := stringValue(row["feature_name"])
		if name == "" {
			continue
		}
		q := FeatureQuota{}
		if x, ok := row["remaining"].(float64); ok && x >= 0 {
			n := int(x)
			q.Remaining = &n
		}
		switch x := row["reset_after"].(type) {
		case float64:
			if x > 0 && !observed.IsZero() {
				q.ResetAt = observed.Add(time.Duration(x * float64(time.Second))).UTC().Format(time.RFC3339)
			}
		case string:
			if t, e := time.Parse(time.RFC3339, x); e == nil {
				q.ResetAt = t.UTC().Format(time.RFC3339)
			}
		}
		out[name] = q
	}
	return out
}
func accountQuotas(a Account) map[string]FeatureQuota {
	if a.Fields["capability_quotas"] != nil {
		return DecodeQuotas(a.Fields["capability_quotas"])
	}
	return Quotas(a.Fields["limits_progress"], time.Time{})
}
func confirmedImageOnlyLimit(a Account) bool {
	status := strings.ToLower(stringValue(a.Fields["status"]))
	if status != "限流" && status != "limited" && status != "rate_limited" {
		return false
	}
	if stringValue(a.Fields["last_error_kind"]) == "auth_invalid" {
		return false
	}
	reason := stringValue(a.Fields["status_reason_code"])
	if reason != "" && reason != "image_quota_exhausted" {
		return false
	}
	q := accountQuotas(a)["image_gen"]
	return q.Remaining != nil && *q.Remaining == 0
}
func (p *Pool) availableForIntent(a Account, pools []string, now time.Time, i Intent) bool {
	if i.Kind == "" {
		return p.available(a, pools, now)
	}
	check := a
	if confirmedImageOnlyLimit(a) {
		fields := map[string]any{}
		for k, v := range a.Fields {
			fields[k] = v
		}
		fields["status"] = "正常"
		check.Fields = fields
	}
	if !p.available(check, pools, now) {
		return false
	}
	q := accountQuotas(a)
	pending := pendingQuotas(a.Fields["quota_pending"])
	blocks, _ := a.Fields["capability_cooldowns"].(map[string]any)
	for k, cost := range i.Costs() {
		if t, e := time.Parse(time.RFC3339, stringValue(blocks[k])); e == nil && t.After(now) {
			return false
		}
		if v := q[k]; v.Remaining != nil && *v.Remaining-pending[k] < cost {
			return false
		}
	}
	if t, e := time.Parse(time.RFC3339, stringValue(blocks["model:"+i.Model])); e == nil && t.After(now) {
		return false
	}
	return true
}
func (p *Pool) currentAccountLocked(a Account) (Account, error) {
	rows, _, err := p.repository.AccountSnapshot()
	if err != nil {
		return Account{}, err
	}
	for _, row := range rows {
		if c, ok := normalize(row); ok && Identity(c) == Identity(a) {
			return c, nil
		}
	}
	return Account{}, fmt.Errorf("account removed during request")
}
func (p *Pool) reserveQuotaLocked(l *Lease) error {
	a, err := p.currentAccountLocked(l.Account)
	if err != nil {
		return err
	}
	pending := pendingQuotas(a.Fields["quota_pending"])
	for k, n := range l.intent.Costs() {
		pending[k] += n
	}
	fields, _, err := p.repository.UpdateAccount(a.Token, map[string]any{"cfm_account_id": Identity(a), "quota_pending": pending, "quota_last_used_at": time.Now().UTC().Format(time.RFC3339)})
	if err == nil {
		l.Account.Fields = fields
	}
	return err
}

// MarkSent is called immediately before a quota-consuming request. A transport
// timeout after this boundary is ambiguous and must not refund or regenerate.
func (p *Pool) MarkSent(l *Lease, feature string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l == nil || l.finished {
		return
	}
	l.used[feature] = true
}
func (p *Pool) Sent(l *Lease, feature string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return l != nil && l.used[feature]
}
func (p *Pool) finishQuotaLocked(l *Lease) {
	l.finished = true
	a, err := p.currentAccountLocked(l.Account)
	if err != nil {
		return
	}
	pending := pendingQuotas(a.Fields["quota_pending"])
	for k, n := range l.intent.Costs() {
		if !l.used[k] {
			pending[k] = max(0, pending[k]-n)
		}
	}
	// Pending estimates remain durable until a fresh snapshot with no in-flight
	// requests reconciles them. Restart never invents refunded quota.
	_, _ = p.repository.UpdateAccountRuntime(a.Token, map[string]any{"quota_pending": pending, "quota_refresh_needed": true})
}

// RefreshCapabilities gates only this account while fetching. No pool lock is
// held across network I/O; new leases wait, and credential rotation is detected.
func (p *Pool) RefreshCapabilities(a Account, fetch func(Account) (map[string]any, error)) error {
	p.mu.Lock()
	id := Identity(a)
	if p.refreshing[id] || p.capabilityActive[id] > 0 {
		p.mu.Unlock()
		return nil
	}
	p.refreshing[id] = true
	current, err := p.currentAccountLocked(a)
	p.mu.Unlock()
	if err == nil {
		a = current
	}
	var fields map[string]any
	if err == nil {
		fields, err = fetch(a)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.signalLocked()
	defer delete(p.refreshing, id)
	latest, e := p.currentAccountLocked(a)
	if e != nil {
		return e
	}
	if latest.Token != a.Token {
		return nil
	}
	updates := map[string]any{"cfm_account_id": id, "quota_last_attempt_at": time.Now().UTC().Format(time.RFC3339)}
	if err != nil {
		updates["quota_refresh_error"] = "upstream quota refresh failed; previous snapshot retained"
	} else {
		for k, v := range fields {
			updates[k] = v
		}
		// Preserve the legacy image-only status for safe rollback to main.
		if q := DecodeQuotas(fields["capability_quotas"])["image_gen"]; q.Remaining != nil {
			if *q.Remaining == 0 && stringValue(latest.Fields["status"]) == "正常" {
				updates["status"] = "限流"
				updates["status_reason_code"] = "image_quota_exhausted"
			}
			if *q.Remaining > 0 && confirmedImageOnlyLimit(latest) {
				updates["status"] = "正常"
				updates["status_reason_code"] = nil
			}
		}
		updates["quota_pending"] = map[string]int{}
		updates["quota_refresh_needed"] = false
		updates["quota_refresh_error"] = nil
		// Only clear a block when the corresponding upstream balance is known positive.
		blocks := map[string]any{}
		old, _ := latest.Fields["capability_cooldowns"].(map[string]any)
		for k, v := range old {
			blocks[k] = v
		}
		for k, q := range DecodeQuotas(fields["capability_quotas"]) {
			if q.Remaining != nil && *q.Remaining > 0 {
				delete(blocks, k)
			}
		}
		updates["capability_cooldowns"] = blocks
	}
	_, _, saveErr := p.repository.UpdateAccount(latest.Token, updates)
	if saveErr != nil {
		return saveErr
	}
	return err
}

// FeedbackIntent scopes rate limiting to its actual quota group (when known),
// otherwise to the requested model. Authentication failures remain account-wide.
func (p *Pool) FeedbackIntent(l *Lease, status int, err error) {
	if status != 429 {
		p.Feedback(l.Account, status, err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a, e := p.currentAccountLocked(l.Account)
	if e != nil {
		return
	}
	feature := "model:" + l.intent.Model
	text := ""
	if err != nil {
		text = strings.ToLower(err.Error())
	}
	for _, f := range []string{"image_gen", "file_upload", "reason", "deep_research"} {
		if strings.Contains(text, f) {
			feature = f
			break
		}
	}
	if strings.HasPrefix(feature, "model:") && strings.Contains(text, "image") && (strings.Contains(text, "limit") || strings.Contains(text, "quota")) {
		feature = "image_gen"
	}
	duration := retryAfterDuration(err)
	if duration <= 0 {
		duration = time.Minute
	}
	blocks := map[string]any{}
	old, _ := a.Fields["capability_cooldowns"].(map[string]any)
	for k, v := range old {
		blocks[k] = v
	}
	blocks[feature] = time.Now().Add(duration).UTC().Format(time.RFC3339)
	_, _ = p.repository.UpdateAccountRuntime(a.Token, map[string]any{"capability_cooldowns": blocks, "quota_refresh_needed": true})
}

func (p *Pool) CapabilityRefreshCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.refreshing)
}
