package adobe

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type mediaPreparationGuard struct {
	ctx          context.Context
	taskKey      string
	proxyBinding *proxyruntime.Binding
	warm         bool
}

func newMediaPreparationGuard(ctx context.Context, taskKey string, a Account) *mediaPreparationGuard {
	guard := &mediaPreparationGuard{ctx: ctx, taskKey: taskKey}
	if a.ProxyBinding != nil {
		binding := *a.ProxyBinding
		guard.proxyBinding = &binding
	}
	return guard
}

// PrepareMediaForTask reuses a trusted recent media session or collects one for
// this already reserved task. Either path durably records the task preparation;
// it never submits a job, renews after submission, or replays an earlier task.
func (s *Store) PrepareMediaForTask(ctx context.Context, taskKey, accountID string, revision uint64) (Account, error) {
	if ctx.Err() != nil {
		return Account{}, &Error{Stage: "media_prepare", Code: "cancelled"}
	}
	s.mu.Lock()
	d, err := s.load()
	if err != nil {
		s.mu.Unlock()
		return Account{}, err
	}
	var a Account
	for _, candidate := range d.Accounts {
		if candidate.ID == accountID {
			a = candidate
			break
		}
	}
	if a.ID == "" {
		s.mu.Unlock()
		return a, ErrNotFound
	}
	if a.Revision != revision {
		s.mu.Unlock()
		return a, ErrRevision
	}
	guard := newMediaPreparationGuard(ctx, taskKey, a)
	if err := s.checkMediaPreparationLocked(d, a, a, guard); err != nil {
		s.mu.Unlock()
		return a, err
	}
	if s.maintenance[accountID] || s.quotaQueries[accountID] {
		s.mu.Unlock()
		return a, ErrBusy
	}
	if s.mediaReadyLocked(ctx, a, 0, time.Now()) {
		if d.PreparedMediaTasks == nil {
			d.PreparedMediaTasks = map[string]uint64{}
		}
		d.PreparedMediaTasks[taskKey] = a.Revision
		err = s.save(d)
		s.mu.Unlock()
		return a, err
	}
	if s.mediaBackoffLocked(d, ctx, a, time.Now()) {
		s.mu.Unlock()
		return a, &Error{Stage: "media_prepare", Code: "cooldown"}
	}
	if strings.TrimSpace(a.ProfileID) == "" {
		s.mu.Unlock()
		return a, &Error{Stage: "media_prepare", Code: "profile_required"}
	}
	refresher := s.browserRefresher
	if refresher == nil {
		s.mu.Unlock()
		return a, &Error{Stage: "media_prepare", Code: "browser_unavailable"}
	}
	flight := s.startMediaFlightLocked(ctx, a)
	s.mu.Unlock()
	return s.collectMediaSession(ctx, a, guard, flight, refresher)
}

// Call under mu both before collection and at the credential commit. Keeping
// the task, identity, quota and route snapshot stable prevents a delayed browser
// result from overriding another login, an operator change, or a submitted job.
func (s *Store) checkMediaPreparationLocked(d diskState, current, expected Account, guard *mediaPreparationGuard) error {
	if guard.ctx.Err() != nil {
		return &Error{Stage: "media_prepare", Code: "cancelled"}
	}
	if !current.Enabled {
		return &Error{Stage: "media_prepare", Code: "account_disabled"}
	}
	if _, selected := mediaContextFingerprint(guard.ctx, current); !selected {
		return &Error{Stage: "media_prepare", Code: "egress_required"}
	}
	if guard.warm {
		if current.InFlight != "" {
			return ErrBusy
		}
	} else if guard.taskKey == "" || d.Bindings[guard.taskKey] != current.ID || current.InFlight != guard.taskKey {
		return ErrResolutionConflict
	}
	if current.Status == "submission_unknown" || current.Status == "invalid" || current.SettlementUntil > time.Now().Unix() {
		return ErrBusy
	}
	if current.AccountID != expected.AccountID || !strings.EqualFold(strings.TrimSpace(current.Email), strings.TrimSpace(expected.Email)) {
		return ErrIdentity
	}
	if current.Revision != expected.Revision || current.QuotaRevision != expected.QuotaRevision || current.Status != expected.Status || current.ProfileID != expected.ProfileID || current.CreatedAt != expected.CreatedAt || current.LastUsedAt != expected.LastUsedAt || current.SettlementUntil != expected.SettlementUntil || current.Proxy != expected.Proxy || current.ProxyPolicy != expected.ProxyPolicy || !reflect.DeepEqual(current.ProxyBinding, guard.proxyBinding) {
		return &Error{Stage: "media_prepare", Code: "context_changed"}
	}
	if !guard.warm {
		return s.checkUnsubmittedMediaTaskLocked(d, guard.taskKey)
	}
	return nil
}

func (s *Store) checkUnsubmittedMediaTaskLocked(d diskState, taskKey string) error {
	if _, ok := d.Resolutions[taskKey]; ok {
		return ErrTaskResolved
	}
	if _, ok := d.Reconciliations[taskKey]; ok {
		return &Error{Stage: "media_prepare", Code: "submission_started"}
	}
	if _, ok := d.PreparedMediaTasks[taskKey]; ok {
		return &Error{Stage: "media_prepare", Code: "already_prepared"}
	}
	if _, err := s.loadJobLocked(taskKey); err == nil {
		return &Error{Stage: "media_prepare", Code: "submission_started"}
	} else if !errors.Is(err, os.ErrNotExist) {
		return &Error{Stage: "media_prepare", Code: "task_state_invalid"}
	}
	return nil
}

// ReleaseUnsubmittedTask is only for failed preparation. It retains the durable
// binding and refuses any successful preparation, job file, reconciliation or
// unknown submission. Since no upstream generation began, quota and settlement
// state remain unchanged. Callers must not fall back to Release on rejection.
func (s *Store) ReleaseUnsubmittedTask(taskKey, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return err
	}
	if taskKey == "" || d.Bindings[taskKey] != accountID {
		return ErrResolutionConflict
	}
	if s.maintenance[accountID] || s.quotaQueries[accountID] {
		return ErrBusy
	}
	if err := s.checkUnsubmittedMediaTaskLocked(d, taskKey); err != nil {
		return err
	}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		if a.ID != accountID {
			continue
		}
		if a.InFlight != taskKey {
			return ErrResolutionConflict
		}
		if a.Status == "submission_unknown" {
			return ErrBusy
		}
		a.InFlight = ""
		a.UpdatedAt = time.Now().Unix()
		return s.save(d)
	}
	return ErrNotFound
}

// ResolveUnsubmittedMediaTask closes a crashed preparation without replay. The
// HTTP coordinator must hold both task locks, exclude active/recovering runners,
// and verify a single durable route is still selected/media_session_preparing.
// Unlike failure cleanup, this may retain an existing preparation marker: the
// durable route proves no submit-start checkpoint was ever committed.
func (s *Store) ResolveUnsubmittedMediaTask(taskKey, accountID string, revision uint64) (Account, SubmissionResolution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return Account{}, SubmissionResolution{}, err
	}
	if _, ok := d.Resolutions[taskKey]; ok {
		return Account{}, SubmissionResolution{}, ErrTaskResolved
	}
	if taskKey == "" || d.Bindings[taskKey] != accountID {
		return Account{}, SubmissionResolution{}, ErrResolutionConflict
	}
	if s.maintenance[accountID] || s.quotaQueries[accountID] {
		return Account{}, SubmissionResolution{}, ErrBusy
	}
	if _, ok := d.Reconciliations[taskKey]; ok {
		return Account{}, SubmissionResolution{}, ErrResolutionConflict
	}
	if _, err := s.loadJobLocked(taskKey); !errors.Is(err, os.ErrNotExist) {
		return Account{}, SubmissionResolution{}, ErrResolutionConflict
	}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		if a.ID != accountID {
			continue
		}
		if a.Revision != revision || a.InFlight != taskKey {
			return *a, SubmissionResolution{}, ErrResolutionConflict
		}
		if a.Status == "submission_unknown" || a.Status == "invalid" || a.LastError == "adobe_submission_unknown" {
			return *a, SubmissionResolution{}, ErrBusy
		}
		now := time.Now()
		resolution := SubmissionResolution{
			TaskKey: taskKey, AccountID: accountID, CredentialRevision: revision,
			ResolvedAt: now.UTC().Format(time.RFC3339Nano), ReviewedBy: "system",
			TaskRecordState: "terminal", PreviousError: a.LastError,
			Method: "automatic_before_submit", Outcome: "not_submitted", QuotaStatus: "not_checked",
			Reason: "系统核对确认任务中断于提交前的会话准备阶段，已结束本地预留并保留原任务记录；未提交或重发生成请求。",
		}
		if d.Resolutions == nil {
			d.Resolutions = map[string]SubmissionResolution{}
		}
		d.Resolutions[taskKey] = resolution
		a.InFlight = ""
		a.UpdatedAt = now.Unix()
		if err := s.save(d); err != nil {
			return Account{}, SubmissionResolution{}, err
		}
		return *a, resolution, nil
	}
	return Account{}, SubmissionResolution{}, ErrNotFound
}
