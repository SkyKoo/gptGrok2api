package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

type adobeCompensationMail struct {
	prepared, acquired, committed atomic.Int32
	claim                         atomic.Value
}

func (m *adobeCompensationMail) Acquire(context.Context, string) (Mailbox, error) {
	m.acquired.Add(1)
	return Mailbox{}, fail("mail", "unexpected_acquire")
}
func (m *adobeCompensationMail) PrepareFor(_ context.Context, id string, _ Mailbox) error {
	m.prepared.Add(1)
	m.claim.Store(id)
	return nil
}
func (m *adobeCompensationMail) WaitCode(context.Context, Mailbox, time.Time) (string, error) {
	return "", fail("mail", "unexpected_wait")
}
func (m *adobeCompensationMail) Commit(context.Context, string, Mailbox) error {
	m.committed.Add(1)
	return nil
}
func (*adobeCompensationMail) Close() {}

type adobeCompensationFlow struct {
	run func(context.Context, AdobeConfig, Progress) (map[string]any, error)
}

func (f adobeCompensationFlow) Register(ctx context.Context, _ string, _ Mailbox, cfg AdobeConfig, _ func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	return f.run(ctx, cfg, progress)
}
func (adobeCompensationFlow) Close() {}

func adobeCompensationFixture(t *testing.T, now time.Time) RegistrationJob {
	t.Helper()
	cfg, err := ParseAdobeConfig(adobeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	return RegistrationJob{
		ID: "original-adobe-profile", Status: "registration_pending", Stage: "adobe_creation_submitted", Error: "adobe: submission_result_unknown",
		CreatedAt: now.Add(-time.Hour).UTC().Format(time.RFC3339Nano), UpdatedAt: now.Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		Mailbox: Mailbox{ID: "saved-asset", Email: "existing@example.test", AccountID: cfg.HME.AccountID, SourceID: cfg.MailboxSourceID}, MailboxAPIBase: cfg.HME.BaseURL,
		AdobeProfile:      &adobeRegistrationProfile{Country: "US", BirthYear: 1988, BirthMonth: 10, BirthDay: 2},
		AdobeRegistration: &AdobeRegistrationState{BrowserProfileID: "original-adobe-profile", Proxy: "direct", ProxyRecorded: true, FlowStarted: true, SubmissionStarted: true, CreationOutcome: "accepted"},
		AutoRetry:         &RegistrationCompensation{State: "waiting", NextAttemptAt: now.Add(-time.Second).UTC().Format(time.RFC3339Nano)},
	}
}

func seedAdobeCompensation(t *testing.T, e *AdobeEngine, jobs ...RegistrationJob) {
	t.Helper()
	e.jobs = jobs
	if err := e.saveLocked(); err != nil {
		t.Fatal(err)
	}
}

func latestAdobe(t *testing.T, e *AdobeEngine, root string) RegistrationJob {
	t.Helper()
	job, err := latestAttempt(e.getJob, root)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestAdobeCompensationPlansBoundedResumeAndSavedResultRecovery(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name   string
		cause  error
		state  string
		reason string
	}{
		{"unknown", fail("adobe", "submission_result_unknown"), "waiting", ""},
		{"verification", fail("adobe", "human_verification_required"), "waiting", ""},
		{"timeout", context.DeadlineExceeded, "waiting", ""},
		{"cancel", context.Canceled, "manual", "cancelled"},
		{"invalid_identity", fail("adobe", "identity_not_verified"), "manual", "failure_requires_review"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := adobeCompensationFixture(t, now)
			job.AutoRetry, job.Error = nil, safeFailure(test.cause)
			planAdobeRegistrationCompensation(&job, test.cause, now)
			if job.AutoRetry.State != test.state || job.AutoRetry.Reason != test.reason || adobeRegistrationRecoveryMode(job) != "resume_session" {
				t.Fatalf("unexpected decision: %+v", job.AutoRetry)
			}
			if test.state == "waiting" && job.AutoRetry.NextAttemptAt != now.Add(5*time.Minute).Format(time.RFC3339Nano) {
				t.Fatal("first recovery did not wait five minutes")
			}
		})
	}
	for attempt, delay := range compensationDelays {
		job := adobeCompensationFixture(t, now)
		job.AutoRetry.Attempts = attempt
		planAdobeRegistrationCompensation(&job, nil, now)
		if job.AutoRetry.NextAttemptAt != now.Add(delay).Format(time.RFC3339Nano) {
			t.Fatalf("attempt %d has wrong delay: %+v", attempt, job.AutoRetry)
		}
	}
	job := adobeCompensationFixture(t, now)
	job.Error = (&Failure{Stage: "adobe", Code: "network_error", HTTPStatus: 429, RetryAfter: "14400"}).Error()
	planAdobeRegistrationCompensation(&job, nil, now)
	if job.AutoRetry.NextAttemptAt != now.Add(4*time.Hour).Format(time.RFC3339Nano) {
		t.Fatal("ignored longer Retry-After")
	}
}

func TestAdobeAutomaticRecoveryKeepsOriginalProfileEgressAndClaim(t *testing.T) {
	now := time.Now()
	job := adobeCompensationFixture(t, now)
	routing := routingFixture()
	lease, err := routing.Manager.AcquireTask(context.Background(), "group:shared", "adobe", "registration", nil, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.ProxyBinding = &lease.Binding
	job.AdobeRegistration.Proxy = lease.URL
	lease.Release()
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	mail := &adobeCompensationMail{}
	var calls atomic.Int32
	var e *AdobeEngine
	e = NewAdobeEngine(path, "key", func(cfg AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		raw, readErr := os.ReadFile(path)
		var envelope struct {
			Data []byte `json:"data"`
		}
		var persisted []RegistrationJob
		if readErr != nil || json.Unmarshal(raw, &envelope) != nil {
			t.Error("cannot read persisted child")
			return nil, nil, fail("test", "journal_missing")
		}
		plain, openErr := adobe.Open("key", envelope.Data)
		if openErr != nil || json.Unmarshal(plain, &persisted) != nil || len(persisted) != 2 {
			t.Error("cannot decode persisted child")
			return nil, nil, fail("test", "journal_missing")
		}
		child := persisted[1]
		if child.ParentJobID != job.ID || child.AutoRetry.Attempts != 1 || cfg.BrowserProfileID != job.ID || cfg.RecoveryMode != "resume_session" || cfg.Proxy != job.AdobeRegistration.Proxy || child.ProxyBinding.NodeID != job.ProxyBinding.NodeID {
			t.Error("child claim/profile/binding was not durable before network")
		}
		return mail, adobeCompensationFlow{run: func(_ context.Context, actual AdobeConfig, progress Progress) (map[string]any, error) {
			calls.Add(1)
			if actual.RecoveryMode != "resume_session" || !reflect.DeepEqual(actual.Profile, job.AdobeProfile.fields()) {
				t.Fatal("unknown registration would be submitted again")
			}
			if err := progress("adobe_recovery_started"); err != nil {
				return nil, err
			}
			return map[string]any{"email": job.Mailbox.Email, "account_id": "adobe-identity", "token": "PRIVATE"}, nil
		}}, nil
	}, func(_ context.Context, id string, account map[string]any) error {
		if id != job.ID || account["registration_job_id"] != job.ID {
			t.Error("import changed mailbox claim owner")
		}
		return nil
	}, func(context.Context, string, map[string]any) error { return nil })
	e.SetProxyRouting(routing)
	seedAdobeCompensation(t, e, job)
	cfg := adobeTestConfig()
	cfg["proxy"] = "http://different.invalid:9999"
	if started, err := e.RunCompensation(cfg, now); !started || err != nil {
		t.Fatalf("not started: %v", err)
	}
	waitAdobe(t, e)
	child := latestAdobe(t, e, job.ID)
	if child.Status != "completed" || child.AutoRetry.Attempts != 1 || mail.acquired.Load() != 0 || mail.prepared.Load() != 1 || mail.claim.Load() != job.ID || calls.Load() != 1 {
		t.Fatalf("wrong recovery: %+v", child)
	}
	if started, err := e.RunCompensation(cfg, now.Add(24*time.Hour)); started || err != nil {
		t.Fatal("successful attempt replayed", err)
	}
	raw, _ := json.Marshal(e.Snapshot())
	if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), job.AdobeRegistration.Proxy) || strings.Contains(string(raw), "browser_profile_id") {
		t.Fatal("public progress leaked recovery secrets")
	}
}

func TestAdobeSavedResultAutomaticCompensationNeverCreatesMailboxOrBrowser(t *testing.T) {
	now := time.Now()
	job := adobeCompensationFixture(t, now)
	job.Status, job.Stage, job.Error = "verification_pending", "verify", "verify: account_unverified"
	job.Account = map[string]any{"email": job.Mailbox.Email, "account_id": "saved", "token": "PRIVATE"}
	job.Imported = true
	var verifies atomic.Int32
	e := NewAdobeEngine(filepath.Join(t.TempDir(), "adobe.enc.json"), "key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		t.Error("saved result recovery constructed a browser/mail client")
		return nil, nil, errors.New("unexpected")
	}, func(context.Context, string, map[string]any) error {
		t.Error("already imported result was imported again")
		return nil
	}, func(_ context.Context, id string, _ map[string]any) error {
		verifies.Add(1)
		if id != job.ID {
			t.Error("verification changed original registration identity")
		}
		return nil
	})
	seedAdobeCompensation(t, e, job)
	if started, err := e.RunCompensation(adobeTestConfig(), now); !started || err != nil {
		t.Fatal("saved result was not recovered", err)
	}
	waitAdobe(t, e)
	if child := latestAdobe(t, e, job.ID); child.Status != "completed" || child.AutoRetry.Attempts != 1 || verifies.Load() != 1 {
		t.Fatal("saved result incomplete", child.Status)
	}
}

func TestAdobePreSubmissionRecoveryCanRegisterButNotAfterFlowFence(t *testing.T) {
	for _, submitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_flow", true: "flow_unknown"}[submitted], func(t *testing.T) {
			now := time.Now()
			job := adobeCompensationFixture(t, now)
			job.Stage, job.Error = "waiting_browser", "browser: adobe_browser_unavailable"
			job.AdobeRegistration.FlowStarted, job.AdobeRegistration.SubmissionStarted = submitted, false
			job.AdobeRegistration.CreationOutcome = ""
			e := NewAdobeEngine(filepath.Join(t.TempDir(), "adobe.enc.json"), "key", func(cfg AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				if cfg.RecoveryMode != "retry_registration" {
					t.Errorf("mode=%s want=retry_registration", cfg.RecoveryMode)
				}
				return &adobeCompensationMail{}, adobeCompensationFlow{run: func(_ context.Context, cfg AdobeConfig, _ Progress) (map[string]any, error) {
					if err := cfg.creationProgress(adobeCreationObservation{Outcome: "unknown"}, false); err != nil {
						return nil, err
					}
					return nil, fail("adobe", "submission_result_unknown")
				}}, nil
			}, nil, nil)
			seedAdobeCompensation(t, e, job)
			if started, err := e.RunCompensation(adobeTestConfig(), now); started == submitted || err != nil {
				t.Fatal(err)
			}
			waitAdobe(t, e)
			if adobeRegistrationRecoveryMode(latestAdobe(t, e, job.ID)) != "result_uncertain" {
				t.Fatal("unknown result lost its no-Create fence")
			}
		})
	}
}

func TestAdobeLegacyRecoveryWaitsAndDoesNotMutatePausedConstruction(t *testing.T) {
	now := time.Now()
	job := adobeCompensationFixture(t, now)
	job.AdobeRegistration, job.AutoRetry = nil, nil
	job.Stage, job.Error = "adobe_registration_needs_review", "adobe: human_verification_required"
	routing := routingFixture()
	lease, err := routing.Manager.AcquireTask(context.Background(), "direct", "adobe", "registration", nil, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.ProxyBinding = &lease.Binding
	lease.Release()
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	e := NewAdobeEngine(path, "key", nil, nil, nil)
	seedAdobeCompensation(t, e, job)
	before, _ := os.ReadFile(path)
	e = NewAdobeEngine(path, "key", nil, nil, nil)
	after, _ := os.ReadFile(path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("constructor migrated inactive historical state")
	}
	if started, err := e.RunCompensation(adobeTestConfig(), now); started || err != nil {
		t.Fatal("historical job replayed immediately", err)
	}
	got := latestAdobe(t, e, job.ID)
	if got.AutoRetry.State != "manual" || got.AutoRetry.Reason != "registration_result_uncertain" || got.AutoRetry.NextAttemptAt != "" || adobeRegistrationRecoveryMode(got) != "result_uncertain" {
		t.Fatalf("legacy migration=%+v", got.AutoRetry)
	}
}

func TestAdobeCompensationSourceMismatchAndFailedJournalDoNotStart(t *testing.T) {
	for _, failure := range []string{"source", "account", "api", "journal"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Now()
			job := adobeCompensationFixture(t, now)
			var factories atomic.Int32
			path := filepath.Join(t.TempDir(), "adobe.enc.json")
			e := NewAdobeEngine(path, "key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				factories.Add(1)
				return nil, nil, errors.New("unexpected")
			}, nil, nil)
			switch failure {
			case "source":
				job.Mailbox.SourceID = "another-source"
			case "account":
				job.Mailbox.AccountID = "another-account"
			case "api":
				job.MailboxAPIBase = "https://another-source.invalid"
			}
			seedAdobeCompensation(t, e, job)
			if failure == "journal" {
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if started, _ := e.RunCompensation(adobeTestConfig(), now); started || factories.Load() != 0 {
				t.Fatal("unusable source/journal admitted network work")
			}
			got := latestAdobe(t, e, job.ID)
			if got.ID != job.ID || got.AutoRetry.Attempts != 0 || got.RetryJobID != "" || len(e.jobs) != 1 {
				t.Fatal("failed admission consumed a child or budget")
			}
		})
	}
}

func TestAdobeCompensationBudgetSharedByMailboxAcrossRestart(t *testing.T) {
	now := time.Now()
	job := adobeCompensationFixture(t, now)
	other := cloneJob(job)
	other.ID, other.AdobeRegistration.BrowserProfileID = "another-root", "another-root"
	other.AutoRetry.State, other.AutoRetry.Attempts = "exhausted", 2
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	var calls atomic.Int32
	factory := func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		calls.Add(1)
		return nil, nil, fail("mail", "network_error")
	}
	e := NewAdobeEngine(path, "key", factory, nil, nil)
	seedAdobeCompensation(t, e, other, job)
	if started, err := e.RunCompensation(adobeTestConfig(), now.Add(24*time.Hour)); !started || err != nil {
		t.Fatal("last shared attempt unavailable", err)
	}
	waitAdobe(t, e)
	e = NewAdobeEngine(path, "key", factory, nil, nil)
	if started, err := e.RunCompensation(adobeTestConfig(), now.Add(48*time.Hour)); started || err != nil {
		t.Fatal("restarted chain exceeded mailbox budget", err)
	}
	child := latestAdobe(t, e, job.ID)
	if child.AutoRetry.Attempts != 1 || child.AutoRetry.State != "exhausted" || e.adobeMailboxAttemptsLocked(job.Mailbox.Email) != 3 || calls.Load() != 1 {
		t.Fatal("mailbox budget was reset or ancestors counted twice", child.AutoRetry)
	}
}

func TestAdobeStopCancelsWaitingAndActiveRecovery(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiting", true: "active"}[active], func(t *testing.T) {
			now := time.Now()
			job := adobeCompensationFixture(t, now)
			entered := make(chan struct{})
			e := NewAdobeEngine(filepath.Join(t.TempDir(), "adobe.enc.json"), "key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				return &adobeCompensationMail{}, adobeCompensationFlow{run: func(ctx context.Context, _ AdobeConfig, progress Progress) (map[string]any, error) {
					if err := progress("adobe_recovery_started"); err != nil {
						return nil, err
					}
					close(entered)
					<-ctx.Done()
					return nil, fail("adobe", "submission_result_unknown")
				}}, nil
			}, nil, nil)
			seedAdobeCompensation(t, e, job)
			if active {
				if started, err := e.RunCompensation(adobeTestConfig(), now); !started || err != nil {
					t.Fatal(err)
				}
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("recovery never started")
				}
			}
			e.Stop()
			waitAdobe(t, e)
			got := latestAdobe(t, e, job.ID)
			if got.AutoRetry.State != "manual" || got.AutoRetry.Reason != "cancelled" {
				t.Fatal("Stop scheduled another attempt", got.AutoRetry)
			}
			if started, err := e.RunCompensation(adobeTestConfig(), now.Add(48*time.Hour)); started || err != nil {
				t.Fatal("stopped job restarted", err)
			}
		})
	}
}

func TestAdobeRestartRetainsReservedDeadlineAndSubmissionFence(t *testing.T) {
	now := time.Now()
	job := adobeCompensationFixture(t, now)
	job.Status, job.AutoRetry.State, job.AutoRetry.Attempts = "running", "running", 1
	job.AutoRetry.NextAttemptAt = now.Add(3 * time.Hour).UTC().Format(time.RFC3339Nano)
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	e := NewAdobeEngine(path, "key", nil, nil, nil)
	seedAdobeCompensation(t, e, job)
	e = NewAdobeEngine(path, "key", nil, nil, nil)
	got := latestAdobe(t, e, job.ID)
	if got.AutoRetry.State != "waiting" || got.AutoRetry.Attempts != 1 || got.AutoRetry.NextAttemptAt != job.AutoRetry.NextAttemptAt || adobeRegistrationRecoveryMode(got) != "resume_session" {
		t.Fatalf("restart lost safe recovery: %+v", got)
	}
}

func TestAdobeSavedResultIgnoresMissingOrChangedCurrentMailboxConfiguration(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			name := fmt.Sprintf("automatic_%t_missing_%t", automatic, missing)
			t.Run(name, func(t *testing.T) {
				now := time.Now()
				job := adobeCompensationFixture(t, now)
				job.Status, job.Stage, job.Error = "import_pending", "import", "import: account_write_failed"
				job.Account = map[string]any{"account_id": "saved", "email": job.Mailbox.Email, "token": "PRIVATE"}
				var imports atomic.Int32
				e := NewAdobeEngine(filepath.Join(t.TempDir(), "adobe.enc.json"), "key", nil, func(_ context.Context, claim string, _ map[string]any) error {
					imports.Add(1)
					if claim != job.ID {
						t.Error("saved result changed mailbox claim")
					}
					return nil
				}, func(context.Context, string, map[string]any) error { return nil })
				seedAdobeCompensation(t, e, job)
				raw := map[string]any{"target": "adobe"}
				if !missing {
					raw = adobeTestConfig()
					e.jobs[0].Mailbox.SourceID = "original-source-not-current"
					e.jobs[0].MailboxAPIBase = "https://original-hme.invalid"
				}
				if automatic {
					if started, err := e.RunCompensation(raw, now); !started || err != nil {
						t.Fatal("saved result blocked by current config", err)
					}
				} else if err := e.RetrySavedResultWithConfig(raw, job.ID); err != nil {
					t.Fatal("saved result blocked by current config", err)
				}
				waitAdobe(t, e)
				if child := latestAdobe(t, e, job.ID); child.Status != "completed" || imports.Load() != 1 {
					t.Fatal("saved result did not complete")
				}
			})
		}
	}
}

func TestAdobeGracefulShutdownDefersInitialAndManualWorkButStopStaysStopped(t *testing.T) {
	for _, mode := range []string{"initial", "manual", "stop_then_shutdown"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			job := adobeCompensationFixture(t, now)
			entered := make(chan struct{})
			path := filepath.Join(t.TempDir(), "adobe.enc.json")
			e := NewAdobeEngine(path, "key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				var mail MailSource = &adobeCompensationMail{}
				if mode != "manual" {
					mail = adobeTestMail{}
				}
				return mail, adobeCompensationFlow{run: func(ctx context.Context, _ AdobeConfig, progress Progress) (map[string]any, error) {
					if err := progress("adobe_flow_started"); err != nil {
						return nil, err
					}
					close(entered)
					<-ctx.Done()
					return nil, fail("adobe", "submission_result_unknown")
				}}, nil
			}, nil, nil)
			rootID := job.ID
			if mode == "manual" {
				seedAdobeCompensation(t, e, job)
				if err := e.RetryRegistration(adobeTestConfig(), job.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				var err error
				rootID, err = e.Start(adobeTestConfig())
				if err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("flow did not begin")
			}
			if mode == "stop_then_shutdown" {
				e.Stop()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := e.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			e = NewAdobeEngine(path, "key", nil, nil, nil)
			got := latestAdobe(t, e, rootID)
			want := "waiting"
			if mode != "manual" {
				want = "manual"
			}
			wantMode := "result_uncertain"
			if mode == "manual" {
				wantMode = "resume_session"
			}
			if got.AutoRetry == nil || got.AutoRetry.State != want || adobeRegistrationRecoveryMode(got) != wantMode {
				t.Fatalf("shutdown=%s recovery=%+v", mode, got.AutoRetry)
			}
			if mode == "stop_then_shutdown" && got.AutoRetry.Reason != "cancelled" {
				t.Fatal("explicit stop was undone by shutdown")
			}
		})
	}
}
