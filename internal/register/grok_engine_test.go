package register

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type grokSavedMailStub struct{ prepared atomic.Int32 }

func (m *grokSavedMailStub) Acquire(context.Context, string) (Mailbox, error) {
	return Mailbox{}, fail("mail", "unexpected_acquire")
}
func (m *grokSavedMailStub) Prepare(context.Context, Mailbox) error {
	m.prepared.Add(1)
	return nil
}
func (m *grokSavedMailStub) WaitCode(context.Context, Mailbox, time.Time) (string, error) {
	return "230250", nil
}
func (m *grokSavedMailStub) Close() {}

type grokSavedFlowStub struct{ calls atomic.Int32 }

func (f *grokSavedFlowStub) Register(ctx context.Context, box Mailbox, cfg GrokConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	f.calls.Add(1)
	if err := progress("wait_code"); err != nil {
		return nil, err
	}
	if _, err := wait(ctx, time.Now().UTC().Add(-time.Second)); err != nil {
		return nil, err
	}
	return map[string]any{"sso": "saved-session"}, nil
}
func (f *grokSavedFlowStub) Close() {}

func TestGrokRetryRegistrationPreparesSavedMailbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grok-tasks.json")
	job := RegistrationJob{
		ID:     "saved-grok-job",
		Status: "registration_pending",
		Stage:  "send_code",
		Mailbox: Mailbox{
			ID:        "mbx-1",
			Email:     "alias@example.test",
			AccountID: "hme-account",
			SourceID:  "hme-source",
		},
	}
	raw, err := json.Marshal([]RegistrationJob{job})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	mail := &grokSavedMailStub{}
	flow := &grokSavedFlowStub{}
	engine := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) {
		return mail, flow, nil
	}, func(context.Context, string, map[string]any) error { return nil }, func(context.Context, string, map[string]any) error { return nil })
	if err := engine.RetryRegistration(grokConfigFixture(), job.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		engine.mu.Lock()
		active := len(engine.active)
		engine.mu.Unlock()
		if active == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	engine.mu.Lock()
	if len(engine.active) != 0 {
		engine.mu.Unlock()
		t.Fatal("Grok retry did not finish")
	}
	engine.mu.Unlock()
	got, err := latestAttempt(engine.getJob, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || mail.prepared.Load() != 1 || flow.calls.Load() != 1 {
		t.Fatalf("saved mailbox was not prepared before retry: status=%s prepared=%d calls=%d", got.Status, mail.prepared.Load(), flow.calls.Load())
	}
}

func TestGrokSavedResultRetainsSafeVerificationFailureAndResumesWithoutSignup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	job := RegistrationJob{ID: "saved", Status: "verification_pending", Stage: "verify", Imported: true, Account: map[string]any{"sso": "secret"}}
	raw, _ := json.Marshal([]RegistrationJob{job})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	failure := &Failure{Stage: "verify", Code: "account_unverified", Operation: "quota_probe=access_denied", UpstreamStatus: 403}
	e := NewGrokEngine(path, nil, func(context.Context, string, map[string]any) error { t.Fatal("reimported account"); return nil }, func(context.Context, string, map[string]any) error { return failure })
	err := e.importAndVerify(context.Background(), job.ID)
	if err != failure {
		t.Fatalf("discarded safe failure: %v", err)
	}
	e.failJob(job.ID, err)
	persisted := NewGrokEngine(path, nil, nil, nil)
	got, _ := persisted.getJob(job.ID)
	if got.Error != failure.Error() || got.Status != "verification_pending" || got.Account["sso"] != "secret" {
		t.Fatal("saved state lost")
	}
	e.verifyAccount = func(context.Context, string, map[string]any) error { return nil }
	if err := e.importAndVerify(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = e.getJob(job.ID)
	if got.Status != "completed" || !got.Verified || got.Error != "" || len(got.Account) != 0 {
		t.Fatal("resume did not complete")
	}
}
