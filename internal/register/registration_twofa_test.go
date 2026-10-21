package register

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const registrationTestSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

type registrationTwoFATestFlow struct {
	fakeFlow
	begin, verify, enroll, activate atomic.Int32
	failure                         string
	onEnroll                        func()
	onActivate                      func()
	started                         chan struct{}
}

func (f *registrationTwoFATestFlow) BeginTwoFA(ctx context.Context, email string) (time.Time, error) {
	f.begin.Add(1)
	if f.failure == "reauth" {
		return time.Time{}, errors.New("PRIVATE-REAUTH-TOKEN")
	}
	return time.Now(), nil
}
func (f *registrationTwoFATestFlow) VerifyTwoFAEmail(ctx context.Context, email, user, code string) (string, error) {
	f.verify.Add(1)
	if f.failure == "verify" {
		return "", errors.New("PRIVATE-OTP-" + code)
	}
	return "PRIVATE-REAUTH-TOKEN", nil
}
func (f *registrationTwoFATestFlow) EnrollTwoFA(ctx context.Context, token string) (string, string, error) {
	f.enroll.Add(1)
	if f.onEnroll != nil {
		f.onEnroll()
	}
	if f.failure == "enroll" {
		return "", "", errors.New("PRIVATE-ENROLLMENT")
	}
	return registrationTestSecret, "PRIVATE-ENROLLMENT", nil
}
func (f *registrationTwoFATestFlow) ActivateTwoFA(ctx context.Context, token, secret, session string) error {
	f.activate.Add(1)
	if f.onActivate != nil {
		f.onActivate()
	}
	if f.failure == "activate" {
		return context.DeadlineExceeded
	}
	if f.failure == "cancel" {
		close(f.started)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func TestRegistrationAutomaticTwoFA(t *testing.T) {
	for _, scenario := range []string{"off", "success", "reauth", "verify", "enroll", "activate", "import_retry", "pending_save_failure", "final_save_failure", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			mail := &fakeMail{}
			flow := &registrationTwoFATestFlow{failure: scenario, started: make(chan struct{})}
			var imported map[string]any
			var imports atomic.Int32
			var engine *FreeEngine
			factory := func(cfg FreeConfig) (MailSource, RegistrationFlow, error) {
				if cfg.AutoTwoFA != (scenario != "off") {
					t.Error("switch not captured in run config")
				}
				return mail, flow, nil
			}
			importer := func(ctx context.Context, id string, account map[string]any) error {
				n := imports.Add(1)
				imported = account
				if scenario == "import_retry" && n == 1 {
					return errors.New("write failed")
				}
				return nil
			}
			verifier := func(context.Context, string, map[string]any) error { return nil }
			engine = NewFreeEngine(path, factory, importer, verifier)
			if scenario == "pending_save_failure" {
				flow.onEnroll = func() { engine.path = filepath.Join(path, "cannot-write.json") }
			}
			flow.onActivate = func() {
				raw, _ := os.ReadFile(path)
				if !strings.Contains(string(raw), registrationTestSecret) || !strings.Contains(string(raw), "PRIVATE-ENROLLMENT") {
					t.Error("activation before durable pending credential backup")
				}
				if scenario == "final_save_failure" {
					engine.path = filepath.Join(path, "cannot-write.json")
				}
			}
			cfg := freeTestConfig()
			if scenario != "off" {
				cfg["openai_free"] = map[string]any{"auto_twofa": true}
			}
			id, err := engine.Start(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cancel" {
				select {
				case <-flow.started:
					engine.Stop()
				case <-time.After(3 * time.Second):
					t.Fatal("activation did not start")
				}
			}
			waitEngine(t, engine)
			job, _ := engine.getJob(id)
			public, _ := json.Marshal(engine.Snapshot())
			for _, secret := range []string{registrationTestSecret, "PRIVATE-TOKEN", "PRIVATE-REAUTH-TOKEN", "PRIVATE-ENROLLMENT", "987654"} {
				if strings.Contains(string(public), secret) {
					t.Fatal("secret leaked in public progress")
				}
			}
			if scenario == "off" {
				if flow.begin.Load() != 0 || flow.activate.Load() != 0 || job.TwoFA != nil || !job.Verified {
					t.Fatal("disabled switch changed registration")
				}
				return
			}
			if scenario == "pending_save_failure" || scenario == "final_save_failure" {
				expectedActivations := int32(0)
				if scenario == "final_save_failure" {
					expectedActivations = 1
				}
				if flow.activate.Load() != expectedActivations || imports.Load() != 0 {
					t.Fatal("storage failure did not stop traffic/import")
				}
				// Reload the actual durable journal. Recovery can import credentials but
				// never repeats enrollment or activation, even after an unknown result.
				recovered := NewFreeEngine(path, factory, importer, verifier)
				saved, _ := recovered.getJob(id)
				if saved.Status != "import_pending" || len(saved.Account) == 0 {
					t.Fatal("lost durable account")
				}
				if scenario == "final_save_failure" && (!saved.TwoFA.Pending || saved.TwoFA.Status != "interrupted" || len(object(saved.Account["twofa_pending"])) == 0) {
					t.Fatal("lost uncertain activation recovery")
				}
				if err := recovered.RetrySavedResult(id); err != nil {
					t.Fatal(err)
				}
				waitEngine(t, recovered)
				if flow.activate.Load() != expectedActivations || flow.calls.Load() != 1 {
					t.Fatal("recovery replayed upstream action")
				}
				return
			}
			if scenario == "cancel" || scenario == "import_retry" {
				if err := engine.RetrySavedResult(id); err != nil {
					t.Fatal(err)
				}
				waitEngine(t, engine)
				job, _ = engine.getJob(id)
				if flow.activate.Load() != 1 || flow.calls.Load() != 1 {
					t.Fatal("retry repeated registration/activation")
				}
			}
			if !job.Verified || job.Status != "completed" || imported == nil {
				t.Fatalf("new account was not retained and verified: %s", job.Status)
			}
			switch scenario {
			case "success", "import_retry":
				if job.TwoFA.Status != "completed" || imported["two_factor_secret"] != registrationTestSecret || imported["twofa_pending"] != nil {
					t.Fatal("confirmed secret not imported")
				}
			case "activate", "cancel":
				if !job.TwoFA.Pending || object(imported["twofa_pending"])["secret"] != registrationTestSecret || imported["two_factor_secret"] != nil {
					t.Fatal("pending activation was lost or marked active")
				}
			default:
				if job.TwoFA.Status != "failed" || imported["two_factor_secret"] != nil || flow.activate.Load() != 0 {
					t.Fatal("failed 2FA was incorrectly activated")
				}
			}
		})
	}
}
