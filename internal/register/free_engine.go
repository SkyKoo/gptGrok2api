package register

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type RegistrationJob struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	Stage     string  `json:"stage"`
	Error     string  `json:"error,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	Mailbox   Mailbox `json:"mailbox"`
	// BrowserProfile is private task state. Snapshot deliberately omits it,
	// while compensation retries use it to keep the protocol environment stable.
	BrowserProfile *BrowserProfile           `json:"browser_profile,omitempty"`
	MailboxAPIBase string                    `json:"mailbox_api_base,omitempty"`
	AutoRetry      *RegistrationCompensation `json:"auto_retry,omitempty"`
	Imported       bool                      `json:"imported"`
	Verified       bool                      `json:"verified"`
	Account        map[string]any            `json:"account,omitempty"` // private recovery journal only
	TwoFA          *RegistrationTwoFAState   `json:"twofa,omitempty"`
	// CodexOAuth is a credential-free progress record for the optional Codex
	// OAuth step that can be run after Web Free registration completes.
	CodexOAuth *CodexOAuthTask `json:"codex_oauth,omitempty"`
	Logs       []JobLog        `json:"logs"`
}
type CodexOAuthTask struct {
	SessionID  string `json:"session_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	CPAStatus  string `json:"cpa_sync_status,omitempty"`
	CPAPoolID  string `json:"cpa_pool_id,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}
type JobLog struct {
	Time    string `json:"time"`
	Text    string `json:"text"`
	Level   string `json:"level"`
	Account string `json:"account,omitempty"`
	Action  string `json:"action,omitempty"`
	Detail  string `json:"detail,omitempty"`
}
type FreeFactory func(FreeConfig) (MailSource, RegistrationFlow, error)
type ImportAccount func(context.Context, string, map[string]any) error
type VerifyAccount func(context.Context, string, map[string]any) error

type activeRegistration struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// FreeEngine owns one cancellable registration run. A run uses a bounded worker
// pool; each worker has its own mailbox source and protocol client. Durable
// credentials are recorded before import, so retry/restart cannot accidentally
// create another account.
type FreeEngine struct {
	mu                sync.Mutex
	path              string
	jobs              []RegistrationJob
	active            map[string]*activeRegistration
	runCancel         context.CancelFunc
	runDone           chan struct{}
	runThreads        int
	loadError         error
	compensationError string
	closing           bool
	factory           FreeFactory
	importAccount     ImportAccount
	verifyAccount     VerifyAccount
}

func NewFreeEngine(path string, factory FreeFactory, importer ImportAccount, verifier VerifyAccount) *FreeEngine {
	e := &FreeEngine{path: path, factory: factory, importAccount: importer, verifyAccount: verifier, jobs: []RegistrationJob{}, active: map[string]*activeRegistration{}}
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
			if j.TwoFA != nil && j.TwoFA.Status == "running" {
				j.TwoFA.Status, j.TwoFA.Stage = "interrupted", "interrupted"
				j.TwoFA.Message = "服务重启中断，请在账号管理中核对或补充开启 2FA"
				j.TwoFA.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				if len(j.Account) > 0 {
					j.Account["twofa_setup"] = registrationTwoFAAccountState(j.TwoFA)
				}
				changed = true
			}
			if j.Status == "running" || j.Status == "queued" {
				j.Status = "interrupted"
				j.Error = "process_restarted_no_automatic_registration_retry"
				if j.Mailbox.Email != "" && len(j.Account) == 0 {
					j.Status = "registration_pending"
				} else if len(j.Account) > 0 {
					if j.Imported {
						j.Status = "verification_pending"
					} else {
						j.Status = "import_pending"
					}
				}
				j.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				recoverRegistrationCompensation(j, time.Now())
				if j.AutoRetry != nil && j.AutoRetry.State == "waiting" {
					j.Error = "process_restarted_compensation_delayed"
				}
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
	if e.loadError != nil {
		e.mu.Unlock()
		return "", e.loadError
	}
	if e.closing {
		e.mu.Unlock()
		return "", fail("task", "shutting_down")
	}
	if e.runCancel != nil || len(e.active) > 0 {
		e.mu.Unlock()
		return "", fail("task", "already_running")
	}
	id, err := e.createJobLocked()
	if err != nil {
		e.mu.Unlock()
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.runCancel = cancel
	e.runDone = make(chan struct{})
	e.runThreads = cfg.Threads
	e.mu.Unlock()
	go e.runBatch(ctx, cfg, id)
	return id, nil
}

func (e *FreeEngine) createJobLocked() (string, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fail("task", "random_failed")
	}
	id := hex.EncodeToString(idBytes)
	now := time.Now().UTC().Format(time.RFC3339)
	e.jobs = append(e.jobs, RegistrationJob{ID: id, Status: "queued", Stage: "queued", CreatedAt: now, UpdatedAt: now, Logs: []JobLog{}})
	if err := e.saveLocked(); err != nil {
		e.jobs = e.jobs[:len(e.jobs)-1]
		return "", err
	}
	return id, nil
}

func (e *FreeEngine) runBatch(ctx context.Context, cfg FreeConfig, firstID string) {
	workers := cfg.Threads
	if workers > cfg.Total {
		workers = cfg.Total
	}
	work := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case id, ok := <-work:
					if !ok {
						return
					}
					e.runTask(ctx, id, cfg)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	enqueue := func(id string) bool {
		select {
		case work <- id:
			return true
		case <-ctx.Done():
			e.cancelQueued(id, ctx.Err())
			return false
		}
	}
	if !enqueue(firstID) {
		close(work)
		wg.Wait()
		e.finishRun()
		return
	}
	for i := 1; i < cfg.Total; i++ {
		e.mu.Lock()
		id, err := e.createJobLocked()
		e.mu.Unlock()
		if err != nil {
			e.cancelRun()
			break
		}
		if !enqueue(id) {
			break
		}
	}
	close(work)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		e.cancelQueued("", err)
	}
	e.finishRun()
}

func (e *FreeEngine) runTask(parent context.Context, id string, cfg FreeConfig) {
	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	done := make(chan struct{})
	e.mu.Lock()
	e.active[id] = &activeRegistration{cancel: cancel, done: done}
	e.mu.Unlock()
	defer e.finishTask(id)
	defer func() {
		if recover() != nil {
			e.failJob(id, fail("task", "internal_error"))
		}
	}()
	mail, flow, err := e.factory(cfg)
	if err != nil {
		e.failJob(id, err)
		return
	}
	defer mail.Close()
	defer flow.Close()
	if provider, ok := flow.(RegistrationProfileProvider); ok {
		profile := provider.BrowserProfile()
		if err := e.change(id, func(j *RegistrationJob) { j.BrowserProfile = &profile; j.MailboxAPIBase = cfg.HME.BaseURL }); err != nil {
			e.failJob(id, err)
			return
		}
	}
	e.run(ctx, id, cfg, mail, flow)
}

func (e *FreeEngine) finishRun() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.runCancel != nil {
		e.runCancel()
	}
	e.runCancel = nil
	e.runThreads = 0
	if e.runDone != nil {
		close(e.runDone)
		e.runDone = nil
	}
}

func (e *FreeEngine) finishTask(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if task, ok := e.active[id]; ok {
		delete(e.active, id)
		if task.cancel != nil {
			task.cancel()
		}
		close(task.done)
	}
}

func (e *FreeEngine) cancelRun() {
	e.mu.Lock()
	cancel := e.runCancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (e *FreeEngine) cancelQueued(id string, reason error) {
	if reason == nil {
		reason = context.Canceled
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	for i := range e.jobs {
		if id != "" && e.jobs[i].ID != id {
			continue
		}
		if e.jobs[i].Status != "queued" {
			continue
		}
		e.jobs[i].Status = "cancelled"
		e.jobs[i].Error = safeFailure(reason)
		appendRegistrationLog(&e.jobs[i], "cancel", e.jobs[i].Error, "error")
		e.jobs[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		changed = true
	}
	if changed && e.saveLocked() != nil {
		e.loadError = fail("storage", "journal_write_failed")
	}
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

// appendRegistrationLog keeps the durable registration journal useful to an
// operator. Text remains compatible with older clients while the structured
// account/action/detail fields let the UI explain which account was affected
// and what the engine was doing, including compensation attempts.
func appendRegistrationLog(job *RegistrationJob, action, detail, level string) {
	if job == nil {
		return
	}
	action = strings.TrimSpace(action)
	detail = strings.TrimSpace(detail)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	job.Logs = append(job.Logs, JobLog{Time: now, Text: detail, Level: level, Account: strings.TrimSpace(job.Mailbox.Email), Action: action, Detail: detail})
	if len(job.Logs) > 60 {
		job.Logs = job.Logs[len(job.Logs)-60:]
	}
}

func appendRegistrationLogAt(job *RegistrationJob, at, action, detail, level string) {
	if job == nil {
		return
	}
	at = strings.TrimSpace(at)
	if at == "" {
		at = time.Now().UTC().Format(time.RFC3339Nano)
	}
	detail = strings.TrimSpace(detail)
	job.Logs = append(job.Logs, JobLog{Time: at, Text: detail, Level: level, Account: strings.TrimSpace(job.Mailbox.Email), Action: strings.TrimSpace(action), Detail: detail})
	if len(job.Logs) > 60 {
		job.Logs = job.Logs[len(job.Logs)-60:]
	}
}

func attachRegistrationLogAccount(job *RegistrationJob) {
	if job == nil {
		return
	}
	account := strings.TrimSpace(job.Mailbox.Email)
	if account == "" {
		return
	}
	for i := range job.Logs {
		if strings.TrimSpace(job.Logs[i].Account) == "" {
			job.Logs[i].Account = account
		}
	}
}

func (e *FreeEngine) progress(id, stage string, profile ...BrowserProfile) error {
	return e.change(id, func(j *RegistrationJob) {
		if len(profile) > 0 {
			j.BrowserProfile = &profile[0]
		}
		j.Status = "running"
		j.Stage = stage
		appendRegistrationLog(j, stage, stage, "info")
	})
}
func (e *FreeEngine) run(ctx context.Context, id string, cfg FreeConfig, mail MailSource, flow RegistrationFlow) {
	var runErr error
	defer func() {
		if recover() != nil {
			runErr = fail("task", "internal_error")
		}
		if runErr != nil {
			if ctx.Err() != nil {
				runErr = ctx.Err()
			}
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
	if runErr = e.change(id, func(j *RegistrationJob) {
		j.Mailbox = box
		attachRegistrationLogAccount(j)
		appendRegistrationLog(j, "mailbox_acquired", "邮箱资源已分配，开始注册", "info")
	}); runErr != nil {
		return
	}
	runErr = e.runWithMailbox(ctx, id, cfg, mail, flow, box)
}

// runWithMailbox performs the ChatGPT portion using the supplied alias. The
// caller must have already acquired or prepared the mailbox session.
func (e *FreeEngine) runWithMailbox(ctx context.Context, id string, cfg FreeConfig, mail MailSource, flow RegistrationFlow, box Mailbox) error {
	account, err := flow.Register(ctx, box, cfg, func(ctx context.Context, after time.Time) (string, error) { return mail.WaitCode(ctx, box, after) }, func(stage string) error {
		// Persist the resolved environment before auth requests, including retries
		// that fail before returning an account. Compensation uses this same path.
		if provider, ok := flow.(RegistrationProfileProvider); ok {
			return e.progress(id, stage, provider.BrowserProfile())
		}
		return e.progress(id, stage)
	})
	if err != nil {
		return err
	}
	if stringValue(account["access_token"]) == "" {
		return fail("session", "missing_access_token")
	}
	if provider, ok := flow.(RegistrationProfileProvider); ok {
		account["browser_profile"] = provider.BrowserProfile()
	}
	account["registration_job_id"] = id
	account["registration_mailbox"] = map[string]any{
		"provider":          "icloud_hme",
		"source_id":         box.SourceID,
		"source_type":       firstNonEmpty(box.SourceType, "hme"),
		"source_mailbox_id": firstNonEmpty(box.SourceMailboxID, box.ID),
		"id":                box.ID,
		"account_id":        box.AccountID,
		"email":             box.Email,
	}
	account["enabled"] = false
	account["status"] = "待验证"
	if err = e.change(id, func(j *RegistrationJob) {
		j.Account = account
		j.Stage = "import"
		j.Status = "import_pending"
		finishRegistrationCompensation(j, "saved_result")
	}); err != nil {
		return err
	}
	if committer, ok := mail.(interface {
		Commit(context.Context, string, Mailbox) error
	}); ok {
		if err := committer.Commit(ctx, id, box); err != nil {
			return err
		}
	}
	if cfg.AutoTwoFA {
		if err = e.enableRegistrationTwoFA(ctx, id, cfg, mail, flow, box); err != nil {
			return err
		}
	}
	return e.importAndVerify(ctx, id)
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

// SetCodexOAuth persists the user-visible state of the optional Codex step.
// Tokens and other credentials never enter the registration journal.
func (e *FreeEngine) SetCodexOAuth(id string, task *CodexOAuthTask) error {
	return e.change(id, func(j *RegistrationJob) {
		if task == nil {
			j.CodexOAuth = nil
			return
		}
		copy := *task
		j.CodexOAuth = &copy
	})
}

// BrowserProfileForAccount reads only the matching account's private profile.
// A journal ID alone is insufficient: an imported account must not inherit
// another mailbox's registration environment.
func (e *FreeEngine) BrowserProfileForAccount(id, email string) (BrowserProfile, bool) {
	if id == "" || email == "" {
		return BrowserProfile{}, false
	}
	job, err := e.getJob(id)
	if err != nil || !strings.EqualFold(job.Mailbox.Email, email) {
		return BrowserProfile{}, false
	}
	return ParseBrowserProfile(job.BrowserProfile)
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
		finishRegistrationCompensation(j, "completed")
		appendRegistrationLog(j, "completed", "账号已入库并通过校验", "success")
	})
}
func (e *FreeEngine) failJob(id string, err error) {
	_ = e.change(id, func(j *RegistrationJob) {
		j.Error = safeFailure(err)
		if j.TwoFA != nil && j.TwoFA.Status == "running" {
			j.TwoFA.Status, j.TwoFA.Stage = "failed", "interrupted"
			j.TwoFA.Error = safeFailure(err)
			j.TwoFA.Message = "2FA 步骤已中断，请在账号管理中核对或补充开启"
			j.TwoFA.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if len(j.Account) > 0 {
				j.Account["twofa_setup"] = registrationTwoFAAccountState(j.TwoFA)
			}
		}
		j.Status = "failed"
		if errors.Is(err, context.Canceled) {
			j.Status = "cancelled"
		}
		if j.Mailbox.Email != "" && len(j.Account) == 0 {
			j.Status = "registration_pending"
		} else if len(j.Account) > 0 {
			if j.Imported {
				j.Status = "verification_pending"
			} else {
				j.Status = "import_pending"
			}
		}
		if errors.Is(err, context.Canceled) && e.closing && j.AutoRetry != nil && j.AutoRetry.State == "running" {
			// Shutdown keeps the durable reservation; an explicit Stop stays manual.
			recoverRegistrationCompensation(j, time.Now())
		} else {
			planRegistrationCompensation(j, err, time.Now())
		}
		action := j.Stage
		if index := strings.Index(j.Error, ":"); index > 0 {
			action = strings.TrimSpace(j.Error[:index])
		}
		appendRegistrationLog(j, action, j.Error, "error")
	})
}
func (e *FreeEngine) Stop() {
	e.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(e.active)+1)
	if e.runCancel != nil {
		cancels = append(cancels, e.runCancel)
	}
	for _, task := range e.active {
		if task.cancel != nil {
			cancels = append(cancels, task.cancel)
		}
	}
	e.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
func (e *FreeEngine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.closing = true
	cancels := make([]context.CancelFunc, 0, len(e.active)+1)
	done := make([]<-chan struct{}, 0, len(e.active)+1)
	if e.runCancel != nil {
		cancels = append(cancels, e.runCancel)
	}
	if e.runDone != nil {
		done = append(done, e.runDone)
	}
	for _, task := range e.active {
		if task.cancel != nil {
			cancels = append(cancels, task.cancel)
		}
		if task.done != nil {
			done = append(done, task.done)
		}
	}
	e.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, channel := range done {
		select {
		case <-channel:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
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
	if e.runCancel != nil || len(e.active) > 0 {
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
	done := make(chan struct{})
	e.active[id] = &activeRegistration{cancel: cancel, done: done}
	go func() {
		defer e.finishTask(id)
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

// RetryRegistration reuses the alias already recorded for a failed job. It
// re-authenticates HME, but never calls Acquire or creates another alias.
func (e *FreeEngine) RetryRegistration(raw map[string]any, id string) error {
	cfg, err := ParseFreeConfig(raw)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.compensationIdleLocked(); err != nil {
		return err
	}
	for i := range e.jobs {
		if e.jobs[i].ID == id {
			return e.startCompensationLocked(i, cfg, false, time.Now())
		}
	}
	return fail("task", "not_found")
}

// RegistrationHistoryPageSize bounds API/SSE payloads, not retained history.
const RegistrationHistoryPageSize = 50

func (e *FreeEngine) Snapshot() map[string]any { return e.SnapshotPage(1) }

// SnapshotPage keeps full-history counters but sends only one fixed-size page.
// Page 1 contains the newest jobs; jobs within a page retain creation order.
func (e *FreeEngine) SnapshotPage(page int) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := len(e.jobs)
	pages := maxInt(1, (total+RegistrationHistoryPageSize-1)/RegistrationHistoryPageSize)
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	end := total - (page-1)*RegistrationHistoryPageSize
	start := maxInt(0, end-RegistrationHistoryPageSize)
	jobs := make([]any, 0, end-start)
	logs := make([]JobLog, 0, 200)
	success, failed := 0, 0
	activeIDs := make([]string, 0, len(e.active))
	for _, j := range e.jobs {
		if _, ok := e.active[j.ID]; ok {
			activeIDs = append(activeIDs, j.ID)
		}
	}
	for index, j := range e.jobs {
		if j.Status == "completed" {
			success++
		} else if j.Status == "failed" || j.Status == "cancelled" || j.Status == "interrupted" {
			failed++
		}
		if index < start || index >= end {
			continue
		}
		_, active := e.active[j.ID]
		jobs = append(jobs, map[string]any{"id": j.ID, "status": j.Status, "stage": j.Stage, "error": j.Error, "email": j.Mailbox.Email, "imported": j.Imported, "verified": j.Verified, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt, "can_retry": len(j.Account) > 0 && !active, "can_retry_registration": j.Mailbox.Email != "" && len(j.Account) == 0 && !j.Imported && !j.Verified && !active, "can_codex_oauth": j.Verified && j.Mailbox.Email != "" && !active, "codex_oauth": j.CodexOAuth, "twofa": registrationTwoFASummary(j.TwoFA), "auto_retry": compensationSummary(j.AutoRetry)})
	}
	// Compensation appends to an old job, so job order is not event order.
	// Sort the bounded live window by event time before returning it; otherwise
	// a later retry can be hidden by the frontend's tail limit.
	for _, job := range e.jobs {
		logs = append(logs, job.Logs...)
	}
	sort.SliceStable(logs, func(i, j int) bool {
		return logs[i].Time < logs[j].Time
	})
	if len(logs) > 200 {
		logs = logs[len(logs)-200:]
	}
	running := len(activeIDs)
	if e.runCancel != nil && running == 0 {
		running = 0
	}
	activeID := ""
	if len(activeIDs) > 0 {
		activeID = activeIDs[0]
	}
	threads := e.runThreads
	if threads == 0 && running > 0 {
		threads = running
	}
	result := map[string]any{"running": e.runCancel != nil || running > 0, "active_job_id": activeID, "active_job_ids": activeIDs, "jobs": jobs, "jobs_total": total, "jobs_page": page, "jobs_page_size": RegistrationHistoryPageSize, "logs": logs, "stats": map[string]any{"success": success, "fail": failed, "done": success + failed, "running": running, "threads": threads}}
	result["compensation_error"] = e.compensationError
	if e.loadError != nil {
		result["error"] = safeFailure(e.loadError)
	}
	return result
}

// ClearFinished removes successful jobs and terminal failures with no recovery
// state. Pending mailboxes and saved accounts must survive history cleanup.
func (e *FreeEngine) ClearFinished() (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.runCancel != nil || len(e.active) > 0 {
		return 0, fail("task", "already_running")
	}
	if e.loadError != nil {
		return 0, e.loadError
	}
	previous := e.jobs
	kept := []RegistrationJob{}
	for _, j := range e.jobs {
		removable := false
		if len(j.Account) == 0 {
			switch j.Status {
			case "completed":
				removable = true
			case "failed", "cancelled", "interrupted":
				removable = j.Mailbox == (Mailbox{}) && !j.Imported && !j.Verified
			}
		}
		if !removable {
			kept = append(kept, j)
		}
	}
	removed := len(previous) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	e.jobs = kept
	if err := e.saveLocked(); err != nil {
		e.jobs = previous
		return 0, err
	}
	return removed, nil
}
