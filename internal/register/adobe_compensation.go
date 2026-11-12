package register

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AdobeRegistrationState is encrypted with the private task journal. A child
// keeps the original browser and actual egress, independently of UI settings.
// FlowStarted is a monotonic fence: later browser errors cannot clear it.
type AdobeRegistrationState struct {
	BrowserProfileID  string `json:"browser_profile_id"`
	Proxy             string `json:"proxy,omitempty"`
	ProxyRecorded     bool   `json:"proxy_recorded,omitempty"`
	FlowStarted       bool   `json:"flow_started,omitempty"`
	SubmissionStarted bool   `json:"submission_started,omitempty"`
}

func adobeStageMayHaveSubmitted(stage string) bool {
	switch stage {
	case "adobe_flow_started", "adobe_recovery_started", "adobe_creation_submitted", "adobe_original_submission_recorded", "awaiting_verification", "adobe_registration_needs_review":
		return true
	}
	return strings.HasPrefix(stage, "adobe_verification_") || stage == "adobe_captcha_required"
}

func adobeLegacyMayHaveSubmitted(job RegistrationJob) bool {
	if adobeStageMayHaveSubmitted(job.Stage) {
		return true
	}
	for _, log := range job.Logs {
		if adobeStageMayHaveSubmitted(log.Action) {
			return true
		}
	}
	for _, marker := range []string{"submission_result_unknown", "worker_result_unknown", "human_verification", "captcha_required", "verification_dns_failed", "verification_request_timeout", "verification_request_aborted"} {
		if strings.Contains(job.Error, marker) {
			return true
		}
	}
	if job.AdobeRegistration != nil {
		return false // New journals explicitly fenced every possible run command.
	}
	// Older journals only prove non-submission at these pre-browser stages.
	switch job.Stage {
	case "queued", "mailbox", "waiting_browser", "starting_browser":
		return false
	default:
		return true
	}
}

func adobeBrowserProfileID(job RegistrationJob) string {
	if job.AdobeRegistration != nil && job.AdobeRegistration.BrowserProfileID != "" {
		return job.AdobeRegistration.BrowserProfileID
	}
	return mailboxClaimJobID(job)
}

func ensureAdobeRegistrationState(job *RegistrationJob) {
	if job.AdobeRegistration == nil {
		job.AdobeRegistration = &AdobeRegistrationState{BrowserProfileID: adobeBrowserProfileID(*job), FlowStarted: adobeLegacyMayHaveSubmitted(*job)}
	}
	if job.AdobeRegistration.BrowserProfileID == "" {
		job.AdobeRegistration.BrowserProfileID = mailboxClaimJobID(*job)
	}
	if adobeLegacyMayHaveSubmitted(*job) {
		job.AdobeRegistration.FlowStarted = true
	}
}

func adobeRegistrationRecoveryMode(job RegistrationJob) string {
	if len(job.Account) > 0 || job.Imported || job.Verified {
		return "saved_result"
	}
	if job.AdobeRegistration != nil && (job.AdobeRegistration.FlowStarted || job.AdobeRegistration.SubmissionStarted) || adobeLegacyMayHaveSubmitted(job) {
		return "resume_session"
	}
	return "retry_registration"
}

func adobeCompensationPending(job RegistrationJob) bool {
	if job.RetryJobID != "" || job.Verified || job.Mailbox.Email == "" || job.Status == "completed" || job.Status == "running" || job.Status == "queued" {
		return false
	}
	return !job.Imported || len(job.Account) > 0
}

func adobeCompensationEnvironment(job RegistrationJob) bool {
	if adobeRegistrationRecoveryMode(job) == "saved_result" {
		return len(job.Account) > 0
	}
	if job.AdobeProfile == nil || adobeBrowserProfileID(job) == "" || job.Mailbox.AccountID == "" {
		return false
	}
	return adobeRegistrationRecoveryMode(job) != "resume_session" || job.ProxyBinding != nil || job.AdobeRegistration != nil && job.AdobeRegistration.ProxyRecorded
}

func adobeAutomaticFailure(job RegistrationJob) bool {
	if len(job.Account) > 0 && job.Error != "task_cancelled" && job.Error != "import: account_writer_not_configured" && job.Error != "verify: account_verifier_not_configured" {
		return true // The identity is already captured; import/verify is idempotent.
	}
	if transientRegistrationFailure(job.Error) {
		return true
	}
	switch job.Error {
	case "process_restarted_no_automatic_registration_retry", "process_restarted_compensation_delayed":
		return true
	case "adobe: submission_result_unknown", "adobe: worker_result_unknown", "adobe: human_verification_required", "adobe: captcha_required", "adobe: verification_dns_failed", "adobe: verification_request_timeout", "adobe: verification_request_aborted", "adobe: registration_needs_review":
		return true // Recovery is session/login only; this never authorizes Create.
	case "browser: adobe_browser_unavailable", "adobe: browser_unavailable", "adobe: resume_login_pending", "adobe: resume_login_completion_unconfirmed", "adobe: resume_otp_required", "adobe: resume_verification_required", "adobe: resume_challenge_required":
		return true
	case "adobe: recovery_login_entry_unrecognized", "adobe: recovery_login_form_unavailable", "adobe: recovery_login_form_unrecognized", "adobe: recovery_login_result_unknown", "adobe: recovery_verification_unavailable", "adobe: recovery_verification_result_unknown", "adobe: recovery_progress_stalled", "adobe: recovery_step_failed", "adobe: verification_result_unknown", "adobe: verification_not_ready", "adobe: continuation_result_unknown", "adobe: stage_transition_timeout", "adobe: firefly_session_not_ready", "adobe: media_session_incomplete":
		return true
	case "import: account_write_failed", "verify: account_unverified":
		return len(job.Account) > 0
	}
	return false
}

func planAdobeRegistrationCompensation(job *RegistrationJob, cause error, now time.Time) {
	if !adobeCompensationPending(*job) {
		finishRegistrationCompensation(job, "completed")
		return
	}
	if job.AutoRetry == nil {
		job.AutoRetry = &RegistrationCompensation{}
	}
	state := job.AutoRetry
	state.State, state.Reason, state.NextAttemptAt = "manual", "failure_requires_review", ""
	if errors.Is(cause, context.Canceled) || job.Error == "task_cancelled" {
		state.Reason = "cancelled"
		return
	}
	if state.Attempts < 0 {
		state.Reason = "invalid_retry_state"
		return
	}
	if state.Attempts >= len(compensationDelays) {
		state.State, state.Reason = "exhausted", "retry_limit_reached"
		return
	}
	if !adobeCompensationEnvironment(*job) {
		state.Reason = "saved_environment_missing"
		return
	}
	if !adobeAutomaticFailure(*job) {
		return
	}
	ensureAdobeRegistrationState(job)
	due := now.Add(registrationRetryDelay(job.Error, now, compensationDelays[state.Attempts]))
	if job.ProxyBinding != nil && job.ProxyBinding.BlockedUntil.After(due) {
		due = job.ProxyBinding.BlockedUntil
	}
	if previous, err := time.Parse(time.RFC3339Nano, state.NotBeforeAt); err == nil && previous.After(due) {
		due = previous
	}
	state.State, state.Reason = "waiting", ""
	state.NextAttemptAt = due.UTC().Format(time.RFC3339Nano)
	// This lower bound survives Stop, manual retry and child-task creation.
	state.NotBeforeAt = state.NextAttemptAt
}

func recoverAdobeRegistrationCompensation(job *RegistrationJob, now time.Time) {
	if job.AutoRetry == nil {
		planAdobeRegistrationCompensation(job, nil, now)
		return
	}
	if job.AutoRetry.State != "running" && job.AutoRetry.State != "manual_running" {
		return
	}
	reserved := job.AutoRetry.NextAttemptAt
	planAdobeRegistrationCompensation(job, nil, now)
	if job.AutoRetry.State == "waiting" {
		if at, err := time.Parse(time.RFC3339Nano, reserved); err == nil {
			if due, err := time.Parse(time.RFC3339Nano, job.AutoRetry.NextAttemptAt); err == nil && at.After(due) {
				job.AutoRetry.NextAttemptAt, job.AutoRetry.NotBeforeAt = reserved, reserved
			}
		}
	}
}

func adobeNeedsCompensationPlan(job RegistrationJob) bool {
	return adobeCompensationPending(job) && job.AutoRetry == nil && adobeAutomaticFailure(job)
}

func adobeCompensationSummary(job RegistrationJob, attempts int) *RegistrationCompensation {
	if job.AutoRetry == nil {
		return nil
	}
	state := *job.AutoRetry
	if job.RetryJobID == "" {
		state.Attempts = attempts
	}
	return &state
}

// Root counters are cumulative: sum the maximum within each root, never every
// ancestor's counter. This also protects historical duplicate mailbox chains.
func (e *AdobeEngine) adobeMailboxAttemptsLocked(email string) int {
	byRoot := map[string]int{}
	for _, job := range e.jobs {
		if grokMailboxKey(job.Mailbox.Email) != grokMailboxKey(email) || job.AutoRetry == nil {
			continue
		}
		root := mailboxClaimJobID(job)
		byRoot[root] = max(byRoot[root], job.AutoRetry.Attempts)
	}
	total := 0
	for _, attempts := range byRoot {
		total += min(attempts, len(compensationDelays))
	}
	return total
}

func (e *AdobeEngine) normalizeAdobeMailboxRetriesLocked(now time.Time) bool {
	groups := map[string][]int{}
	for i, job := range e.jobs {
		if job.Mailbox.Email != "" {
			groups[grokMailboxKey(job.Mailbox.Email)] = append(groups[grokMailboxKey(job.Mailbox.Email)], i)
		}
	}
	changed := false
	for email, indices := range groups {
		attempts := e.adobeMailboxAttemptsLocked(email)
		owner, saved, active := -1, -1, false
		complete, stopped := false, false
		var latest time.Time
		for _, i := range indices {
			job := e.jobs[i]
			if job.ProxyBinding != nil && job.ProxyBinding.BlockedUntil.After(latest) {
				latest = job.ProxyBinding.BlockedUntil
			}
			if job.AutoRetry != nil {
				if at, err := time.Parse(time.RFC3339Nano, job.AutoRetry.NotBeforeAt); err == nil && at.After(latest) {
					latest = at
				}
			}
			if job.RetryJobID != "" {
				continue
			}
			complete = complete || job.Verified
			active = active || job.Status == "running" || job.Status == "queued"
			if len(job.Account) > 0 {
				saved = i
			}
			if job.AutoRetry == nil {
				continue
			}
			state := job.AutoRetry
			stopped = stopped || state.State == "manual" && state.Reason == "cancelled"
			if state.State != "waiting" || !adobeCompensationPending(job) {
				continue
			}
			// A submitted/unknown attempt owns this email before any earlier
			// pre-submit failure can schedule another Create.
			if owner < 0 || adobeRegistrationRecoveryMode(e.jobs[owner]) == "retry_registration" && adobeRegistrationRecoveryMode(job) != "retry_registration" {
				owner = i
			}
			if at, err := time.Parse(time.RFC3339Nano, state.NextAttemptAt); err == nil && at.After(latest) {
				latest = at
			}
			if attempts > state.Attempts && attempts < len(compensationDelays) {
				if failed, err := time.Parse(time.RFC3339Nano, job.UpdatedAt); err == nil && failed.Add(compensationDelays[attempts]).After(latest) {
					latest = failed.Add(compensationDelays[attempts])
				}
			}
		}
		if saved >= 0 {
			owner = saved
		}
		for _, i := range indices {
			job := e.jobs[i]
			if !adobeCompensationPending(job) || job.AutoRetry == nil || job.AutoRetry.State != "waiting" {
				continue
			}
			state := *job.AutoRetry
			_, dueErr := time.Parse(time.RFC3339Nano, state.NextAttemptAt)
			switch {
			case state.Attempts < 0 || dueErr != nil:
				state.State, state.Reason, state.NextAttemptAt = "manual", "invalid_retry_state", ""
			case complete:
				state.State, state.Reason, state.NextAttemptAt = "manual", "saved_account_exists", ""
			case stopped:
				state.State, state.Reason, state.NextAttemptAt = "manual", "cancelled", ""
			case active || i != owner:
				state.State, state.Reason, state.NextAttemptAt = "manual", "duplicate_mailbox_retry", ""
			case attempts >= len(compensationDelays):
				state.State, state.Reason, state.NextAttemptAt = "exhausted", "retry_limit_reached", ""
			case !latest.IsZero():
				state.NextAttemptAt, state.NotBeforeAt = latest.UTC().Format(time.RFC3339Nano), latest.UTC().Format(time.RFC3339Nano)
			}
			if state != *job.AutoRetry {
				job = cloneJob(job)
				job.AutoRetry = &state
				e.jobs[i], changed = job, true
			}
		}
	}
	return changed
}

func (e *AdobeEngine) adobeCompensationIdleLocked() error {
	if e.loadError != nil {
		return e.loadError
	}
	if e.closing {
		return fail("task", "shutting_down")
	}
	if e.cancel != nil || e.active > 0 {
		return fail("task", "already_running")
	}
	return nil
}

func adobeSavedSourceMatches(job RegistrationJob, cfg AdobeConfig) bool {
	return job.Mailbox.AccountID == cfg.HME.AccountID &&
		(job.MailboxAPIBase == "" || job.MailboxAPIBase == cfg.HME.BaseURL) &&
		(job.Mailbox.SourceID == "" || job.Mailbox.SourceID == cfg.MailboxSourceID)
}

// RunCompensation is called only by the maintenance-aware scheduler. Legacy
// records are adopted here, never during a paused candidate's construction.
func (e *AdobeEngine) RunCompensation(raw map[string]any, now time.Time) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.adobeCompensationIdleLocked(); err != nil {
		return false, err
	}
	e.compensationError = ""
	if registrationTarget(raw["target"]) != "adobe" {
		return false, nil
	}
	pending := false
	for _, job := range e.jobs {
		pending = pending || adobeNeedsCompensationPlan(job) || adobeCompensationPending(job) && job.AutoRetry != nil && job.AutoRetry.State == "waiting"
	}
	if !pending {
		return false, nil
	}
	cfg, configErr := ParseAdobeConfig(raw)
	if configErr != nil {
		// Captured identities no longer depend on current HME/browser settings.
		// Their original mailbox metadata remains part of the saved result.
		cfg = AdobeConfig{Timeout: 2 * time.Minute}
	}
	before := append([]RegistrationJob(nil), e.jobs...)
	changed := false
	for i, job := range e.jobs {
		if adobeNeedsCompensationPlan(job) {
			job = cloneJob(job)
			planAdobeRegistrationCompensation(&job, nil, now)
			e.jobs[i], changed = job, true
		}
	}
	if e.normalizeAdobeMailboxRetriesLocked(now) {
		changed = true
	}
	if changed {
		if err := e.saveLocked(); err != nil {
			e.jobs, e.loadError = before, fail("storage", "journal_write_failed")
			return false, e.loadError
		}
	}
	selected := -1
	var earliest time.Time
	for i, job := range e.jobs {
		if !adobeCompensationPending(job) || job.AutoRetry == nil || job.AutoRetry.State != "waiting" {
			continue
		}
		if adobeRegistrationRecoveryMode(job) != "saved_result" {
			if configErr != nil {
				e.compensationError = safeFailure(configErr)
				continue
			}
			if !adobeSavedSourceMatches(job, cfg) {
				e.compensationError = "config: saved_mailbox_source_unavailable"
				continue
			}
		}
		if !adobeCompensationEnvironment(job) {
			e.compensationError = "task: saved_environment_missing"
			continue
		}
		due, err := time.Parse(time.RFC3339Nano, job.AutoRetry.NextAttemptAt)
		if err != nil || now.Before(due) {
			continue
		}
		if selected < 0 || due.Before(earliest) {
			selected, earliest = i, due
		}
	}
	if selected < 0 {
		if e.compensationError != "" && configErr != nil {
			return false, configErr
		}
		return false, nil
	}
	if adobeRegistrationRecoveryMode(e.jobs[selected]) == "saved_result" {
		cfg = AdobeConfig{Timeout: 2 * time.Minute}
	}
	if err := e.startAdobeCompensationLocked(selected, cfg, true, now); err != nil {
		e.compensationError = safeFailure(err)
		return false, err
	}
	return true, nil
}

func (e *AdobeEngine) RetryRegistration(raw map[string]any, id string) error {
	return e.retryAdobeWithConfig(raw, id, false)
}

func (e *AdobeEngine) RetrySavedResultWithConfig(raw map[string]any, id string) error {
	return e.retryAdobeWithConfig(raw, id, true)
}

func (e *AdobeEngine) retryAdobeWithConfig(raw map[string]any, id string, savedOnly bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.adobeCompensationIdleLocked(); err != nil {
		return err
	}
	for i, job := range e.jobs {
		if job.ID == id {
			if savedOnly && len(job.Account) == 0 {
				return fail("task", "no_saved_result")
			}
			cfg := AdobeConfig{Timeout: 2 * time.Minute}
			if len(job.Account) == 0 {
				var err error
				cfg, err = ParseAdobeConfig(raw)
				if err != nil {
					return err
				}
			}
			return e.startAdobeCompensationLocked(i, cfg, false, time.Now())
		}
	}
	return fail("task", "not_found")
}

func (e *AdobeEngine) startAdobeCompensationLocked(index int, cfg AdobeConfig, automatic bool, now time.Time) error {
	parent := cloneJob(e.jobs[index])
	if !adobeCompensationPending(parent) {
		return fail("task", "no_saved_mailbox")
	}
	if adobeRegistrationRecoveryMode(parent) != "saved_result" && !adobeSavedSourceMatches(parent, cfg) {
		return fail("config", "saved_mailbox_source_unavailable")
	}
	if !adobeCompensationEnvironment(parent) {
		return fail("task", "saved_environment_missing")
	}
	if parent.AutoRetry != nil && parent.AutoRetry.Reason == "duplicate_mailbox_retry" {
		return fail("task", "mailbox_retry_already_scheduled")
	}
	for i, other := range e.jobs {
		if grokMailboxKey(other.Mailbox.Email) != grokMailboxKey(parent.Mailbox.Email) {
			continue
		}
		if other.ProxyBinding != nil && now.Before(other.ProxyBinding.BlockedUntil) {
			return fail("proxy", "registration_cooldown")
		}
		if other.AutoRetry != nil {
			if due, err := time.Parse(time.RFC3339Nano, other.AutoRetry.NotBeforeAt); err == nil && now.Before(due) {
				return fail("task", "mailbox_registration_cooldown")
			}
		}
		if i == index || other.RetryJobID != "" {
			continue
		}
		if other.Verified || other.Imported || len(other.Account) > 0 {
			return fail("task", "mailbox_already_has_saved_account")
		}
		if adobeRegistrationRecoveryMode(parent) == "retry_registration" && adobeRegistrationRecoveryMode(other) == "resume_session" {
			return fail("task", "mailbox_registration_requires_resume")
		}
	}
	attempts := e.adobeMailboxAttemptsLocked(parent.Mailbox.Email)
	if automatic && attempts >= len(compensationDelays) {
		return fail("task", "retry_limit_reached")
	}
	if parent.AutoRetry != nil && parent.AutoRetry.Attempts < 0 {
		return fail("task", "invalid_retry_state")
	}
	ensureAdobeRegistrationState(&parent)
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fail("task", "random_failed")
	}
	job := cloneJob(parent)
	job.ID, job.ParentJobID, job.RootJobID, job.RetryJobID = hex.EncodeToString(raw), parent.ID, mailboxClaimJobID(parent), ""
	job.Status, job.Stage, job.Error = "running", "compensation", ""
	job.CreatedAt, job.UpdatedAt = now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano)
	job.Logs = []JobLog{}
	ensureAdobeRegistrationState(&job)
	job.AdobeRegistration.BrowserProfileID = adobeBrowserProfileID(parent)
	if adobeRegistrationRecoveryMode(parent) != "saved_result" {
		job.MailboxAPIBase = cfg.HME.BaseURL
	}
	if job.AutoRetry == nil {
		job.AutoRetry = &RegistrationCompensation{}
	}
	action, detail := "manual_compensation", "复用原邮箱与原环境，开始恢复 Adobe 账号"
	job.AutoRetry.State, job.AutoRetry.Reason = "manual_running", ""
	job.AutoRetry.NextAttemptAt = ""
	if automatic {
		job.AutoRetry.Attempts++
		job.AutoRetry.State, job.AutoRetry.LastAttemptAt = "running", job.CreatedAt
		delay := compensationDelays[min(attempts+1, len(compensationDelays)-1)]
		job.AutoRetry.NextAttemptAt = now.Add(cfg.Timeout + delay).UTC().Format(time.RFC3339Nano)
		action, detail = "automatic_compensation", fmt.Sprintf("复用原邮箱与原环境，开始第 %d/%d 次 Adobe 自动恢复", attempts+1, len(compensationDelays))
	}
	appendRegistrationLogAt(&job, job.CreatedAt, action, detail+"；恢复方式 "+adobeRegistrationRecoveryMode(job)+"；原任务 "+parent.ID, "info")
	before := append([]RegistrationJob(nil), e.jobs...)
	e.jobs[index] = registrationParentWithChild(parent, job)
	e.jobs = append(e.jobs, job)
	e.normalizeAdobeMailboxRetriesLocked(now)
	if err := e.saveLocked(); err != nil {
		e.jobs, e.loadError = before, fail("storage", "journal_write_failed")
		return e.loadError
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.stopRequested = false
	e.cancel, e.done = cancel, make(chan struct{})
	go func() {
		defer e.finish()
		e.runTask(ctx, cfg, job.ID)
	}()
	return nil
}

func prepareSavedAdobeMailbox(ctx context.Context, rootID string, box Mailbox, mail MailSource) error {
	if withJob, ok := mail.(interface {
		PrepareFor(context.Context, string, Mailbox) error
	}); ok {
		return withJob.PrepareFor(ctx, rootID, box)
	}
	if preparer, ok := mail.(MailSessionPreparer); ok {
		return preparer.Prepare(ctx, box)
	}
	return fail("mail", "resume_not_supported")
}
