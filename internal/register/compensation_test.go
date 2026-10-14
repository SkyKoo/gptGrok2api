package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func compensationFixture(now time.Time) RegistrationJob {
	profile := NewBrowserProfile()
	return RegistrationJob{ID: "saved-mailbox", Status: "registration_pending", Stage: "wait_code", Error: "task_timeout", Mailbox: Mailbox{Email: "saved@example.test", AccountID: "acc-test"}, MailboxAPIBase: "https://mail.example.test", BrowserProfile: &profile, AutoRetry: &RegistrationCompensation{State: "waiting", NextAttemptAt: now.UTC().Format(time.RFC3339Nano)}}
}
func writeCompensationJobs(t *testing.T, path string, jobs ...RegistrationJob) {
	t.Helper()
	if err := writeJSON0600(path, jobs); err != nil {
		t.Fatal(err)
	}
}
func mustCompensationJob(t *testing.T, e *FreeEngine, id string) RegistrationJob {
	t.Helper()
	j, err := e.getJob(id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func compensationDue(t *testing.T, j RegistrationJob) time.Time {
	t.Helper()
	if j.AutoRetry == nil {
		t.Fatal("no retry state")
	}
	at, err := time.Parse(time.RFC3339Nano, j.AutoRetry.NextAttemptAt)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

type compensationFlow struct {
	profile    BrowserProfile
	calls      atomic.Int32
	firstError error
}

func (f *compensationFlow) Close()                         {}
func (f *compensationFlow) BrowserProfile() BrowserProfile { return f.profile }
func (f *compensationFlow) Register(ctx context.Context, box Mailbox, cfg FreeConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	if err := progress("wait_code"); err != nil {
		return nil, err
	}
	if f.calls.Add(1) == 1 && f.firstError != nil {
		return nil, f.firstError
	}
	return map[string]any{"access_token": "PRIVATE-TOKEN", "email": box.Email}, nil
}

func TestAutomaticCompensationReusesMailboxProfileAndDurableClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	mail := &fakeMail{}
	flow := &compensationFlow{profile: NewBrowserProfile(), firstError: context.DeadlineExceeded}
	var factories, imports atomic.Int32
	engine := NewFreeEngine(path, func(cfg FreeConfig) (MailSource, RegistrationFlow, error) {
		if factories.Add(1) > 1 {
			if cfg.BrowserProfile == nil || !reflect.DeepEqual(*cfg.BrowserProfile, flow.profile) {
				t.Error("original profile not reused")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
			}
			var jobs []RegistrationJob
			_ = json.Unmarshal(raw, &jobs)
			if len(jobs) != 1 || jobs[0].AutoRetry == nil || jobs[0].AutoRetry.Attempts != 1 || jobs[0].AutoRetry.State != "running" {
				t.Error("network started before durable claim")
			}
		}
		return mail, flow, nil
	}, func(context.Context, string, map[string]any) error { imports.Add(1); return nil }, func(context.Context, string, map[string]any) error { return nil })
	id, err := engine.Start(freeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	waitEngine(t, engine)
	job := mustCompensationJob(t, engine, id)
	due := compensationDue(t, job)
	if job.AutoRetry.State != "waiting" || job.AutoRetry.Attempts != 0 || time.Until(due) < 4*time.Minute || time.Until(due) > 5*time.Minute {
		t.Fatalf("initial cooldown: %+v", job.AutoRetry)
	}
	if started, err := engine.RunCompensation(freeTestConfig(), due.Add(-time.Second)); started || err != nil {
		t.Fatal("ran early", err)
	}
	if started, err := engine.RunCompensation(freeTestConfig(), due); !started || err != nil {
		t.Fatal("did not run when due", err)
	}
	waitEngine(t, engine)
	job = mustCompensationJob(t, engine, id)
	if job.Status != "completed" || !job.Verified || job.AutoRetry.State != "completed" || job.AutoRetry.Attempts != 1 {
		t.Fatalf("bad completion: %+v", job)
	}
	if mail.calls.Load() != 1 || mail.prepared.Load() != 1 || flow.calls.Load() != 2 || imports.Load() != 1 {
		t.Fatal("duplicate mailbox/registration or missing import")
	}
	if started, err := engine.RunCompensation(freeTestConfig(), due.Add(24*time.Hour)); started || err != nil {
		t.Fatal("replayed completed task", err)
	}
	public, _ := json.Marshal(engine.Snapshot())
	for _, private := range []string{"PRIVATE", "browser_profile", "mailbox_api_base", "acc-test"} {
		if strings.Contains(string(public), private) {
			t.Errorf("leaked %s", private)
		}
	}
	if engine.Snapshot()["jobs_total"] != 1 {
		t.Fatal("created new task instead of compensating")
	}
}

func TestAutomaticCompensationBudgetPersistsAcrossRestartsAndManualRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	job := compensationFixture(time.Now())
	writeCompensationJobs(t, path, job)
	var calls atomic.Int32
	factory := func(FreeConfig) (MailSource, RegistrationFlow, error) {
		calls.Add(1)
		return nil, nil, fail("mail", "network_error")
	}
	var engine *FreeEngine
	for attempt := 1; attempt <= 3; attempt++ {
		engine = NewFreeEngine(path, factory, nil, nil)
		before := mustCompensationJob(t, engine, job.ID)
		if before.AutoRetry.Attempts != attempt-1 {
			t.Fatal("restart lost retry budget")
		}
		if started, err := engine.RunCompensation(freeTestConfig(), compensationDue(t, before)); !started || err != nil {
			t.Fatal(started, err)
		}
		waitEngine(t, engine)
		after := mustCompensationJob(t, engine, job.ID)
		if after.AutoRetry.Attempts != attempt {
			t.Fatal("attempt not persisted")
		}
		if attempt < 3 {
			delay := time.Until(compensationDue(t, after))
			if delay < compensationDelays[attempt]-time.Minute || delay > compensationDelays[attempt] {
				t.Fatalf("incorrect backoff: %v", delay)
			}
		} else if after.AutoRetry.State != "exhausted" || after.AutoRetry.NextAttemptAt != "" {
			t.Fatal("did not exhaust")
		}
	}
	engine = NewFreeEngine(path, factory, nil, nil)
	if started, err := engine.RunCompensation(freeTestConfig(), time.Now().Add(7*24*time.Hour)); started || err != nil {
		t.Fatal("restarted exhausted retry", err)
	}
	if calls.Load() != 3 {
		t.Fatal("exceeded automatic retry limit")
	}
	if err := engine.RetryRegistration(freeTestConfig(), job.ID); err != nil {
		t.Fatal(err)
	}
	waitEngine(t, engine)
	after := mustCompensationJob(t, engine, job.ID)
	if calls.Load() != 4 || after.AutoRetry.Attempts != 3 || after.AutoRetry.State != "exhausted" {
		t.Fatal("manual retry reset budget")
	}
	if removed, err := engine.ClearFinished(); err != nil || removed != 0 {
		t.Fatal("cleanup removed saved mailbox", err)
	}
}

func TestCompensationFailureClassificationAndRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"timeout", context.DeadlineExceeded, true},
		{"network", fail("authorize", "network_error"), true},
		{"read", fail("session", "read_error"), true},
		{"429", &Failure{Stage: "send_code", Code: "rate_limited", HTTPStatus: 429}, true},
		{"502", &Failure{Stage: "mail", Code: "upstream_error", HTTPStatus: 502}, true},
		{"408", &Failure{Stage: "authorize", Code: "upstream_rejected", HTTPStatus: 408}, true},
		{"bad request", &Failure{Stage: "submit_email", Code: "upstream_rejected", HTTPStatus: 400}, false},
		{"auth", &Failure{Stage: "mail", Code: "upstream_error", HTTPStatus: 401}, false},
		{"forbidden", &Failure{Stage: "submit_email", Code: "upstream_rejected", HTTPStatus: 403}, false},
		{"challenge503", &Failure{Stage: "authorize", Code: "verification_required", HTTPStatus: 503}, false},
		{"cancel", context.Canceled, false},
		{"unknown", errors.New("private response"), false},
		{"config", fail("config", "invalid_proxy"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := compensationFixture(now)
			j.Error = safeFailure(tc.err)
			planRegistrationCompensation(&j, tc.err, now)
			if (j.AutoRetry.State == "waiting") != tc.retry {
				t.Fatalf("bad classification: %+v", j.AutoRetry)
			}
		})
	}
	for _, retryAfter := range []string{"7200", now.Add(2 * time.Hour).Format(http.TimeFormat)} {
		j := compensationFixture(now)
		err := &Failure{Stage: "authorize", Code: "rate_limited", HTTPStatus: 429, RetryAfter: retryAfter}
		j.Error = safeFailure(err)
		planRegistrationCompensation(&j, err, now)
		if got := compensationDue(t, j); !got.Equal(now.Add(2 * time.Hour)) {
			t.Fatal("ignored upstream cooldown", got)
		}
	}
}

func TestCompensationLegacyAdoptionAndEligibility(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "jobs.json")
	legacy := compensationFixture(now)
	legacy.AutoRetry = nil
	unknown := cloneJob(legacy)
	unknown.ID = "unknown"
	unknown.Error = "internal_error"
	missing := cloneJob(legacy)
	missing.ID = "missing"
	missing.BrowserProfile = nil
	saved := cloneJob(legacy)
	saved.ID = "saved-result"
	saved.Account = map[string]any{"access_token": "PRIVATE"}
	imported := cloneJob(legacy)
	imported.ID = "imported"
	imported.Imported = true
	verified := cloneJob(legacy)
	verified.ID = "verified"
	verified.Verified = true
	noMailbox := cloneJob(legacy)
	noMailbox.ID = "no-mailbox"
	noMailbox.Mailbox = Mailbox{}
	writeCompensationJobs(t, path, legacy, unknown, missing, saved, imported, verified, noMailbox)
	engine := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) {
		t.Error("should not reach network")
		return nil, nil, errors.New("unexpected")
	}, nil, nil)
	if started, err := engine.RunCompensation(freeTestConfig(), now); started || err != nil {
		t.Fatal(started, err)
	}
	after := mustCompensationJob(t, engine, legacy.ID)
	if !compensationDue(t, after).Equal(now.Add(5*time.Minute)) || after.AutoRetry.Attempts != 0 {
		t.Fatal("legacy cooldown missing")
	}
	if _, err := engine.RunCompensation(freeTestConfig(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, mustCompensationJob(t, engine, legacy.ID)) {
		t.Fatal("ticker keeps pushing deadline")
	}
	if got := mustCompensationJob(t, engine, missing.ID); got.AutoRetry.State != "manual" || got.AutoRetry.Reason != "saved_environment_missing" {
		t.Fatal("missing environment not held")
	}
	for _, want := range []RegistrationJob{unknown, saved, imported, verified, noMailbox} {
		if !reflect.DeepEqual(want, mustCompensationJob(t, engine, want.ID)) {
			t.Fatal("altered ineligible history", want.ID)
		}
	}
}

func TestCompensationDefersConfigMismatchAndDoesNotSpendAttempts(t *testing.T) {
	for _, kind := range []string{"invalid-config", "different-account", "different-server"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			j := compensationFixture(time.Now())
			writeCompensationJobs(t, path, j)
			var calls atomic.Int32
			e := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) {
				calls.Add(1)
				return nil, nil, fail("test", "fixture")
			}, nil, nil)
			cfg := freeTestConfig()
			provider := cfg["mail"].(map[string]any)["providers"].([]any)[0].(map[string]any)
			switch kind {
			case "invalid-config":
				cfg["total"] = 0
			case "different-account":
				provider["account_id"] = "other"
			case "different-server":
				provider["api_base"] = "https://other.example.test"
			}
			if started, _ := e.RunCompensation(cfg, time.Now().Add(time.Hour)); started {
				t.Fatal("mismatched config launched")
			}
			if got := mustCompensationJob(t, e, j.ID); !reflect.DeepEqual(got, j) || calls.Load() != 0 {
				t.Fatal("mismatch consumed budget or changed history")
			}
			if e.Snapshot()["compensation_error"] == "" {
				t.Fatal("missing defer reason")
			}
			if started, err := e.RunCompensation(freeTestConfig(), time.Now().Add(time.Hour)); !started || err != nil {
				t.Fatal("did not resume after configuration restored", err)
			}
			waitEngine(t, e)
		})
	}
}

func TestCompensationDuplicateSavedAccountAndPersistenceFailure(t *testing.T) {
	for _, kind := range []string{"duplicate", "storage"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			j := compensationFixture(time.Now())
			jobs := []RegistrationJob{j}
			if kind == "duplicate" {
				other := cloneJob(j)
				other.ID = "other"
				other.Mailbox.Email = strings.ToUpper(j.Mailbox.Email)
				other.Account = map[string]any{"access_token": "PRIVATE"}
				jobs = append(jobs, other)
			}
			writeCompensationJobs(t, path, jobs...)
			e := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) {
				t.Error("unsafe network launch")
				return nil, nil, fail("test", "fixture")
			}, nil, nil)
			if kind == "storage" {
				blocker := filepath.Join(t.TempDir(), "not-directory")
				if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
					t.Fatal(err)
				}
				e.path = filepath.Join(blocker, "jobs.json")
			}
			if started, err := e.RunCompensation(freeTestConfig(), time.Now()); started || err == nil {
				t.Fatal("unsafe claim succeeded")
			}
			after := mustCompensationJob(t, e, j.ID)
			if after.AutoRetry.Attempts != 0 {
				t.Fatal("failed claim consumed budget")
			}
			if kind == "duplicate" && (after.AutoRetry.State != "manual" || after.AutoRetry.Reason != "saved_account_exists") {
				t.Fatal("did not hold duplicate mailbox")
			}
			if kind == "storage" && !reflect.DeepEqual(after, j) {
				t.Fatal("failed save mutated memory")
			}
		})
	}
}

func TestCompensationManualAndAutomaticClaimsSerialize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	j := compensationFixture(time.Now())
	writeCompensationJobs(t, path, j)
	mail := &fakeMail{}
	flow := &fakeFlow{block: true, started: make(chan struct{})}
	e := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) { return mail, flow, nil }, nil, nil)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
	gate := make(chan struct{})
	var wg sync.WaitGroup
	var launched atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			if i%2 == 0 {
				started, _ := e.RunCompensation(freeTestConfig(), time.Now())
				if started {
					launched.Add(1)
				}
			} else if e.RetryRegistration(freeTestConfig(), j.ID) == nil {
				launched.Add(1)
			}
		}(i)
	}
	close(gate)
	wg.Wait()
	select {
	case <-flow.started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	if launched.Load() != 1 || mail.calls.Load() != 0 || mail.prepared.Load() != 1 {
		t.Fatal("multiple claims or new alias")
	}
	if _, err := e.Start(freeTestConfig()); err == nil {
		t.Fatal("started batch during compensation")
	}
	e.Stop()
	waitEngine(t, e)
	after := mustCompensationJob(t, e, j.ID)
	if after.AutoRetry.State != "manual" || after.AutoRetry.Reason != "cancelled" {
		t.Fatal("manual stop scheduled another retry")
	}
	if started, err := e.RunCompensation(freeTestConfig(), time.Now().Add(24*time.Hour)); started || err != nil {
		t.Fatal("restarted manually stopped job", err)
	}
}

func TestCompensationCrashRecoveryAndShutdown(t *testing.T) {
	now := time.Now()
	reserved := now.Add(45 * time.Minute)
	for _, attempts := range []int{-1, 1, 2, 3} {
		t.Run(fmt.Sprint(attempts), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			j := compensationFixture(now)
			j.Status = "running"
			j.AutoRetry = &RegistrationCompensation{State: "running", Attempts: attempts, NextAttemptAt: reserved.UTC().Format(time.RFC3339Nano)}
			writeCompensationJobs(t, path, j)
			e := NewFreeEngine(path, nil, nil, nil)
			after := mustCompensationJob(t, e, j.ID)
			if after.AutoRetry.Attempts != attempts {
				t.Fatal("restart reset attempts")
			}
			switch {
			case attempts < 0:
				if after.AutoRetry.State != "manual" {
					t.Fatal("invalid budget not held")
				}
			case attempts >= 3:
				if after.AutoRetry.State != "exhausted" {
					t.Fatal("exhaustion not restored")
				}
			default:
				if after.AutoRetry.State != "waiting" || !compensationDue(t, after).Equal(reserved) {
					t.Fatal("reservation lost")
				}
				if started, err := e.RunCompensation(freeTestConfig(), now); started || err != nil {
					t.Fatal("crash triggered immediate replay", err)
				}
				reopened := NewFreeEngine(path, nil, nil, nil)
				if !reflect.DeepEqual(after, mustCompensationJob(t, reopened, j.ID)) {
					t.Fatal("repeated restart postponed or reset retry")
				}
			}
		})
	}
	path := filepath.Join(t.TempDir(), "jobs.json")
	j := compensationFixture(now)
	writeCompensationJobs(t, path, j)
	flow := &fakeFlow{block: true, started: make(chan struct{})}
	e := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) { return &fakeMail{}, flow, nil }, nil, nil)
	if started, err := e.RunCompensation(freeTestConfig(), now); !started || err != nil {
		t.Fatal(err)
	}
	select {
	case <-flow.started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	claimed := mustCompensationJob(t, e, j.ID)
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened := NewFreeEngine(path, nil, nil, nil)
	after := mustCompensationJob(t, reopened, j.ID)
	if after.AutoRetry.State != "waiting" || after.AutoRetry.Attempts != 1 || after.AutoRetry.NextAttemptAt != claimed.AutoRetry.NextAttemptAt {
		t.Fatal("graceful restart lost retry reservation")
	}
}

func TestCompensationStopsRegisteringOnceCredentialsAreSaved(t *testing.T) {
	for _, stage := range []string{"import", "verify"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			j := compensationFixture(time.Now())
			writeCompensationJobs(t, path, j)
			flow := &fakeFlow{}
			e := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) { return &fakeMail{}, flow, nil }, func(context.Context, string, map[string]any) error {
				if stage == "import" {
					return context.DeadlineExceeded
				}
				return nil
			}, func(context.Context, string, map[string]any) error { return context.DeadlineExceeded })
			if started, err := e.RunCompensation(freeTestConfig(), time.Now()); !started || err != nil {
				t.Fatal(err)
			}
			waitEngine(t, e)
			after := mustCompensationJob(t, e, j.ID)
			if len(after.Account) == 0 || after.AutoRetry.State != "saved_result" {
				t.Fatal("lost recovery credentials")
			}
			if started, err := e.RunCompensation(freeTestConfig(), time.Now().Add(24*time.Hour)); started || err != nil {
				t.Fatal("repeated signup after saving credentials", err)
			}
			if err := e.RetryRegistration(freeTestConfig(), j.ID); err == nil || flow.calls.Load() != 1 {
				t.Fatal("manual retry repeated signup")
			}
		})
	}
}
