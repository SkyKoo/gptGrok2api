package register

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

type RegistrationJob struct {
	ID        string         `json:"id"`
	Status    string         `json:"status"`
	Stage     string         `json:"stage"`
	Error     string         `json:"error,omitempty"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	Mailbox   Mailbox        `json:"mailbox"`
	Imported  bool           `json:"imported"`
	Verified  bool           `json:"verified"`
	Account   map[string]any `json:"account,omitempty"` // private recovery journal only
	Logs      []JobLog       `json:"logs"`
}
type JobLog struct {
	Time  string `json:"time"`
	Text  string `json:"text"`
	Level string `json:"level"`
}
type FreeFactory func(FreeConfig) (MailSource, RegistrationFlow, error)
type ImportAccount func(context.Context, string, map[string]any) error
type VerifyAccount func(context.Context, string, map[string]any) error

// FreeEngine owns one cancellable task. Durable credentials are recorded before
// import, so retry/restart cannot accidentally start another registration.
type FreeEngine struct {
	mu            sync.Mutex
	path          string
	jobs          []RegistrationJob
	active        string
	cancel        context.CancelFunc
	done          chan struct{}
	loadError     error
	closing       bool
	factory       FreeFactory
	importAccount ImportAccount
	verifyAccount VerifyAccount
}

func NewFreeEngine(path string, factory FreeFactory, importer ImportAccount, verifier VerifyAccount) *FreeEngine {
	e := &FreeEngine{path: path, factory: factory, importAccount: importer, verifyAccount: verifier, jobs: []RegistrationJob{}}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		e.loadError = fail("storage", "journal_unreadable")
		return e
	}
	if err == nil {
		if json.Unmarshal(raw, &e.jobs) != nil || e.jobs == nil {
			e.loadError = fail("storage", "journal_invalid")
			return e
		}
		changed := false
		for i := range e.jobs {
			j := &e.jobs[i]
			if j.Status == "running" || j.Status == "queued" {
				j.Status = "interrupted"
				j.Error = "process_restarted_no_automatic_registration_retry"
				if len(j.Account) > 0 {
					if j.Imported {
						j.Status = "verification_pending"
					} else {
						j.Status = "import_pending"
					}
				}
				j.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				changed = true
			}
		}
		if changed && writeJSON0600(path, e.jobs) != nil {
			e.loadError = fail("storage", "journal_write_failed")
		}
	}
	return e
}

func (e *FreeEngine) Start(raw map[string]any) (string, error) {
	cfg, err := ParseFreeConfig(raw)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loadError != nil {
		return "", e.loadError
	}
	if e.closing {
		return "", fail("task", "shutting_down")
	}
	if e.active != "" {
		return "", fail("task", "already_running")
	}
	// Never evict a pending result just to make space for another registration.
	if len(e.jobs) >= 100 {
		return "", fail("task", "history_full_archive_completed_jobs_first")
	}
	mail, flow, err := e.factory(cfg)
	if err != nil {
		return "", err
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		mail.Close()
		flow.Close()
		return "", fail("task", "random_failed")
	}
	id := hex.EncodeToString(idBytes)
	now := time.Now().UTC().Format(time.RFC3339)
	j := RegistrationJob{ID: id, Status: "queued", Stage: "queued", CreatedAt: now, UpdatedAt: now, Logs: []JobLog{}}
	e.jobs = append(e.jobs, j)
	if err = e.saveLocked(); err != nil {
		e.jobs = e.jobs[:len(e.jobs)-1]
		mail.Close()
		flow.Close()
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	e.active, e.cancel, e.done = id, cancel, make(chan struct{})
	go e.run(ctx, id, cfg, mail, flow)
	return id, nil
}
func (e *FreeEngine) saveLocked() error {
	if writeJSON0600(e.path, e.jobs) != nil {
		return fail("storage", "journal_write_failed")
	}
	return nil
}
func (e *FreeEngine) change(id string, fn func(*RegistrationJob)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.jobs {
		if e.jobs[i].ID == id {
			old := cloneJob(e.jobs[i])
			fn(&e.jobs[i])
			e.jobs[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := e.saveLocked(); err != nil {
				e.jobs[i] = old
				e.loadError = err
				return err
			}
			return nil
		}
	}
	return fail("task", "not_found")
}
func cloneJob(j RegistrationJob) RegistrationJob {
	raw, _ := json.Marshal(j)
	var out RegistrationJob
	_ = json.Unmarshal(raw, &out)
	return out
}
func (e *FreeEngine) progress(id, stage string) error {
	return e.change(id, func(j *RegistrationJob) {
		j.Status = "running"
		j.Stage = stage
		j.Logs = append(j.Logs, JobLog{Time: time.Now().UTC().Format(time.RFC3339), Text: stage, Level: "info"})
		if len(j.Logs) > 60 {
			j.Logs = j.Logs[len(j.Logs)-60:]
		}
	})
}
func (e *FreeEngine) run(ctx context.Context, id string, cfg FreeConfig, mail MailSource, flow RegistrationFlow) {
	defer e.finishActive()
	defer mail.Close()
	defer flow.Close()
	var runErr error
	defer func() {
		if recover() != nil {
			runErr = fail("task", "internal_error")
		}
		if runErr != nil {
			e.failJob(id, runErr)
		}
	}()
	if runErr = e.progress(id, "mailbox"); runErr != nil {
		return
	}
	box, err := mail.Acquire(ctx, id)
	if err != nil {
		runErr = err
		return
	}
	if runErr = e.change(id, func(j *RegistrationJob) { j.Mailbox = box }); runErr != nil {
		return
	}
	account, err := flow.Register(ctx, box, cfg, func(ctx context.Context, after time.Time) (string, error) { return mail.WaitCode(ctx, box, after) }, func(stage string) error { return e.progress(id, stage) })
	if err != nil {
		runErr = err
		return
	}
	if stringValue(account["access_token"]) == "" {
		runErr = fail("session", "missing_access_token")
		return
	}
	account["registration_job_id"] = id
	account["registration_mailbox"] = map[string]any{"provider": "icloud_hme", "account_id": box.AccountID, "email": box.Email}
	account["enabled"] = false
	account["status"] = "待验证"
	if runErr = e.change(id, func(j *RegistrationJob) { j.Account = account; j.Stage = "import"; j.Status = "import_pending" }); runErr != nil {
		return
	}
	runErr = e.importAndVerify(ctx, id)
}
func (e *FreeEngine) getJob(id string) (RegistrationJob, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, j := range e.jobs {
		if j.ID == id {
			return cloneJob(j), nil
		}
	}
	return RegistrationJob{}, fail("task", "not_found")
}
func (e *FreeEngine) importAndVerify(ctx context.Context, id string) error {
	j, err := e.getJob(id)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !j.Imported {
		if err = e.importAccount(ctx, id, j.Account); err != nil {
			return fail("import", "account_write_failed")
		}
		if err = e.change(id, func(j *RegistrationJob) {
			j.Imported = true
			j.Status = "verification_pending"
			j.Stage = "verify"
			j.Error = ""
		}); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = e.verifyAccount(ctx, id, j.Account); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail("verify", "account_unverified")
	}
	return e.change(id, func(j *RegistrationJob) {
		j.Verified = true
		j.Status = "completed"
		j.Stage = "completed"
		j.Error = ""
		j.Account = nil
		j.Logs = append(j.Logs, JobLog{Time: time.Now().UTC().Format(time.RFC3339), Text: "account_imported_and_verified", Level: "success"})
	})
}
func (e *FreeEngine) failJob(id string, err error) {
	_ = e.change(id, func(j *RegistrationJob) {
		j.Error = safeFailure(err)
		j.Status = "failed"
		if errors.Is(err, context.Canceled) {
			j.Status = "cancelled"
		}
		if len(j.Account) > 0 {
			if j.Imported {
				j.Status = "verification_pending"
			} else {
				j.Status = "import_pending"
			}
		}
		j.Logs = append(j.Logs, JobLog{Time: time.Now().UTC().Format(time.RFC3339), Text: j.Error, Level: "error"})
	})
}
func (e *FreeEngine) finishActive() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
	}
	e.active = ""
	e.cancel = nil
	if e.done != nil {
		close(e.done)
		e.done = nil
	}
}
func (e *FreeEngine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
	}
}
func (e *FreeEngine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.closing = true
	if e.cancel != nil {
		e.cancel()
	}
	done := e.done
	e.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RetrySavedResult resumes only import/verification. It never logs in to HME,
// sends another OTP, or creates a new account/alias.
func (e *FreeEngine) RetrySavedResult(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loadError != nil {
		return e.loadError
	}
	if e.closing {
		return fail("task", "shutting_down")
	}
	if e.active != "" {
		return fail("task", "already_running")
	}
	found := false
	for _, j := range e.jobs {
		if j.ID == id && len(j.Account) > 0 && (j.Status == "import_pending" || j.Status == "verification_pending") {
			found = true
		}
	}
	if !found {
		return fail("task", "no_saved_result")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	e.active, e.cancel, e.done = id, cancel, make(chan struct{})
	go func() {
		defer e.finishActive()
		defer func() {
			if recover() != nil {
				e.failJob(id, fail("task", "internal_error"))
			}
		}()
		if err := e.importAndVerify(ctx, id); err != nil {
			e.failJob(id, err)
		}
	}()
	return nil
}
func (e *FreeEngine) Snapshot() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	jobs := []any{}
	logs := []JobLog{}
	success, failed := 0, 0
	for _, j := range e.jobs {
		if j.Status == "completed" {
			success++
		} else if j.ID != e.active {
			failed++
		}
		jobs = append(jobs, map[string]any{"id": j.ID, "status": j.Status, "stage": j.Stage, "error": j.Error, "email": maskEmail(j.Mailbox.Email), "imported": j.Imported, "verified": j.Verified, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt, "can_retry": len(j.Account) > 0 && j.ID != e.active})
		logs = append(logs, j.Logs...)
	}
	if len(logs) > 200 {
		logs = logs[len(logs)-200:]
	}
	running := 0
	if e.active != "" {
		running = 1
	}
	result := map[string]any{"running": running == 1, "active_job_id": e.active, "jobs": jobs, "logs": logs, "stats": map[string]any{"success": success, "fail": failed, "done": success + failed, "running": running, "threads": 1}}
	if e.loadError != nil {
		result["error"] = safeFailure(e.loadError)
	}
	return result
}
func (e *FreeEngine) ResetCompleted() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active != "" {
		return fail("task", "already_running")
	}
	if e.loadError != nil {
		return e.loadError
	}
	previous := e.jobs
	kept := []RegistrationJob{}
	for _, j := range e.jobs {
		if len(j.Account) > 0 || j.Status != "completed" {
			kept = append(kept, j)
		}
	}
	e.jobs = kept
	if err := e.saveLocked(); err != nil {
		e.jobs = previous
		return err
	}
	return nil
}
