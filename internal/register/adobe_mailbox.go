package register

import (
	"context"
	"strings"
	"time"
)

// A reservation protects work in progress, not an indefinite retry plan. The
// mailbox store additionally checks platform/root ownership in its transaction.
func (e *AdobeEngine) canReleaseAdobeMailboxLocked(job RegistrationJob, finishingID string) bool {
	if e.loadError != nil || strings.TrimSpace(job.Mailbox.Email) == "" {
		return false
	}
	switch job.Status {
	case "registration_pending", "failed", "cancelled", "interrupted":
	default:
		return false
	}
	for _, other := range e.jobs {
		if !strings.EqualFold(strings.TrimSpace(other.Mailbox.Email), strings.TrimSpace(job.Mailbox.Email)) {
			continue
		}
		if other.Imported || other.Verified || len(other.Account) > 0 || other.Status == "completed" || other.Status == "queued" || other.Status == "running" {
			return false
		}
		if e.activeJobs[other.ID] && other.ID != finishingID {
			return false
		}
	}
	return true
}

// These local callbacks do not construct an HME source or open a browser.
// The production callbacks serialize account-store checks with imports.
func (e *AdobeEngine) SetMailboxReservationHandlers(prepare, release func(context.Context, RegistrationJob) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.prepareMailboxReservation, e.releaseMailboxReservation = prepare, release
}

func (e *AdobeEngine) prepareAdobeMailboxReservation(ctx context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loadError != nil {
		return e.loadError
	}
	for _, job := range e.jobs {
		if job.ID != id {
			continue
		}
		if e.prepareMailboxReservation == nil {
			return nil // Embedders may provide reservation through MailSource.
		}
		if err := e.prepareMailboxReservation(ctx, cloneJob(job)); err != nil {
			return fail("mailbox", "reservation_prepare_failed")
		}
		return nil
	}
	return fail("task", "not_found")
}

func (e *AdobeEngine) recordAdobeMailboxCleanupErrorLocked(index int, code string) error {
	e.mailboxCleanupError = safeFailure(fail("mailbox", code))
	if index >= 0 && index < len(e.jobs) {
		job := &e.jobs[index]
		// Repeated reconciliation is observable without growing duplicate logs.
		if len(job.Logs) == 0 || job.Logs[len(job.Logs)-1].Action != "mailbox_release_failed" {
			appendRegistrationLog(job, "mailbox_release_failed", "注册未入库，释放邮箱预留未完成；系统核对时会重试", "error")
			if err := e.saveLocked(); err != nil {
				e.loadError = fail("storage", "journal_write_failed")
				return e.loadError
			}
		}
	}
	return fail("mailbox", code)
}

// Startup/manual reconciliation keeps task outcomes, automatic budgets and
// historical records intact. Claim release itself supplies the durable audit.
func (e *AdobeEngine) ReconcileMailboxReservations(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loadError != nil {
		return e.loadError
	}
	seen := map[string]bool{}
	for i, job := range e.jobs {
		if !e.canReleaseAdobeMailboxLocked(job, "") {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(job.Mailbox.Email)) + "/" + mailboxClaimJobID(job)
		if seen[key] {
			continue
		}
		if e.releaseMailboxReservation == nil {
			return e.recordAdobeMailboxCleanupErrorLocked(i, "reservation_release_unavailable")
		}
		if err := e.releaseMailboxReservation(ctx, cloneJob(job)); err != nil {
			return e.recordAdobeMailboxCleanupErrorLocked(i, "reservation_release_failed")
		}
		seen[key] = true
	}
	e.mailboxCleanupError = ""
	return nil
}

func (e *AdobeEngine) releaseFailedAdobeMailboxReservation(id string, mail MailSource) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, job := range e.jobs {
		if job.ID != id || !e.canReleaseAdobeMailboxLocked(job, id) {
			continue
		}
		var err error
		if e.releaseMailboxReservation != nil {
			err = e.releaseMailboxReservation(ctx, cloneJob(job))
		} else if releaser, ok := mail.(interface {
			Release(context.Context, Mailbox, string) error
		}); ok {
			err = releaser.Release(ctx, job.Mailbox, "registration_failed_without_account")
		} else {
			_ = e.recordAdobeMailboxCleanupErrorLocked(i, "reservation_release_unavailable")
			return
		}
		if err != nil {
			_ = e.recordAdobeMailboxCleanupErrorLocked(i, "reservation_release_failed")
		}
		return
	}
}
