package register

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type adobeWaitTestMail struct {
	adobeReleaseMailbox
	wait func(context.Context) (string, error)
}

func (m *adobeWaitTestMail) WaitCodeFor(ctx context.Context, box Mailbox, after time.Time, sender func(string) bool) (string, error) {
	if box.Email != m.box.Email || after.IsZero() || !sender("account-noreply@adobe.com") || sender("no-reply@openai.com") {
		return "", fail("mail", "invalid_wait_arguments")
	}
	return m.wait(ctx)
}

type adobeWaitTestFlow struct{}

func (adobeWaitTestFlow) Close() {}
func (adobeWaitTestFlow) Register(ctx context.Context, _ string, _ Mailbox, cfg AdobeConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	if err := cfg.creationProgress(adobeCreationObservation{Outcome: "accepted", HTTPStatus: 200}, true); err != nil {
		return nil, err
	}
	if err := progress("awaiting_verification"); err != nil {
		return nil, err
	}
	_, err := wait(ctx, time.Now().UTC())
	return nil, err
}

func TestAdobeMailWaitDeadlineRetainsAcceptedAccountAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, wantError, wantState string
		mailTimeout, taskTimeout   time.Duration
		cancelParent               bool
	}{
		{"mail_wait", "mail: timeout", "waiting", 5 * time.Millisecond, time.Second, false},
		{"task_deadline", "task_timeout", "waiting", time.Second, 200 * time.Millisecond, false},
		{"parent_cancel", "task_cancelled", "manual", time.Second, time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseAdobeConfig(adobeTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			cfg.MailTimeout, cfg.Timeout = tc.mailTimeout, tc.taskTimeout
			job := adobeCompensationFixture(t, time.Now())
			job.Status, job.Stage, job.Error, job.AutoRetry = "queued", "queued", "", nil
			job.AdobeRegistration.CreationOutcome = "not_submitted"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mail := &adobeWaitTestMail{adobeReleaseMailbox: adobeReleaseMailbox{box: job.Mailbox}}
			job.Mailbox = Mailbox{}
			mail.wait = func(ctx context.Context) (string, error) {
				if tc.cancelParent {
					cancel()
				}
				<-ctx.Done()
				return "", ctx.Err()
			}
			path := filepath.Join(t.TempDir(), "registration.enc.json")
			engine := NewAdobeEngine(path, "test", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				return mail, adobeWaitTestFlow{}, nil
			}, nil, nil)
			seedAdobeCompensation(t, engine, job)
			engine.runTask(ctx, cfg, job.ID)
			// Reload the journal so this asserts durable state, not only the live task.
			engine = NewAdobeEngine(path, "test", nil, nil, nil)
			got, err := engine.getJob(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Error != tc.wantError || got.Status != "registration_pending" || got.Stage != "awaiting_verification" {
				t.Fatalf("failure classification: error=%q status=%q stage=%q", got.Error, got.Status, got.Stage)
			}
			if adobeCreationOutcome(got) != "accepted" || adobeRegistrationRecoveryMode(got) != "resume_session" || len(got.Account) != 0 || mail.releases != 0 {
				t.Fatal("mail wait failure changed creation evidence, released its claim, or saved an unverified account")
			}
			if got.AutoRetry == nil || got.AutoRetry.State != tc.wantState || got.AutoRetry.Attempts != 0 {
				t.Fatalf("changed recovery policy: %+v", got.AutoRetry)
			}
			if tc.wantState == "waiting" {
				due, err := time.Parse(time.RFC3339Nano, got.AutoRetry.NextAttemptAt)
				if err != nil || due.Before(time.Now().Add(4*time.Minute)) {
					t.Fatal("mail timeout bypassed the saved recovery cooldown")
				}
			}
		})
	}
}

func TestAdobeMailWaitPreservesProviderResultsAndRequiresSenderFilter(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		err        error
	}{
		{"success", "123456", nil},
		{"network_failure", "", fail("mail", "network_error")},
		{"provider_deadline", "", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := Mailbox{Email: "fixture@example.test"}
			mail := &adobeWaitTestMail{adobeReleaseMailbox: adobeReleaseMailbox{box: box}, wait: func(context.Context) (string, error) { return tc.code, tc.err }}
			code, err := waitAdobeVerificationCode(context.Background(), mail, box, time.Now(), time.Second)
			if code != tc.code || !errors.Is(err, tc.err) {
				t.Fatalf("changed provider result: code_present=%v error=%v", code != "", err)
			}
		})
	}
	_, err := waitAdobeVerificationCode(context.Background(), adobeTestMail{}, Mailbox{}, time.Now(), time.Second)
	if safeFailure(err) != "mail: adobe_sender_filter_required" {
		t.Fatal("mailbox without the Adobe sender filter was accepted")
	}
}

type adobePreferredWaitTestMail struct {
	adobeTestMail
	wait                     func(context.Context) (string, error)
	adobeCalls, genericCalls int
}

func (m *adobePreferredWaitTestMail) WaitAdobeCode(ctx context.Context, _ Mailbox, _ time.Time) (string, error) {
	m.adobeCalls++
	return m.wait(ctx)
}

func (m *adobePreferredWaitTestMail) WaitCodeFor(context.Context, Mailbox, time.Time, func(string) bool) (string, error) {
	m.genericCalls++
	return "", fail("mail", "unexpected_generic_extractor")
}

func TestAdobeMailWaitPrefersAdobeSpecificExtractionWithSameDeadline(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "mail_timeout"}[timeout], func(t *testing.T) {
			mail := &adobePreferredWaitTestMail{wait: func(ctx context.Context) (string, error) {
				if timeout {
					<-ctx.Done()
					return "", ctx.Err()
				}
				return "123456", nil
			}}
			code, err := waitAdobeVerificationCode(context.Background(), mail, Mailbox{}, time.Now(), 5*time.Millisecond)
			if timeout && (safeFailure(err) != "mail: timeout" || code != "") || !timeout && (err != nil || code != "123456") {
				t.Fatalf("Adobe extractor result changed: code_present=%v error=%v", code != "", err)
			}
			if mail.adobeCalls != 1 || mail.genericCalls != 0 {
				t.Fatal("Adobe source fell back to the generic extractor")
			}
		})
	}
}

func TestAdobeRecoveryFailureCodesKeepCreationAndRetryGuards(t *testing.T) {
	for _, code := range []string{"mail: timeout", "adobe: recovery_page_observation_timeout", "adobe: recovery_page_observation_failed", "adobe: recovery_page_closed", "adobe: recovery_page_untrusted"} {
		for _, outcome := range []string{"accepted", "unknown"} {
			t.Run(code+"/"+outcome, func(t *testing.T) {
				now := time.Now()
				job := adobeCompensationFixture(t, now)
				job.Error, job.AutoRetry, job.AdobeRegistration.CreationOutcome = code, nil, outcome
				planAdobeRegistrationCompensation(&job, nil, now)
				want := "manual"
				if outcome == "accepted" && code != "adobe: recovery_page_untrusted" {
					want = "waiting"
				}
				if job.AutoRetry.State != want || adobeCreationOutcome(job) != outcome {
					t.Fatalf("state=%+v creation=%s", job.AutoRetry, adobeCreationOutcome(job))
				}
			})
		}
	}
}
