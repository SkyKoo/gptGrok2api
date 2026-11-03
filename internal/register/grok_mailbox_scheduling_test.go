package register

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGrokDuplicateRootsCannotResetExhaustedMailboxBudget(t *testing.T) {
	now := time.Now()
	old := grokCompensationFixture(now)
	old.ID, old.AutoRetry.State, old.AutoRetry.Attempts = "old", "exhausted", 3
	old.AutoRetry.NextAttemptAt = ""
	jobs := []RegistrationJob{old}
	for _, id := range []string{"new1", "new2", "new3", "new4", "new5"} {
		j := grokCompensationFixture(now)
		j.ID = id
		jobs = append(jobs, j)
	}
	path := filepath.Join(t.TempDir(), "jobs.json")
	writeCompensationJobs(t, path, jobs...)
	for restart := 0; restart < 2; restart++ {
		e := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) {
			t.Fatal("exhausted email retried")
			return nil, nil, nil
		}, nil, nil)
		if started, err := e.RunCompensation(grokConfigFixture(), now.Add(24*time.Hour)); started || err != nil {
			t.Fatal(started, err)
		}
		if len(e.jobs) != 6 {
			t.Fatal("history changed", len(e.jobs))
		}
		for _, job := range e.jobs {
			if job.AutoRetry.State != "exhausted" || job.AutoRetry.NextAttemptAt != "" {
				t.Fatal(job.ID, job.AutoRetry)
			}
			if e.mailboxCompensationSummaryLocked(job).Attempts != 3 {
				t.Fatal("misleading shared budget")
			}
		}
		if e.jobs[1].AutoRetry.Attempts != 0 {
			t.Fatal("shared projection overwrote per-root history")
		}
	}
}

func TestGrokDuplicateWaitingPlansKeepOneOwnerAndLatestCooldown(t *testing.T) {
	now := time.Now()
	first := grokCompensationFixture(now.Add(5 * time.Minute))
	first.ID = "first"
	second := grokCompensationFixture(now.Add(time.Hour))
	second.ID = "second"
	second.ProxyBinding = nil
	path := filepath.Join(t.TempDir(), "jobs.json")
	writeCompensationJobs(t, path, first, second)
	e := NewGrokEngine(path, nil, nil, nil)
	if e.jobs[0].AutoRetry.State != "waiting" || !compensationDue(t, e.jobs[0]).Equal(now.Add(time.Hour)) {
		t.Fatal("cooldown shortened", e.jobs[0].AutoRetry)
	}
	if e.jobs[1].AutoRetry.State != "manual" || e.jobs[1].AutoRetry.Reason != "duplicate_mailbox_retry" {
		t.Fatal(e.jobs[1].AutoRetry)
	}
	if e.normalizeMailboxRetriesLocked(now.Add(time.Minute)) {
		t.Fatal("normalization not idempotent")
	}
	if started, err := e.RunCompensation(grokConfigFixture(), now.Add(30*time.Minute)); started || err != nil {
		t.Fatal("early retry", started, err)
	}
	e = NewGrokEngine(path, nil, nil, nil)
	if e.jobs[1].AutoRetry.Reason != "duplicate_mailbox_retry" || !compensationDue(t, e.jobs[0]).Equal(now.Add(time.Hour)) {
		t.Fatal("restart lost dedup/cooldown")
	}
}

func TestGrokMailboxBudgetSumsRootsWithoutCountingChildrenTwice(t *testing.T) {
	now := time.Now()
	old := grokCompensationFixture(now)
	old.ID = "old"
	old.AutoRetry.Attempts = 1
	parent := cloneJob(old)
	parent.ID = "parent"
	parent.RootJobID = "old"
	parent.RetryJobID = "old"
	parent.AutoRetry.State = "superseded"
	other := grokCompensationFixture(now)
	other.ID = "other"
	other.AutoRetry.Attempts = 1
	path := filepath.Join(t.TempDir(), "jobs.json")
	writeCompensationJobs(t, path, parent, old, other)
	e := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) {
		return nil, nil, fail("mail", "network_error")
	}, nil, nil)
	if got := e.mailboxAutomaticAttemptsLocked(old.Mailbox.Email); got != 2 {
		t.Fatal(got)
	}
	if started, err := e.RunCompensation(grokConfigFixture(), now); !started || err != nil {
		t.Fatal(started, err)
	}
	waitGrokCompensation(t, e)
	latest := grokLatest(t, e, old.ID)
	if latest.AutoRetry.Attempts != 2 || latest.AutoRetry.State != "exhausted" {
		t.Fatal(latest.AutoRetry)
	}
	if got := e.mailboxAutomaticAttemptsLocked(old.Mailbox.Email); got != 3 {
		t.Fatal(got)
	}
	if started, err := e.RunCompensation(grokConfigFixture(), now.Add(48*time.Hour)); started || err != nil {
		t.Fatal(started, err)
	}
}

func TestGrokMailboxManualDecisionAndSavedResultStopOtherRoots(t *testing.T) {
	for _, kind := range []string{"uncertain", "refused", "saved"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			old := grokCompensationFixture(now)
			old.ID = "old"
			old.AutoRetry.State = "manual"
			switch kind {
			case "uncertain":
				old.Stage = "signup"
				old.AutoRetry.Reason = "registration_result_uncertain"
			case "refused":
				old.AutoRetry.Reason = "upstream_signup_refused"
			case "saved":
				old.Account = map[string]any{"sso": "fixture"}
			}
			fresh := grokCompensationFixture(now)
			fresh.ID = "fresh"
			path := filepath.Join(t.TempDir(), "jobs.json")
			writeCompensationJobs(t, path, old, fresh)
			e := NewGrokEngine(path, nil, nil, nil)
			if e.jobs[1].AutoRetry.State != "manual" {
				t.Fatal(e.jobs[1].AutoRetry)
			}
			if started, err := e.RunCompensation(grokConfigFixture(), now.Add(48*time.Hour)); started || err != nil {
				t.Fatal(started, err)
			}
		})
	}
}

func TestGrokManualRetryCannotBypassAnotherRootsRateLimit(t *testing.T) {
	now := time.Now()
	old := grokCompensationFixture(now)
	old.ID = "old"
	old.AutoRetry.State = "manual"
	rate := grokCompensationFixture(now.Add(time.Hour))
	rate.ID = "rate"
	rate.Error = "send_code: browser_worker_rejected (HTTP 429) operation=worker_error=signup_rate_limited"
	path := filepath.Join(t.TempDir(), "jobs.json")
	writeCompensationJobs(t, path, old, rate)
	e := NewGrokEngine(path, nil, nil, nil)
	if err := e.RetryRegistration(grokConfigFixture(), old.ID); err == nil || !strings.Contains(err.Error(), "cooldown") {
		t.Fatal(err)
	}
	if len(e.jobs) != 2 {
		t.Fatal("cooldown created a child")
	}
}

type schedulingMailbox struct {
	releasingGrokMailbox
	selected []string
	mu       sync.Mutex
}

func (m *schedulingMailbox) AcquireExcluding(_ context.Context, jobID string, excluded []string) (Mailbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	blocked := map[string]bool{}
	for _, email := range excluded {
		blocked[grokMailboxKey(email)] = true
	}
	for _, email := range []string{"old@example.test", "next@example.test", "third@example.test"} {
		if !blocked[email] {
			m.selected = append(m.selected, email)
			return Mailbox{Email: email, AccountID: "hme-account"}, nil
		}
	}
	return Mailbox{}, fail("mailbox", "none")
}

func TestGrokConcurrentNewTasksNeverReacquireFailedMailbox(t *testing.T) {
	now := time.Now()
	old := grokCompensationFixture(now)
	old.ID = "old"
	old.Mailbox.Email = "OLD@example.test"
	path := filepath.Join(t.TempDir(), "jobs.json")
	writeCompensationJobs(t, path, old)
	e := NewGrokEngine(path, nil, nil, nil)
	mail := &schedulingMailbox{}
	e.mu.Lock()
	one, _ := e.createJobLocked()
	two, _ := e.createJobLocked()
	e.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range []string{one, two} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			box, err := e.acquireNewMailbox(context.Background(), id, mail)
			if err != nil {
				t.Error(err)
				return
			}
			e.failJob(id, fail("send_code", "network_error"))
			e.releaseFailedMailboxReservation(id, mail)
			if box.Email == old.Mailbox.Email {
				t.Error("reused old mailbox")
			}
		}(id)
	}
	wg.Wait()
	if len(mail.selected) != 2 || mail.selected[0] != "next@example.test" || mail.selected[1] != "third@example.test" {
		t.Fatal(mail.selected)
	}
	if mail.released.Load() != 2 {
		t.Fatal("failure did not release claims", mail.released.Load())
	}
}
