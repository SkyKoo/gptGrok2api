package register

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func freeTestConfig() map[string]any {
	return map[string]any{"target": "openai", "mode": "total", "total": 1, "threads": 1, "mail": map[string]any{"providers": []any{map[string]any{"id": "test", "type": "icloud_hme", "api_base": "https://mail.example.test", "admin_password": "fake-password", "account_id": "acc-test", "enable": true}}}}
}

type fakeMail struct {
	calls    atomic.Int32
	prepared atomic.Int32
}

func (m *fakeMail) Acquire(context.Context, string) (Mailbox, error) {
	m.calls.Add(1)
	return Mailbox{Email: "alias@example.test", AccountID: "acc-test"}, nil
}
func (m *fakeMail) Prepare(context.Context, Mailbox) error {
	m.prepared.Add(1)
	return nil
}
func (m *fakeMail) WaitCode(context.Context, Mailbox, time.Time) (string, error) {
	return "987654", nil
}
func (m *fakeMail) Close() {}

type fakeFlow struct {
	calls     atomic.Int32
	block     bool
	failFirst bool
	started   chan struct{}
	profile   BrowserProfile
}

func (f *fakeFlow) Register(ctx context.Context, box Mailbox, cfg FreeConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	call := f.calls.Add(1)
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
	if f.failFirst && call == 1 {
		return nil, errors.New("simulated_registration_failure")
	}
	return map[string]any{"access_token": "PRIVATE-TOKEN", "email": box.Email, "source_type": "chatgpt_web"}, nil
}
func (f *fakeFlow) Close() {}
func (f *fakeFlow) BrowserProfile() BrowserProfile {
	if f.profile.valid() {
		return f.profile
	}
	return browserProfileBase(1440, 900, 8, 4294967296)
}
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
	if strings.Contains(string(public), "PRIVATE") || strings.Contains(string(public), "secret-password") {
		t.Fatalf("secret in snapshot: %s", public)
	}
	if strings.Contains(string(public), "browser_profile") {
		t.Fatalf("private browser profile leaked into snapshot: %s", public)
	}
	if !strings.Contains(string(public), "alias@example.test") {
		t.Fatal("registration task email was not fully exposed in the admin snapshot")
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

func TestFreeEngineCompensationReusesSavedMailbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	mail, flow := &fakeMail{}, &fakeFlow{failFirst: true, profile: browserProfileBase(1728, 1117, 10, 4294967296)}
	var retryProfile *BrowserProfile
	engine := NewFreeEngine(path, func(cfg FreeConfig) (MailSource, RegistrationFlow, error) {
		if cfg.BrowserProfile != nil {
			profile := *cfg.BrowserProfile
			retryProfile = &profile
		}
		return mail, flow, nil
	}, func(context.Context, string, map[string]any) error { return nil }, func(context.Context, string, map[string]any) error { return nil })
	id, err := engine.Start(freeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	waitEngine(t, engine)
	job, _ := engine.getJob(id)
	if job.Status != "registration_pending" || job.Mailbox.Email != "alias@example.test" || len(job.Account) != 0 {
		t.Fatalf("failed registration did not retain mailbox: %+v", job)
	}
	if job.BrowserProfile == nil || job.BrowserProfile.Name != flow.profile.Name {
		t.Fatalf("failed registration did not retain browser profile: %#v", job.BrowserProfile)
	}
	if err := engine.RetryRegistration(freeTestConfig(), id); err != nil {
		t.Fatal(err)
	}
	waitEngine(t, engine)
	job, _ = engine.getJob(id)
	if job.Status != "completed" || !job.Verified || job.Mailbox.Email != "alias@example.test" {
		t.Fatalf("compensation did not complete: %+v", job)
	}
	if mail.calls.Load() != 1 || mail.prepared.Load() != 1 || flow.calls.Load() != 2 {
		t.Fatalf("compensation created a new mailbox or skipped retry: acquire=%d prepare=%d register=%d", mail.calls.Load(), mail.prepared.Load(), flow.calls.Load())
	}
	if retryProfile == nil || retryProfile.Name != flow.profile.Name {
		t.Fatalf("compensation did not reuse browser profile: %#v", retryProfile)
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
	if _, err := engine.ClearFinished(); err == nil {
		t.Fatal("reset running task")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := engine.getJob(id)
	if job.Status != "registration_pending" {
		t.Fatalf("got %s", job.Status)
	}
}

type parallelFlow struct {
	started chan<- struct{}
	release <-chan struct{}
	active  *atomic.Int32
	max     *atomic.Int32
}

func (f *parallelFlow) Register(ctx context.Context, box Mailbox, cfg FreeConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	if err := progress("wait_code"); err != nil {
		return nil, err
	}
	current := f.active.Add(1)
	for {
		previous := f.max.Load()
		if current <= previous || f.max.CompareAndSwap(previous, current) {
			break
		}
	}
	f.started <- struct{}{}
	select {
	case <-f.release:
	case <-ctx.Done():
		f.active.Add(-1)
		return nil, ctx.Err()
	}
	f.active.Add(-1)
	return map[string]any{"access_token": "PRIVATE-TOKEN", "email": box.Email, "source_type": "chatgpt_web"}, nil
}
func (f *parallelFlow) Close() {}

func TestFreeEngineRunsConfiguredTotalWithThreadPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var active, max atomic.Int32
	var created atomic.Int32
	engine := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) {
		created.Add(1)
		return &fakeMail{}, &parallelFlow{started: started, release: release, active: &active, max: &max}, nil
	}, func(context.Context, string, map[string]any) error { return nil }, func(context.Context, string, map[string]any) error { return nil })
	cfg := freeTestConfig()
	cfg["total"], cfg["threads"] = 2, 2
	if _, err := engine.Start(cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("configured workers did not start concurrently")
		}
	}
	if max.Load() != 2 || created.Load() != 2 {
		t.Fatalf("expected two concurrent jobs, max=%d factories=%d", max.Load(), created.Load())
	}
	runningStats := engine.Snapshot()["stats"].(map[string]any)
	if runningStats["threads"] != 2 || runningStats["running"] != 2 {
		t.Fatalf("unexpected running batch stats: %#v", runningStats)
	}
	close(release)
	waitEngine(t, engine)
	snapshot := engine.Snapshot()
	stats := snapshot["stats"].(map[string]any)
	if stats["done"] != 2 || stats["success"] != 2 || stats["running"] != 0 || stats["threads"] != 0 {
		t.Fatalf("unexpected batch stats: %#v", stats)
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
	if _, err := engine.ClearFinished(); err != nil {
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
		func(c map[string]any) { c["threads"] = 0 }, func(c map[string]any) { c["total"] = 0 }, func(c map[string]any) { c["mode"] = "quota" }, func(c map[string]any) { c["proxy"] = "group:foo" },
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
	accepted := freeTestConfig()
	accepted["threads"] = 2
	accepted["total"] = 2
	if _, err := ParseFreeConfig(accepted); err != nil {
		t.Fatalf("configurable total/thread count rejected: %v", err)
	}
}

func TestFreeEngineClearFinishedPreservesRecoveryState(t *testing.T) {
	jobs := []RegistrationJob{
		{ID: "completed", Status: "completed", Imported: true, Verified: true, Mailbox: Mailbox{Email: "done@example.test"}},
		{ID: "failed-empty", Status: "failed"},
		{ID: "cancelled-empty", Status: "cancelled"},
		{ID: "interrupted-empty", Status: "interrupted"},
		{ID: "failed-mailbox", Status: "failed", Mailbox: Mailbox{Email: "retry@example.test"}},
		{ID: "cancelled-mailbox", Status: "cancelled", Mailbox: Mailbox{Email: "retry@example.test"}},
		{ID: "interrupted-mailbox", Status: "interrupted", Mailbox: Mailbox{Email: "retry@example.test"}},
		{ID: "partial-mailbox", Status: "failed", Mailbox: Mailbox{AccountID: "hme-test"}},
		{ID: "failed-account", Status: "failed", Account: map[string]any{"access_token": "saved-result"}},
		{ID: "completed-account", Status: "completed", Account: map[string]any{"access_token": "saved-result"}},
		{ID: "imported", Status: "failed", Imported: true},
		{ID: "verified", Status: "failed", Verified: true},
		{ID: "registration-pending", Status: "registration_pending", Mailbox: Mailbox{Email: "retry@example.test"}},
		{ID: "import-pending", Status: "import_pending", Account: map[string]any{"access_token": "saved-result"}},
		{ID: "verification-pending", Status: "verification_pending", Imported: true, Account: map[string]any{"access_token": "saved-result"}},
		{ID: "queued", Status: "queued"},
		{ID: "running", Status: "running"},
		{ID: "unknown", Status: "future-state"},
	}
	path := filepath.Join(t.TempDir(), "tasks.json")
	engine := NewFreeEngine(path, nil, nil, nil)
	engine.jobs = jobs
	removed, err := engine.ClearFinished()
	if err != nil || removed != 4 {
		t.Fatalf("cleanup removed %d: %v", removed, err)
	}
	want := jobs[4:]
	if !reflect.DeepEqual(engine.jobs, want) {
		t.Fatal("cleanup changed recoverable or unfinished jobs")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted []RegistrationJob
	if err := json.Unmarshal(raw, &persisted); err != nil || !reflect.DeepEqual(persisted, want) {
		t.Fatalf("cleanup did not persist the retained recovery state: %v", err)
	}
	if removed, err := engine.ClearFinished(); err != nil || removed != 0 {
		t.Fatalf("repeated cleanup changed retained jobs: %d, %v", removed, err)
	}
}

func TestFreeEngineClearFinishedWriteFailureKeepsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	engine := NewFreeEngine(path, nil, nil, nil)
	jobs := []RegistrationJob{{ID: "failed", Status: "failed"}, {ID: "pending", Status: "registration_pending", Mailbox: Mailbox{Email: "retry@example.test"}}}
	engine.jobs = jobs
	if err := engine.saveLocked(); err != nil {
		t.Fatal(err)
	}
	engine.path = t.TempDir() // A directory cannot be replaced by the journal file.
	if removed, err := engine.ClearFinished(); err == nil || removed != 0 {
		t.Fatalf("cleanup reported success after storage failure: %d, %v", removed, err)
	}
	if !reflect.DeepEqual(engine.jobs, jobs) {
		t.Fatal("storage failure lost in-memory recovery state")
	}
	reloaded := NewFreeEngine(path, nil, nil, nil)
	if !reflect.DeepEqual(reloaded.jobs, jobs) {
		t.Fatal("storage failure changed the existing journal")
	}
}
