package adobe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func reconciliationFixture(t *testing.T, age time.Duration) (*Store, Account, time.Time) {
	t.Helper()
	s, a := readyStore(t)
	if _, err := s.Reserve("unknown-task"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnknown("unknown-task"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.update(a.ID, a.Revision, func(a *Account) { a.LastUsedAt = now.Add(-age).Unix() }); err != nil {
		t.Fatal(err)
	}
	a, _ = s.Get(a.ID)
	return s, a, now
}

func recordReconciliation(t *testing.T, s *Store, a Account, now time.Time, quotaStatus string) SubmissionReconciliation {
	t.Helper()
	r, err := s.BeginSubmissionReconciliation(a.ID, a.InFlight, a.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	err = s.RecordSubmissionReconciliation(a.ID, a.InFlight, a.Revision, SubmissionReconciliationObservation{
		Attempt: r.Attempts, State: "waiting", CheckedAt: now, NextCheckAt: now.Add(time.Minute), QuotaStatus: quotaStatus,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAutomaticReconciliationKeepsOriginalGraceAcrossRefreshAndRestart(t *testing.T) {
	s, a, now := reconciliationFixture(t, 10*time.Minute)
	r := recordReconciliation(t, s, a, now, "known")
	if r.ReleaseAfter != now.Add(20*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("wrong original grace: %s", r.ReleaseAfter)
	}
	if _, _, err := s.AutoResolveSubmission(a.ID, a.InFlight, a.Revision, "terminal", now); !errors.Is(err, ErrResolutionConflict) {
		t.Fatalf("released too early: %v", err)
	}
	// Token replacement updates UpdatedAt, but must not start a new task grace.
	fresh, err := s.ReplaceCredentials(a, a)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(s.path, s.secret)
	if _, err := restarted.BeginSubmissionReconciliation(a.ID, a.InFlight, fresh.Revision, now.Add(30*time.Second)); !errors.Is(err, ErrBusy) {
		t.Fatalf("lost backoff: %v", err)
	}
	r2, err := restarted.BeginSubmissionReconciliation(a.ID, a.InFlight, fresh.Revision, now.Add(2*time.Minute))
	if err != nil || r2.ReleaseAfter != r.ReleaseAfter || r2.FirstObservedAt != r.FirstObservedAt || r2.Attempts != 2 {
		t.Fatalf("reconciliation reset: %+v %v", r2, err)
	}
	if err := restarted.RecordSubmissionReconciliation(a.ID, a.InFlight, a.Revision, SubmissionReconciliationObservation{Attempt: r.Attempts, State: "waiting", CheckedAt: now.Add(3 * time.Minute), NextCheckAt: now.Add(4 * time.Minute), QuotaStatus: "known"}); !errors.Is(err, ErrResolutionConflict) {
		t.Fatal("old credential result was accepted")
	}
}

func TestAutomaticReconciliationMissingOrFutureLegacyTimeGetsPersistedGrace(t *testing.T) {
	for _, stamp := range []int64{0, time.Now().Add(time.Hour).Unix()} {
		s, a, now := reconciliationFixture(t, time.Hour)
		_ = s.update(a.ID, a.Revision, func(a *Account) { a.LastUsedAt = stamp })
		r := recordReconciliation(t, s, a, now, "error")
		if r.ReleaseAfter != now.Add(SubmissionReconciliationGrace).Format(time.RFC3339Nano) {
			t.Fatal("legacy timestamp released early")
		}
		restarted := NewStore(s.path, s.secret)
		r2, err := restarted.BeginSubmissionReconciliation(a.ID, a.InFlight, a.Revision, now.Add(2*time.Minute))
		if err != nil || r2.ReleaseAfter != r.ReleaseAfter {
			t.Fatal("first-observation deadline not persisted")
		}
	}
}

func TestAutomaticResolutionAuditsUnknownAndPreventsReplay(t *testing.T) {
	s, a, now := reconciliationFixture(t, time.Hour)
	_ = s.SaveJob(a.InFlight, Job{Status: "SUBMISSION_UNKNOWN", RequestID: "retained-request", Model: "gemini-3.1-flash-image-2.1", Kind: "image"})
	recordReconciliation(t, s, a, now, "known")
	resolved, audit, err := s.AutoResolveSubmission(a.ID, a.InFlight, a.Revision, "terminal", now)
	if err != nil || resolved.InFlight != "" || resolved.Status != "active" || resolved.SettlementUntil != now.Add(30*time.Second).Unix() {
		t.Fatalf("resolution failed: %+v %v", resolved, err)
	}
	if audit.ReviewedBy != "system" || audit.Method != "automatic_no_handle" || audit.Outcome != "unknown" || audit.QuotaStatus != "known" {
		t.Fatalf("misleading audit: %+v", audit)
	}
	if resolved.Token != a.Token || resolved.Cookie != a.Cookie || resolved.Revision != a.Revision || resolved.Enabled != a.Enabled || resolved.Quota.Status != "stale" {
		t.Fatal("release changed auth or invented quota")
	}
	if Public(resolved)["task_reconciliation"] != nil {
		t.Fatal("completed hold still shown as active reconciliation")
	}
	restarted := NewStore(s.path, s.secret)
	job, err := restarted.LoadJob(a.InFlight)
	if err != nil || job.Status != "SUBMISSION_UNKNOWN" || job.RequestID != "retained-request" {
		t.Fatal("original unknown outcome lost")
	}
	if _, err := restarted.Reserve(a.InFlight); err == nil {
		t.Fatal("resolved task can regenerate")
	}
	if err := restarted.SaveJob(a.InFlight, Job{}); !errors.Is(err, ErrTaskResolved) {
		t.Fatal("resolved task can replace handle")
	}
	if _, _, err := restarted.AutoResolveSubmission(a.ID, a.InFlight, a.Revision, "terminal", now); !errors.Is(err, ErrTaskResolved) {
		t.Fatal("resolution duplicated")
	}
	audits, err := restarted.SubmissionResolutions(a.ID)
	if err != nil || len(audits) != 1 || audits[0].PreviousError != "adobe_submission_unknown" {
		t.Fatal("durable audit missing")
	}
}

func TestAutomaticResolutionPreservesFailedQuotaAndDisabledAccount(t *testing.T) {
	s, a, now := reconciliationFixture(t, time.Hour)
	_ = s.update(a.ID, a.Revision, func(a *Account) {
		a.Quota.Status = "error"
		a.Quota.Error = "adobe_quota_unavailable"
		a.Enabled = false
		a.ExpiresAt = now.Add(-time.Minute).Unix()
	})
	recordReconciliation(t, s, a, now, "error")
	got, audit, err := s.AutoResolveSubmission(a.ID, a.InFlight, a.Revision, "missing", now)
	if err != nil || got.Quota.Status != "error" || got.Quota.Error != "adobe_quota_unavailable" || got.Enabled || Eligible(got, now.Add(time.Minute)) || audit.Outcome != "unknown" {
		t.Fatalf("unsafe failed-quota release: %v", err)
	}
}

func TestAutomaticResolutionRequiresFinishedObservationAndAbsentHandle(t *testing.T) {
	for _, mode := range []string{"unchecked", "checking", "polling", "handle", "invalid-url", "corrupt", "wrong-revision", "wrong-task", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			s, a, now := reconciliationFixture(t, time.Hour)
			r, err := s.BeginSubmissionReconciliation(a.ID, a.InFlight, a.Revision, now)
			if err != nil {
				t.Fatal(err)
			}
			state, quota := "waiting", "known"
			if mode == "unchecked" {
				quota = "not_checked"
			}
			if mode == "polling" {
				state = "polling"
			}
			if mode != "checking" {
				err = s.RecordSubmissionReconciliation(a.ID, a.InFlight, a.Revision, SubmissionReconciliationObservation{Attempt: r.Attempts, State: state, CheckedAt: now, NextCheckAt: now.Add(time.Minute), QuotaStatus: quota})
				if err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "handle":
				_ = s.SaveJob(a.InFlight, Job{PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/test"})
			case "invalid-url":
				_ = s.SaveJob(a.InFlight, Job{PollURL: "untrusted-nonempty"})
			case "corrupt":
				if err := os.MkdirAll(filepath.Dir(s.jobPath(a.InFlight)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.jobPath(a.InFlight), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "maintenance":
				s.mu.Lock()
				s.maintenance[a.ID] = true
				s.mu.Unlock()
			case "wrong-revision":
				a.Revision++
			case "wrong-task":
				a.InFlight = "different-task"
			}
			if _, _, err := s.AutoResolveSubmission(a.ID, a.InFlight, a.Revision, "terminal", now); err == nil {
				t.Fatal("unsafe resolution accepted")
			}
			current, _ := s.Get(a.ID)
			if current.InFlight != "unknown-task" || current.Status != "submission_unknown" {
				t.Fatal("failed auto resolution released task")
			}
		})
	}
}

func TestAutomaticReconciliationSerializesClaimsAndRejectsOldObservation(t *testing.T) {
	s, a, now := reconciliationFixture(t, time.Hour)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.BeginSubmissionReconciliation(a.ID, a.InFlight, a.Revision, now)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrBusy) {
				t.Errorf("unexpected claim error: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("duplicate observation claim")
	}
	newer, err := s.BeginSubmissionReconciliation(a.ID, a.InFlight, a.Revision, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	obs := SubmissionReconciliationObservation{Attempt: newer.Attempts - 1, State: "waiting", CheckedAt: now.Add(4 * time.Minute), NextCheckAt: now.Add(5 * time.Minute), QuotaStatus: "known"}
	if err := s.RecordSubmissionReconciliation(a.ID, a.InFlight, a.Revision, obs); !errors.Is(err, ErrResolutionConflict) {
		t.Fatal("expired attempt overwrote current observation")
	}
	obs.Attempt = newer.Attempts
	obs.LastError = "Bearer secret-cookie-value"
	if err := s.RecordSubmissionReconciliation(a.ID, a.InFlight, a.Revision, obs); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Get(a.ID)
	pub := Public(current)["task_reconciliation"].(map[string]any)
	if pub["last_error"] != "check_failed" || pub["state"] != "waiting" {
		t.Fatal("unbounded error retained")
	}
}

func TestAutomaticResolutionPersistenceFailureRetainsOccupancy(t *testing.T) {
	s, a, now := reconciliationFixture(t, time.Hour)
	recordReconciliation(t, s, a, now, "known")
	original := s.path
	f, err := os.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s.path = "/dev/fd/" + fmt.Sprint(f.Fd())
	if _, err := os.Stat(s.path); err != nil {
		t.Skip("descriptor paths unavailable")
	}
	if _, _, err := s.AutoResolveSubmission(a.ID, a.InFlight, a.Revision, "terminal", now); err == nil {
		t.Fatal("failed persistence accepted")
	}
	got, err := NewStore(original, s.secret).Get(a.ID)
	if err != nil || got.InFlight != a.InFlight || got.Status != "submission_unknown" {
		t.Fatal("failed save lost occupancy")
	}
}

func TestReleaseKnownOutcomeClearsOnlyUnknownAccountStatus(t *testing.T) {
	s, a, _ := reconciliationFixture(t, time.Hour)
	if err := s.Release(a.InFlight); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(a.ID)
	if got.Status != "active" || got.LastError != "" || got.InFlight != "" {
		t.Fatalf("stale unknown status after release: %s", got.Status)
	}
	if got.Token != a.Token || got.Cookie != a.Cookie || !reflect.DeepEqual(got.RegistrationMailbox, a.RegistrationMailbox) {
		t.Fatal("release changed credentials/source")
	}
}

func TestAutomaticPollReleaseKeepsTerminalAuditAndPreventsReopening(t *testing.T) {
	for _, status := range []string{"COMPLETED", "FAILED", "ERROR", "CANCELLED", "CANCELED", "REJECTED"} {
		for _, phase := range []string{"checking", "polling"} {
			t.Run(status+"/"+phase, func(t *testing.T) {
				s, a, now := reconciliationFixture(t, time.Hour)
				job := Job{Kind: "image", Model: "gemini-3.1-flash-image-2.1", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: status, RequestID: "original-request"}
				if err := s.SaveJob(a.InFlight, job); err != nil {
					t.Fatal(err)
				}
				r, err := s.BeginSubmissionReconciliation(a.ID, a.InFlight, a.Revision, now)
				if err != nil {
					t.Fatal(err)
				}
				if phase == "polling" {
					err = s.RecordSubmissionReconciliation(a.ID, a.InFlight, a.Revision, SubmissionReconciliationObservation{Attempt: r.Attempts, State: "polling", CheckedAt: now, NextCheckAt: now.Add(time.Minute), QuotaStatus: "not_checked"})
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Release(a.InFlight); err != nil {
					t.Fatal(err)
				}
				restarted := NewStore(s.path, s.secret)
				records, err := restarted.SubmissionResolutions(a.ID)
				outcome := "failed"
				if status == "COMPLETED" {
					outcome = "completed"
				}
				if err != nil || len(records) != 1 || records[0].Method != "automatic_poll" || records[0].Outcome != outcome || records[0].ReviewedBy != "system" || records[0].QuotaStatus != "not_checked" {
					t.Fatalf("terminal poll audit missing: %+v %v", records, err)
				}
				if _, err := restarted.Resume(a.InFlight, a.ID); !errors.Is(err, ErrTaskResolved) {
					t.Fatalf("terminal poll reopened: %v", err)
				}
				if err := restarted.SaveJob(a.InFlight, Job{}); !errors.Is(err, ErrTaskResolved) {
					t.Fatal("terminal upstream history overwritten")
				}
				preserved, err := restarted.LoadJob(a.InFlight)
				if err != nil || !reflect.DeepEqual(preserved, job) {
					t.Fatal("terminal poll changed upstream evidence")
				}
				if err := restarted.Release(a.InFlight); err != nil {
					t.Fatal(err)
				}
				again, _ := restarted.SubmissionResolutions(a.ID)
				if !reflect.DeepEqual(records, again) {
					t.Fatal("repeat release changed audit")
				}
			})
		}
	}
}

func TestOrdinaryOrUnfinishedReleaseDoesNotInventAutomaticPollAudit(t *testing.T) {
	for _, mode := range []string{"ordinary", "nonterminal", "missing-handle", "waiting"} {
		t.Run(mode, func(t *testing.T) {
			s, a, now := reconciliationFixture(t, time.Hour)
			job := Job{PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "COMPLETED"}
			if mode == "nonterminal" {
				job.Status = "IN_PROGRESS"
			}
			if mode == "missing-handle" {
				job.PollURL = ""
			}
			if err := s.SaveJob(a.InFlight, job); err != nil {
				t.Fatal(err)
			}
			if mode == "waiting" {
				recordReconciliation(t, s, a, now, "known")
			} else if mode != "ordinary" {
				if _, err := s.BeginSubmissionReconciliation(a.ID, a.InFlight, a.Revision, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Release(a.InFlight); err != nil {
				t.Fatal(err)
			}
			records, err := s.SubmissionResolutions(a.ID)
			if err != nil || len(records) != 0 {
				t.Fatalf("invented automatic poll audit: %+v", records)
			}
		})
	}
}
