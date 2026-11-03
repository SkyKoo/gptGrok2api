package register

import (
	"context"
	"strings"
	"time"
)

func grokRegistrationRateLimited(job RegistrationJob) bool {
	return strings.Contains(job.Error, "(HTTP 429)") || strings.Contains(job.Error, "worker_error=signup_rate_limited")
}

func grokMailboxKey(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// A released asset is free, but its failed registration still owns the retry
// decision. Only an explicit retry may reuse it; a new batch must move on.
func (e *GrokEngine) acquireNewMailbox(ctx context.Context, id string, mail MailSource) (Mailbox, error) {
	// Serialize selection through journal persistence, including provider Prepare.
	// Otherwise another worker could take an exclusion snapshot before this
	// mailbox is recorded, then acquire it just after its failed claim is released.
	e.mailboxMu.Lock()
	defer e.mailboxMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Mailbox{}, err
	}
	e.mu.Lock()
	blocked := map[string]bool{}
	for _, job := range e.jobs {
		if job.ID != id && job.Mailbox.Email != "" {
			blocked[grokMailboxKey(job.Mailbox.Email)] = true
		}
	}
	e.mu.Unlock()
	excluded := make([]string, 0, len(blocked))
	for email := range blocked {
		excluded = append(excluded, email)
	}
	var box Mailbox
	var err error
	if filtered, ok := mail.(interface {
		AcquireExcluding(context.Context, string, []string) (Mailbox, error)
	}); ok {
		box, err = filtered.AcquireExcluding(ctx, id, excluded)
	} else {
		box, err = mail.Acquire(ctx, id)
	}
	if err != nil {
		return Mailbox{}, err
	}
	if blocked[grokMailboxKey(box.Email)] {
		if releaser, ok := mail.(interface {
			Release(context.Context, Mailbox, string) error
		}); ok {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := releaser.Release(releaseCtx, box, "duplicate_registration_allocation"); err != nil {
				return Mailbox{}, fail("mailbox", "duplicate_allocation_release_failed")
			}
		}
		return Mailbox{}, fail("mailbox", "existing_registration_requires_retry")
	}
	err = e.change(id, func(j *RegistrationJob) {
		j.Mailbox = box
		attachRegistrationLogAccount(j)
		appendRegistrationLog(j, "mailbox_acquired", "邮箱资源已分配，开始注册", "info")
	})
	return box, err
}

// Each root stores its own cumulative counter. Sum root maxima, not every child
// counter; projecting a shared count must never write it back into a root.
func (e *GrokEngine) mailboxAutomaticAttemptsLocked(email string) int {
	byRoot := map[string]int{}
	for _, job := range e.jobs {
		if grokMailboxKey(job.Mailbox.Email) != grokMailboxKey(email) || job.AutoRetry == nil {
			continue
		}
		root := mailboxClaimJobID(job)
		if job.AutoRetry.Attempts > byRoot[root] {
			byRoot[root] = job.AutoRetry.Attempts
		}
	}
	total := 0
	for _, count := range byRoot {
		total += min(count, len(compensationDelays))
	}
	return total
}

func (e *GrokEngine) mailboxCompensationSummaryLocked(job RegistrationJob) *RegistrationCompensation {
	state := grokRegistrationCompensation(job)
	if state != nil && job.RetryJobID == "" && job.Mailbox.Email != "" {
		state.Attempts = e.mailboxAutomaticAttemptsLocked(job.Mailbox.Email)
	}
	return state
}

// Reconcile legacy duplicate roots without deleting history, creating a new
// mailbox state, resetting counters or sending upstream requests. Call under mu.
func (e *GrokEngine) normalizeMailboxRetriesLocked(now time.Time) bool {
	groups := map[string][]int{}
	for i, job := range e.jobs {
		if job.Mailbox.Email != "" && job.RetryJobID == "" {
			groups[grokMailboxKey(job.Mailbox.Email)] = append(groups[grokMailboxKey(job.Mailbox.Email)], i)
		}
	}
	changed := false
	for email, indices := range groups {
		attempts := e.mailboxAutomaticAttemptsLocked(email)
		owner, active := -1, -1
		reason := ""
		var latestDue time.Time
		for _, i := range indices {
			job := e.jobs[i]
			if len(job.Account) > 0 || job.Imported || job.Verified {
				reason = "saved_account_exists"
			}
			if job.Status == "running" || job.Status == "queued" {
				active = i
			}
			if !savedRegistrationMailbox(job) {
				continue
			}
			if grokRegistrationResultUncertain(job) && reason == "" {
				reason = "registration_result_uncertain"
			}
			if job.AutoRetry == nil {
				continue
			}
			state := job.AutoRetry
			if due, err := time.Parse(time.RFC3339Nano, state.NotBeforeAt); err == nil && due.After(latestDue) {
				latestDue = due
			}
			if state.State == "manual" && state.Reason != "duplicate_mailbox_retry" && reason == "" {
				reason = "mailbox_requires_review"
			}
			if state.State != "waiting" {
				continue
			}
			if owner < 0 {
				owner = i
			}
			if due, err := time.Parse(time.RFC3339Nano, state.NextAttemptAt); err == nil && due.After(latestDue) {
				latestDue = due
			}
			if job.ProxyBinding != nil && job.ProxyBinding.BlockedUntil.After(latestDue) {
				latestDue = job.ProxyBinding.BlockedUntil
			}
			// A second root with a zero counter cannot shorten the shared backoff.
			if attempts > state.Attempts && attempts < len(compensationDelays) {
				if failedAt, err := time.Parse(time.RFC3339Nano, job.UpdatedAt); err == nil {
					if due := failedAt.Add(compensationDelays[attempts]); due.After(latestDue) {
						latestDue = due
					}
				}
			}
		}
		for _, i := range indices {
			job := e.jobs[i]
			if !savedRegistrationMailbox(job) || job.AutoRetry == nil || job.AutoRetry.State != "waiting" {
				continue
			}
			state := *job.AutoRetry
			if grokRegistrationRateLimited(job) {
				if due, err := time.Parse(time.RFC3339Nano, state.NextAttemptAt); err == nil {
					if old, err := time.Parse(time.RFC3339Nano, state.NotBeforeAt); err != nil || due.After(old) {
						state.NotBeforeAt = state.NextAttemptAt
					}
				}
			}
			switch {
			case active >= 0:
				state.State, state.Reason, state.NextAttemptAt = "manual", "duplicate_mailbox_retry", ""
			case attempts >= len(compensationDelays):
				state.State, state.Reason, state.NextAttemptAt = "exhausted", "retry_limit_reached", ""
			case reason != "":
				state.State, state.Reason, state.NextAttemptAt = "manual", reason, ""
			case i != owner:
				state.State, state.Reason, state.NextAttemptAt = "manual", "duplicate_mailbox_retry", ""
			case !latestDue.IsZero():
				state.NextAttemptAt = latestDue.UTC().Format(time.RFC3339Nano)
			}
			if state == *job.AutoRetry {
				continue
			}
			job = cloneJob(job)
			job.AutoRetry = &state
			appendRegistrationLogAt(&job, now.UTC().Format(time.RFC3339Nano), "mailbox_retry_reconciled", "已核对同邮箱补偿计划，保留历史次数与冷却", "info")
			e.jobs[i] = job
			changed = true
		}
	}
	return changed
}
