package register

import (
	"context"
	"strings"
	"time"
)

// 预留只保护正在执行的注册。单次失败后释放，自动重试开始前重新取得；
// 已保存账号、其他正在执行的同邮箱任务及其他任务链的预留不能释放。
func (e *GrokEngine) canReleaseMailboxLocked(job RegistrationJob, finishingID string) bool {
	if e.loadError != nil || job.Mailbox.Email == "" {
		return false
	}
	switch job.Status {
	case "registration_pending", "failed", "cancelled", "interrupted":
	default:
		return false
	}
	for _, other := range e.jobs {
		if !strings.EqualFold(other.Mailbox.Email, job.Mailbox.Email) {
			continue
		}
		if other.Imported || other.Verified || len(other.Account) > 0 || other.Status == "queued" || other.Status == "running" {
			return false
		}
		if _, active := e.active[other.ID]; active && other.ID != finishingID {
			return false
		}
	}
	return true
}

// 直接操作本地邮箱资源，不依赖注册浏览器或邮箱上游是否能启动。
func (e *GrokEngine) SetMailboxReservationReleaser(release func(context.Context, RegistrationJob) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.releaseMailboxReservation = release
}

// 启动时和手动核对邮箱时清理旧失败任务的遗留预留；不改变任务重试计划。
func (e *GrokEngine) ReconcileMailboxReservations(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loadError != nil {
		return e.loadError
	}
	if e.releaseMailboxReservation == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, job := range e.jobs {
		if !e.canReleaseMailboxLocked(job, "") {
			continue
		}
		key := strings.ToLower(job.Mailbox.Email) + "/" + mailboxClaimJobID(job)
		if seen[key] {
			continue
		}
		if err := e.releaseMailboxReservation(ctx, cloneJob(job)); err != nil {
			return err
		}
		seen[key] = true
	}
	return nil
}

func (e *GrokEngine) releaseFailedMailboxReservation(id string, mail MailSource) {
	// 释放不复用已经取消的注册 context。锁内执行，避免手动重试与释放交叉。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.mu.Lock()
	var job RegistrationJob
	for _, candidate := range e.jobs {
		if candidate.ID == id {
			job = cloneJob(candidate)
			break
		}
	}
	if !e.canReleaseMailboxLocked(job, id) {
		e.mu.Unlock()
		return
	}
	var err error
	if e.releaseMailboxReservation != nil {
		err = e.releaseMailboxReservation(ctx, job)
	} else if releaser, ok := mail.(interface {
		Release(context.Context, Mailbox, string) error
	}); ok {
		err = releaser.Release(ctx, job.Mailbox, "registration_failed_without_account")
	}
	e.mu.Unlock()
	if err != nil {
		_ = e.change(id, func(j *RegistrationJob) {
			appendRegistrationLog(j, "mailbox_release_failed", "注册未成功，释放邮箱预留失败", "error")
		})
	}
}
