package register

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdobeHistoricalMailboxCleanupPreservesRecoveryPlanAndNeverOpensUpstream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adobe.enc")
	e := NewAdobeEngine(path, "test", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		t.Fatal("cleanup constructed upstream")
		return nil, nil, nil
	}, nil, nil)
	old := adobeCompensationFixture(t, time.Now())
	old.AutoRetry.State = "superseded"
	child := cloneJob(old)
	child.ID = "child"
	child.ParentJobID = old.ID
	child.RootJobID = old.ID
	child.AutoRetry.State = "exhausted"
	waiting := cloneJob(old)
	waiting.ID = "waiting"
	waiting.Mailbox.Email = "waiting@example.test"
	waiting.AutoRetry.State = "waiting"
	interrupted := cloneJob(old)
	interrupted.ID = "interrupted"
	interrupted.Mailbox.Email = "interrupted@example.test"
	interrupted.Status = "running"
	interrupted.AutoRetry.State = "running"
	seedAdobeCompensation(t, e, old, child, waiting, interrupted)
	e = NewAdobeEngine(path, "test", e.factory, nil, nil)
	before := append([]RegistrationJob(nil), e.jobs...)
	roots := map[string]int{}
	e.SetMailboxReservationHandlers(nil, func(ctx context.Context, j RegistrationJob) error {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		roots[mailboxClaimJobID(j)]++
		return nil
	})
	if err := e.ReconcileMailboxReservations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if roots[old.ID] != 1 || roots[waiting.ID] != 1 || roots[interrupted.ID] != 1 {
		t.Fatal(roots)
	}
	if !reflect.DeepEqual(before, e.jobs) {
		t.Fatal("cleanup rewrote recovery outcomes or budgets")
	}
}

func TestAdobeMailboxCleanupProtectsAnySavedAccountOrActiveTask(t *testing.T) {
	for _, kind := range []string{"saved", "imported", "verified", "completed", "queued", "running", "active_closing", "broken_journal"} {
		t.Run(kind, func(t *testing.T) {
			e := NewAdobeEngine(filepath.Join(t.TempDir(), "jobs.enc"), "test", nil, nil, nil)
			j := adobeCompensationFixture(t, time.Now())
			other := cloneJob(j)
			other.ID = "other-chain"
			other.Mailbox.Email = strings.ToUpper(j.Mailbox.Email)
			switch kind {
			case "saved":
				other.Account = map[string]any{"account_id": "saved"}
			case "imported":
				other.Imported = true
			case "verified":
				other.Verified = true
			case "completed", "queued", "running":
				other.Status = kind
			case "active_closing":
				e.activeJobs = map[string]bool{other.ID: true}
			case "broken_journal":
				e.loadError = errors.New("unreadable")
			}
			e.jobs = []RegistrationJob{j, other}
			calls := 0
			e.SetMailboxReservationHandlers(nil, func(context.Context, RegistrationJob) error { calls++; return nil })
			_ = e.ReconcileMailboxReservations(context.Background())
			if calls != 0 {
				t.Fatal("released protected mailbox")
			}
		})
	}
}

func TestAdobeMailboxCleanupFailureRemainsVisibleAndRetryable(t *testing.T) {
	e := NewAdobeEngine(filepath.Join(t.TempDir(), "jobs.enc"), "test", nil, nil, nil)
	job := adobeCompensationFixture(t, time.Now())
	seedAdobeCompensation(t, e, job)
	failRelease := true
	e.SetMailboxReservationHandlers(nil, func(context.Context, RegistrationJob) error {
		if failRelease {
			return errors.New("private db detail")
		}
		return nil
	})
	for i := 0; i < 2; i++ {
		if err := e.ReconcileMailboxReservations(context.Background()); err == nil {
			t.Fatal("cleanup error swallowed")
		}
	}
	snapshot := e.Snapshot()
	if snapshot["mailbox_cleanup_error"] != "mailbox: reservation_release_failed" {
		t.Fatal(snapshot["mailbox_cleanup_error"])
	}
	got, _ := e.getJob(job.ID)
	if len(got.Logs) != 1 || got.Logs[0].Action != "mailbox_release_failed" || strings.Contains(got.Logs[0].Detail, "private") {
		t.Fatal(got.Logs)
	}
	failRelease = false
	if err := e.ReconcileMailboxReservations(context.Background()); err != nil || e.Snapshot()["mailbox_cleanup_error"] != "" {
		t.Fatal(err)
	}
}

func TestAdobeMediaOnlyRetryReacquiresAndReleasesWithoutMailSource(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "released", true: "other_owner"}[blocked], func(t *testing.T) {
			e := NewAdobeEngine(filepath.Join(t.TempDir(), "jobs.enc"), "test", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				t.Fatal("constructed HME")
				return nil, nil, nil
			}, nil, nil)
			job := adobeCompensationFixture(t, time.Now())
			job.Error = "adobe: media_session_incomplete"
			seedAdobeCompensation(t, e, job)
			prepared, released, browser := 0, 0, 0
			e.SetMailboxReservationHandlers(func(_ context.Context, j RegistrationJob) error {
				prepared++
				if mailboxClaimJobID(j) != job.ID {
					t.Fatal("wrong root")
				}
				if blocked {
					return errors.New("taken")
				}
				return nil
			}, func(ctx context.Context, j RegistrationJob) error {
				released++
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				return nil
			})
			e.SetMediaFlowFactory(func(AdobeConfig) AdobeRegistrationFlow {
				return adobeCompensationFlow{run: func(_ context.Context, cfg AdobeConfig, _ Progress) (map[string]any, error) {
					browser++
					if !cfg.mediaOnly {
						t.Fatal("not media-only")
					}
					return nil, fail("adobe", "media_session_incomplete")
				}}
			})
			cfg, _ := ParseAdobeConfig(adobeTestConfig())
			e.runTask(context.Background(), cfg, job.ID)
			want := 1
			if blocked {
				want = 0
			}
			if prepared != 1 || released != 1 || browser != want {
				t.Fatal(prepared, released, browser)
			}
		})
	}
}

func TestAdobeFactoryFailureReleasesLocalReservationAndSavedResultDoesNot(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(map[bool]string{false: "factory_failed", true: "saved_result"}[saved], func(t *testing.T) {
			e := NewAdobeEngine(filepath.Join(t.TempDir(), "jobs.enc"), "test", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				return nil, nil, errors.New("factory unavailable")
			}, nil, nil)
			job := adobeCompensationFixture(t, time.Now())
			job.ParentJobID = "parent"
			job.RootJobID = "root"
			if saved {
				job.Account = map[string]any{"account_id": "saved"}
			}
			seedAdobeCompensation(t, e, job)
			var release atomic.Int32
			e.SetMailboxReservationHandlers(func(context.Context, RegistrationJob) error { return nil }, func(context.Context, RegistrationJob) error { release.Add(1); return nil })
			cfg, _ := ParseAdobeConfig(adobeTestConfig())
			e.runTask(context.Background(), cfg, job.ID)
			want := int32(1)
			if saved {
				want = 0
			}
			if release.Load() != want {
				t.Fatal(release.Load())
			}
		})
	}
}
