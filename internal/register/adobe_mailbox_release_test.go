package register

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type adobeReleaseMailbox struct {
	jobID               string
	box                 Mailbox
	releaseErr          error
	releases            int
	releaseJob          string
	onAcquire           func()
	releaseContextValid bool
	reason              string
}

func (m *adobeReleaseMailbox) Acquire(_ context.Context, id string) (Mailbox, error) {
	m.jobID = id
	if m.onAcquire != nil {
		m.onAcquire()
	}
	return m.box, nil
}
func (m *adobeReleaseMailbox) WaitCode(context.Context, Mailbox, time.Time) (string, error) {
	return "", errors.New("unexpected code request")
}
func (m *adobeReleaseMailbox) Close() {}
func (m *adobeReleaseMailbox) Release(ctx context.Context, box Mailbox, reason string) error {
	m.releases++
	m.releaseJob, m.reason = m.jobID, reason
	deadline, bounded := ctx.Deadline()
	m.releaseContextValid = ctx.Err() == nil && bounded && deadline.After(time.Now()) && box.ID == m.box.ID
	return m.releaseErr
}

func TestAdobeAcquireJournalFailureReleasesUnstartedReservation(t *testing.T) {
	for _, releaseFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "released", true: "release_failed"}[releaseFails], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registration.json.enc")
			mail := &adobeReleaseMailbox{box: Mailbox{ID: "asset", Email: "fixture@example.test", SourceID: "fixture"}}
			if releaseFails {
				mail.releaseErr = errors.New("private provider response must not leak")
			}
			engine := NewAdobeEngine(path, "test", nil, nil, nil)
			engine.jobs = []RegistrationJob{{ID: "current-job", Status: "queued", Stage: "queued"}}
			if err := engine.saveLocked(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mail.onAcquire = func() {
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			_, err := engine.acquire(ctx, "current-job", mail)
			if err == nil {
				t.Fatal("expected journal failure")
			}
			if mail.releases != 1 || mail.releaseJob != "current-job" || mail.reason != "registration_not_submitted" || !mail.releaseContextValid {
				t.Fatalf("release=%+v", mail)
			}
			diagnostic := safeFailure(err)
			if !strings.Contains(diagnostic, "journal_write_failed") {
				t.Fatalf("lost original failure: %s", diagnostic)
			}
			if strings.Contains(diagnostic, "private provider") {
				t.Fatal("provider data leaked")
			}
			snapshot := engine.Snapshot()
			if releaseFails && (!strings.Contains(diagnostic, "reservation_release_failed") || !strings.Contains(snapshot["error"].(string), "reservation_release_failed")) {
				t.Fatal("failed cleanup was hidden by journal failure")
			}
			job, _ := engine.getJob("current-job")
			if job.Stage != "queued" || job.Mailbox.Email != "" {
				t.Fatal("failed journal mutation was not rolled back")
			}
		})
	}
}

func TestAdobeAcquireExcludedLegacyMailboxReleasesOnlyCurrentTask(t *testing.T) {
	engine := NewAdobeEngine(filepath.Join(t.TempDir(), "registration.json.enc"), "test", nil, nil, nil)
	box := Mailbox{ID: "asset", Email: "shared@example.test"}
	engine.jobs = []RegistrationJob{{ID: "earlier-job", Mailbox: box, Status: "registration_pending"}, {ID: "current-job", Status: "queued"}}
	mail := &adobeReleaseMailbox{box: box}
	_, err := engine.acquire(context.Background(), "current-job", mail)
	if err == nil || !strings.Contains(safeFailure(err), "existing_registration_requires_review") {
		t.Fatalf("error=%v", err)
	}
	if mail.releases != 1 || mail.releaseJob != "current-job" || !mail.releaseContextValid {
		t.Fatalf("release=%+v", mail)
	}
}

type adobeStageFailureFlow struct {
	stage string
	calls *atomic.Int32
}

func (f adobeStageFailureFlow) Register(_ context.Context, _ string, _ Mailbox, _ AdobeConfig, _ func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	f.calls.Add(1)
	if err := progress(f.stage); err != nil {
		return nil, err
	}
	return nil, fail("adobe", "fixture_failure")
}
func (adobeStageFailureFlow) Close() {}

func TestAdobeFailureReleaseStopsAtBrowserSubmissionBoundary(t *testing.T) {
	for _, stage := range []string{"waiting_browser", "starting_browser", "adobe_flow_started", "awaiting_verification"} {
		t.Run(stage, func(t *testing.T) {
			mail := &adobeReleaseMailbox{box: Mailbox{ID: "asset", Email: "fixture@example.test"}}
			var calls atomic.Int32
			engine := NewAdobeEngine(filepath.Join(t.TempDir(), "registration.json.enc"), "test", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				return mail, adobeStageFailureFlow{stage: stage, calls: &calls}, nil
			}, nil, nil)
			if _, err := engine.Start(adobeTestConfig()); err != nil {
				t.Fatal(err)
			}
			waitAdobe(t, engine)
			want := 0
			if stage == "waiting_browser" || stage == "starting_browser" {
				want = 1
			}
			if mail.releases != want || calls.Load() != 1 {
				t.Fatalf("releases=%d calls=%d", mail.releases, calls.Load())
			}
		})
	}
}
