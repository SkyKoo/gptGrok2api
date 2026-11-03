package register

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type releasingGrokMailbox struct {
	attemptMailbox
	box        Mailbox
	released   atomic.Int32
	releaseErr error
}

func (m *releasingGrokMailbox) Acquire(_ context.Context, jobID string) (Mailbox, error) {
	m.claimJob.Store(jobID)
	return m.box, nil
}
func (m *releasingGrokMailbox) Release(ctx context.Context, box Mailbox, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.released.Add(1)
	return m.releaseErr
}

func TestGrokFailureReleasesOnlyFinishedUnusedReservation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stage         string
		failure       error
		verifyFailure bool
		wantRelease   int32
	}{
		{name: "rejected before signup", stage: "send_code", failure: fail("send_code", "verification_required"), wantRelease: 1},
		{name: "cancelled before signup", stage: "wait_code", failure: context.Canceled, wantRelease: 1},
		{name: "waiting automatic retry", stage: "send_code", failure: fail("send_code", "network_error")},
		{name: "uncertain signup", stage: "signup", failure: context.DeadlineExceeded},
		{name: "saved account verification failed", verifyFailure: true},
		{name: "completed account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := grokCompensationFixture(time.Now())
			mail := &releasingGrokMailbox{box: fixture.Mailbox}
			flow := &reservationFlow{profile: *fixture.BrowserProfile, stage: tc.stage, failure: tc.failure}
			e := NewGrokEngine(filepath.Join(t.TempDir(), "jobs.json"), func(GrokConfig) (MailSource, GrokRegistrationFlow, error) { return mail, flow, nil }, func(context.Context, string, map[string]any) error { return nil }, func(context.Context, string, map[string]any) error {
				if tc.verifyFailure {
					return fail("verify", "network_error")
				}
				return nil
			})
			cfg, err := ParseGrokConfig(grokConfigFixture())
			if err != nil {
				t.Fatal(err)
			}
			e.mu.Lock()
			id, err := e.createJobLocked()
			e.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			e.runTask(context.Background(), id, cfg)
			if got := mail.released.Load(); got != tc.wantRelease {
				t.Fatalf("releases=%d want=%d", got, tc.wantRelease)
			}
			job, _ := e.getJob(id)
			if tc.verifyFailure && (!job.Imported || job.Status != "verification_pending" || len(job.Account) == 0) {
				t.Fatalf("lost saved result: %+v", job)
			}
		})
	}
}

type reservationFlow struct {
	profile BrowserProfile
	stage   string
	failure error
}

func (f *reservationFlow) Close()                         {}
func (f *reservationFlow) BrowserProfile() BrowserProfile { return f.profile }
func (f *reservationFlow) Register(ctx context.Context, box Mailbox, cfg GrokConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	if f.stage != "" {
		if err := progress(f.stage); err != nil {
			return nil, err
		}
	}
	if f.failure != nil {
		return nil, f.failure
	}
	return map[string]any{"sso": "test-session"}, nil
}

func TestGrokExhaustedRetryReleasesRootReservation(t *testing.T) {
	parent := grokCompensationFixture(time.Now())
	parent.AutoRetry.Attempts = 2
	path := filepath.Join(t.TempDir(), "jobs.json")
	writeCompensationJobs(t, path, parent)
	mail := &releasingGrokMailbox{box: parent.Mailbox}
	flow := &reservationFlow{profile: *parent.BrowserProfile, stage: "send_code", failure: fail("send_code", "network_error")}
	e := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) { return mail, flow, nil }, nil, nil)
	started, err := e.RunCompensation(grokConfigFixture(), compensationDue(t, parent))
	if err != nil || !started {
		t.Fatal(started, err)
	}
	waitGrokCompensation(t, e)
	child := grokLatest(t, e, parent.ID)
	if child.AutoRetry.State != "exhausted" || mail.released.Load() != 1 || mail.claimJob.Load() != parent.ID {
		t.Fatalf("release=%d job=%+v", mail.released.Load(), child)
	}
}

func TestGrokReleasePreservesUncertainHistoryAndReportsCleanupFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		uncertain     bool
		brokenJournal bool
		releaseErr    error
		want          int32
	}{
		{name: "old uncertain signup", uncertain: true},
		{name: "journal failure", brokenJournal: true},
		{name: "database failure", releaseErr: errors.New("private db error"), want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := grokCompensationFixture(time.Now())
			current.AutoRetry.State = "exhausted"
			current.AutoRetry.Attempts = 3
			path := filepath.Join(t.TempDir(), "jobs.json")
			writeCompensationJobs(t, path, current)
			e := NewGrokEngine(path, nil, nil, nil)
			if tc.uncertain {
				old := cloneJob(current)
				old.ID = "old"
				old.Stage = "signup"
				old.AutoRetry.State = "superseded"
				e.jobs = append(e.jobs, old)
			}
			if tc.brokenJournal {
				e.loadError = errors.New("journal unavailable")
			}
			mail := &releasingGrokMailbox{releaseErr: tc.releaseErr}
			e.releaseFailedMailboxReservation(current.ID, mail)
			if mail.released.Load() != tc.want {
				t.Fatal("unexpected release", mail.released.Load())
			}
			if tc.releaseErr != nil {
				job, _ := e.getJob(current.ID)
				if len(job.Logs) == 0 || job.Logs[len(job.Logs)-1].Action != "mailbox_release_failed" {
					t.Fatal("missing cleanup diagnostic")
				}
			}
		})
	}
}
