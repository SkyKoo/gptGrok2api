package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdobeCreationOutcomeSeparatesRetryFromAcceptedAndUnknown(t *testing.T) {
	for _, tc := range []struct{ outcome, want string }{
		{"not_submitted", "retry_registration"}, {"rejected", "retry_registration"},
		{"accepted", "resume_session"}, {"unknown", "result_uncertain"}, {"", "result_uncertain"}, {"PRIVATE", "result_uncertain"},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			job := adobeCompensationFixture(t, time.Now())
			job.AdobeRegistration.CreationOutcome = tc.outcome
			job.AdobeRegistration.CreationErrorCode, job.AdobeRegistration.CreationHTTPStatus = "captcha_required", 400
			if got := adobeRegistrationRecoveryMode(job); got != tc.want {
				t.Fatalf("mode=%s want=%s", got, tc.want)
			}
			job.Account = map[string]any{"account_id": "saved"}
			if adobeRegistrationRecoveryMode(job) != "saved_result" {
				t.Fatal("saved identity lost priority")
			}
		})
	}
}

func TestAdobeCreationObservationCannotDowngradeAcceptedOrTrustGenericFailure(t *testing.T) {
	for _, tc := range []struct {
		name, previous, observed, code, want string
		status                               int
		registration                         bool
	}{
		{"explicit_not_submitted", "unknown", "not_submitted", "", "not_submitted", 0, true},
		{"explicit_rejection", "unknown", "rejected", "captcha_required", "rejected", 400, true},
		{"generic_rejection", "unknown", "rejected", "PRIVATE", "unknown", 400, true},
		{"wrong_status", "unknown", "rejected", "captcha_required", "unknown", 500, true},
		{"missing_result", "not_submitted", "", "", "unknown", 0, true},
		{"lost_rpc", "not_submitted", "unknown", "", "unknown", 0, false},
		{"resume_no_create", "unknown", "not_submitted", "", "unknown", 0, false},
		{"resume_form_rejection", "unknown", "rejected", "captcha_required", "unknown", 400, false},
		{"accepted_then_lost", "accepted", "unknown", "", "accepted", 0, false},
		{"accepted_then_rejected", "accepted", "rejected", "captcha_required", "accepted", 400, true},
		{"accepted_then_not_submitted", "accepted", "not_submitted", "", "accepted", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := adobeCompensationFixture(t, time.Now())
			job.AdobeRegistration.CreationOutcome = tc.previous
			recordAdobeCreationOutcome(&job, adobeCreationObservation{Outcome: tc.observed, ErrorCode: tc.code, HTTPStatus: tc.status}, tc.registration)
			if adobeCreationOutcome(job) != tc.want {
				t.Fatalf("outcome=%s want=%s", adobeCreationOutcome(job), tc.want)
			}
			if strings.Contains(job.AdobeRegistration.CreationErrorCode, "PRIVATE") {
				t.Fatal("untrusted error persisted")
			}
		})
	}
}

func TestAdobePersistedRejectionNeedsCompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		want   string
	}{{"captcha_required", 400, "retry_registration"}, {"", 400, "result_uncertain"}, {"captcha_required", 0, "result_uncertain"}, {"captcha_required", 503, "result_uncertain"}, {"PRIVATE", 400, "result_uncertain"}} {
		job := adobeCompensationFixture(t, time.Now())
		job.AdobeRegistration.CreationOutcome = "rejected"
		job.AdobeRegistration.CreationErrorCode, job.AdobeRegistration.CreationHTTPStatus = tc.code, tc.status
		if got := adobeRegistrationRecoveryMode(job); got != tc.want {
			t.Fatalf("mode=%s want=%s", got, tc.want)
		}
	}
}

func TestAdobeCreationBrowserResultPersistsBeforeNextCommand(t *testing.T) {
	for _, scenario := range []string{"not_submitted", "rejected", "unknown", "legacy_missing", "accepted_then_lost", "not_submitted_otp", "missing_outcome_otp", "resume_cannot_clear", "journal_failure"} {
		t.Run(scenario, func(t *testing.T) {
			job := adobeCompensationFixture(t, time.Now())
			job.AdobeRegistration.CreationOutcome = "not_submitted"
			var jobMu sync.Mutex
			var runs, closes, mailCalls int
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					closes++
					json.NewEncoder(w).Encode(map[string]string{"status": "closed"})
					return
				}
				if r.URL.Path == "/v1/browser/reservations" {
					json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
					return
				}
				runs++
				jobMu.Lock()
				current := adobeCreationOutcome(job)
				jobMu.Unlock()
				if scenario != "resume_cannot_clear" && current != "unknown" && !(runs == 2 && scenario == "accepted_then_lost" && current == "accepted") {
					t.Error("request started without durable uncertainty fence")
				}
				if runs == 2 {
					w.WriteHeader(503)
					return
				}
				outcome := scenario
				stage := "needs_manual"
				if scenario == "legacy_missing" {
					outcome = ""
				}
				if scenario == "accepted_then_lost" {
					outcome, stage = "accepted", "awaiting_verification"
				}
				if scenario == "not_submitted_otp" {
					outcome, stage = "not_submitted", "awaiting_verification"
				}
				if scenario == "missing_outcome_otp" {
					outcome, stage = "", "awaiting_verification"
				}
				if scenario == "resume_cannot_clear" {
					outcome = "not_submitted"
				}
				json.NewEncoder(w).Encode(map[string]any{"stage": stage, "code": "login_ui_not_ready", "creation_outcome": outcome, "creation_error_code": "captcha_required", "creation_http_status": 400})
			}))
			defer worker.Close()
			cfg := AdobeConfig{WorkerURL: worker.URL, BrowserProfileID: "fixture"}
			if scenario == "resume_cannot_clear" {
				cfg.RecoveryMode = "resume_session"
				job.AdobeRegistration.CreationOutcome = "accepted"
			}
			cfg.creationProgress = func(observed adobeCreationObservation, registration bool) error {
				if scenario == "journal_failure" {
					return fail("storage", "journal_write_failed")
				}
				jobMu.Lock()
				defer jobMu.Unlock()
				recordAdobeCreationOutcome(&job, observed, registration)
				return nil
			}
			flow := AdobeBrowserFlow{Client: worker.Client()}
			_, err := flow.Register(context.Background(), "fixture", job.Mailbox, cfg, func(context.Context, time.Time) (string, error) { mailCalls++; return "123456", nil }, func(string) error { return nil })
			flow.Close()
			if err == nil || closes != 1 {
				t.Fatalf("missing failure or lease cleanup: %v %d", err, closes)
			}
			want := map[string]string{"not_submitted": "not_submitted", "rejected": "rejected", "accepted_then_lost": "accepted", "resume_cannot_clear": "accepted", "journal_failure": "not_submitted"}[scenario]
			if want == "" {
				want = "unknown"
			}
			if adobeCreationOutcome(job) != want {
				t.Fatalf("outcome=%s want=%s", adobeCreationOutcome(job), want)
			}
			if scenario == "journal_failure" && runs != 0 {
				t.Fatal("failed fence write reached Worker")
			}
			if scenario == "not_submitted_otp" || scenario == "missing_outcome_otp" {
				if runs != 1 || mailCalls != 0 || safeFailure(err) != "adobe: creation_result_unknown" {
					t.Fatal("unaccepted OTP page entered HME or verification")
				}
			}
		})
	}
}

func TestAdobeRejectedAndUnsubmittedRetryUsesNewAttemptAndSameMailbox(t *testing.T) {
	for _, outcome := range []string{"not_submitted", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Now()
			parent := adobeCompensationFixture(t, now)
			parent.AdobeRegistration.CreationOutcome = outcome
			parent.AdobeRegistration.CreationErrorCode, parent.AdobeRegistration.CreationHTTPStatus = "captcha_required", 400
			parent.AdobeRegistration.Proxy = "http://previous-attempt.invalid:8080"
			parent.Error = "adobe: login_ui_not_ready"
			path := filepath.Join(t.TempDir(), "jobs.enc.json")
			mail := &adobeCompensationMail{}
			var calls atomic.Int32
			var engine *AdobeEngine
			engine = NewAdobeEngine(path, "test", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				return mail, adobeCompensationFlow{run: func(_ context.Context, cfg AdobeConfig, _ Progress) (map[string]any, error) {
					calls.Add(1)
					child := latestAdobe(t, engine, parent.ID)
					if cfg.RecoveryMode != "retry_registration" || cfg.BrowserProfileID != child.ID || cfg.BrowserProfileID == parent.ID || cfg.Proxy == parent.AdobeRegistration.Proxy {
						t.Error("retry retained old browser/profile/egress")
					}
					if child.AdobeRegistration.FlowStarted || child.AdobeRegistration.SubmissionStarted || child.AdobeRegistration.CreationOutcome != "not_submitted" {
						t.Error("new attempt inherited old submission")
					}
					if !reflect.DeepEqual(cfg.Profile, parent.AdobeProfile.fields()) {
						t.Error("saved user profile changed")
					}
					if err := cfg.creationProgress(adobeCreationObservation{Outcome: "not_submitted"}, true); err != nil {
						return nil, err
					}
					return nil, fail("adobe", "login_ui_not_ready")
				}}, nil
			}, nil, nil)
			seedAdobeCompensation(t, engine, parent)
			if started, err := engine.RunCompensation(adobeTestConfig(), now); !started || err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			waitAdobe(t, engine)
			child := latestAdobe(t, engine, parent.ID)
			if child.ParentJobID != parent.ID || child.RootJobID != parent.ID || child.ID == parent.ID || child.AutoRetry.Attempts != 1 || calls.Load() != 1 || mail.prepared.Load() != 1 || mail.acquired.Load() != 0 || mail.claim.Load() != parent.ID {
				t.Fatal("attempt/mailbox/root/budget changed")
			}
			original, _ := engine.getJob(parent.ID)
			if original.AdobeRegistration.CreationOutcome != outcome || original.AdobeRegistration.BrowserProfileID != parent.ID || !original.AdobeRegistration.SubmissionStarted {
				t.Fatal("parent evidence overwritten")
			}
			reloaded := NewAdobeEngine(path, "test", nil, nil, nil)
			if got := latestAdobe(t, reloaded, parent.ID); got.AdobeRegistration.BrowserProfileID != child.ID || adobeRegistrationRecoveryMode(got) != "retry_registration" {
				t.Fatal("new attempt decision lost after restart")
			}
		})
	}
}

func TestAdobeUnknownAncestorBlocksAllRegistrationRetries(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "superseded_ancestor"}[ancestor], func(t *testing.T) {
			now := time.Now()
			parent := adobeCompensationFixture(t, now)
			parent.AdobeRegistration.CreationOutcome = "unknown"
			jobs := []RegistrationJob{parent}
			id := parent.ID
			if ancestor {
				child := cloneJob(parent)
				child.ID, child.ParentJobID, child.RootJobID = "old-recovery-child", parent.ID, parent.ID
				child.AdobeRegistration.CreationOutcome = "not_submitted"
				child.Error = "adobe: login_ui_not_ready"
				jobs[0].RetryJobID = child.ID
				jobs = append(jobs, child)
				id = child.ID
			}
			var calls atomic.Int32
			engine := NewAdobeEngine(filepath.Join(t.TempDir(), "jobs.enc.json"), "test", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				calls.Add(1)
				return nil, nil, fail("test", "unexpected")
			}, nil, nil)
			seedAdobeCompensation(t, engine, jobs...)
			if started, err := engine.RunCompensation(adobeTestConfig(), now.Add(24*time.Hour)); started || err != nil {
				t.Fatalf("unknown auto retry: %v", err)
			}
			if err := engine.RetryRegistration(adobeTestConfig(), id); safeFailure(err) != "task: registration_result_uncertain" {
				t.Fatalf("unknown manual retry: %v", err)
			}
			if calls.Load() != 0 || len(engine.jobs) != len(jobs) {
				t.Fatal("unknown created child or browser")
			}
			current, _ := engine.getJob(id)
			if current.AutoRetry.State != "manual" || current.AutoRetry.Reason != "registration_result_uncertain" {
				t.Fatalf("wrong decision %+v", current.AutoRetry)
			}
			for _, raw := range engine.Snapshot()["jobs"].([]any) {
				row := raw.(map[string]any)
				if row["id"] == id && (row["can_retry_registration"] != false || row["can_resume_registration"] != false) {
					t.Fatal("unknown exposed retry action")
				}
			}
		})
	}
}

func TestAdobeCreationMigrationPreservesStopsBudgetAndCooldown(t *testing.T) {
	for _, state := range []string{"legacy_review", "stopped", "exhausted", "unknown", "waiting"} {
		t.Run(state, func(t *testing.T) {
			now := time.Now()
			job := adobeCompensationFixture(t, now)
			job.Error = "adobe: login_ui_not_ready"
			job.AdobeRegistration.CreationOutcome = "not_submitted"
			job.AutoRetry = &RegistrationCompensation{State: "manual", Reason: "failure_requires_review", Attempts: 1, NotBeforeAt: now.Add(time.Hour).UTC().Format(time.RFC3339Nano)}
			switch state {
			case "stopped":
				job.AutoRetry.Reason = "cancelled"
			case "exhausted":
				job.AutoRetry.State = "exhausted"
				job.AutoRetry.Attempts = 3
			case "unknown":
				job.AdobeRegistration.CreationOutcome = ""
			case "waiting":
				job.AutoRetry.State = "waiting"
				job.AutoRetry.Reason = ""
				job.AutoRetry.NextAttemptAt = job.AutoRetry.NotBeforeAt
			}
			engine := NewAdobeEngine(filepath.Join(t.TempDir(), "jobs.enc.json"), "test", nil, nil, nil)
			seedAdobeCompensation(t, engine, job)
			if started, err := engine.RunCompensation(adobeTestConfig(), now); started || err != nil {
				t.Fatalf("migration started traffic: %v", err)
			}
			got, _ := engine.getJob(job.ID)
			if got.AutoRetry.Attempts != job.AutoRetry.Attempts || got.AutoRetry.NotBeforeAt != job.AutoRetry.NotBeforeAt {
				t.Fatal("migration reset budget/cooldown")
			}
			if state == "legacy_review" || state == "waiting" {
				if got.AutoRetry.State != "waiting" || got.AutoRetry.NextAttemptAt != job.AutoRetry.NotBeforeAt {
					t.Fatalf("safe retry not scheduled %+v", got.AutoRetry)
				}
			} else if !reflect.DeepEqual(got.AutoRetry, job.AutoRetry) {
				t.Fatal("explicit/unknown decision changed")
			}
		})
	}
}
