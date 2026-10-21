package register

import (
	"context"
	"errors"
	"time"
)

// RegistrationTwoFAState contains only public progress. Credentials remain in
// the private Account journal until the normal account import succeeds.
type RegistrationTwoFAState struct {
	ProgressID string `json:"progress_id"`
	Status     string `json:"status"`
	Stage      string `json:"stage"`
	Message    string `json:"message"`
	Error      string `json:"error,omitempty"`
	Pending    bool   `json:"pending"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
}

type registrationTwoFAFlow interface {
	BeginTwoFA(context.Context, string) (time.Time, error)
	VerifyTwoFAEmail(context.Context, string, string, string) (string, error)
	EnrollTwoFA(context.Context, string) (string, string, error)
	ActivateTwoFA(context.Context, string, string, string) error
}

func registrationTwoFASummary(state *RegistrationTwoFAState) *RegistrationTwoFAState {
	if state == nil {
		return nil
	}
	copy := *state
	return &copy
}

func registrationTwoFAAccountState(s *RegistrationTwoFAState) map[string]any {
	return map[string]any{"progress_id": s.ProgressID, "status": s.Status, "stage": s.Stage, "message": s.Message, "error": s.Error, "started_at": s.StartedAt, "updated_at": s.UpdatedAt}
}

func (e *FreeEngine) registrationTwoFAProgress(id, status, stage, message string, cause error, fields map[string]any) error {
	return e.change(id, func(j *RegistrationJob) {
		now := time.Now().UTC().Format(time.RFC3339)
		if j.TwoFA == nil {
			j.TwoFA = &RegistrationTwoFAState{ProgressID: "register-" + id, StartedAt: now}
		}
		for k, v := range fields {
			j.Account[k] = v
		}
		state := j.TwoFA
		state.Status, state.Stage, state.Message, state.UpdatedAt = status, stage, message, now
		state.Pending = len(object(j.Account["twofa_pending"])) > 0
		state.Error = ""
		level := "info"
		if cause != nil {
			state.Error = safeFailure(cause)
			level = "error"
		}
		if status == "completed" {
			level = "success"
		}
		j.Account["twofa_setup"] = registrationTwoFAAccountState(state)
		j.Status, j.Stage = "import_pending", "import"
		if status == "running" {
			j.Status, j.Stage = "running", "twofa_"+stage
		}
		j.Logs = append(j.Logs, JobLog{Time: now, Text: "twofa_" + stage + ": " + message, Level: level})
	})
}

// enableRegistrationTwoFA runs once after credentials have been durably saved.
// Network failures are a separate optional-step result: the new account still
// proceeds to import/verification. Storage errors stop before further traffic.
func (e *FreeEngine) enableRegistrationTwoFA(ctx context.Context, id string, cfg FreeConfig, mail MailSource, flow RegistrationFlow, box Mailbox) error {
	job, err := e.getJob(id)
	if err != nil {
		return err
	}
	if stringValue(job.Account["two_factor_secret"]) != "" || stringValue(job.Account["totp_secret"]) != "" {
		return e.registrationTwoFAProgress(id, "completed", "completed", "2FA 密钥已配置", nil, nil)
	}
	failed := func(cause error) error {
		status := "failed"
		if errors.Is(cause, context.Canceled) {
			status = "cancelled"
		}
		return e.registrationTwoFAProgress(id, status, "failed", "自动开启 2FA 未完成；账号会保留，请到账号管理中核对或补充开启", cause, nil)
	}
	if len(object(job.Account["twofa_pending"])) > 0 {
		return failed(fail("twofa", "pending_enrollment_requires_manual_resume"))
	}
	client, ok := flow.(registrationTwoFAFlow)
	if !ok {
		return failed(fail("twofa", "flow_not_supported"))
	}
	step := func(stage, message string) error {
		return e.registrationTwoFAProgress(id, "running", stage, message, nil, nil)
	}
	if err = ctx.Err(); err != nil {
		return failed(err)
	}
	if err = step("reauth", "正在复用注册会话进行邮箱重认证"); err != nil {
		return err
	}
	after, err := client.BeginTwoFA(ctx, box.Email)
	if err != nil {
		return failed(err)
	}
	if err = step("waiting_email_code", "等待本次 2FA 邮箱验证码"); err != nil {
		return err
	}
	mailCtx, cancel := context.WithTimeout(ctx, cfg.MailTimeout)
	code, err := mail.WaitCode(mailCtx, box, after)
	cancel()
	if err != nil {
		return failed(err)
	}
	if err = step("verifying_email_code", "正在验证 2FA 邮箱验证码"); err != nil {
		return err
	}
	token, err := client.VerifyTwoFAEmail(ctx, box.Email, stringValue(job.Account["user_id"]), code)
	if err != nil {
		return failed(err)
	}
	if err = step("enroll", "正在申请 TOTP 密钥"); err != nil {
		return err
	}
	secret, sessionID, err := client.EnrollTwoFA(ctx, token)
	if err != nil {
		return failed(err)
	}
	// Persist the only recovery copy before sending the activation request.
	if err = e.registrationTwoFAProgress(id, "running", "activate", "待激活密钥已保存，正在激活 2FA", nil, map[string]any{"twofa_pending": map[string]any{"secret": secret, "session_id": sessionID}}); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return failed(err)
	}
	if err = client.ActivateTwoFA(ctx, token, secret, sessionID); err != nil {
		return failed(err)
	}
	return e.registrationTwoFAProgress(id, "completed", "completed", "2FA 已开启，密钥已保存", nil, map[string]any{"two_factor_secret": secret, "twofa_pending": nil})
}
