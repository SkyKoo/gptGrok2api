package register

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Each retry is a durable child task. The chain keeps one mailbox claim and
// automatic retry budget; proxy identity and runtime state belong to an attempt.
func newRegistrationAttempt(parent RegistrationJob, now time.Time) (RegistrationJob, error) {
	if parent.RetryJobID != "" {
		return RegistrationJob{}, fail("task", "retry_already_created")
	}
	if parent.Mailbox.Email == "" || len(parent.Account) > 0 || parent.Imported || parent.Verified {
		return RegistrationJob{}, fail("task", "no_saved_mailbox")
	}
	// A new task ID must not erase an existing upstream refusal/rate-limit wait.
	if parent.ProxyBinding != nil && now.Before(parent.ProxyBinding.BlockedUntil) {
		return RegistrationJob{}, &Failure{Stage: "proxy", Code: "registration_cooldown", RetryAfter: parent.ProxyBinding.BlockedUntil.UTC().Format(time.RFC3339)}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return RegistrationJob{}, fail("task", "random_failed")
	}
	child := cloneJob(parent)
	child.ID = hex.EncodeToString(raw)
	child.ParentJobID = parent.ID
	child.RootJobID = mailboxClaimJobID(parent)
	child.RetryJobID = ""
	child.ProxyBinding = nil
	child.Status, child.Stage, child.Error = "running", "compensation", ""
	child.CreatedAt = now.UTC().Format(time.RFC3339Nano)
	child.UpdatedAt = child.CreatedAt
	child.Logs = []JobLog{}
	child.TwoFA = nil
	child.CodexOAuth = nil
	return child, nil
}

func mailboxClaimJobID(job RegistrationJob) string {
	if job.RootJobID != "" {
		return job.RootJobID
	}
	return job.ID
}

func registrationParentWithChild(parent RegistrationJob, child RegistrationJob) RegistrationJob {
	parent = cloneJob(parent)
	parent.RetryJobID = child.ID
	parent.UpdatedAt = child.CreatedAt
	if parent.AutoRetry != nil {
		parent.AutoRetry.State = "superseded"
		parent.AutoRetry.NextAttemptAt = ""
	}
	appendRegistrationLogAt(&parent, child.CreatedAt, "retry_created", "已创建补偿任务 "+child.ID+"，复用原邮箱", "info")
	return parent
}
