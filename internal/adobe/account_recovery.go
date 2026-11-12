package adobe

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// SubmissionResolution records why a local lease ended, not a refund guarantee.
// The automatic polling method can record a confirmed upstream terminal state;
// manual and no-handle releases retain unknown outcomes. It lives in the same
// atomic document as the lease release and permanently closes task recovery.
type SubmissionResolution struct {
	TaskKey            string `json:"task_key"`
	AccountID          string `json:"account_id"`
	CredentialRevision uint64 `json:"credential_revision"`
	ResolvedAt         string `json:"resolved_at"`
	Reason             string `json:"reason"`
	ReviewedBy         string `json:"reviewed_by"`
	TaskRecordState    string `json:"task_record_state"`
	PreviousError      string `json:"previous_error,omitempty"`
	Method             string `json:"method,omitempty"`
	Outcome            string `json:"outcome,omitempty"`
	QuotaStatus        string `json:"quota_status,omitempty"`
}

func (s *Store) SubmissionResolutions(id string) ([]SubmissionResolution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []SubmissionResolution{}
	for _, resolution := range d.Resolutions {
		if resolution.AccountID == id {
			out = append(out, resolution)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResolvedAt > out[j].ResolvedAt })
	return out, nil
}

func (s *Store) SubmissionResolved(taskKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return false, err
	}
	_, found := d.Resolutions[taskKey]
	return found, nil
}

// ResolveSubmission must be called while the HTTP task coordinator excludes
// active execution/recovery. Neither this method nor its caller contacts Adobe.
func (s *Store) ResolveSubmission(id, taskKey string, revision uint64, confirmed bool, reason, actor, taskRecordState string) (Account, SubmissionResolution, error) {
	reason = strings.TrimSpace(reason)
	if !confirmed || utf8.RuneCountInString(reason) < 8 || utf8.RuneCountInString(reason) > 500 || strings.TrimSpace(actor) == "" || (taskRecordState != "terminal" && taskRecordState != "missing") {
		return Account{}, SubmissionResolution{}, ErrResolutionReview
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return Account{}, SubmissionResolution{}, err
	}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		if a.ID != id {
			continue
		}
		if _, resolved := d.Resolutions[taskKey]; resolved {
			return *a, SubmissionResolution{}, ErrTaskResolved
		}
		if taskKey == "" || a.InFlight != taskKey || d.Bindings[taskKey] != id || a.Status != "submission_unknown" || a.Revision != revision {
			return *a, SubmissionResolution{}, ErrResolutionConflict
		}
		if s.maintenance[id] {
			return *a, SubmissionResolution{}, ErrBusy
		}
		now := time.Now()
		resolution := SubmissionResolution{TaskKey: taskKey, AccountID: id, CredentialRevision: revision, ResolvedAt: now.UTC().Format(time.RFC3339Nano), Reason: reason, ReviewedBy: actor, PreviousError: a.LastError, TaskRecordState: taskRecordState}
		if d.Resolutions == nil {
			d.Resolutions = map[string]SubmissionResolution{}
		}
		d.Resolutions[taskKey] = resolution
		a.InFlight = ""
		a.Status = "pending"
		if a.LastRefreshAt > 0 || a.Quota.LastSuccessAt != "" || a.Quota.Status == "known" {
			a.Status = "active"
		}
		if a.LastError == "adobe_submission_unknown" {
			a.LastError = ""
		}
		a.SettlementUntil = now.Add(30 * time.Second).Unix()
		a.QuotaRevision++
		a.Quota.Status = "stale"
		if a.Quota.Generation != nil {
			a.Quota.Generation.Status = "stale"
		}
		a.UpdatedAt = now.Unix()
		if err := s.save(d); err != nil {
			return Account{}, SubmissionResolution{}, err
		}
		return *a, resolution, nil
	}
	return Account{}, SubmissionResolution{}, ErrNotFound
}

// QueryQuota uses existing credentials only. It never opens a browser, refreshes
// tokens, changes login/task status, releases a task or submits media.
// A quota read may coexist with a pinned generation; its result is discarded if
// that reservation changes while the upstream reads are in progress.
func (s *Store) QueryQuota(ctx context.Context, c *Client, id string) (Account, error) {
	s.mu.Lock()
	d, err := s.load()
	if err != nil {
		s.mu.Unlock()
		return Account{}, err
	}
	var before Account
	for _, a := range d.Accounts {
		if a.ID == id {
			before = a
			break
		}
	}
	if before.ID == "" {
		s.mu.Unlock()
		return before, ErrNotFound
	}
	if s.maintenance[id] || s.quotaQueries[id] {
		s.mu.Unlock()
		return before, ErrBusy
	}
	s.quotaQueries[id] = true
	s.mu.Unlock()
	defer s.endAccountQuotaQuery(id)

	quota, balanceErr := c.Balance(ctx, before)
	if balanceErr != nil {
		quota = before.Quota
		quota.Status, quota.Error = "error", SafeError(balanceErr)
		quota.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	} else {
		// Balance and generation permissions are independent observations.
		quota.Generation = before.Quota.Generation
	}
	generation, generationErr := c.GenerationPermissions(ctx, before)
	if generationErr != nil {
		if before.Quota.Generation != nil {
			generation = *before.Quota.Generation
		}
		generation.Status, generation.Error = "error", SafeError(generationErr)
		generation.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	quota.Generation = &generation

	s.mu.Lock()
	defer s.mu.Unlock()
	d, err = s.load()
	if err != nil {
		return before, err
	}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		if a.ID != id {
			continue
		}
		if s.maintenance[id] || a.Revision != before.Revision || a.QuotaRevision != before.QuotaRevision || a.InFlight != before.InFlight || a.Status != before.Status || a.SettlementUntil != before.SettlementUntil {
			return *a, ErrResolutionConflict
		}
		a.Quota = quota
		a.QuotaRevision++
		if err := s.save(d); err != nil {
			return before, err
		}
		s.recordModelPermissionObservationLocked(ctx, before, *a, generation, "")
		if balanceErr != nil {
			return *a, balanceErr
		}
		return *a, generationErr
	}
	return before, ErrNotFound
}

// RecordQuotaQueryFailure checkpoints a routing failure before an upstream read
// could begin. It preserves all previous successful values and changes only the
// observation metadata. A changed account, route or in-progress query rejects
// the checkpoint rather than overwriting newer observations.
func (s *Store) RecordQuotaQueryFailure(expected Account, code string, checkedAt time.Time) (Account, error) {
	if code != "proxy_unavailable" || checkedAt.IsZero() {
		return Account{}, ErrResolutionReview
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return Account{}, err
	}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		if a.ID != expected.ID {
			continue
		}
		if s.maintenance[a.ID] || s.quotaQueries[a.ID] {
			return *a, ErrBusy
		}
		if a.Revision != expected.Revision || a.QuotaRevision != expected.QuotaRevision || a.InFlight != expected.InFlight || a.Status != expected.Status || a.SettlementUntil != expected.SettlementUntil || a.LastUsedAt != expected.LastUsedAt || a.CreatedAt != expected.CreatedAt || a.Proxy != expected.Proxy || a.ProxyPolicy != expected.ProxyPolicy || !reflect.DeepEqual(a.ProxyBinding, expected.ProxyBinding) {
			return *a, ErrResolutionConflict
		}
		stamp := checkedAt.UTC().Format(time.RFC3339Nano)
		const failure = "adobe_routing_proxy_unavailable"
		a.Quota.Status, a.Quota.Error, a.Quota.CheckedAt = "error", failure, stamp
		generation := GenerationEntitlements{}
		if a.Quota.Generation != nil {
			generation = *a.Quota.Generation
		}
		generation.Status, generation.Error, generation.CheckedAt = "error", failure, stamp
		a.Quota.Generation = &generation
		a.QuotaRevision++
		if err := s.save(d); err != nil {
			return Account{}, err
		}
		return *a, nil
	}
	return Account{}, ErrNotFound
}
