package adobe

import (
	"os"
	"time"
)

const SubmissionReconciliationGrace = 30 * time.Minute

// SubmissionReconciliation records observation and local occupancy policy only.
// It does not assert that Adobe accepted/cancelled a request or refunded credits.
type SubmissionReconciliation struct {
	TaskKey            string `json:"task_key"`
	AccountID          string `json:"account_id"`
	CredentialRevision uint64 `json:"credential_revision"`
	State              string `json:"state"`
	FirstObservedAt    string `json:"first_observed_at"`
	AttemptStartedAt   string `json:"attempt_started_at,omitempty"`
	LastCheckedAt      string `json:"last_checked_at,omitempty"`
	NextCheckAt        string `json:"next_check_at,omitempty"`
	ReleaseAfter       string `json:"release_after"`
	LastError          string `json:"last_error,omitempty"`
	QuotaStatus        string `json:"quota_status"`
	Attempts           uint64 `json:"attempts"`
}

type SubmissionReconciliationObservation struct {
	Attempt     uint64
	State       string
	CheckedAt   time.Time
	NextCheckAt time.Time
	LastError   string
	QuotaStatus string
}

func newSubmissionReconciliation(a Account, now time.Time) SubmissionReconciliation {
	started := now
	// UpdatedAt and credential refresh timestamps may change during an unknown
	// submission. LastUsedAt belongs to its durable reservation and does not.
	if a.LastUsedAt > 0 && a.LastUsedAt <= now.Unix() {
		started = time.Unix(a.LastUsedAt, 0)
	}
	return SubmissionReconciliation{
		TaskKey: a.InFlight, AccountID: a.ID, CredentialRevision: a.Revision,
		State: "waiting", FirstObservedAt: now.UTC().Format(time.RFC3339Nano),
		ReleaseAfter: started.Add(SubmissionReconciliationGrace).UTC().Format(time.RFC3339Nano),
		QuotaStatus:  "not_checked",
	}
}

func reconciliationForAccount(d diskState, a Account, now time.Time) *SubmissionReconciliation {
	if a.InFlight == "" {
		return nil
	}
	if r, ok := d.Reconciliations[a.InFlight]; ok && r.AccountID == a.ID {
		return &r
	}
	return nil
}

func publicSubmissionReconciliation(a Account, now time.Time) any {
	r := a.TaskReconciliation
	if r == nil && a.InFlight != "" && a.Status == "submission_unknown" {
		initial := newSubmissionReconciliation(a, now)
		r = &initial
	}
	if r == nil || a.InFlight == "" || r.TaskKey != a.InFlight || r.AccountID != a.ID {
		return nil
	}
	return map[string]any{
		"state": r.State, "first_observed_at": r.FirstObservedAt,
		"last_checked_at": r.LastCheckedAt, "next_check_at": r.NextCheckAt,
		"release_after": r.ReleaseAfter, "last_error": safeReconciliationError(r.LastError),
		"quota_status": r.QuotaStatus, "attempts": r.Attempts,
	}
}

// BeginSubmissionReconciliation durably claims a bounded observation slot.
// The HTTP task coordinator must separately exclude an active task runner.
func (s *Store) BeginSubmissionReconciliation(id, taskKey string, revision uint64, now time.Time) (SubmissionReconciliation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return SubmissionReconciliation{}, err
	}
	a, err := s.reconciliationAccountLocked(d, id, taskKey, revision)
	if err != nil {
		return SubmissionReconciliation{}, err
	}
	r, exists := d.Reconciliations[taskKey]
	if exists {
		if r.AccountID != id || r.TaskKey != taskKey {
			return SubmissionReconciliation{}, ErrResolutionConflict
		}
		if next, e := time.Parse(time.RFC3339Nano, r.NextCheckAt); e == nil && next.After(now) {
			return r, ErrBusy
		}
	} else {
		r = newSubmissionReconciliation(a, now)
	}
	r.CredentialRevision = revision
	r.Attempts++
	r.State = "checking"
	r.AttemptStartedAt = now.UTC().Format(time.RFC3339Nano)
	r.NextCheckAt = now.Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
	r.QuotaStatus = "not_checked"
	if d.Reconciliations == nil {
		d.Reconciliations = map[string]SubmissionReconciliation{}
	}
	d.Reconciliations[taskKey] = r
	if err := s.save(d); err != nil {
		return SubmissionReconciliation{}, err
	}
	return r, nil
}

// RecordSubmissionReconciliation accepts only the latest claimed observation.
// The server supplies fixed categories, never response text or authentication data.
func (s *Store) RecordSubmissionReconciliation(id, taskKey string, revision uint64, observation SubmissionReconciliationObservation) error {
	if observation.State != "waiting" && observation.State != "retry_wait" && observation.State != "polling" {
		return ErrResolutionReview
	}
	if observation.QuotaStatus != "known" && observation.QuotaStatus != "error" && observation.QuotaStatus != "not_checked" {
		return ErrResolutionReview
	}
	if observation.CheckedAt.IsZero() || observation.NextCheckAt.Before(observation.CheckedAt) || observation.NextCheckAt.After(observation.CheckedAt.Add(24*time.Hour)) {
		return ErrResolutionReview
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return err
	}
	if _, err := s.reconciliationAccountLocked(d, id, taskKey, revision); err != nil {
		return err
	}
	r, ok := d.Reconciliations[taskKey]
	started, parseErr := time.Parse(time.RFC3339Nano, r.AttemptStartedAt)
	if !ok || r.AccountID != id || r.CredentialRevision != revision || r.Attempts != observation.Attempt || r.State != "checking" || parseErr != nil || observation.CheckedAt.Before(started) {
		return ErrResolutionConflict
	}
	r.State = observation.State
	r.LastCheckedAt = observation.CheckedAt.UTC().Format(time.RFC3339Nano)
	r.NextCheckAt = observation.NextCheckAt.UTC().Format(time.RFC3339Nano)
	r.LastError = safeReconciliationError(observation.LastError)
	r.QuotaStatus = observation.QuotaStatus
	d.Reconciliations[taskKey] = r
	return s.save(d)
}

func safeReconciliationError(value string) string {
	switch value {
	case "", "quota_query_failed", "credentials_unavailable", "proxy_unavailable", "poll_failed", "handle_invalid", "task_record_missing", "upstream_pending", "check_failed", "cancelled":
		return value
	default:
		return "check_failed"
	}
}

func (s *Store) reconciliationAccountLocked(d diskState, id, taskKey string, revision uint64) (Account, error) {
	if _, resolved := d.Resolutions[taskKey]; resolved {
		return Account{}, ErrTaskResolved
	}
	for _, a := range d.Accounts {
		if a.ID != id {
			continue
		}
		if taskKey == "" || a.InFlight != taskKey || d.Bindings[taskKey] != id || a.Revision != revision {
			return a, ErrResolutionConflict
		}
		if s.maintenance[id] {
			return a, ErrBusy
		}
		return a, nil
	}
	return Account{}, ErrNotFound
}

// A resumed poll may finish before its coordinator stores the "polling"
// observation. Both the claimed and polling phases therefore count, but only
// with a persisted terminal upstream handle for this exact binding. The audit
// and local release are saved together by finish; the original job is untouched.
func (s *Store) recordAutomaticPollResolutionLocked(d *diskState, a Account, taskKey string, now time.Time) {
	if _, exists := d.Resolutions[taskKey]; exists {
		return
	}
	r, exists := d.Reconciliations[taskKey]
	if !exists || d.Bindings[taskKey] != a.ID || r.TaskKey != taskKey || r.AccountID != a.ID || r.Attempts == 0 || r.CredentialRevision == 0 || r.CredentialRevision > a.Revision || (r.State != "checking" && r.State != "polling") {
		return
	}
	if _, err := time.Parse(time.RFC3339Nano, r.AttemptStartedAt); err != nil {
		return
	}
	job, err := s.loadJobLocked(taskKey)
	if err != nil || job.PollURL == "" {
		return
	}
	outcome, reason := "", ""
	switch job.Status {
	case "COMPLETED":
		outcome = "completed"
		reason = "系统查询确认上游任务已完成，结束本地占用并保留原任务记录；未重新生成。交付结果以原任务为准，额度等待重新查询。"
	case "FAILED", "ERROR", "CANCELLED", "CANCELED", "REJECTED":
		outcome = "failed"
		reason = "系统查询确认上游任务已失败、拒绝或取消，结束本地占用并保留原任务记录；未重新生成，不代表已退回额度。"
	default:
		return
	}
	if d.Resolutions == nil {
		d.Resolutions = map[string]SubmissionResolution{}
	}
	d.Resolutions[taskKey] = SubmissionResolution{
		TaskKey: taskKey, AccountID: a.ID, CredentialRevision: a.Revision,
		ResolvedAt: now.UTC().Format(time.RFC3339Nano), ReviewedBy: "system",
		TaskRecordState: "terminal", PreviousError: a.LastError,
		Method: "automatic_poll", Outcome: outcome, QuotaStatus: "not_checked", Reason: reason,
	}
}

// AutoResolveSubmission ends only the local hold for an unqueryable submission.
// It requires a completed read-only observation, the unchanged binding/revision,
// and the original 30-minute grace. A nonempty or unreadable saved handle is never
// treated as absent. The coordinator must exclude active execution/recovery.
func (s *Store) AutoResolveSubmission(id, taskKey string, revision uint64, taskRecordState string, now time.Time) (Account, SubmissionResolution, error) {
	if taskRecordState != "terminal" && taskRecordState != "missing" {
		return Account{}, SubmissionResolution{}, ErrResolutionReview
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return Account{}, SubmissionResolution{}, err
	}
	a, err := s.reconciliationAccountLocked(d, id, taskKey, revision)
	if err != nil {
		return a, SubmissionResolution{}, err
	}
	if a.Status != "submission_unknown" || s.quotaQueries[id] {
		return a, SubmissionResolution{}, ErrBusy
	}
	r, ok := d.Reconciliations[taskKey]
	deadline, deadlineErr := time.Parse(time.RFC3339Nano, r.ReleaseAfter)
	checked, checkedErr := time.Parse(time.RFC3339Nano, r.LastCheckedAt)
	started, startedErr := time.Parse(time.RFC3339Nano, r.AttemptStartedAt)
	if !ok || r.AccountID != id || r.CredentialRevision != revision || (r.State != "waiting" && r.State != "retry_wait") || (r.QuotaStatus != "known" && r.QuotaStatus != "error") || deadlineErr != nil || checkedErr != nil || startedErr != nil || deadline.After(now) || checked.Before(started) || checked.After(now) {
		return a, SubmissionResolution{}, ErrResolutionConflict
	}
	job, jobErr := s.loadJobLocked(taskKey)
	if (jobErr != nil && !os.IsNotExist(jobErr)) || (jobErr == nil && job.PollURL != "") {
		return a, SubmissionResolution{}, ErrResolutionConflict
	}
	resolution := SubmissionResolution{
		TaskKey: taskKey, AccountID: id, CredentialRevision: revision,
		ResolvedAt: now.UTC().Format(time.RFC3339Nano), ReviewedBy: "system",
		TaskRecordState: taskRecordState, PreviousError: a.LastError,
		Method: "automatic_no_handle", Outcome: "unknown", QuotaStatus: r.QuotaStatus,
		Reason: "超过30分钟仍无可查询的上游任务地址，系统已尝试只读查询额度并结束本地占用；原生成结果与扣费仍未知，未重发、未取消上游任务、未退款。",
	}
	if d.Resolutions == nil {
		d.Resolutions = map[string]SubmissionResolution{}
	}
	d.Resolutions[taskKey] = resolution
	for i := range d.Accounts {
		if d.Accounts[i].ID != id {
			continue
		}
		current := &d.Accounts[i]
		current.InFlight = ""
		current.Status = "pending"
		if current.LastRefreshAt > 0 || current.Quota.LastSuccessAt != "" || current.Quota.Status == "known" {
			current.Status = "active"
		}
		if current.LastError == "adobe_submission_unknown" {
			current.LastError = ""
		}
		current.SettlementUntil = now.Add(30 * time.Second).Unix()
		current.QuotaRevision++
		// Preserve failed observations so a background scheduler can distinguish
		// them from successful snapshots needing only settlement revalidation.
		if current.Quota.Status != "error" {
			current.Quota.Status = "stale"
		}
		if current.Quota.Generation != nil && current.Quota.Generation.Status != "error" {
			current.Quota.Generation.Status = "stale"
		}
		current.UpdatedAt = now.Unix()
		a = *current
		break
	}
	if err := s.save(d); err != nil {
		return Account{}, SubmissionResolution{}, err
	}
	return a, resolution, nil
}
