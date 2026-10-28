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

type GrokRegistrationFlow interface {
	Register(context.Context, Mailbox, GrokConfig, func(context.Context, time.Time) (string, error), Progress) (map[string]any, error)
	Close()
}

type GrokFactory func(GrokConfig) (MailSource, GrokRegistrationFlow, error)

func waitGrokCode(mail MailSource, ctx context.Context, box Mailbox, after time.Time) (string, error) {
	if targetAware, ok := mail.(interface {
		WaitCodeFor(context.Context, Mailbox, time.Time, func(string) bool) (string, error)
	}); ok {
		return targetAware.WaitCodeFor(ctx, box, after, isGrokSender)
	}
	// Keep custom MailSource implementations used by embedders and tests
	// compatible with the original interface.
	return mail.WaitCode(ctx, box, after)
}

// GrokEngine mirrors FreeEngine's durable task lifecycle while keeping its
// journal and account callbacks separate from OpenAI.
type GrokEngine struct {
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
	factory           GrokFactory
	importAccount     ImportAccount
	verifyAccount     VerifyAccount
}

func NewGrokEngine(path string, factory GrokFactory, importer ImportAccount, verifier VerifyAccount) *GrokEngine {
	e := &GrokEngine{path: path, factory: factory, importAccount: importer, verifyAccount: verifier, jobs: []RegistrationJob{}, active: map[string]*activeRegistration{}}
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
			job := &e.jobs[i]
			if job.Status == "running" || job.Status == "queued" {
				job.Status = "interrupted"
				job.Stage = "interrupted"
				job.Error = "process_restarted_no_automatic_registration_retry"
				if job.Mailbox.Email != "" && len(job.Account) == 0 {
					job.Status = "registration_pending"
				} else if len(job.Account) > 0 {
					if job.Imported {
						job.Status = "verification_pending"
					} else {
						job.Status = "import_pending"
					}
				}
				recoverRegistrationCompensation(job, time.Now())
				job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				changed = true
			}
		}
		if changed && e.saveLocked() != nil {
			e.loadError = fail("storage", "journal_write_failed")
		}
	}
	return e
}

func (e *GrokEngine) Start(raw map[string]any) (string, error) {
	cfg, err := ParseGrokConfig(raw)
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
	if e.runCancel != nil || len(e.active) > 0 {
		return "", fail("task", "already_running")
	}
	id, err := e.createJobLocked()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.runCancel, e.runDone, e.runThreads = cancel, make(chan struct{}), cfg.Threads
	go e.runBatch(ctx, cfg, id)
	return id, nil
}

func (e *GrokEngine) createJobLocked() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fail("task", "random_failed")
	}
	id := hex.EncodeToString(raw)
	now := time.Now().UTC().Format(time.RFC3339)
	e.jobs = append(e.jobs, RegistrationJob{ID: id, Status: "queued", Stage: "queued", CreatedAt: now, UpdatedAt: now, Logs: []JobLog{}})
	if err := e.saveLocked(); err != nil {
		e.jobs = e.jobs[:len(e.jobs)-1]
		return "", err
	}
	return id, nil
}

func (e *GrokEngine) runBatch(parent context.Context, cfg GrokConfig, firstID string) {
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
					e.runTask(parent, id, cfg)
				case <-parent.Done():
					return
				}
			}
		}()
	}
	enqueue := func(id string) bool {
		select {
		case work <- id:
			return true
		case <-parent.Done():
			e.cancelQueued(id, parent.Err())
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
		if err != nil || !enqueue(id) {
			if err != nil {
				e.cancelRun()
			}
			break
		}
	}
	close(work)
	wg.Wait()
	if parent.Err() != nil {
		e.cancelQueued("", parent.Err())
	}
	e.finishRun()
}

func (e *GrokEngine) runTask(parent context.Context, id string, cfg GrokConfig) {
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

func (e *GrokEngine) run(ctx context.Context, id string, cfg GrokConfig, mail MailSource, flow GrokRegistrationFlow) {
	if err := e.progress(id, "mailbox", flow); err != nil {
		e.failJob(id, err)
		return
	}
	box, err := mail.Acquire(ctx, id)
	if err != nil {
		e.failJob(id, err)
		return
	}
	if err := e.change(id, func(j *RegistrationJob) {
		j.Mailbox = box
		attachRegistrationLogAccount(j)
		appendRegistrationLog(j, "mailbox_acquired", "邮箱资源已分配，开始注册", "info")
	}); err != nil {
		e.failJob(id, err)
		return
	}
	account, err := flow.Register(ctx, box, cfg, func(waitCtx context.Context, after time.Time) (string, error) {
		return waitGrokCode(mail, waitCtx, box, after)
	}, func(stage string) error {
		return e.progress(id, stage, flow)
	})
	if err != nil {
		e.failJob(id, err)
		return
	}
	if stringValue(account["sso"]) == "" {
		e.failJob(id, fail("sso", "missing_sso"))
		return
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
	if err := e.change(id, func(j *RegistrationJob) {
		j.Account = account
		j.Stage = "import"
		j.Status = "import_pending"
		finishRegistrationCompensation(j, "saved_result")
	}); err != nil {
		e.failJob(id, err)
		return
	}
	if committer, ok := mail.(interface {
		Commit(context.Context, string, Mailbox) error
	}); ok {
		if err := committer.Commit(ctx, id, box); err != nil {
			e.failJob(id, err)
			return
		}
	}
	if err := e.importAndVerify(ctx, id); err != nil {
		e.failJob(id, err)
		return
	}
}

func (e *GrokEngine) importAndVerify(ctx context.Context, id string) error {
	job, err := e.getJob(id)
	if err != nil {
		return err
	}
	if !job.Imported {
		if e.importAccount == nil {
			return fail("import", "account_writer_not_configured")
		}
		if err := e.importAccount(ctx, id, job.Account); err != nil {
			return fail("import", "account_write_failed")
		}
		if err := e.change(id, func(j *RegistrationJob) {
			j.Imported = true
			j.Status = "verification_pending"
			j.Stage = "verify"
			j.Error = ""
		}); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.verifyAccount == nil {
		return fail("verify", "account_verifier_not_configured")
	}
	if err := e.verifyAccount(ctx, id, job.Account); err != nil {
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

func (e *GrokEngine) getJob(id string) (RegistrationJob, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, job := range e.jobs {
		if job.ID == id {
			return cloneJob(job), nil
		}
	}
	return RegistrationJob{}, fail("task", "not_found")
}

func (e *GrokEngine) progress(id, stage string, flow GrokRegistrationFlow) error {
	return e.change(id, func(job *RegistrationJob) {
		job.Status = "running"
		job.Stage = stage
		if provider, ok := flow.(RegistrationProfileProvider); ok {
			profile := provider.BrowserProfile()
			job.BrowserProfile = &profile
		}
		appendRegistrationLog(job, stage, stage, "info")
	})
}

func (e *GrokEngine) change(id string, fn func(*RegistrationJob)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for index := range e.jobs {
		if e.jobs[index].ID != id {
			continue
		}
		previous := cloneJob(e.jobs[index])
		fn(&e.jobs[index])
		e.jobs[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := e.saveLocked(); err != nil {
			e.jobs[index] = previous
			e.loadError = err
			return err
		}
		return nil
	}
	return fail("task", "not_found")
}

func (e *GrokEngine) saveLocked() error {
	return writeJSON0600(e.path, e.jobs)
}

func (e *GrokEngine) failJob(id string, err error) {
	_ = e.change(id, func(job *RegistrationJob) {
		job.Error = safeFailure(err)
		if errors.Is(err, context.Canceled) {
			job.Status = "cancelled"
		} else {
			job.Status = "failed"
		}
		if job.Mailbox.Email != "" && len(job.Account) == 0 {
			job.Status = "registration_pending"
		} else if len(job.Account) > 0 {
			if job.Imported {
				job.Status = "verification_pending"
			} else {
				job.Status = "import_pending"
			}
		}
		planRegistrationCompensation(job, err, time.Now())
		action := job.Stage
		if index := strings.Index(job.Error, ":"); index > 0 {
			action = strings.TrimSpace(job.Error[:index])
		}
		appendRegistrationLog(job, action, job.Error, "error")
	})
}

func (e *GrokEngine) cancelQueued(id string, reason error) {
	if reason == nil {
		reason = context.Canceled
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	for index := range e.jobs {
		if id != "" && e.jobs[index].ID != id || e.jobs[index].Status != "queued" {
			continue
		}
		e.jobs[index].Status = "cancelled"
		e.jobs[index].Error = safeFailure(reason)
		e.jobs[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		changed = true
	}
	if changed {
		_ = e.saveLocked()
	}
}

func (e *GrokEngine) finishTask(id string) {
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

func (e *GrokEngine) finishRun() {
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

func (e *GrokEngine) cancelRun() {
	e.mu.Lock()
	cancel := e.runCancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (e *GrokEngine) Stop() {
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

func (e *GrokEngine) Shutdown(ctx context.Context) error {
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

func (e *GrokEngine) RetrySavedResult(id string) error {
	e.mu.Lock()
	if e.loadError != nil {
		e.mu.Unlock()
		return e.loadError
	}
	if e.runCancel != nil || len(e.active) > 0 {
		e.mu.Unlock()
		return fail("task", "already_running")
	}
	if e.closing {
		e.mu.Unlock()
		return fail("task", "shutting_down")
	}
	var index = -1
	for i, job := range e.jobs {
		if job.ID == id && len(job.Account) > 0 && (job.Status == "import_pending" || job.Status == "verification_pending") {
			index = i
			break
		}
	}
	if index < 0 {
		e.mu.Unlock()
		return fail("task", "no_saved_result")
	}
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	done := make(chan struct{})
	e.mu.Lock()
	e.active[id] = &activeRegistration{cancel: cancel, done: done}
	e.mu.Unlock()
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

// RetryRegistration reuses the mailbox already recorded for a failed Grok
// task, but starts a fresh browser environment for the new attempt. It never
// acquires another alias or creates a second mailbox.
func (e *GrokEngine) RetryRegistration(raw map[string]any, id string) error {
	cfg, err := ParseGrokConfig(raw)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.loadError != nil {
		e.mu.Unlock()
		return e.loadError
	}
	if e.closing {
		e.mu.Unlock()
		return fail("task", "shutting_down")
	}
	if e.runCancel != nil || len(e.active) > 0 {
		e.mu.Unlock()
		return fail("task", "already_running")
	}
	job, found := RegistrationJob{}, false
	for _, candidate := range e.jobs {
		if candidate.ID == id && candidate.Mailbox.Email != "" && len(candidate.Account) == 0 && !candidate.Imported && !candidate.Verified && candidate.Status == "registration_pending" {
			job, found = cloneJob(candidate), true
			break
		}
	}
	if !found {
		e.mu.Unlock()
		return fail("task", "no_saved_mailbox")
	}
	// Do not restore job.BrowserProfile here. A Grok retry must get a new
	// protocol fingerprint, HTTP client and browser-worker session; the saved
	// profile remains private history for the original attempt/account.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	done := make(chan struct{})
	e.active[id] = &activeRegistration{cancel: cancel, done: done}
	e.mu.Unlock()
	if err := e.progress(id, "compensation", nil); err != nil {
		e.finishTask(id)
		return err
	}
	go e.runSavedRegistration(ctx, id, job, cfg)
	return nil
}

func (e *GrokEngine) runSavedRegistration(ctx context.Context, id string, job RegistrationJob, cfg GrokConfig) {
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
	if err := prepareSavedGrokMailbox(ctx, id, job.Mailbox, mail); err != nil {
		e.failJob(id, err)
		return
	}
	e.registerSaved(ctx, id, job, cfg, mail, flow)
}

func prepareSavedGrokMailbox(ctx context.Context, jobID string, box Mailbox, mail MailSource) error {
	preparer, ok := mail.(MailSessionPreparer)
	if !ok {
		return fail("mail", "resume_not_supported")
	}
	if withJob, ok := mail.(interface {
		PrepareFor(context.Context, string, Mailbox) error
	}); ok {
		return withJob.PrepareFor(ctx, jobID, box)
	}
	return preparer.Prepare(ctx, box)
}

func (e *GrokEngine) registerSaved(ctx context.Context, id string, job RegistrationJob, cfg GrokConfig, mail MailSource, flow GrokRegistrationFlow) {
	account, err := flow.Register(ctx, job.Mailbox, cfg, func(waitCtx context.Context, after time.Time) (string, error) {
		return waitGrokCode(mail, waitCtx, job.Mailbox, after)
	}, func(stage string) error {
		return e.progress(id, stage, flow)
	})
	if err != nil {
		e.failJob(id, err)
		return
	}
	if stringValue(account["sso"]) == "" {
		e.failJob(id, fail("sso", "missing_sso"))
		return
	}
	account["registration_job_id"] = id
	account["registration_mailbox"] = map[string]any{
		"provider":          "icloud_hme",
		"source_id":         job.Mailbox.SourceID,
		"source_type":       firstNonEmpty(job.Mailbox.SourceType, "hme"),
		"source_mailbox_id": firstNonEmpty(job.Mailbox.SourceMailboxID, job.Mailbox.ID),
		"id":                job.Mailbox.ID,
		"account_id":        job.Mailbox.AccountID,
		"email":             job.Mailbox.Email,
	}
	account["enabled"] = false
	account["status"] = "待验证"
	if err := e.change(id, func(current *RegistrationJob) {
		current.Account = account
		current.Stage = "import"
		current.Status = "import_pending"
	}); err != nil {
		e.failJob(id, err)
		return
	}
	if committer, ok := mail.(interface {
		Commit(context.Context, string, Mailbox) error
	}); ok {
		if err := committer.Commit(ctx, id, job.Mailbox); err != nil {
			e.failJob(id, err)
			return
		}
	}
	if err := e.importAndVerify(ctx, id); err != nil {
		e.failJob(id, err)
	}
}

func (e *GrokEngine) Snapshot() map[string]any { return e.SnapshotPage(1) }

func (e *GrokEngine) SnapshotPage(page int) map[string]any {
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
	for _, job := range e.jobs {
		if _, ok := e.active[job.ID]; ok {
			activeIDs = append(activeIDs, job.ID)
		}
	}
	for index, job := range e.jobs {
		if job.Status == "completed" {
			success++
		} else if job.Status == "failed" || job.Status == "cancelled" || job.Status == "interrupted" {
			failed++
		}
		if index < start || index >= end {
			continue
		}
		_, active := e.active[job.ID]
		jobs = append(jobs, map[string]any{"id": job.ID, "status": job.Status, "stage": job.Stage, "error": job.Error, "email": job.Mailbox.Email, "imported": job.Imported, "verified": job.Verified, "created_at": job.CreatedAt, "updated_at": job.UpdatedAt, "can_retry": len(job.Account) > 0 && !active, "can_retry_registration": job.Mailbox.Email != "" && len(job.Account) == 0 && !job.Imported && !job.Verified && !active, "twofa": nil, "auto_retry": nil})
	}
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
	threads := e.runThreads
	if threads == 0 && running > 0 {
		threads = running
	}
	result := map[string]any{"running": e.runCancel != nil || running > 0, "active_job_id": "", "active_job_ids": activeIDs, "jobs": jobs, "jobs_total": total, "jobs_page": page, "jobs_page_size": RegistrationHistoryPageSize, "logs": logs, "stats": map[string]any{"success": success, "fail": failed, "done": success + failed, "running": running, "threads": threads}, "compensation_error": e.compensationError}
	if len(activeIDs) > 0 {
		result["active_job_id"] = activeIDs[0]
	}
	if e.loadError != nil {
		result["error"] = safeFailure(e.loadError)
	}
	return result
}

func (e *GrokEngine) ClearFinished() (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.runCancel != nil || len(e.active) > 0 {
		return 0, fail("task", "already_running")
	}
	if e.loadError != nil {
		return 0, e.loadError
	}
	kept := make([]RegistrationJob, 0, len(e.jobs))
	removed := 0
	for _, job := range e.jobs {
		removable := len(job.Account) == 0 && ((job.Status == "completed") || ((job.Status == "failed" || job.Status == "cancelled" || job.Status == "interrupted") && job.Mailbox == (Mailbox{}) && !job.Imported && !job.Verified))
		if removable {
			removed++
			continue
		}
		kept = append(kept, job)
	}
	if removed == 0 {
		return 0, nil
	}
	e.jobs = kept
	return removed, e.saveLocked()
}
