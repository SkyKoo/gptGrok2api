package register

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// AdobeEngine journals each attempt before work. Rejected/unsubmitted creation
// retries registration, accepted creation continues verification, and unknown
// creation pauses. Saved identities only resume import and verification.
type AdobeEngine struct {
	mu                         sync.Mutex
	mailboxMu                  sync.Mutex
	path, secret               string
	jobs                       []RegistrationJob
	factory                    AdobeFactory
	mediaFactory               func(AdobeConfig) AdobeRegistrationFlow
	importer                   ImportAccount
	verifier                   VerifyAccount
	ProxyRouting               *ProxyRouting
	cancel                     context.CancelFunc
	done                       chan struct{}
	active                     int
	closing                    bool
	stopRequested              bool
	loadError                  error
	compensationError          string
	reregistrationAccountCheck func(string) (bool, error)
	activeJobs                 map[string]bool
	mailboxCleanupError        string
	prepareMailboxReservation  func(context.Context, RegistrationJob) error
	releaseMailboxReservation  func(context.Context, RegistrationJob) error
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
		if j.Status == "running" || j.Status == "queued" || j.AutoRetry != nil && (j.AutoRetry.State == "running" || j.AutoRetry.State == "manual_running") {
			j.Status = "registration_pending"
			j.Error = "process_restarted_compensation_delayed"
			if len(j.Account) > 0 {
				j.Status = "import_pending"
				if j.Imported {
					j.Status = "verification_pending"
				}
			}
			recoverAdobeRegistrationCompensation(j, time.Now())
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

// A separate internal factory lets verified identities finish media preparation
// without acquiring another registration proxy or reopening an HME source.
func (e *AdobeEngine) SetMediaFlowFactory(factory func(AdobeConfig) AdobeRegistrationFlow) {
	e.mediaFactory = factory
}
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
		e.jobs = append(e.jobs, RegistrationJob{ID: ids[i], Status: "queued", Stage: "queued", CreatedAt: now, UpdatedAt: now, Logs: []JobLog{}, AdobeProfile: profile, MailboxAPIBase: cfg.HME.BaseURL, AdobeRegistration: &AdobeRegistrationState{BrowserProfileID: ids[i]}})
	}
	if err := e.saveLocked(); err != nil {
		e.jobs = e.jobs[:before]
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.stopRequested = false
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
					e.releaseFailedAdobeMailboxReservation(id, nil)
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
	if e.activeJobs == nil {
		e.activeJobs = map[string]bool{}
	}
	e.activeJobs[id] = true
	e.mu.Unlock()
	var activeMail MailSource
	defer func() {
		if recover() != nil {
			e.failJob(id, fail("task", "internal_error"))
		}
		e.releaseFailedAdobeMailboxReservation(id, activeMail)
		e.mu.Lock()
		delete(e.activeJobs, id)
		e.active--
		e.mu.Unlock()
	}()
	failTask := func(err error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		e.failJob(id, err)
	}
	job, err := e.getJob(id)
	if err == nil && len(job.Account) > 0 {
		if err := e.prepareAdobeMailboxReservation(ctx, id); err != nil {
			failTask(err)
			return
		}
		if err := e.importAndVerify(ctx, id); err != nil {
			failTask(err)
		}
		return
	}
	if err != nil || job.AdobeProfile == nil {
		failTask(fail("task", "adobe_profile_missing"))
		return
	}
	cfg.Profile = job.AdobeProfile.fields()
	cfg.BrowserProfileID = adobeBrowserProfileID(job)
	if job.ParentJobID != "" || (e.mediaFactory != nil && adobeMediaPreparationPending(job)) {
		if err := e.prepareAdobeMailboxReservation(ctx, id); err != nil {
			failTask(err)
			return
		}
	}
	if e.mediaFactory != nil && adobeMediaPreparationPending(job) {
		if err := e.change(id, markAdobeMediaPreparation); err != nil {
			failTask(err)
			return
		}
		job, err = e.getJob(id)
		if err != nil {
			failTask(err)
			return
		}
		cfg.mediaOnly, cfg.legacyMedia = true, job.AdobeRegistration.LegacyMedia
		cfg.RecoveryMode = "resume_session"
		flow := e.mediaFactory(cfg)
		if flow == nil {
			failTask(fail("adobe", "media_flow_unavailable"))
			return
		}
		defer flow.Close()
		cfg.diagnosticProgress = e.adobeDiagnosticProgress(id)
		account, err := flow.Register(ctx, id, job.Mailbox, cfg, nil, func(stage string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return e.change(id, func(j *RegistrationJob) {
				if j.Stage != stage || j.Status != "running" {
					appendRegistrationLog(j, stage, adobeRegistrationStageText(stage), "info")
				}
				j.Status, j.Stage = "running", stage
			})
		})
		if err == nil {
			err = e.completeRegisteredAccount(ctx, id, job, job.Mailbox, account, nil)
		}
		if err != nil {
			failTask(err)
		}
		return
	}
	if job.ParentJobID != "" {
		cfg.RecoveryMode = adobeRegistrationRecoveryMode(job)
		if job.AdobeRegistration != nil && job.AdobeRegistration.ProxyRecorded {
			cfg.Proxy = job.AdobeRegistration.Proxy
		}
		if cfg.RecoveryMode == "resume_session" && job.ProxyBinding == nil && (job.AdobeRegistration == nil || !job.AdobeRegistration.ProxyRecorded) {
			failTask(fail("task", "saved_environment_missing"))
			return
		}
	}
	release, err := e.ProxyRouting.acquire(ctx, id, "adobe", &cfg.Proxy, e.getJob, e.change)
	if err != nil {
		failTask(err)
		return
	}
	defer release()
	if cfg.Proxy == "" {
		cfg.Proxy = "direct"
	}
	if job.AdobeRegistration != nil && job.AdobeRegistration.ProxyRecorded && cfg.Proxy != job.AdobeRegistration.Proxy {
		failTask(fail("proxy", "saved_registration_egress_changed"))
		return
	}
	if err := e.change(id, func(j *RegistrationJob) {
		ensureAdobeRegistrationState(j)
		j.AdobeRegistration.Proxy, j.AdobeRegistration.ProxyRecorded = cfg.Proxy, true
	}); err != nil {
		failTask(err)
		return
	}
	mail, flow, err := e.factory(cfg)
	if err != nil {
		failTask(err)
		return
	}
	activeMail = mail
	defer mail.Close()
	defer flow.Close()
	box := job.Mailbox
	if job.ParentJobID == "" {
		box, err = e.acquire(ctx, id, mail)
	} else {
		err = prepareSavedAdobeMailbox(ctx, mailboxClaimJobID(job), box, mail)
	}
	if err != nil {
		failTask(err)
		return
	}
	cfg.creationProgress = func(observed adobeCreationObservation, registrationResponse bool) error {
		return e.change(id, func(j *RegistrationJob) {
			recordAdobeCreationOutcome(j, observed, registrationResponse)
		})
	}
	cfg.countryProgress = func(country adobeCountrySelection) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return e.recordRegistrationCountry(id, country)
	}
	cfg.diagnosticProgress = e.adobeDiagnosticProgress(id)
	cfg.mediaProgress = func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := e.getJob(id)
		if err != nil || adobeCreationOutcome(current) != "accepted" {
			return fail("adobe", "media_identity_not_verified")
		}
		return e.change(id, func(j *RegistrationJob) {
			ensureAdobeRegistrationState(j)
			j.AdobeRegistration.MediaPending, j.AdobeRegistration.LegacyMedia = true, false
			j.Stage = "adobe_media_pending"
			appendRegistrationLog(j, j.Stage, adobeRegistrationStageText(j.Stage), "info")
		})
	}
	cfg.startupProgress = func(event browserqueue.StartupEvent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !event.Valid() {
			return nil
		}
		return e.change(id, func(j *RegistrationJob) {
			ensureAdobeRegistrationState(j)
			// A generic queue notification may precede the first detailed event.
			// Keep one start line while always retaining completion/failure timing.
			duplicateStart := event.Status == "started" && j.Stage == event.Stage && j.Status == "running"
			j.Status, j.Stage = "running", event.Stage
			if !duplicateStart {
				level := "info"
				if event.Status == "failed" {
					level = "error"
				}
				appendRegistrationLog(j, event.Stage+"_"+event.Status, adobeStartupEventText(event), level)
			}
		})
	}
	account, err := flow.Register(ctx, id, box, cfg, func(ctx context.Context, after time.Time) (string, error) {
		return waitAdobeVerificationCode(ctx, mail, box, after, cfg.MailTimeout)
	}, func(stage string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return e.change(id, func(j *RegistrationJob) {
			ensureAdobeRegistrationState(j)
			if adobeStageMayHaveSubmitted(stage) {
				j.AdobeRegistration.FlowStarted = true
			}
			if stage == "adobe_creation_submitted" || stage == "adobe_original_submission_recorded" {
				j.AdobeRegistration.SubmissionStarted = true
			}
			changed := j.Stage != stage || j.Status != "running"
			j.Status = "running"
			j.Stage = stage
			if changed {
				appendRegistrationLog(j, stage, adobeRegistrationStageText(stage), "info")
			}
		})
	})
	if err != nil {
		failTask(err)
		return
	}
	if err = e.completeRegisteredAccount(ctx, id, job, box, account, mail); err != nil {
		failTask(err)
	}
}

func (e *AdobeEngine) adobeDiagnosticProgress(id string) func(adobeRegistrationDiagnostics) error {
	return func(diagnostics adobeRegistrationDiagnostics) error {
		detail := adobeFailureDiagnosticText(diagnostics)
		if detail == "" {
			return nil
		}
		return e.change(id, func(j *RegistrationJob) {
			appendRegistrationLog(j, "adobe_registration_diagnostics", detail, "info")
		})
	}
}

func (e *AdobeEngine) completeRegisteredAccount(ctx context.Context, id string, job RegistrationJob, box Mailbox, account map[string]any, mail MailSource) error {
	account["platform"], account["registration_job_id"], account["enabled"] = "adobe", mailboxClaimJobID(job), false
	account["registration_mailbox"] = map[string]any{"provider": "icloud_hme", "platform": "adobe", "source_id": box.SourceID, "source_type": box.SourceType, "source_mailbox_id": box.SourceMailboxID, "id": box.ID, "account_id": box.AccountID, "email": box.Email}
	if err := e.change(id, func(j *RegistrationJob) {
		UseDefaultAccountProxy(account)
		j.Account = account
		j.Status = "import_pending"
		j.Stage = "import"
		if j.AdobeRegistration != nil {
			j.AdobeRegistration.MediaPending, j.AdobeRegistration.LegacyMedia = false, false
		}
	}); err != nil {
		return err
	}
	// Production import validates the saved account before atomically committing
	// its exact local claim. Legacy embedders keep their MailSource contract.
	if e.prepareMailboxReservation == nil {
		if m, ok := mail.(interface {
			Commit(context.Context, string, Mailbox) error
		}); ok {
			if err := m.Commit(ctx, "adobe:"+stringValue(account["account_id"]), box); err != nil {
				return err
			}
		}
	}
	return e.importAndVerify(ctx, id)
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
	} else {
		err = e.prepareAdobeMailboxReservation(ctx, id)
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
		if err := e.importer(ctx, mailboxClaimJobID(j), j.Account); err != nil {
			return fail("import", "account_write_failed")
		}
		if err := e.change(id, func(j *RegistrationJob) { j.Imported = true; j.Status = "verification_pending"; j.Stage = "verify" }); err != nil {
			return err
		}
	}
	if e.verifier == nil {
		return fail("verify", "account_verifier_not_configured")
	}
	if err := e.verifier(ctx, mailboxClaimJobID(j), j.Account); err != nil {
		return fail("verify", "account_unverified")
	}
	return e.change(id, func(j *RegistrationJob) {
		j.Verified = true
		j.Account = nil
		j.Status = "completed"
		j.Stage = "completed"
		j.Error = ""
		finishRegistrationCompensation(j, "completed")
		appendRegistrationLog(j, "completed", "Adobe 账号已入库并通过校验", "success")
	})
}
func (e *AdobeEngine) failJob(id string, err error) {
	_ = e.change(id, func(j *RegistrationJob) {
		e.ProxyRouting.recordFailure(j, err)
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
		if errors.Is(err, context.Canceled) && e.closing && !e.stopRequested {
			j.Error = "process_restarted_compensation_delayed"
			recoverAdobeRegistrationCompensation(j, time.Now())
		} else {
			planAdobeRegistrationCompensation(j, err, time.Now())
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
	e.stopRequested = false
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
	e.stopRequested = true
	cancel := e.cancel
	before := e.jobs
	e.jobs = append([]RegistrationJob(nil), e.jobs...)
	changed := false
	for i, job := range e.jobs {
		if adobeNeedsCompensationPlan(job) || job.AutoRetry != nil && (job.AutoRetry.State == "waiting" || job.AutoRetry.State == "running" || job.AutoRetry.State == "manual_running") {
			job = cloneJob(job)
			if job.AutoRetry == nil {
				job.AutoRetry = &RegistrationCompensation{}
			}
			finishRegistrationCompensation(&job, "manual")
			job.AutoRetry.Reason = "cancelled"
			appendRegistrationLog(&job, "compensation_stopped", "已停止自动补偿，保留邮箱、历史次数与原提交记录", "info")
			e.jobs[i], changed = job, true
		}
	}
	if changed && e.loadError == nil {
		if err := e.saveLocked(); err != nil {
			e.jobs, e.loadError = before, fail("storage", "journal_write_failed")
		}
	}
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
		mode := adobeRegistrationRecoveryMode(j)
		canReregister := e.loadError == nil && !e.closing && e.adobeReregistrationEligibleLocked(i) == nil
		reregisterNotBefore := ""
		if canReregister {
			if due := e.adobeReregistrationNotBeforeLocked(j.Mailbox.Email); !due.IsZero() {
				reregisterNotBefore = due.UTC().Format(time.RFC3339Nano)
			}
		}
		blocked := e.adobeMailboxCreationBlocked(j)
		eligible := adobeCompensationPending(j) && !blocked && e.cancel == nil && (j.AutoRetry == nil || j.AutoRetry.Reason != "duplicate_mailbox_retry")
		state := adobeCompensationSummary(j, e.adobeMailboxAttemptsLocked(j.Mailbox.Email))
		hint := adobeRegistrationReviewHint(j.Error)
		if blocked {
			hint = "本次注册失败；缺少 Adobe 创建结果，已停止自动重试。"
		} else if state != nil && (state.State == "waiting" || state.State == "running") {
			switch mode {
			case "retry_registration":
				hint = "系统将复用原邮箱并创建新的浏览器会话，重新执行注册；原失败记录、次数与冷却时间保留。"
			case "resume_session":
				hint = "创建账户已被接受，系统将沿用原会话继续邮箱验证和凭据采集。"
			case "saved_result":
				hint = "账号结果已保存，系统仅继续入库和验证，不重新注册。"
			}
		}
		if j.Status == "running" || j.Status == "queued" || j.Status == "completed" || j.RetryJobID != "" {
			hint = ""
			if mode == "result_uncertain" {
				mode = ""
			}
		}
		jobs = append(jobs, map[string]any{"id": j.ID, "parent_job_id": j.ParentJobID, "root_job_id": j.RootJobID, "retry_job_id": j.RetryJobID, "status": j.Status, "stage": j.Stage, "error": j.Error, "email": j.Mailbox.Email, "imported": j.Imported, "verified": j.Verified, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt, "can_retry": len(j.Account) > 0 && eligible, "can_retry_registration": eligible && mode == "retry_registration", "can_resume_registration": eligible && mode == "resume_session", "can_reregister": canReregister, "reregister_not_before": reregisterNotBefore, "creation_outcome": adobeCreationOutcome(j), "recovery_mode": mode, "auto_retry": compensationSummary(state), "review_hint": hint})
	}
	// Older tasks can receive new compensation logs. Order events before the
	// live-window limit; RFC3339Nano strings with unequal precision are not
	// lexically chronological. Invalid legacy times remain first and stable.
	sort.SliceStable(logs, func(i, j int) bool {
		left, _ := time.Parse(time.RFC3339Nano, logs[i].Time)
		right, _ := time.Parse(time.RFC3339Nano, logs[j].Time)
		return left.Before(right)
	})
	if len(logs) > 200 {
		logs = logs[len(logs)-200:]
	}
	result := map[string]any{"running": e.cancel != nil, "jobs": jobs, "jobs_total": total, "jobs_page": page, "jobs_page_size": RegistrationHistoryPageSize, "logs": logs, "compensation_error": e.compensationError, "mailbox_cleanup_error": e.mailboxCleanupError, "stats": map[string]any{"success": success, "fail": failed, "done": success + failed, "running": e.active, "threads": e.active}}
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
		if j.Status == "completed" && j.ParentJobID == "" && j.RetryJobID == "" && (j.AutoRetry == nil || j.AutoRetry.Attempts == 0) {
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
