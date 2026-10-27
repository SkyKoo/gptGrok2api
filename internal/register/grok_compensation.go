package register

import (
	"context"
	"strings"
	"time"
)

// RunCompensation mirrors the OpenAI scheduler contract for the independent
// Grok engine. It only retries a task whose HME mailbox was already reserved;
// it never creates another alias or starts a second registration batch.
func (e *GrokEngine) RunCompensation(raw map[string]any, now time.Time) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loadError != nil {
		return false, e.loadError
	}
	if e.closing {
		return false, fail("task", "shutting_down")
	}
	if e.runCancel != nil || len(e.active) > 0 {
		return false, fail("task", "already_running")
	}
	e.compensationError = ""
	if !strings.EqualFold(stringValue(raw["target"]), "grok") {
		return false, nil
	}

	hasPending := false
	for _, job := range e.jobs {
		if savedRegistrationMailbox(job) && ((job.AutoRetry == nil && transientRegistrationFailure(job.Error)) || (job.AutoRetry != nil && job.AutoRetry.State == "waiting")) {
			hasPending = true
			break
		}
	}
	if !hasPending {
		return false, nil
	}
	cfg, err := ParseGrokConfig(raw)
	if err != nil {
		e.compensationError = safeFailure(err)
		return false, err
	}

	// Adopt legacy pending rows once, applying the same cooldown rules used by
	// the OpenAI engine. The copied row is persisted before any network work.
	for i := range e.jobs {
		job := cloneJob(e.jobs[i])
		if !savedRegistrationMailbox(job) || job.AutoRetry != nil || !transientRegistrationFailure(job.Error) {
			continue
		}
		planRegistrationCompensation(&job, nil, now)
		e.jobs[i] = job
	}
	if err := e.saveLocked(); err != nil {
		e.loadError = err
		return false, err
	}

	selected := -1
	var oldest time.Time
	for i, job := range e.jobs {
		if !savedRegistrationMailbox(job) || job.AutoRetry == nil || job.AutoRetry.State != "waiting" {
			continue
		}
		due, parseErr := time.Parse(time.RFC3339Nano, job.AutoRetry.NextAttemptAt)
		if parseErr != nil || job.AutoRetry.Attempts < 0 || job.AutoRetry.Attempts >= len(compensationDelays) {
			continue
		}
		if job.Mailbox.AccountID != cfg.HME.AccountID || (job.MailboxAPIBase != "" && job.MailboxAPIBase != cfg.HME.BaseURL) {
			e.compensationError = "config: saved_mailbox_source_unavailable"
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
	if err := e.startGrokCompensationLocked(selected, cfg, now); err != nil {
		e.compensationError = safeFailure(err)
		return false, err
	}
	return true, nil
}

func (e *GrokEngine) startGrokCompensationLocked(index int, cfg GrokConfig, now time.Time) error {
	job := cloneJob(e.jobs[index])
	if !savedRegistrationMailbox(job) {
		return fail("task", "no_saved_mailbox")
	}
	for i, other := range e.jobs {
		if i != index && strings.EqualFold(other.Mailbox.Email, job.Mailbox.Email) && (other.Verified || other.Imported || len(other.Account) > 0) {
			return fail("task", "mailbox_already_has_saved_account")
		}
	}
	if job.Mailbox.AccountID != cfg.HME.AccountID || (job.MailboxAPIBase != "" && job.MailboxAPIBase != cfg.HME.BaseURL) {
		return fail("config", "saved_mailbox_source_unavailable")
	}
	if job.AutoRetry == nil {
		job.AutoRetry = &RegistrationCompensation{}
	}
	job.AutoRetry.Attempts++
	job.AutoRetry.State = "running"
	job.AutoRetry.Reason = ""
	job.AutoRetry.LastAttemptAt = now.UTC().Format(time.RFC3339Nano)
	job.AutoRetry.NextAttemptAt = now.Add(cfg.Timeout + compensationDelays[min(job.AutoRetry.Attempts, len(compensationDelays)-1)]).UTC().Format(time.RFC3339Nano)
	job.Status, job.Stage, job.Error = "running", "compensation", ""
	job.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	job.Logs = append(job.Logs, JobLog{Time: job.UpdatedAt, Text: "automatic_compensation_started", Level: "info"})
	previous := e.jobs[index]
	e.jobs[index] = job
	if err := e.saveLocked(); err != nil {
		e.jobs[index] = previous
		e.loadError = err
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	e.active[job.ID] = &activeRegistration{cancel: cancel, done: make(chan struct{})}
	go e.runGrokCompensation(ctx, job, cfg)
	return nil
}

func (e *GrokEngine) runGrokCompensation(ctx context.Context, job RegistrationJob, cfg GrokConfig) {
	defer e.finishTask(job.ID)
	defer func() {
		if recover() != nil {
			e.failJob(job.ID, fail("task", "internal_error"))
		}
	}()
	mail, flow, err := e.factory(cfg)
	if err != nil {
		e.failJob(job.ID, err)
		return
	}
	defer mail.Close()
	defer flow.Close()
	if err := prepareSavedGrokMailbox(ctx, job.ID, job.Mailbox, mail); err != nil {
		e.failJob(job.ID, err)
		return
	}
	e.registerSaved(ctx, job.ID, job, cfg, mail, flow)
}

// min is kept local to this file to avoid changing the shared registration
// helpers used by the OpenAI engine.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
