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

func freeTestConfig() map[string]any {
	return map[string]any{"target": "openai", "mode": "total", "total": 1, "threads": 1, "mail": map[string]any{"providers": []any{map[string]any{"id": "test", "type": "icloud_hme", "api_base": "https://mail.example.test", "admin_password": "fake-password", "account_id": "acc-test", "enable": true}}}}
}

type fakeMail struct{ calls atomic.Int32 }

func (m *fakeMail) Acquire(context.Context, string) (Mailbox, error) {
	m.calls.Add(1)
	return Mailbox{Email: "alias@example.test", AccountID: "acc-test"}, nil
}
func (m *fakeMail) WaitCode(context.Context, Mailbox, time.Time) (string, error) {
	return "987654", nil
}
func (m *fakeMail) Close() {}

type fakeFlow struct {
	calls   atomic.Int32
	block   bool
	started chan struct{}
}

func (f *fakeFlow) Register(ctx context.Context, box Mailbox, cfg FreeConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	f.calls.Add(1)
	if err := progress("wait_code"); err != nil {
		return nil, err
	}
	if f.started != nil {
		close(f.started)
	}
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return map[string]any{"access_token": "PRIVATE-TOKEN", "email": box.Email, "source_type": "chatgpt_web"}, nil
}
func (f *fakeFlow) Close() {}
func waitEngine(t *testing.T, e *FreeEngine) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.Snapshot()["running"] == false {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("engine did not finish")
}

func TestFreeEnginePreservesResultAndRetriesOnlyImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	mail, flow := &fakeMail{}, &fakeFlow{}
	var imports, verifies atomic.Int32
	factory := func(FreeConfig) (MailSource, RegistrationFlow, error) { return mail, flow, nil }
	engine := NewFreeEngine(path, factory, func(ctx context.Context, id string, account map[string]any) error {
		if account["enabled"] != false {
			t.Error("unverified account enabled")
		}
		raw, _ := os.ReadFile(path)
		if !strings.Contains(string(raw), "PRIVATE-TOKEN") {
			t.Error("credentials not journaled before import")
		}
		if imports.Add(1) == 1 {
			return errors.New("contains PRIVATE-TOKEN and secret-password")
		}
		return nil
	}, func(context.Context, string, map[string]any) error { verifies.Add(1); return nil })
	id, err := engine.Start(freeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	waitEngine(t, engine)
	job, _ := engine.getJob(id)
	if job.Status != "import_pending" || len(job.Account) == 0 || job.Imported {
		t.Fatalf("lost pending account: %+v", job)
	}
	public, _ := json.Marshal(engine.Snapshot())
	if strings.Contains(string(public), "PRIVATE") || strings.Contains(string(public), "secret-password") || strings.Contains(string(public), "alias@example.test") {
		t.Fatalf("secret in snapshot: %s", public)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("journal is not private")
	}
	if err := engine.RetrySavedResult(id); err != nil {
		t.Fatal(err)
	}
	waitEngine(t, engine)
	job, _ = engine.getJob(id)
	if !job.Imported || !job.Verified || job.Status != "completed" || len(job.Account) != 0 {
		t.Fatalf("bad final state %+v", job)
	}
	if mail.calls.Load() != 1 || flow.calls.Load() != 1 || imports.Load() != 2 || verifies.Load() != 1 {
		t.Fatal("retry repeated registration or skipped verification")
	}
}

func TestFreeEngineCancellationAndConcurrentStart(t *testing.T) {
	mail := &fakeMail{}
	flow := &fakeFlow{block: true, started: make(chan struct{})}
	engine := NewFreeEngine(filepath.Join(t.TempDir(), "tasks.json"), func(FreeConfig) (MailSource, RegistrationFlow, error) { return mail, flow, nil }, func(context.Context, string, map[string]any) error { t.Error("import after cancellation"); return nil }, nil)
	id, err := engine.Start(freeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-flow.started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	if _, err := engine.Start(freeTestConfig()); err == nil {
		t.Fatal("accepted second active task")
	}
	if err := engine.ResetCompleted(); err == nil {
		t.Fatal("reset running task")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := engine.getJob(id)
	if job.Status != "cancelled" {
		t.Fatalf("got %s", job.Status)
	}
}

func TestFreeEngineRestartDoesNotRepeatRegistration(t *testing.T) {
	for _, withResult := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_credentials", true: "after_credentials"}[withResult], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tasks.json")
			job := RegistrationJob{ID: "job", Status: "running", Stage: "session"}
			if withResult {
				job.Imported = true
				job.Account = map[string]any{"access_token": "PRIVATE-TOKEN"}
			}
			if err := writeJSON0600(path, []RegistrationJob{job}); err != nil {
				t.Fatal(err)
			}
			var verifies atomic.Int32
			engine := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) {
				t.Error("restart ran registration")
				return nil, nil, nil
			}, func(context.Context, string, map[string]any) error {
				t.Error("already imported account imported again")
				return nil
			}, func(context.Context, string, map[string]any) error { verifies.Add(1); return nil })
			job, _ = engine.getJob("job")
			if withResult {
				if job.Status != "verification_pending" {
					t.Fatal(job.Status)
				}
				if err := engine.RetrySavedResult("job"); err != nil {
					t.Fatal(err)
				}
				waitEngine(t, engine)
				if verifies.Load() != 1 {
					t.Fatal("not verified")
				}
			} else {
				if job.Status != "interrupted" {
					t.Fatal(job.Status)
				}
				if engine.RetrySavedResult("job") == nil {
					t.Fatal("restarted unsafe registration")
				}
			}
		})
	}
}

func TestFreeEngineVerificationFailureRetainsAccount(t *testing.T) {
	engine := NewFreeEngine(filepath.Join(t.TempDir(), "tasks.json"), func(FreeConfig) (MailSource, RegistrationFlow, error) { return &fakeMail{}, &fakeFlow{}, nil }, func(context.Context, string, map[string]any) error { return nil }, func(context.Context, string, map[string]any) error { return errors.New("PRIVATE error") })
	id, err := engine.Start(freeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	waitEngine(t, engine)
	job, _ := engine.getJob(id)
	if !job.Imported || job.Verified || job.Status != "verification_pending" || len(job.Account) == 0 {
		t.Fatalf("bad state %+v", job)
	}
	if err := engine.ResetCompleted(); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.getJob(id); err != nil {
		t.Fatal("reset discarded recoverable account")
	}
}

func TestFreeEngineCorruptJournalAndInvalidConfigFailBeforeNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	_ = os.WriteFile(path, []byte("{corrupt"), 0o600)
	engine := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) {
		t.Fatal("unexpected network setup")
		return nil, nil, nil
	}, nil, nil)
	if _, err := engine.Start(freeTestConfig()); err == nil {
		t.Fatal("ignored corrupt journal")
	}
	for _, change := range []func(map[string]any){
		func(c map[string]any) { c["threads"] = 2 }, func(c map[string]any) { c["total"] = 0 }, func(c map[string]any) { c["mode"] = "quota" }, func(c map[string]any) { c["proxy"] = "group:foo" },
		func(c map[string]any) { c["checkout"] = map[string]any{"enabled": true} }, func(c map[string]any) { c["openai_free"] = map[string]any{"timeout_seconds": 1} },
		func(c map[string]any) {
			p := object(c["mail"])["providers"].([]any)
			object(p[0])["type"] = "icloud_api"
		},
	} {
		c := freeTestConfig()
		change(c)
		if _, err := ParseFreeConfig(c); err == nil {
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
}
