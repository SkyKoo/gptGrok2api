package register

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"os"
	"strings"
	"sync"
	"time"
)

// AdobeEngine persists an encrypted result before import. A failed/unknown
// signup is never resubmitted: only the already captured result can recover.
type AdobeEngine struct {
	mu           sync.Mutex
	mailboxMu    sync.Mutex
	path, secret string
	jobs         []RegistrationJob
	factory      AdobeFactory
	importer     ImportAccount
	verifier     VerifyAccount
	ProxyRouting *ProxyRouting
	cancel       context.CancelFunc
	done         chan struct{}
	active       int
	closing      bool
	loadError    error
}

func NewAdobeEngine(path, secret string, factory AdobeFactory, importer ImportAccount, verifier VerifyAccount) *AdobeEngine {
	e := &AdobeEngine{path: path, secret: secret, factory: factory, importer: importer, verifier: verifier, jobs: []RegistrationJob{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return e
	}
	if err != nil {
		e.loadError = fail("storage", "journal_unreadable")
		return e
	}
	var envelope struct {
		Data []byte `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		e.loadError = fail("storage", "journal_invalid")
		return e
	}
	plain, err := adobe.Open(secret, envelope.Data)
	if err != nil || json.Unmarshal(plain, &e.jobs) != nil {
		e.loadError = fail("storage", "journal_invalid")
		return e
	}
	changed := false
	for i := range e.jobs {
		j := &e.jobs[i]
		if j.Status == "running" || j.Status == "queued" {
			j.Status = "registration_pending"
			j.Error = "process_restarted_no_automatic_registration_retry"
			if len(j.Account) > 0 {
				j.Status = "import_pending"
				if j.Imported {
					j.Status = "verification_pending"
				}
			}
			changed = true
		}
	}
	if changed {
		e.loadError = e.saveLocked()
	}
	return e
}
func (e *AdobeEngine) saveLocked() error {
	plain, err := json.Marshal(e.jobs)
	if err != nil {
		return fail("storage", "journal_invalid")
	}
	sealed, err := adobe.Seal(e.secret, plain)
	if err != nil {
		return fail("storage", "journal_encrypt_failed")
	}
	return writeJSON0600(e.path, map[string]any{"version": 1, "data": sealed})
}
func (e *AdobeEngine) change(id string, fn func(*RegistrationJob)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loadError != nil {
		return e.loadError
	}
	for i := range e.jobs {
		if e.jobs[i].ID == id {
			before := cloneJob(e.jobs[i])
			fn(&e.jobs[i])
			e.jobs[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := e.saveLocked(); err != nil {
				e.jobs[i] = before
				e.loadError = fail("storage", "journal_write_failed")
				return e.loadError
			}
			return nil
		}
	}
	return fail("task", "not_found")
}
func (e *AdobeEngine) getJob(id string) (RegistrationJob, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, j := range e.jobs {
		if j.ID == id {
			return cloneJob(j), nil
		}
	}
	return RegistrationJob{}, fail("task", "not_found")
}
func (e *AdobeEngine) SetProxyRouting(r *ProxyRouting) { e.ProxyRouting = r; r.restore(e.jobs) }
func (e *AdobeEngine) Start(raw map[string]any) (string, error) {
	cfg, err := ParseAdobeConfig(raw)
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
	if e.cancel != nil {
		return "", fail("task", "already_running")
	}
	ids := make([]string, cfg.Total)
	started := time.Now().UTC()
	now := started.Format(time.RFC3339)
	before := len(e.jobs)
	for i := range ids {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			e.jobs = e.jobs[:before]
			return "", fail("task", "random_failed")
		}
		profile, err := newAdobeRegistrationProfile(started, rand.Reader)
		if err != nil {
			e.jobs = e.jobs[:before]
			return "", err
		}
		ids[i] = hex.EncodeToString(b)
		e.jobs = append(e.jobs, RegistrationJob{ID: ids[i], Status: "queued", Stage: "queued", CreatedAt: now, UpdatedAt: now, Logs: []JobLog{}, AdobeProfile: profile})
	}
	if err := e.saveLocked(); err != nil {
		e.jobs = e.jobs[:before]
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan struct{})
	go e.batch(ctx, cfg, ids)
	return ids[0], nil
}
func (e *AdobeEngine) batch(ctx context.Context, cfg AdobeConfig, ids []string) {
	defer e.finish()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < min(cfg.Threads, cfg.Total); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					e.failJob(id, ctx.Err())
					continue
				}
				e.runTask(ctx, cfg, id)
			}
		}()
	}
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
}
func (e *AdobeEngine) finish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
	}
	e.cancel = nil
	if e.done != nil {
		close(e.done)
		e.done = nil
	}
}
func (e *AdobeEngine) runTask(parent context.Context, cfg AdobeConfig, id string) {
	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()
	e.mu.Lock()
	e.active++
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.active--
		e.mu.Unlock()
		if recover() != nil {
			e.failJob(id, fail("task", "internal_error"))
		}
	}()
	job, err := e.getJob(id)
	if err != nil || job.AdobeProfile == nil {
		e.failJob(id, fail("task", "adobe_profile_missing"))
		return
	}
	cfg.Profile = job.AdobeProfile.fields()
	release, err := e.ProxyRouting.acquire(ctx, id, "adobe", &cfg.Proxy, e.getJob, e.change)
	if err != nil {
		e.failJob(id, err)
		return
	}
	defer release()
	mail, flow, err := e.factory(cfg)
	if err != nil {
		e.failJob(id, err)
		return
	}
	defer mail.Close()
	defer flow.Close()
	box, err := e.acquire(ctx, id, mail)
	if err != nil {
		e.failJob(id, err)
		return
	}
	defer func() {
		job, _ := e.getJob(id)
		// Only pre-submission failures may release a reservation. Once the
		// Worker flow begins, its outcome must be reviewed before reuse.
		if len(job.Account) > 0 || job.Imported || (job.Stage != "mailbox" && job.Stage != "waiting_browser" && job.Stage != "starting_browser") {
			return
		}
		if releaser, ok := mail.(interface {
			Release(context.Context, Mailbox, string) error
		}); ok {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = releaser.Release(releaseCtx, box, "registration_not_submitted")
		}
	}()
	account, err := flow.Register(ctx, id, box, cfg, func(ctx context.Context, after time.Time) (string, error) {
		waitCtx, cancel := context.WithTimeout(ctx, cfg.MailTimeout)
		defer cancel()
		if m, ok := mail.(interface {
			WaitCodeFor(context.Context, Mailbox, time.Time, func(string) bool) (string, error)
		}); ok {
			return m.WaitCodeFor(waitCtx, box, after, isAdobeSender)
		}
		return "", fail("mail", "adobe_sender_filter_required")
	}, func(stage string) error {
		return e.change(id, func(j *RegistrationJob) {
			j.Status = "running"
			j.Stage = stage
			appendRegistrationLog(j, stage, stage, "info")
		})
	})
	if err != nil {
		e.failJob(id, err)
		return
	}
	account["platform"], account["registration_job_id"], account["enabled"] = "adobe", id, false
	account["registration_mailbox"] = map[string]any{"provider": "icloud_hme", "platform": "adobe", "source_id": box.SourceID, "source_type": box.SourceType, "source_mailbox_id": box.SourceMailboxID, "id": box.ID, "account_id": box.AccountID, "email": box.Email}
	if err = e.change(id, func(j *RegistrationJob) {
		attachProxyBinding(j, account)
		j.Account = account
		j.Status = "import_pending"
		j.Stage = "import"
	}); err != nil {
		e.failJob(id, err)
		return
	}
	// Commit the upstream identity, not the registration task identifier.
	if m, ok := mail.(interface {
		Commit(context.Context, string, Mailbox) error
	}); ok {
		if err = m.Commit(ctx, "adobe:"+stringValue(account["account_id"]), box); err != nil {
			e.failJob(id, err)
			return
		}
	}
	if err = e.importAndVerify(ctx, id); err != nil {
		e.failJob(id, err)
	}
}
func (e *AdobeEngine) acquire(ctx context.Context, id string, mail MailSource) (Mailbox, error) {
	e.mailboxMu.Lock()
	defer e.mailboxMu.Unlock()
	e.mu.Lock()
	excluded := []string{}
	blocked := map[string]bool{}
	for _, j := range e.jobs {
		if j.Mailbox.Email != "" {
			key := strings.ToLower(j.Mailbox.Email)
			blocked[key] = true
			excluded = append(excluded, key)
		}
	}
	e.mu.Unlock()
	var box Mailbox
	var err error
	if m, ok := mail.(interface {
		AcquireExcluding(context.Context, string, []string) (Mailbox, error)
	}); ok {
		box, err = m.AcquireExcluding(ctx, id, excluded)
	} else {
		box, err = mail.Acquire(ctx, id)
	}
	if err != nil {
		return box, err
	}
	if blocked[strings.ToLower(box.Email)] {
		return box, e.releaseUnstartedMailbox(mail, box, fail("mailbox", "existing_registration_requires_review"))
	}
	err = e.change(id, func(j *RegistrationJob) {
		j.Mailbox = box
		j.Stage = "mailbox"
		j.Status = "running"
		attachRegistrationLogAccount(j)
	})
	if err != nil {
		err = e.releaseUnstartedMailbox(mail, box, err)
	}
	return box, err
}

// Allocation can succeed before the journal accepts the mailbox. No browser
// work has started here, so release this source's platform/job reservation even
// if the request context was cancelled or the journal is now unwritable.
func (e *AdobeEngine) releaseUnstartedMailbox(mail MailSource, box Mailbox, cause error) error {
	releaser, ok := mail.(interface {
		Release(context.Context, Mailbox, string) error
	})
	code := "reservation_release_unavailable"
	if ok {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := releaser.Release(ctx, box, "registration_not_submitted"); err == nil {
			return cause
		}
		code = "reservation_release_failed"
	}
	failure := &Failure{Stage: "mailbox", Code: code, Operation: safeFailure(cause)}
	e.mu.Lock()
	if e.loadError != nil {
		// failJob cannot persist after a journal error; keep both failures in
		// the engine error returned by Snapshot instead of hiding the leak.
		e.loadError = failure
	}
	e.mu.Unlock()
	return failure
}
func (e *AdobeEngine) importAndVerify(ctx context.Context, id string) error {
	j, err := e.getJob(id)
	if err != nil {
		return err
	}
	if len(j.Account) == 0 {
		return fail("task", "no_saved_result")
	}
	if !j.Imported {
		if e.importer == nil {
			return fail("import", "account_writer_not_configured")
		}
		if err := e.importer(ctx, id, j.Account); err != nil {
			return fail("import", "account_write_failed")
		}
		if err := e.change(id, func(j *RegistrationJob) { j.Imported = true; j.Status = "verification_pending"; j.Stage = "verify" }); err != nil {
			return err
		}
	}
	if e.verifier == nil {
		return fail("verify", "account_verifier_not_configured")
	}
	if err := e.verifier(ctx, id, j.Account); err != nil {
		return fail("verify", "account_unverified")
	}
	return e.change(id, func(j *RegistrationJob) {
		j.Verified = true
		j.Account = nil
		j.Status = "completed"
		j.Stage = "completed"
		j.Error = ""
		appendRegistrationLog(j, "completed", "Adobe 账号已入库并通过校验", "success")
	})
}
func (e *AdobeEngine) failJob(id string, err error) {
	_ = e.change(id, func(j *RegistrationJob) {
		j.Error = safeFailure(err)
		j.Status = "failed"
		if j.Mailbox.Email != "" {
			j.Status = "registration_pending"
		}
		if len(j.Account) > 0 {
			j.Status = "import_pending"
			if j.Imported {
				j.Status = "verification_pending"
			}
		}
		appendRegistrationLog(j, j.Stage, j.Error, "error")
	})
}
func (e *AdobeEngine) RetrySavedResult(id string) error {
	e.mu.Lock()
	if e.loadError != nil {
		err := e.loadError
		e.mu.Unlock()
		return err
	}
	if e.closing || e.cancel != nil {
		e.mu.Unlock()
		return fail("task", "already_running")
	}
	found := false
	for _, j := range e.jobs {
		if j.ID == id && len(j.Account) > 0 {
			found = true
		}
	}
	if !found {
		e.mu.Unlock()
		return fail("task", "no_saved_result")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	e.cancel = cancel
	e.done = make(chan struct{})
	e.mu.Unlock()
	go func() {
		defer e.finish()
		if err := e.importAndVerify(ctx, id); err != nil {
			e.failJob(id, err)
		}
	}()
	return nil
}
func (e *AdobeEngine) Stop() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (e *AdobeEngine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.closing = true
	cancel, done := e.cancel, e.done
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (e *AdobeEngine) Snapshot() map[string]any { return e.SnapshotPage(1) }
func (e *AdobeEngine) SnapshotPage(page int) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := len(e.jobs)
	pages := max(1, (total+RegistrationHistoryPageSize-1)/RegistrationHistoryPageSize)
	page = max(1, min(page, pages))
	end := total - (page-1)*RegistrationHistoryPageSize
	start := max(0, end-RegistrationHistoryPageSize)
	jobs := []any{}
	logs := []JobLog{}
	success, failed := 0, 0
	for i, j := range e.jobs {
		if j.Status == "completed" {
			success++
		} else if j.Status != "running" && j.Status != "queued" {
			failed++
		}
		logs = append(logs, j.Logs...)
		if i < start || i >= end {
			continue
		}
		jobs = append(jobs, map[string]any{"id": j.ID, "status": j.Status, "stage": j.Stage, "error": j.Error, "email": j.Mailbox.Email, "imported": j.Imported, "verified": j.Verified, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt, "can_retry": len(j.Account) > 0 && e.cancel == nil, "can_retry_registration": false})
	}
	if len(logs) > 200 {
		logs = logs[len(logs)-200:]
	}
	result := map[string]any{"running": e.cancel != nil, "jobs": jobs, "jobs_total": total, "jobs_page": page, "jobs_page_size": RegistrationHistoryPageSize, "logs": logs, "stats": map[string]any{"success": success, "fail": failed, "done": success + failed, "running": e.active, "threads": e.active}}
	if e.loadError != nil {
		result["error"] = safeFailure(e.loadError)
	}
	return result
}
func (e *AdobeEngine) ClearFinished() (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		return 0, fail("task", "already_running")
	}
	if e.loadError != nil {
		return 0, e.loadError
	}
	kept := []RegistrationJob{}
	removed := 0
	for _, j := range e.jobs {
		if j.Status == "completed" {
			removed++
		} else {
			kept = append(kept, j)
		}
	}
	before := e.jobs
	e.jobs = kept
	if err := e.saveLocked(); err != nil {
		e.jobs = before
		return 0, err
	}
	return removed, nil
}
