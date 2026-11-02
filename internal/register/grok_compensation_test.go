package register

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type grokCompensationFlow struct {
	profile BrowserProfile
	calls   atomic.Int32
	run     func(context.Context) error
}

func (f *grokCompensationFlow) Close()                         {}
func (f *grokCompensationFlow) BrowserProfile() BrowserProfile { return f.profile }
func (f *grokCompensationFlow) Register(ctx context.Context, box Mailbox, cfg GrokConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	f.calls.Add(1)
	if err := progress("wait_code"); err != nil {
		return nil, err
	}
	if f.run != nil {
		if err := f.run(ctx); err != nil {
			return nil, err
		}
	}
	return map[string]any{"sso": "PRIVATE-SSO"}, nil
}
func grokCompensationFixture(now time.Time) RegistrationJob {
	j := compensationFixture(now)
	j.Mailbox.AccountID = "hme-account"
	return j
}
func waitGrokCompensation(t *testing.T, e *GrokEngine) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for e.Snapshot()["running"] == true && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if e.Snapshot()["running"] == true {
		t.Fatal("Grok did not finish")
	}
}
func grokLatest(t *testing.T, e *GrokEngine, id string) RegistrationJob {
	t.Helper()
	j, err := latestAttempt(e.getJob, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestGrokFailurePlansAutomaticCompensation(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		state, reason string
	}{
		{"timeout", context.DeadlineExceeded, "waiting", ""},
		{"network", fail("send_code", "network_error"), "waiting", ""},
		{"rate_limit", &Failure{Stage: "send_code", Code: "browser_worker_rejected", HTTPStatus: 429, RetryAfter: "3600"}, "waiting", ""},
		{"cancel", context.Canceled, "manual", "cancelled"},
		{"refused", &Failure{Stage: "send_code", Code: "browser_worker_rejected", HTTPStatus: 504, Operation: "worker_error=signup_unavailable"}, "manual", "upstream_signup_refused"},
		{"challenge", &Failure{Stage: "signup", Code: "turnstile_incomplete", HTTPStatus: 504}, "manual", "human_verification_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			j := grokCompensationFixture(time.Now())
			j.AutoRetry = nil
			writeCompensationJobs(t, path, j)
			e := NewGrokEngine(path, nil, nil, nil)
			e.failJob(j.ID, tc.err)
			got := grokLatest(t, e, j.ID)
			if got.AutoRetry == nil || got.AutoRetry.State != tc.state || got.AutoRetry.Reason != tc.reason {
				t.Fatalf("retry: %+v", got.AutoRetry)
			}
			if tc.state == "waiting" {
				delay := time.Until(compensationDue(t, got))
				want := 5 * time.Minute
				if tc.name == "rate_limit" {
					want = time.Hour
				}
				if delay < want-time.Second || delay > want {
					t.Fatalf("wrong delay: %v", delay)
				}
			}
		})
	}
}

func TestGrokAutomaticCompensationCreatesDurableChildAndReusesMailbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	parent := grokCompensationFixture(time.Now())
	routing := resinRoutingFixture()
	lease, err := routing.Manager.AcquireTask(context.Background(), "group:shared", "grok", "registration", nil, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent.ProxyBinding = &lease.Binding
	lease.Release()
	writeCompensationJobs(t, path, parent)
	mail := &attemptMailbox{}
	flow := &grokCompensationFlow{profile: *parent.BrowserProfile}
	e := NewGrokEngine(path, func(cfg GrokConfig) (MailSource, GrokRegistrationFlow, error) {
		if cfg.BrowserProfile == nil || !reflect.DeepEqual(*cfg.BrowserProfile, *parent.BrowserProfile) {
			t.Error("saved browser profile lost")
		}
		data, err := os.ReadFile(path)
		var jobs []RegistrationJob
		if err != nil || json.Unmarshal(data, &jobs) != nil || len(jobs) != 2 {
			t.Error("child not durable before network")
			return nil, nil, fail("test", "journal")
		}
		child := jobs[1]
		if jobs[0].RetryJobID != child.ID || child.ParentJobID != parent.ID || child.AutoRetry.Attempts != 1 || child.AutoRetry.State != "running" || child.ProxyBinding == nil || child.ProxyBinding.ResinAccount == parent.ProxyBinding.ResinAccount {
			t.Error("claim, budget or new proxy identity missing")
		}
		return mail, flow, nil
	}, func(context.Context, string, map[string]any) error { return nil }, func(context.Context, string, map[string]any) error { return nil })
	e.SetProxyRouting(routing)
	cfg := grokConfigFixture()
	cfg["proxy"] = "group:shared"
	if started, err := e.RunCompensation(cfg, compensationDue(t, parent).Add(-time.Second)); started || err != nil {
		t.Fatal("early attempt", err)
	}
	if started, err := e.RunCompensation(cfg, compensationDue(t, parent)); !started || err != nil {
		t.Fatal("due attempt", err)
	}
	waitGrokCompensation(t, e)
	child := grokLatest(t, e, parent.ID)
	if child.Status != "completed" || child.AutoRetry.State != "completed" || child.AutoRetry.Attempts != 1 || child.Mailbox != parent.Mailbox || flow.calls.Load() != 1 || mail.prepared.Load() != 1 || mail.claimJob.Load() != parent.ID || mail.committedAccount.Load() != child.ID {
		t.Fatalf("bad child result: %+v", child)
	}
	if started, err := e.RunCompensation(cfg, time.Now().Add(24*time.Hour)); started || err != nil {
		t.Fatal("replayed success", err)
	}
}

func TestGrokCompensationBudgetSurvivesRestartAndManualRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	parent := grokCompensationFixture(time.Now())
	writeCompensationJobs(t, path, parent)
	var calls atomic.Int32
	factory := func(GrokConfig) (MailSource, GrokRegistrationFlow, error) {
		calls.Add(1)
		return nil, nil, fail("mail", "network_error")
	}
	var e *GrokEngine
	for n := 1; n <= 3; n++ {
		e = NewGrokEngine(path, factory, nil, nil)
		j := grokLatest(t, e, parent.ID)
		if started, err := e.RunCompensation(grokConfigFixture(), compensationDue(t, j)); !started || err != nil {
			t.Fatal("not started", err)
		}
		waitGrokCompensation(t, e)
		j = grokLatest(t, e, parent.ID)
		if j.AutoRetry.Attempts != n {
			t.Fatal("budget reset", j.AutoRetry)
		}
		if n < 3 {
			delay := time.Until(compensationDue(t, j))
			if delay < compensationDelays[n]-time.Second || delay > compensationDelays[n] {
				t.Fatal("wrong backoff", delay)
			}
		}
	}
	j := grokLatest(t, e, parent.ID)
	if j.AutoRetry.State != "exhausted" {
		t.Fatal("not exhausted", j.AutoRetry)
	}
	if started, err := e.RunCompensation(grokConfigFixture(), time.Now().Add(48*time.Hour)); started || err != nil {
		t.Fatal("budget exceeded", err)
	}
	if err := e.RetryRegistration(grokConfigFixture(), j.ID); err != nil {
		t.Fatal(err)
	}
	waitGrokCompensation(t, e)
	j = grokLatest(t, e, parent.ID)
	if j.AutoRetry == nil || j.AutoRetry.Attempts != 3 || j.AutoRetry.State != "exhausted" || calls.Load() != 4 {
		t.Fatal("manual retry reset budget", j.AutoRetry)
	}
}

func TestGrokLegacyCompensationGetsFullCooldownAndPreservesManualDecisions(t *testing.T) {
	now := time.Now()
	for _, reason := range []string{"grok_manual_retry", "cancelled", "upstream_signup_refused"} {
		t.Run(reason, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			j := grokCompensationFixture(now)
			j.AutoRetry = &RegistrationCompensation{State: "manual", Reason: reason, Attempts: 1}
			writeCompensationJobs(t, path, j)
			e := NewGrokEngine(path, nil, nil, nil)
			if started, err := e.RunCompensation(grokConfigFixture(), now); started || err != nil {
				t.Fatal("legacy replayed immediately", err)
			}
			got := grokLatest(t, e, j.ID)
			if reason == "grok_manual_retry" {
				if got.AutoRetry.State != "waiting" || !compensationDue(t, got).Equal(now.Add(30*time.Minute)) {
					t.Fatal("not safely adopted", got.AutoRetry)
				}
			} else if !reflect.DeepEqual(got.AutoRetry, j.AutoRetry) {
				t.Fatal("manual decision changed")
			}
		})
	}
}

func TestGrokCompensationRecoveryAndSavedAccount(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "jobs.json")
	j := grokCompensationFixture(now.Add(time.Hour))
	j.Status = "running"
	j.AutoRetry.State = "running"
	j.AutoRetry.Attempts = 1
	writeCompensationJobs(t, path, j)
	e := NewGrokEngine(path, nil, nil, nil)
	got := grokLatest(t, e, j.ID)
	if got.AutoRetry.State != "waiting" || got.AutoRetry.Attempts != 1 || got.AutoRetry.NextAttemptAt != j.AutoRetry.NextAttemptAt {
		t.Fatal("crash reset claim", got.AutoRetry)
	}
	if started, err := e.RunCompensation(grokConfigFixture(), now); started || err != nil {
		t.Fatal("replayed in-flight request", err)
	}
	j.Account = map[string]any{"sso": "saved"}
	writeCompensationJobs(t, path, j)
	e = NewGrokEngine(path, nil, nil, nil)
	if started, err := e.RunCompensation(grokConfigFixture(), now.Add(48*time.Hour)); started || err != nil {
		t.Fatal("re-registered saved account", err)
	}
	if got = grokLatest(t, e, j.ID); got.Status != "import_pending" || got.AutoRetry.State != "saved_result" {
		t.Fatal("lost saved result")
	}
}

func TestGrokCompensationStopAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "shutdown"}[shutdown], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			parent := grokCompensationFixture(time.Now())
			writeCompensationJobs(t, path, parent)
			entered := make(chan struct{})
			flow := &grokCompensationFlow{profile: *parent.BrowserProfile, run: func(ctx context.Context) error { close(entered); <-ctx.Done(); return fail("signup", "read_error") }}
			e := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) { return &grokSavedMailStub{}, flow, nil }, nil, nil)
			if started, err := e.RunCompensation(grokConfigFixture(), compensationDue(t, parent)); !started || err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("not running")
			}
			claimed := grokLatest(t, e, parent.ID)
			rows := e.Snapshot()["jobs"].([]any)
			if rows[1].(map[string]any)["auto_retry"].(map[string]any)["state"] != "running" {
				t.Fatal("running state hidden")
			}
			if started, err := e.RunCompensation(grokConfigFixture(), time.Now().Add(time.Hour)); started || err == nil {
				t.Fatal("overlapping retry allowed")
			}
			if err := e.RetryRegistration(grokConfigFixture(), parent.ID); err == nil {
				t.Fatal("manual retry overlapped")
			}
			if shutdown {
				if err := e.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				e.Stop()
			}
			waitGrokCompensation(t, e)
			reopened := NewGrokEngine(path, nil, nil, nil)
			got := grokLatest(t, reopened, parent.ID)
			if got.AutoRetry.Attempts != 1 {
				t.Fatal("budget changed")
			}
			if shutdown {
				if got.AutoRetry.State != "waiting" || got.AutoRetry.NextAttemptAt != claimed.AutoRetry.NextAttemptAt {
					t.Fatal("shutdown lost reservation", got.AutoRetry)
				}
			} else if got.AutoRetry.State != "manual" || got.AutoRetry.Reason != "cancelled" {
				t.Fatal("stop scheduled another attempt", got.AutoRetry)
			}
		})
	}
}

func TestGrokCompensationRejectsUnsafeClaims(t *testing.T) {
	for _, mode := range []string{"source", "duplicate", "persist", "legacy_persist", "cooldown"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			path := filepath.Join(t.TempDir(), "jobs.json")
			parent := grokCompensationFixture(now)
			jobs := []RegistrationJob{parent}
			if mode == "source" {
				jobs[0].Mailbox.AccountID = "other-account"
			}
			if mode == "duplicate" {
				other := cloneJob(parent)
				other.ID = "saved-account"
				other.Account = map[string]any{"sso": "saved"}
				jobs = append(jobs, other)
			}
			if mode == "legacy_persist" {
				jobs[0].AutoRetry = &RegistrationCompensation{State: "manual", Reason: "grok_manual_retry"}
			}
			if mode == "cooldown" {
				routing := resinRoutingFixture()
				lease, err := routing.Manager.AcquireTask(context.Background(), "group:shared", "grok", "registration", nil, parent.ID)
				if err != nil {
					t.Fatal(err)
				}
				jobs[0].ProxyBinding = &lease.Binding
				jobs[0].ProxyBinding.BlockedUntil = now.Add(time.Hour)
				lease.Release()
			}
			writeCompensationJobs(t, path, jobs...)
			var calls atomic.Int32
			e := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) {
				calls.Add(1)
				return nil, nil, fail("test", "unexpected")
			}, nil, nil)
			if mode == "persist" || mode == "legacy_persist" {
				e.path = t.TempDir()
			}
			started, err := e.RunCompensation(grokConfigFixture(), now)
			if started || calls.Load() != 0 {
				t.Fatal("unsafe network start")
			}
			got := grokLatest(t, e, parent.ID)
			if got.AutoRetry.Attempts != 0 || got.RetryJobID != "" || e.Snapshot()["jobs_total"] != len(jobs) {
				t.Fatal("failed claim spent attempt")
			}
			switch mode {
			case "source":
				if err != nil || e.Snapshot()["compensation_error"] != "config: saved_mailbox_source_unavailable" {
					t.Fatal("source mismatch not deferred")
				}
			case "duplicate":
				if err == nil || got.AutoRetry.State != "manual" || got.AutoRetry.Reason != "saved_account_exists" {
					t.Fatal("duplicate not held")
				}
			case "persist", "legacy_persist":
				if err == nil || !reflect.DeepEqual(got, jobs[0]) {
					t.Fatal("failed save changed state")
				}
			case "cooldown":
				if err != nil || got.AutoRetry.State != "waiting" {
					t.Fatal("cooldown lost retry")
				}
			}
		})
	}
}

func TestGrokCompensationSavedResultStopsSignupAndCanResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	parent := grokCompensationFixture(time.Now())
	writeCompensationJobs(t, path, parent)
	flow := &grokCompensationFlow{profile: *parent.BrowserProfile}
	e := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) { return &grokSavedMailStub{}, flow, nil }, func(context.Context, string, map[string]any) error { return nil }, func(context.Context, string, map[string]any) error { return fail("verify", "network_error") })
	if started, err := e.RunCompensation(grokConfigFixture(), compensationDue(t, parent)); !started || err != nil {
		t.Fatal(err)
	}
	waitGrokCompensation(t, e)
	got := grokLatest(t, e, parent.ID)
	if got.Status != "verification_pending" || got.AutoRetry.State != "saved_result" || len(got.Account) == 0 {
		t.Fatal("saved SSO lost")
	}
	if started, err := e.RunCompensation(grokConfigFixture(), time.Now().Add(24*time.Hour)); started || err != nil {
		t.Fatal("re-registered saved result", err)
	}
	e.verifyAccount = func(context.Context, string, map[string]any) error { return nil }
	if err := e.RetrySavedResult(got.ID); err != nil {
		t.Fatal(err)
	}
	waitGrokCompensation(t, e)
	got = grokLatest(t, e, parent.ID)
	if got.Status != "completed" || got.AutoRetry.State != "completed" || flow.calls.Load() != 1 {
		t.Fatal("resume repeated signup")
	}
}

func TestGrokFinalSignupUncertaintyRequiresReview(t *testing.T) {
	for _, mode := range []string{"failure", "legacy", "crash"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			path := filepath.Join(t.TempDir(), "jobs.json")
			j := grokCompensationFixture(now)
			j.Stage = "signup"
			if mode == "legacy" {
				j.AutoRetry = &RegistrationCompensation{State: "manual", Reason: "grok_manual_retry"}
			}
			if mode == "crash" {
				j.Status = "running"
				j.AutoRetry.State = "running"
				j.AutoRetry.Attempts = 1
			}
			writeCompensationJobs(t, path, j)
			var calls atomic.Int32
			e := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) {
				calls.Add(1)
				return nil, nil, fail("test", "unexpected")
			}, nil, nil)
			if mode == "failure" {
				e.failJob(j.ID, context.DeadlineExceeded)
			}
			if started, err := e.RunCompensation(grokConfigFixture(), now.Add(48*time.Hour)); started || err != nil {
				t.Fatal("unknown signup replayed", err)
			}
			got := grokLatest(t, e, j.ID)
			if got.AutoRetry.State != "manual" || got.AutoRetry.NextAttemptAt != "" || got.AutoRetry.Reason != "registration_result_uncertain" || calls.Load() != 0 {
				t.Fatal("uncertain result not held", got.AutoRetry)
			}
		})
	}
}
