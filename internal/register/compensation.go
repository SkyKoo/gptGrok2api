package register

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var compensationDelays = [...]time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// The retry budget follows the task chain. Each child durably claims its attempt
// before network traffic, so a new ID or restart cannot reset the retry budget.
type RegistrationCompensation struct {
	State         string `json:"state"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt string `json:"next_attempt_at,omitempty"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

func compensationSummary(state *RegistrationCompensation) any {
	if state == nil {
		return nil
	}
	return map[string]any{"state": state.State, "attempts": state.Attempts, "max_attempts": len(compensationDelays), "next_attempt_at": state.NextAttemptAt, "last_attempt_at": state.LastAttemptAt, "reason": state.Reason}
}

func savedRegistrationMailbox(j RegistrationJob) bool {
	return j.RetryJobID == "" && j.Status == "registration_pending" && j.Mailbox.Email != "" && !j.Imported && !j.Verified && len(j.Account) == 0
}

var legacyFailureHTTP = regexp.MustCompile(`\(HTTP ([0-9]{3})\)`)
var legacyRetryAfter = regexp.MustCompile(`retry_after=("[^"\r\n]*")`)

// Recognize the fixed diagnostics emitted by the registration journal.
// Unknown errors, challenge/identity failures and cancellations require review.
func registrationReviewReason(message string) string {
	for _, item := range []struct{ marker, reason string }{
		{"email_signup_unavailable", "upstream_signup_refused"},
		{"worker_error=signup_unavailable", "upstream_signup_refused"},
		{"worker_error=email_already_registered", "email_already_registered"},
		{"worker_error=email_invalid", "email_invalid"},
		{"turnstile_incomplete", "human_verification_required"},
		{"worker_error=cloudflare_challenge", "human_verification_required"},
	} {
		if strings.Contains(message, item.marker) {
			return item.reason
		}
	}
	return ""
}

func transientRegistrationFailure(message string) bool {
	if registrationReviewReason(message) != "" {
		return false
	}
	if message == "task_timeout" {
		return true
	}
	code := strings.Fields(message)
	if len(code) < 2 {
		return false
	}
	if code[1] == "verification_required" {
		return false
	}
	// xAI has explicitly rejected the email path. Repeating the same browser
	// action only burns another attempt while the upstream capability is
	// unavailable (or the address is permanently invalid/already registered).
	for _, reason := range []string{"signup_unavailable", "email_already_registered", "email_invalid"} {
		if strings.Contains(message, "worker_error="+reason) {
			return false
		}
	}
	if match := legacyFailureHTTP.FindStringSubmatch(message); len(match) > 1 {
		status, _ := strconv.Atoi(match[1])
		return status == 408 || status == 429 || status >= 500 && status <= 599
	}
	if strings.HasPrefix(code[0], "turnstile:") {
		switch code[1] {
		case "browser_worker_unreachable", "browser_worker_read_failed", "browser_worker_rejected", "browser_worker_invalid_response":
			return true
		}
	}
	return code[1] == "network_error" || code[1] == "read_error"
}

func registrationRetryDelay(message string, now time.Time, base time.Duration) time.Duration {
	match := legacyRetryAfter.FindStringSubmatch(message)
	if len(match) < 2 {
		return base
	}
	raw, err := strconv.Unquote(match[1])
	if err != nil {
		return base
	}
	if seconds, err := strconv.ParseUint(raw, 10, 31); err == nil {
		if delay := time.Duration(seconds) * time.Second; delay > base {
			return delay
		}
	} else if at, err := http.ParseTime(raw); err == nil && at.Sub(now) > base {
		return at.Sub(now)
	}
	return base
}

func planRegistrationCompensation(j *RegistrationJob, err error, now time.Time) {
	if !savedRegistrationMailbox(*j) {
		finishRegistrationCompensation(j, "saved_result")
		return
	}
	if j.AutoRetry == nil {
		j.AutoRetry = &RegistrationCompensation{}
	}
	state := j.AutoRetry
	state.NextAttemptAt = ""
	state.State, state.Reason = "manual", "failure_requires_review"
	if reason := registrationReviewReason(j.Error); reason != "" {
		state.Reason = reason
		return
	}
	if errors.Is(err, context.Canceled) || j.Error == "task_cancelled" {
		state.Reason = "cancelled"
		return
	}
	if j.BrowserProfile == nil || !j.BrowserProfile.valid() || j.Mailbox.AccountID == "" {
		state.Reason = "saved_environment_missing"
		return
	}
	if !transientRegistrationFailure(j.Error) {
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
	state.State, state.Reason = "waiting", ""
	state.NextAttemptAt = now.Add(registrationRetryDelay(j.Error, now, compensationDelays[state.Attempts])).UTC().Format(time.RFC3339Nano)
}

func finishRegistrationCompensation(j *RegistrationJob, state string) {
	if j.AutoRetry != nil {
		j.AutoRetry.State = state
		j.AutoRetry.NextAttemptAt = ""
		j.AutoRetry.Reason = ""
	}
}

func recoverRegistrationCompensation(j *RegistrationJob, now time.Time) {
	if j.AutoRetry == nil || j.AutoRetry.State != "running" {
		return
	}
	if !savedRegistrationMailbox(*j) {
		finishRegistrationCompensation(j, "saved_result")
		return
	}
	if j.AutoRetry.Attempts < 0 {
		finishRegistrationCompensation(j, "manual")
		j.AutoRetry.Reason = "invalid_retry_state"
		return
	}
	if j.AutoRetry.Attempts >= len(compensationDelays) {
		finishRegistrationCompensation(j, "exhausted")
		return
	}
	j.AutoRetry.State = "waiting"
	// Keep the reserved request deadline + backoff after a crash.
	if _, err := time.Parse(time.RFC3339Nano, j.AutoRetry.NextAttemptAt); err != nil {
		j.AutoRetry.NextAttemptAt = now.Add(compensationDelays[j.AutoRetry.Attempts]).UTC().Format(time.RFC3339Nano)
	}
}

func (e *FreeEngine) compensationIdleLocked() error {
	if e.loadError != nil {
		return e.loadError
	}
	if e.closing {
		return fail("task", "shutting_down")
	}
	if e.runCancel != nil || len(e.active) > 0 {
		return fail("task", "already_running")
	}
	return nil
}

// RunCompensation is called by the maintenance-aware registration scheduler.
// It never starts a new batch and only launches one existing mailbox at a time.
func (e *FreeEngine) RunCompensation(raw map[string]any, now time.Time) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.compensationIdleLocked(); err != nil {
		return false, err
	}
	e.compensationError = ""
	if stringValue(raw["target"]) != "openai" {
		return false, nil
	}
	hasPending := false
	for _, j := range e.jobs {
		if savedRegistrationMailbox(j) && (j.AutoRetry == nil && transientRegistrationFailure(j.Error) || j.AutoRetry != nil && j.AutoRetry.State == "waiting") {
			hasPending = true
			break
		}
	}
	if !hasPending {
		return false, nil
	}
	cfg, err := ParseFreeConfig(raw)
	if err != nil {
		e.compensationError = safeFailure(err)
		return false, err
	}
	// Adopt legacy pending mailboxes once. Start with a full cooldown from this
	// observation, instead of replaying all overdue history immediately on upgrade.
	var old []RegistrationJob
	changed := false
	for i, j := range e.jobs {
		if !savedRegistrationMailbox(j) || j.AutoRetry != nil || !transientRegistrationFailure(j.Error) {
			continue
		}
		if !changed {
			old = append([]RegistrationJob(nil), e.jobs...)
		}
		j = cloneJob(j)
		planRegistrationCompensation(&j, nil, now)
		e.jobs[i] = j
		changed = true
	}
	if changed {
		if err := e.saveLocked(); err != nil {
			e.jobs = old
			e.loadError = err
			return false, err
		}
	}
	selected := -1
	var oldest time.Time
	for i, j := range e.jobs {
		if !savedRegistrationMailbox(j) || j.AutoRetry == nil || j.AutoRetry.State != "waiting" {
			continue
		}
		due, err := time.Parse(time.RFC3339Nano, j.AutoRetry.NextAttemptAt)
		if err != nil || j.AutoRetry.Attempts < 0 || j.AutoRetry.Attempts >= len(compensationDelays) {
			continue
		}
		if j.Mailbox.AccountID != cfg.HME.AccountID || j.MailboxAPIBase != "" && j.MailboxAPIBase != cfg.HME.BaseURL {
			e.compensationError = "config: saved_mailbox_source_unavailable"
			continue
		}
		if j.BrowserProfile == nil || !j.BrowserProfile.valid() {
			continue
		}
		if now.Before(due) {
			continue
		}
		if selected < 0 || due.Before(oldest) {
			selected, oldest = i, due
		}
	}
	if selected < 0 {
		return false, nil
	}
	if err := e.startCompensationLocked(selected, cfg, true, now); err != nil {
		e.compensationError = safeFailure(err)
		if e.loadError == nil {
			before := cloneJob(e.jobs[selected])
			finishRegistrationCompensation(&e.jobs[selected], "manual")
			e.jobs[selected].AutoRetry.Reason = "failure_requires_review"
			if safeFailure(err) == "task: mailbox_already_has_saved_account" {
				e.jobs[selected].AutoRetry.Reason = "saved_account_exists"
			}
			if saveErr := e.saveLocked(); saveErr != nil {
				e.jobs[selected] = before
				e.loadError = saveErr
				return false, saveErr
			}
		}
		return false, err
	}
	return true, nil
}

// The snapshot, duplicate check, durable claim and active slot share one lock
// with manual retry/start/cleanup. There is no unlocked stale-job launch window.
func (e *FreeEngine) startCompensationLocked(index int, cfg FreeConfig, automatic bool, now time.Time) error {
	parent := cloneJob(e.jobs[index])
	job, err := newRegistrationAttempt(parent, now)
	if err != nil {
		return err
	}
	if job.Mailbox.Email == "" || len(job.Account) > 0 || job.Imported || job.Verified {
		return fail("task", "no_saved_mailbox")
	}
	for i, other := range e.jobs {
		if i != index && strings.EqualFold(other.Mailbox.Email, job.Mailbox.Email) && (other.Verified || other.Imported || len(other.Account) > 0) {
			return fail("task", "mailbox_already_has_saved_account")
		}
	}
	if job.Mailbox.AccountID != cfg.HME.AccountID || job.MailboxAPIBase != "" && job.MailboxAPIBase != cfg.HME.BaseURL {
		return fail("config", "saved_mailbox_source_unavailable")
	}
	if job.BrowserProfile != nil && job.BrowserProfile.valid() {
		profile := *job.BrowserProfile
		cfg.BrowserProfile = &profile
	}
	if automatic {
		if cfg.BrowserProfile == nil {
			return fail("task", "saved_environment_missing")
		}
		job.AutoRetry.Attempts++
		job.AutoRetry.State, job.AutoRetry.Reason = "running", ""
		job.AutoRetry.LastAttemptAt = now.UTC().Format(time.RFC3339Nano)
		delay := compensationDelays[min(job.AutoRetry.Attempts, len(compensationDelays)-1)]
		job.AutoRetry.NextAttemptAt = now.Add(cfg.Timeout + delay).UTC().Format(time.RFC3339Nano)
	} else if job.AutoRetry != nil {
		// An explicit manual retry does not reset the automatic retry budget.
		finishRegistrationCompensation(&job, "manual_running")
	}
	job.Status, job.Stage, job.Error = "running", "compensation", ""
	job.MailboxAPIBase = cfg.HME.BaseURL
	job.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	action := "manual_compensation"
	detail := "复用已创建邮箱，开始手动补偿注册；原任务 " + parent.ID
	if automatic {
		action = "automatic_compensation"
		detail = fmt.Sprintf("复用已创建邮箱，开始第 %d/%d 次自动补偿注册", job.AutoRetry.Attempts, len(compensationDelays)) + "；原任务 " + parent.ID
	}
	appendRegistrationLogAt(&job, job.UpdatedAt, action, detail, "info")
	before := e.jobs[index]
	e.jobs[index] = registrationParentWithChild(parent, job)
	e.jobs = append(e.jobs, job)
	if err := e.saveLocked(); err != nil {
		e.jobs = e.jobs[:len(e.jobs)-1]
		e.jobs[index] = before
		e.loadError = err
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	e.active[job.ID] = &activeRegistration{cancel: cancel, done: make(chan struct{})}
	go e.runCompensation(ctx, job, cfg)
	return nil
}

func (e *FreeEngine) runCompensation(ctx context.Context, job RegistrationJob, cfg FreeConfig) {
	defer e.finishTask(job.ID)
	var runErr error
	defer func() {
		if recover() != nil {
			runErr = fail("task", "internal_error")
		}
		if runErr != nil {
			// A cancelled response-body read may have been mapped to read_error.
			// An explicit stop must never turn into another scheduled attempt.
			if ctx.Err() != nil {
				runErr = ctx.Err()
			}
			e.failJob(job.ID, runErr)
		}
	}()
	releaseProxy, err := e.ProxyRouting.acquire(ctx, job.ID, "openai", &cfg.Proxy, e.getJob, e.change)
	if err != nil {
		runErr = err
		return
	}
	defer releaseProxy()
	mail, flow, err := e.factory(cfg)
	if err != nil {
		runErr = err
		return
	}
	defer mail.Close()
	defer flow.Close()
	if provider, ok := flow.(RegistrationProfileProvider); ok && cfg.BrowserProfile == nil {
		// Manual recovery of legacy jobs can establish a profile for later retries.
		profile := provider.BrowserProfile()
		if runErr = e.change(job.ID, func(j *RegistrationJob) { j.BrowserProfile = &profile }); runErr != nil {
			return
		}
	}
	preparer, ok := mail.(MailSessionPreparer)
	if !ok {
		runErr = fail("mail", "resume_not_supported")
		return
	}
	if withJob, ok := mail.(interface {
		PrepareFor(context.Context, string, Mailbox) error
	}); ok {
		runErr = withJob.PrepareFor(ctx, mailboxClaimJobID(job), job.Mailbox)
	} else {
		runErr = preparer.Prepare(ctx, job.Mailbox)
	}
	if runErr != nil {
		return
	}
	runErr = e.runWithMailbox(ctx, job.ID, cfg, mail, flow, job.Mailbox)
}
