package register

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 最终提交后的断流/超时无法证明上游未建号，不能重新提交注册。
func grokRegistrationResultUncertain(job RegistrationJob) bool {
	return job.Stage == "signup" || job.Stage == "sso" || strings.HasPrefix(job.Error, "signup:") || strings.HasPrefix(job.Error, "sso:")
}

func holdUncertainGrokRegistration(job *RegistrationJob) {
	if savedRegistrationMailbox(*job) && grokRegistrationResultUncertain(*job) && job.AutoRetry != nil && job.AutoRetry.State == "waiting" {
		finishRegistrationCompensation(job, "manual")
		job.AutoRetry.Reason = "registration_result_uncertain"
	}
}

func planGrokRegistrationCompensation(job *RegistrationJob, err error, now time.Time) {
	planRegistrationCompensation(job, err, now)
	if job.AutoRetry != nil && grokRegistrationRateLimited(*job) {
		due := now.Add(registrationRetryDelay(job.Error, now, compensationDelays[min(max(job.AutoRetry.Attempts, 0), len(compensationDelays)-1)]))
		if old, err := time.Parse(time.RFC3339Nano, job.AutoRetry.NotBeforeAt); err != nil || due.After(old) {
			job.AutoRetry.NotBeforeAt = due.UTC().Format(time.RFC3339Nano)
		}
	}
	holdUncertainGrokRegistration(job)
}

func recoverGrokRegistrationCompensation(job *RegistrationJob, now time.Time) {
	recoverRegistrationCompensation(job, now)
	holdUncertainGrokRegistration(job)
}

// 旧版曾把可重试错误强制保存为 grok_manual_retry。只接纳这一类历史
// 记录和无状态的临时失败；人工停止、明确拒绝和已耗尽预算保持原决定。
func grokNeedsCompensationPlan(job RegistrationJob) bool {
	if !savedRegistrationMailbox(job) {
		return false
	}
	if job.AutoRetry != nil && job.AutoRetry.State == "waiting" && (registrationReviewReason(job.Error) != "" || grokRegistrationResultUncertain(job)) {
		return true
	}
	return transientRegistrationFailure(job.Error) && (job.AutoRetry == nil || job.AutoRetry.State == "manual" && job.AutoRetry.Reason == "grok_manual_retry")
}

// RunCompensation 使用 Grok 渠道当前配置，复用原邮箱，且与手动注册共用运行锁。
func (e *GrokEngine) RunCompensation(raw map[string]any, now time.Time) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loadError != nil {
		return false, e.loadError
	}
	if e.closing {
		return false, fail("task", "shutting_down")
	}
	if e.runCancel != nil || len(e.active) > 0 {
		return false, fail("task", "already_running")
	}
	e.compensationError = ""
	if !strings.EqualFold(stringValue(raw["target"]), "grok") {
		return false, nil
	}
	hasPending := false
	for _, job := range e.jobs {
		if grokNeedsCompensationPlan(job) || savedRegistrationMailbox(job) && job.AutoRetry != nil && job.AutoRetry.State == "waiting" {
			hasPending = true
			break
		}
	}
	if !hasPending {
		return false, nil
	}
	cfg, err := ParseGrokConfig(raw)
	if err != nil {
		e.compensationError = safeFailure(err)
		return false, err
	}

	// 迁移从观察时刻重新等待完整冷却期，先落盘再允许调度，避免升级后集中重放。
	var before []RegistrationJob
	for i, job := range e.jobs {
		if !grokNeedsCompensationPlan(job) {
			continue
		}
		if before == nil {
			before = append([]RegistrationJob(nil), e.jobs...)
		}
		job = cloneJob(job)
		planGrokRegistrationCompensation(&job, nil, now)
		e.jobs[i] = job
	}
	migrated := before != nil
	if before == nil {
		before = append([]RegistrationJob(nil), e.jobs...)
	}
	normalized := e.normalizeMailboxRetriesLocked(now)
	if normalized || migrated {
		if err := e.saveLocked(); err != nil {
			e.jobs = before
			e.loadError = err
			return false, err
		}
	}
	selected := -1
	var oldest time.Time
	for i, job := range e.jobs {
		if !savedRegistrationMailbox(job) || job.AutoRetry == nil || job.AutoRetry.State != "waiting" {
			continue
		}
		due, err := time.Parse(time.RFC3339Nano, job.AutoRetry.NextAttemptAt)
		if err != nil || job.AutoRetry.Attempts < 0 || job.AutoRetry.Attempts >= len(compensationDelays) {
			continue
		}
		if job.Mailbox.AccountID != cfg.HME.AccountID || job.MailboxAPIBase != "" && job.MailboxAPIBase != cfg.HME.BaseURL {
			e.compensationError = "config: saved_mailbox_source_unavailable"
			continue
		}
		if job.BrowserProfile == nil || !job.BrowserProfile.valid() {
			continue
		}
		// 上游限流可能同时保留更长的任务出口冷却，不提前创建新尝试。
		if job.ProxyBinding != nil && job.ProxyBinding.BlockedUntil.After(due) {
			due = job.ProxyBinding.BlockedUntil
		}
		if now.Before(due) {
			continue
		}
		if selected < 0 || due.Before(oldest) {
			selected, oldest = i, due
		}
	}
	if selected < 0 {
		return false, nil
	}
	if err := e.startGrokCompensationLocked(selected, cfg, true, now); err != nil {
		e.compensationError = safeFailure(err)
		if e.loadError == nil {
			previous := cloneJob(e.jobs[selected])
			finishRegistrationCompensation(&e.jobs[selected], "manual")
			e.jobs[selected].AutoRetry.Reason = "failure_requires_review"
			if safeFailure(err) == "task: mailbox_already_has_saved_account" {
				e.jobs[selected].AutoRetry.Reason = "saved_account_exists"
			}
			if saveErr := e.saveLocked(); saveErr != nil {
				e.jobs[selected] = previous
				e.loadError = saveErr
				return false, saveErr
			}
		}
		return false, err
	}
	return true, nil
}

// 手动和自动补偿都创建子任务；整条任务链共享邮箱占用及自动重试预算。
func (e *GrokEngine) startGrokCompensationLocked(index int, cfg GrokConfig, automatic bool, now time.Time) error {
	parent := cloneJob(e.jobs[index])
	for _, other := range e.jobs {
		if !strings.EqualFold(strings.TrimSpace(other.Mailbox.Email), strings.TrimSpace(parent.Mailbox.Email)) {
			continue
		}
		if other.ProxyBinding != nil && now.Before(other.ProxyBinding.BlockedUntil) {
			return fail("proxy", "registration_cooldown")
		}
		if other.AutoRetry != nil {
			if due, err := time.Parse(time.RFC3339Nano, other.AutoRetry.NotBeforeAt); err == nil && now.Before(due) {
				return fail("task", "mailbox_registration_cooldown")
			}
			if grokRegistrationRateLimited(other) {
				if due, err := time.Parse(time.RFC3339Nano, other.AutoRetry.NextAttemptAt); err == nil && now.Before(due) {
					return fail("task", "mailbox_registration_cooldown")
				}
			}
		}
	}
	if parent.AutoRetry != nil && parent.AutoRetry.Reason == "duplicate_mailbox_retry" {
		return fail("task", "mailbox_retry_already_scheduled")
	}
	attempts := e.mailboxAutomaticAttemptsLocked(parent.Mailbox.Email)
	if automatic && attempts >= len(compensationDelays) {
		return fail("task", "retry_limit_reached")
	}
	job, err := newRegistrationAttempt(parent, now)
	if err != nil {
		return err
	}
	for i, other := range e.jobs {
		if i != index && strings.EqualFold(other.Mailbox.Email, job.Mailbox.Email) && (other.Verified || other.Imported || len(other.Account) > 0) {
			return fail("task", "mailbox_already_has_saved_account")
		}
	}
	if job.Mailbox.AccountID != cfg.HME.AccountID || job.MailboxAPIBase != "" && job.MailboxAPIBase != cfg.HME.BaseURL {
		return fail("config", "saved_mailbox_source_unavailable")
	}
	if job.BrowserProfile != nil && job.BrowserProfile.valid() {
		profile := *job.BrowserProfile
		cfg.BrowserProfile = &profile
	}
	if automatic {
		if cfg.BrowserProfile == nil {
			return fail("task", "saved_environment_missing")
		}
		if job.AutoRetry == nil || job.AutoRetry.Attempts < 0 || job.AutoRetry.Attempts >= len(compensationDelays) {
			return fail("task", "invalid_retry_state")
		}
		job.AutoRetry.Attempts++
		job.AutoRetry.State, job.AutoRetry.Reason = "running", ""
		job.AutoRetry.LastAttemptAt = now.UTC().Format(time.RFC3339Nano)
		delay := compensationDelays[min(attempts+1, len(compensationDelays)-1)]
		job.AutoRetry.NextAttemptAt = now.Add(cfg.Timeout + delay).UTC().Format(time.RFC3339Nano)
	} else if job.AutoRetry != nil {
		finishRegistrationCompensation(&job, "manual_running")
	}
	job.MailboxAPIBase = cfg.HME.BaseURL
	action, detail := "manual_compensation", "复用已创建邮箱，开始手动补偿注册"
	if automatic {
		action, detail = "automatic_compensation", fmt.Sprintf("复用已创建邮箱，开始第 %d/%d 次自动补偿注册", attempts+1, len(compensationDelays))
	}
	appendRegistrationLogAt(&job, job.UpdatedAt, action, detail+"；原任务 "+parent.ID, "info")
	previous := e.jobs[index]
	e.jobs[index] = registrationParentWithChild(parent, job)
	beforeJobs := append([]RegistrationJob(nil), e.jobs...)
	e.jobs = append(e.jobs, job)
	e.normalizeMailboxRetriesLocked(now)
	if err := e.saveLocked(); err != nil {
		e.jobs = beforeJobs
		e.jobs[index] = previous
		e.loadError = err
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	e.active[job.ID] = &activeRegistration{cancel: cancel, done: make(chan struct{})}
	go e.runSavedRegistration(ctx, job.ID, job, cfg)
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
