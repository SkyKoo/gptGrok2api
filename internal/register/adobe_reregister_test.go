package register

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func adobeReregisterFixture(t *testing.T) RegistrationJob {
	t.Helper()
	job := adobeCompensationFixture(t, time.Now())
	job.AdobeRegistration.CreationOutcome = "unknown"
	job.Error = "adobe: form_rejected"
	job.AutoRetry = &RegistrationCompensation{State: "manual", Attempts: 3, Reason: "registration_result_uncertain"}
	job.Logs = []JobLog{{Action: "adobe_creation_submitted", Detail: "original submission retained"}}
	return job
}

func TestAdobeManualReregistrationStartsFreshOnceAndPreservesUnknownHistory(t *testing.T) {
	parent := adobeReregisterFixture(t)
	parent.AutoRetry.NotBeforeAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	mail := &adobeCompensationMail{}
	var seen atomic.Int32
	var cfgSeen AdobeConfig
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	e := NewAdobeEngine(path, "fixture", func(cfg AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		return mail, adobeCompensationFlow{run: func(_ context.Context, cfg AdobeConfig, progress Progress) (map[string]any, error) {
			seen.Add(1)
			cfgSeen = cfg
			if err := progress("adobe_creation_submitted"); err != nil {
				return nil, err
			}
			if err := cfg.creationProgress(adobeCreationObservation{Outcome: "unknown"}, true); err != nil {
				return nil, err
			}
			return nil, fail("adobe", "worker_result_unknown")
		}}, nil
	}, nil, nil)
	seedAdobeCompensation(t, e, parent)
	if err := e.RetryRegistration(adobeTestConfig(), parent.ID); err == nil {
		t.Fatal("legacy recovery must still reject an unknown result")
	}
	if err := e.Reregister(adobeTestConfig(), parent.ID); err != nil {
		t.Fatal(err)
	}
	waitAdobe(t, e)
	child := latestAdobe(t, e, parent.ID)
	if child.ID == parent.ID || child.ParentJobID != parent.ID || child.RootJobID != parent.ID || child.Mailbox != parent.Mailbox {
		t.Fatal("new attempt lost original mailbox or lineage")
	}
	if cfgSeen.RecoveryMode != "retry_registration" || cfgSeen.BrowserProfileID != child.ID || cfgSeen.BrowserProfileID == adobeBrowserProfileID(parent) {
		t.Fatal("manual action resumed the original browser")
	}
	if seen.Load() != 1 || mail.acquired.Load() != 0 || mail.prepared.Load() != 1 || mail.claim.Load() != parent.ID {
		t.Fatal("manual action did not reuse the root mailbox claim exactly once")
	}
	original, _ := e.getJob(parent.ID)
	if original.Error != parent.Error || original.Stage != parent.Stage || !reflect.DeepEqual(original.AdobeRegistration, parent.AdobeRegistration) || original.Logs[0] != parent.Logs[0] {
		t.Fatal("original failure evidence was rewritten")
	}
	if child.AutoRetry.Attempts != parent.AutoRetry.Attempts || e.adobeMailboxAttemptsLocked(parent.Mailbox.Email) != 3 {
		t.Fatal("manual registration reset or consumed the automatic budget")
	}
	if child.AutoRetry.NotBeforeAt != parent.AutoRetry.NotBeforeAt {
		t.Fatal("manual registration discarded the mailbox cooldown history")
	}
	if len(child.Logs) == 0 || child.Logs[0].Action != "manual_reregistration" {
		t.Fatal("explicit administrator action is not auditable")
	}
	if err := e.Reregister(adobeTestConfig(), parent.ID); err == nil {
		t.Fatal("repeated click on the old task created another child")
	}
	if ran, err := e.RunCompensation(adobeTestConfig(), time.Now().Add(24*time.Hour)); ran || err != nil || seen.Load() != 1 {
		t.Fatalf("manual unknown result was automatically replayed: ran=%v err=%v", ran, err)
	}
	reloaded := NewAdobeEngine(path, "fixture", e.factory, nil, nil)
	if ran, err := reloaded.RunCompensation(adobeTestConfig(), time.Now().Add(48*time.Hour)); ran || err != nil || seen.Load() != 1 {
		t.Fatalf("restart replayed an unknown manual attempt: %v %v", ran, err)
	}
	if latestAdobe(t, reloaded, parent.ID).ID != child.ID {
		t.Fatal("restart lost the manual child")
	}
}

func TestAdobeManualReregistrationRejectsProtectedMailboxStates(t *testing.T) {
	for _, scenario := range []string{"saved", "imported", "verified", "completed", "child_exists", "newer_leaf", "another_saved", "another_completed", "invalid_cooldown", "negative_budget", "missing_source", "running"} {
		t.Run(scenario, func(t *testing.T) {
			job := adobeReregisterFixture(t)
			jobs := []RegistrationJob{job}
			switch scenario {
			case "saved":
				jobs[0].Account = map[string]any{"account_id": "saved"}
			case "imported":
				jobs[0].Imported = true
			case "verified":
				jobs[0].Verified = true
			case "completed":
				jobs[0].Status = "completed"
			case "child_exists":
				jobs[0].RetryJobID = "already-created"
			case "invalid_cooldown":
				jobs[0].AutoRetry.NotBeforeAt = "invalid"
			case "negative_budget":
				jobs[0].AutoRetry.Attempts = -1
			case "missing_source":
				jobs[0].Mailbox.AccountID = ""
			case "running":
				jobs[0].Status = "running"
			default:
				other := cloneJob(job)
				other.ID, other.Mailbox.Email = "another-root", " EXISTING@EXAMPLE.TEST "
				if scenario == "another_completed" {
					other.Status = "completed"
				}
				if scenario == "another_saved" {
					other.Account = map[string]any{"account_id": "saved"}
				}
				jobs = append(jobs, other)
			}
			var calls atomic.Int32
			e := NewAdobeEngine(filepath.Join(t.TempDir(), "journal"), "fixture", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				calls.Add(1)
				return nil, nil, errors.New("unexpected Worker access")
			}, nil, nil)
			seedAdobeCompensation(t, e, jobs...)
			before, _ := os.ReadFile(e.path)
			if err := e.Reregister(adobeTestConfig(), job.ID); err == nil {
				t.Fatal("protected mailbox admitted")
			}
			after, _ := os.ReadFile(e.path)
			if calls.Load() != 0 || string(before) != string(after) {
				t.Fatal("rejected manual action changed the journal or reached Worker")
			}
		})
	}
}

func TestAdobeManualReregistrationAcceptsFailedCreationWithoutOldEnvironment(t *testing.T) {
	for _, outcome := range []string{"accepted", "unknown", "rejected", "legacy"} {
		t.Run(outcome, func(t *testing.T) {
			parent := adobeReregisterFixture(t)
			parent.AdobeProfile = nil
			parent.AdobeRegistration.ProxyRecorded = false
			parent.AdobeRegistration.Proxy = "http://old.invalid:8080"
			parent.ProxyBinding = &proxyruntime.Binding{Reference: "node:removed/old", Platform: "adobe", Purpose: "registration"}
			parent.AdobeRegistration.CreationOutcome = outcome
			if outcome == "rejected" {
				parent.AdobeRegistration.CreationErrorCode, parent.AdobeRegistration.CreationHTTPStatus = "captcha_required", 400
			}
			if outcome == "legacy" {
				parent.AdobeRegistration = nil
			}
			mail := &adobeCompensationMail{}
			var observed AdobeConfig
			var calls atomic.Int32
			e := NewAdobeEngine(filepath.Join(t.TempDir(), "journal"), "fixture", func(cfg AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				observed = cfg
				return mail, adobeCompensationFlow{run: func(context.Context, AdobeConfig, Progress) (map[string]any, error) {
					calls.Add(1)
					return nil, fail("adobe", "worker_result_unknown")
				}}, nil
			}, nil, nil)
			seedAdobeCompensation(t, e, parent)
			if e.Snapshot()["jobs"].([]any)[0].(map[string]any)["can_reregister"] != true {
				t.Fatal("missing old browser environment hid explicit fresh registration")
			}
			cfg := adobeTestConfig()
			cfg["proxy"] = "http://current.invalid:8080"
			if err := e.Reregister(cfg, parent.ID); err != nil {
				t.Fatal(err)
			}
			waitAdobe(t, e)
			child := latestAdobe(t, e, parent.ID)
			original, _ := e.getJob(parent.ID)
			if calls.Load() != 1 || observed.Proxy != "http://current.invalid:8080" || observed.BrowserProfileID != child.ID || observed.RecoveryMode != "retry_registration" {
				t.Fatal("manual request did not use exactly one fresh profile with the current route")
			}
			if child.AdobeProfile == nil || child.AdobeProfile.Country != "US" || child.AdobeProfile.CountrySource != "" || child.ProxyBinding != nil {
				t.Fatal("fresh attempt depended on the old identity or proxy binding")
			}
			first, last := adobeBirthdayBounds(time.Now())
			birth := time.Date(child.AdobeProfile.BirthYear, time.Month(child.AdobeProfile.BirthMonth), child.AdobeProfile.BirthDay, 0, 0, 0, 0, time.UTC)
			if birth.Before(first) || birth.After(last) {
				t.Fatal("new profile is outside the configured adult age range")
			}
			if original.AdobeProfile != nil || !reflect.DeepEqual(original.AdobeRegistration, parent.AdobeRegistration) || !reflect.DeepEqual(original.ProxyBinding, parent.ProxyBinding) || original.Error != parent.Error || original.Logs[0] != parent.Logs[0] {
				t.Fatal("fresh attempt modified the old creation evidence")
			}
			if child.Mailbox != parent.Mailbox || child.RootJobID != parent.ID || mail.claim.Load() != parent.ID || child.AutoRetry.Attempts != 3 {
				t.Fatal("fresh attempt changed mailbox ownership or automatic budget")
			}
		})
	}
}

func TestAdobeManualReregistrationSharesMailboxCooldownAndVisibility(t *testing.T) {
	for _, origin := range []string{"task", "proxy"} {
		t.Run(origin, func(t *testing.T) {
			parent := adobeReregisterFixture(t)
			child := cloneJob(parent)
			child.ID, child.ParentJobID, child.RootJobID = "latest", parent.ID, parent.ID
			parent.RetryJobID = child.ID
			due := time.Now().Add(time.Hour).UTC()
			if origin == "task" {
				parent.AutoRetry.NotBeforeAt = due.Format(time.RFC3339Nano)
			} else {
				parent.ProxyBinding = &proxyruntime.Binding{BlockedUntil: due}
			}
			var calls atomic.Int32
			e := NewAdobeEngine(filepath.Join(t.TempDir(), "journal"), "fixture", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				calls.Add(1)
				return nil, nil, fail("test", "stop")
			}, nil, nil)
			seedAdobeCompensation(t, e, parent, child)
			rows := e.Snapshot()["jobs"].([]any)
			if rows[0].(map[string]any)["can_reregister"] != false || rows[1].(map[string]any)["can_reregister"] != true || rows[1].(map[string]any)["reregister_not_before"] != due.Format(time.RFC3339Nano) {
				t.Fatal("latest action or mailbox cooldown was not projected")
			}
			var failure *Failure
			if err := e.Reregister(adobeTestConfig(), child.ID); !errors.As(err, &failure) || failure.Code != "mailbox_registration_cooldown" || failure.RetryAfter != due.Format(time.RFC3339Nano) {
				t.Fatalf("cooldown was not enforced: %v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("cooldown reached Worker")
			}
		})
	}
}

func TestAdobeManualReregistrationChecksImportedAccountAndPersistsBeforeWorker(t *testing.T) {
	for _, scenario := range []string{"account_exists", "lookup_failed", "journal_failed", "source_changed"} {
		t.Run(scenario, func(t *testing.T) {
			job := adobeReregisterFixture(t)
			var calls atomic.Int32
			e := NewAdobeEngine(filepath.Join(t.TempDir(), "journal"), "fixture", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				calls.Add(1)
				return nil, nil, fail("test", "stop")
			}, nil, nil)
			seedAdobeCompensation(t, e, job)
			raw := adobeTestConfig()
			switch scenario {
			case "account_exists", "lookup_failed":
				e.SetReregistrationAccountCheck(func(email string) (bool, error) {
					if email != job.Mailbox.Email {
						t.Error("wrong mailbox checked")
					}
					if scenario == "lookup_failed" {
						return false, errors.New("private store failure")
					}
					return true, nil
				})
			case "journal_failed":
				e.path = t.TempDir()
			case "source_changed":
				e.jobs[0].Mailbox.AccountID = "removed-source"
			}
			if err := e.Reregister(raw, job.ID); err == nil || strings.Contains(err.Error(), "private store") {
				t.Fatalf("guard failed or leaked private error: %v", err)
			}
			if calls.Load() != 0 || len(e.jobs) != 1 || e.jobs[0].RetryJobID != "" {
				t.Fatal("rejected action started work or kept an unpersisted child")
			}
		})
	}
}

func TestAdobeManualReregistrationConcurrentClicksStartOneAttempt(t *testing.T) {
	job := adobeReregisterFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e := NewAdobeEngine(filepath.Join(t.TempDir(), "journal"), "fixture", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		calls.Add(1)
		return &adobeCompensationMail{}, adobeCompensationFlow{run: func(context.Context, AdobeConfig, Progress) (map[string]any, error) {
			close(started)
			<-release
			return nil, fail("adobe", "worker_result_unknown")
		}}, nil
	}, nil, nil)
	seedAdobeCompensation(t, e, job)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.Reregister(adobeTestConfig(), job.ID) == nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("manual task did not start")
	}
	if ran, _ := e.RunCompensation(adobeTestConfig(), time.Now().Add(time.Hour)); ran {
		t.Error("scheduler started concurrent work")
	}
	close(release)
	waitAdobe(t, e)
	if admitted.Load() != 1 || calls.Load() != 1 || len(e.jobs) != 2 {
		t.Fatal("duplicate clicks started multiple registrations")
	}
}
