package register

import (
	"context"
	"strings"
	"time"
)

// 只有取得账号结果后才提交占用。确定失败且不再自动重试时释放注册预留；
// 最终提交结果未知、已保存账号或仍在等待补偿时保留预留，避免重复建号。
func (e *GrokEngine) releaseFailedMailboxReservation(id string, mail MailSource) {
	releaser, ok := mail.(interface {
		Release(context.Context, Mailbox, string) error
	})
	if !ok {
		return
	}
	e.mu.Lock()
	if e.loadError != nil {
		e.mu.Unlock()
		return
	}
	var job RegistrationJob
	for _, candidate := range e.jobs {
		if candidate.ID == id {
			job = cloneJob(candidate)
			break
		}
	}
	release := job.Mailbox.Email != "" && (job.Status == "registration_pending" || job.Status == "failed" || job.Status == "cancelled")
	for _, other := range e.jobs {
		if !strings.EqualFold(other.Mailbox.Email, job.Mailbox.Email) {
			continue
		}
		// 包括同邮箱的旧任务：后续一次失败不能抹掉此前未知的最终注册结果。
		if other.Imported || other.Verified || len(other.Account) > 0 || grokRegistrationResultUncertain(other) || other.Status == "queued" || other.Status == "running" ||
			other.AutoRetry != nil && (other.AutoRetry.State == "waiting" || other.AutoRetry.State == "running") {
			release = false
			break
		}
	}
	e.mu.Unlock()
	if !release {
		return
	}
	// 任务超时/主动取消后仍须完成本地预留清理，不复用已取消的注册 context。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := releaser.Release(ctx, job.Mailbox, "registration_failed_without_account"); err != nil {
		_ = e.change(id, func(j *RegistrationJob) {
			appendRegistrationLog(j, "mailbox_release_failed", "注册未成功，释放邮箱预留失败，需核对邮箱资产", "error")
		})
	}
}
