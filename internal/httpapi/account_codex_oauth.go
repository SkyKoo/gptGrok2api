package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

var codexSensitiveErrorPattern = regexp.MustCompile(`(?i)((?:access|refresh|id)_token["']?\s*[:=]\s*["']?)([^"'\s,}]+)`)
var codexJWTErrorPattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)

type codexOAuthProgress struct {
	SessionID         string
	AccountRef        string
	CPAPoolID         string
	RegistrationJobID string
	EmailCodeSource   string
	MailboxEmail      string
	MailboxAccountID  string
	EmailWaitAfter    time.Time
	EmailAutoStarted  bool
	Stage             string
	Status            string
	Message           string
	Error             string
	StartedAt         string
	UpdatedAt         string
	FinishedAt        string
}

func (s *Server) codexOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccountRef        string `json:"account_ref"`
		EmailHint         string `json:"email_hint"`
		Password          string `json:"password"`
		CPAPoolID         string `json:"cpa_pool_id"`
		RegistrationJobID string `json:"registration_job_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.AccountRef, body.EmailHint, body.Password, body.CPAPoolID, body.RegistrationJobID = strings.TrimSpace(body.AccountRef), strings.TrimSpace(body.EmailHint), strings.TrimSpace(body.Password), strings.TrimSpace(body.CPAPoolID), strings.TrimSpace(body.RegistrationJobID)
	var account map[string]any
	if body.RegistrationJobID != "" {
		var err error
		account, err = s.accountByRegistrationJob(body.RegistrationJobID)
		if err != nil {
			writeError(w, http.StatusNotFound, "registered account not found", "not_found")
			return
		}
		body.AccountRef = accountPublicRef(account)
		if body.EmailHint == "" {
			body.EmailHint = stringValue(account["email"])
		}
		if body.Password == "" {
			body.Password = firstNonEmpty(stringValue(account["login_password"]), stringValue(account["password"]))
		}
	}
	if body.AccountRef != "" {
		var err error
		account, err = s.accountByRef(body.AccountRef)
		if err != nil {
			writeError(w, http.StatusNotFound, "account not found", "not_found")
			return
		}
		if body.EmailHint == "" {
			body.EmailHint = stringValue(account["email"])
		}
		if body.Password == "" {
			body.Password = firstNonEmpty(stringValue(account["login_password"]), stringValue(account["password"]))
		}
	}
	if body.EmailHint == "" {
		writeError(w, http.StatusBadRequest, "email_hint or account_ref is required", "invalid_request_error")
		return
	}
	startedAt := time.Now().UTC()
	result, err := s.codexProtocolLogin.Start(r.Context(), body.EmailHint, body.Password)
	if err != nil {
		writeError(w, http.StatusBadGateway, safeOAuthError(err), "upstream_error")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	progress := &codexOAuthProgress{SessionID: result["session_id"], AccountRef: body.AccountRef, CPAPoolID: body.CPAPoolID, RegistrationJobID: body.RegistrationJobID, Stage: result["stage"], Status: "running", Message: result["message"], StartedAt: startedAt.Format(time.RFC3339Nano), UpdatedAt: now, EmailWaitAfter: startedAt}
	if mailbox, ok := codexHMEMailbox(account); ok {
		progress.EmailCodeSource = "hme"
		progress.MailboxEmail = mailbox.Email
		progress.MailboxAccountID = mailbox.AccountID
		if progress.Stage == "waiting_email_code" {
			progress.Message = "上游要求邮箱验证码，正在从 HME 自动获取；也可手动输入"
		}
	} else {
		progress.EmailCodeSource = "manual"
	}
	s.codexMu.Lock()
	s.codexProgress[progress.SessionID] = progress
	s.codexMu.Unlock()
	result["stage"], result["message"] = progress.Stage, progress.Message
	result["email_code_source"] = progress.EmailCodeSource
	if progress.EmailCodeSource == "hme" {
		result["email_code_auto"] = "true"
	} else {
		result["email_code_auto"] = "false"
	}
	s.saveRegistrationCodexProgress(progress)
	s.maybeStartCodexEmailAuto(progress.SessionID)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) codexOAuthSubmitPhone(w http.ResponseWriter, r *http.Request) {
	s.codexOAuthSubmitInput(w, r, "phone")
}

func (s *Server) codexOAuthSubmitSMS(w http.ResponseWriter, r *http.Request) {
	s.codexOAuthSubmitInput(w, r, "sms_code")
}

func (s *Server) codexOAuthSubmitEmail(w http.ResponseWriter, r *http.Request) {
	s.codexOAuthSubmitInput(w, r, "email_code")
}

func (s *Server) codexOAuthSubmit2FA(w http.ResponseWriter, r *http.Request) {
	s.codexOAuthSubmitInput(w, r, "two_factor_code")
}

func (s *Server) codexOAuthSubmitInput(w http.ResponseWriter, r *http.Request, kind string) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		SessionID  string `json:"session_id"`
		Value      string `json:"value"`
		AccountRef string `json:"account_ref"`
		CPAPoolID  string `json:"cpa_pool_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.SessionID, body.Value, body.AccountRef, body.CPAPoolID = strings.TrimSpace(body.SessionID), strings.TrimSpace(body.Value), strings.TrimSpace(body.AccountRef), strings.TrimSpace(body.CPAPoolID)
	if body.SessionID == "" || body.Value == "" {
		writeError(w, http.StatusBadRequest, "session_id and value are required", "invalid_request_error")
		return
	}
	s.codexMu.RLock()
	progress, ok := s.codexProgress[body.SessionID]
	var stored codexOAuthProgress
	if ok && progress != nil {
		stored = *progress
	}
	s.codexMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "codex oauth session not found", "not_found")
		return
	}
	if body.AccountRef == "" {
		body.AccountRef = stored.AccountRef
	}
	if body.CPAPoolID == "" {
		body.CPAPoolID = stored.CPAPoolID
	}

	var result map[string]string
	var err error
	switch kind {
	case "phone":
		s.setCodexProgress(body.SessionID, "sending_phone", "running", "正在提交手机号", "")
		result, err = s.codexProtocolLogin.SubmitPhone(r.Context(), body.SessionID, body.Value)
	case "sms_code":
		s.setCodexProgress(body.SessionID, "verifying_sms", "running", "正在验证短信验证码", "")
		result, err = s.codexProtocolLogin.SubmitSMSCode(r.Context(), body.SessionID, body.Value)
	case "email_code":
		result, err = s.submitCodexEmailCode(r.Context(), body.SessionID, body.Value, false)
	case "two_factor_code":
		s.setCodexProgress(body.SessionID, "verifying_2fa", "running", "正在验证 2FA", "")
		result, err = s.codexProtocolLogin.Submit2FA(r.Context(), body.SessionID, body.Value)
	default:
		err = fmt.Errorf("unsupported codex input")
	}
	if err != nil {
		s.setCodexProgress(body.SessionID, "failed", "failed", "Codex OAuth 失败", safeOAuthError(err))
		writeError(w, http.StatusBadRequest, safeOAuthError(err), "invalid_request_error")
		return
	}
	if access := result["access_token"]; access != "" {
		response, finishErr := s.finishCodexTokens(r.Context(), body.SessionID, body.AccountRef, body.CPAPoolID, stored.RegistrationJobID, result)
		if finishErr != nil {
			writeError(w, http.StatusBadGateway, safeOAuthError(finishErr), "upstream_error")
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	stage, message := result["stage"], result["message"]
	if stage == "" {
		stage = "waiting_authorization"
	}
	s.setCodexProgress(body.SessionID, stage, "running", message, "")
	writeJSON(w, http.StatusOK, result)
}

// submitCodexEmailCode serializes manual and HME-submitted codes for one
// process.  This prevents a code arriving from HME at the same time as a
// manually entered code from advancing the upstream session twice.
func (s *Server) submitCodexEmailCode(ctx context.Context, sessionID, code string, automatic bool) (map[string]string, error) {
	s.codexEmailMu.Lock()
	defer s.codexEmailMu.Unlock()
	if automatic {
		s.codexMu.RLock()
		progress := s.codexProgress[sessionID]
		stage := ""
		if progress != nil {
			stage = progress.Stage
		}
		s.codexMu.RUnlock()
		if stage != "waiting_email_code" {
			return nil, nil
		}
	}
	s.setCodexProgress(sessionID, "verifying_email_code", "running", "正在验证邮箱验证码", "")
	return s.codexProtocolLogin.SubmitEmailCode(ctx, sessionID, code)
}

// maybeStartCodexEmailAuto starts HME polling only for an account whose
// registration record explicitly identifies an iCloud HME mailbox. Accounts
// imported from another source retain the manual input path.
func (s *Server) maybeStartCodexEmailAuto(sessionID string) {
	s.codexMu.Lock()
	progress := s.codexProgress[sessionID]
	if progress == nil || progress.EmailCodeSource != "hme" || progress.Stage != "waiting_email_code" || progress.MailboxEmail == "" || progress.MailboxAccountID == "" {
		s.codexMu.Unlock()
		return
	}
	if progress.EmailAutoStarted {
		s.codexMu.Unlock()
		return
	}
	if progress.EmailWaitAfter.IsZero() {
		if parsed, err := time.Parse(time.RFC3339Nano, progress.StartedAt); err == nil {
			progress.EmailWaitAfter = parsed
		} else {
			progress.EmailWaitAfter = time.Now().UTC()
		}
	}
	progress.EmailAutoStarted = true
	progress.Message = "HME 正在获取邮箱验证码，也可手动输入"
	progress.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	copyProgress := *progress
	s.codexMu.Unlock()
	s.saveRegistrationCodexProgress(&copyProgress)
	go s.autoSubmitCodexEmailCode(copyProgress)
}

func (s *Server) autoSubmitCodexEmailCode(progress codexOAuthProgress) {
	if s.registerStore == nil {
		s.keepCodexEmailManual(progress.SessionID, "HME 未配置，请手动输入邮箱验证码")
		return
	}
	cfg, err := registerruntime.ParseFreeConfig(s.registerStore.Get())
	if err != nil {
		s.keepCodexEmailManual(progress.SessionID, "HME 配置不可用，请手动输入邮箱验证码")
		return
	}
	mail, err := registerruntime.NewHME(cfg.HME, cfg.PollInterval)
	if err != nil {
		s.keepCodexEmailManual(progress.SessionID, "HME 连接不可用，请手动输入邮箱验证码")
		return
	}
	defer mail.Close()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.MailTimeout)
	defer cancel()
	box := registerruntime.Mailbox{Email: progress.MailboxEmail, AccountID: progress.MailboxAccountID}
	if preparer, ok := any(mail).(registerruntime.MailSessionPreparer); ok {
		if err := preparer.Prepare(ctx, box); err != nil {
			s.keepCodexEmailManual(progress.SessionID, "HME 邮箱会话不可用，请手动输入邮箱验证码")
			return
		}
	}
	code, err := mail.WaitCode(ctx, box, progress.EmailWaitAfter)
	if err != nil {
		s.keepCodexEmailManual(progress.SessionID, "HME 未获取到邮箱验证码，请手动输入")
		return
	}
	result, err := s.submitCodexEmailCode(context.Background(), progress.SessionID, code, true)
	if err != nil {
		// Keep the session alive. A valid HME message can still be rejected by
		// the upstream challenge, in which case the operator may submit a code
		// manually after inspecting the progress details.
		s.keepCodexEmailManual(progress.SessionID, "HME 验证码提交失败，请手动输入邮箱验证码")
		return
	}
	if result == nil {
		return
	}
	if access := result["access_token"]; access != "" {
		stored := s.codexStoredProgress(progress.SessionID)
		if stored == nil {
			return
		}
		if _, finishErr := s.finishCodexTokens(context.Background(), progress.SessionID, stored.AccountRef, stored.CPAPoolID, stored.RegistrationJobID, result); finishErr != nil {
			return
		}
		return
	}
	stage, message := result["stage"], result["message"]
	if stage == "" {
		stage = "waiting_authorization"
	}
	s.setCodexProgress(progress.SessionID, stage, "running", message, "")
}

func (s *Server) keepCodexEmailManual(sessionID, message string) {
	s.codexMu.Lock()
	item := s.codexProgress[sessionID]
	if item == nil || item.Status != "running" || (item.Stage != "waiting_email_code" && item.Stage != "verifying_email_code") {
		s.codexMu.Unlock()
		return
	}
	item.Stage, item.Status, item.Message, item.Error, item.UpdatedAt = "waiting_email_code", "running", message, "", time.Now().UTC().Format(time.RFC3339Nano)
	snapshot := *item
	s.codexMu.Unlock()
	s.saveRegistrationCodexProgress(&snapshot)
}

func (s *Server) codexStoredProgress(sessionID string) *codexOAuthProgress {
	s.codexMu.RLock()
	defer s.codexMu.RUnlock()
	item := s.codexProgress[sessionID]
	if item == nil {
		return nil
	}
	copyItem := *item
	return &copyItem
}

func codexHMEMailbox(account map[string]any) (registerruntime.Mailbox, bool) {
	if account == nil {
		return registerruntime.Mailbox{}, false
	}
	mailbox := mapValue(account["registration_mailbox"])
	if !strings.EqualFold(stringValue(mailbox["provider"]), "icloud_hme") {
		return registerruntime.Mailbox{}, false
	}
	box := registerruntime.Mailbox{Email: strings.TrimSpace(stringValue(mailbox["email"])), AccountID: strings.TrimSpace(stringValue(mailbox["account_id"]))}
	return box, box.Email != "" && box.AccountID != ""
}

func (s *Server) codexOAuthProgressAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/accounts/codex/oauth/progress/")
	s.codexMu.RLock()
	progress, ok := s.codexProgress[id]
	var copyProgress codexOAuthProgress
	if ok && progress != nil {
		copyProgress = *progress
	}
	s.codexMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "codex oauth session not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, codexProgressJSON(&copyProgress))
}

func (s *Server) codexOAuthFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		SessionID         string `json:"session_id"`
		Callback          string `json:"callback"`
		AccountRef        string `json:"account_ref"`
		CPAPoolID         string `json:"cpa_pool_id"`
		RegistrationJobID string `json:"registration_job_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.SessionID, body.Callback, body.AccountRef, body.CPAPoolID, body.RegistrationJobID = strings.TrimSpace(body.SessionID), strings.TrimSpace(body.Callback), strings.TrimSpace(body.AccountRef), strings.TrimSpace(body.CPAPoolID), strings.TrimSpace(body.RegistrationJobID)
	s.codexMu.RLock()
	progress, ok := s.codexProgress[body.SessionID]
	var stored codexOAuthProgress
	if ok && progress != nil {
		stored = *progress
	}
	s.codexMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "codex oauth session not found", "not_found")
		return
	}
	if body.AccountRef == "" {
		body.AccountRef = stored.AccountRef
	}
	if body.CPAPoolID == "" {
		body.CPAPoolID = stored.CPAPoolID
	}
	if body.RegistrationJobID == "" {
		body.RegistrationJobID = stored.RegistrationJobID
	}
	s.setCodexProgress(body.SessionID, "exchanging_token", "running", "正在交换 Codex OAuth Token", "")
	tokens, err := s.codexLogin.Finish(r.Context(), body.SessionID, body.Callback)
	if err != nil {
		s.setCodexProgress(body.SessionID, "failed", "failed", "Codex OAuth 失败", safeOAuthError(err))
		writeError(w, http.StatusBadRequest, safeOAuthError(err), "invalid_request_error")
		return
	}
	result, finishErr := s.finishCodexTokens(r.Context(), body.SessionID, body.AccountRef, body.CPAPoolID, body.RegistrationJobID, tokens)
	if finishErr != nil {
		writeError(w, http.StatusBadGateway, safeOAuthError(finishErr), "upstream_error")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) finishCodexTokens(ctx context.Context, sessionID, accountRef, poolID, registrationJobID string, tokens map[string]string) (map[string]any, error) {
	email, accountID := codexTokenIdentity(tokens["id_token"], tokens["access_token"])
	if accountRef == "" && email != "" {
		if account, lookupErr := s.accountByRef(email); lookupErr == nil {
			accountRef = accountPublicRef(account)
		}
	}
	if accountRef == "" {
		err := fmt.Errorf("account_ref is required; Codex OAuth email %q is not linked to a CFM account", email)
		s.setCodexProgress(sessionID, "failed", "failed", "未绑定 CFM 账号", err.Error())
		return nil, err
	}
	account, err := s.accountByRef(accountRef)
	if err != nil {
		s.setCodexProgress(sessionID, "failed", "failed", "CFM 账号不存在", err.Error())
		return nil, err
	}
	if registrationJobID == "" {
		registrationJobID = stringValue(account["registration_job_id"])
	}
	credential := codexCredential(tokens, email, accountID)
	credential["status"], credential["cpa_sync_status"] = "authorized", "pending"
	updated, _, err := s.store.UpdateAccount(accountToken(account), map[string]any{"codex_credential": credential})
	if err != nil {
		s.setCodexProgress(sessionID, "failed", "failed", "Codex 凭据保存失败", err.Error())
		return nil, err
	}
	s.setCodexProgress(sessionID, "syncing_cpa", "running", "Codex 凭据已保存，正在同步 CPA", "")
	syncResult, syncErr := s.syncCodexCredential(ctx, updated, poolID)
	if syncErr != nil {
		credential["cpa_sync_status"], credential["cpa_last_error"], credential["cpa_last_error_at"] = "failed", safeOAuthError(syncErr), time.Now().UTC().Format(time.RFC3339)
		_, _, _ = s.store.UpdateAccount(accountToken(updated), map[string]any{"codex_credential": credential})
		s.setCodexProgress(sessionID, "cpa_sync_failed", "failed", "Token 已保存，但 CPA 同步失败，可单独重试", safeOAuthError(syncErr))
		latest, _ := s.accountByRef(accountRef)
		return map[string]any{"ok": false, "message": "Codex 登录成功，但 CPA 同步失败；凭据已保留", "cpa_sync": syncResult, "item": accountForAPI(latest)}, nil
	}
	s.setCodexProgress(sessionID, "done", "completed", "Codex 登录并同步 CPA 完成", "")
	latest, _ := s.accountByRef(accountRef)
	return map[string]any{"ok": true, "message": "Codex 登录并同步 CPA 完成", "cpa_sync": syncResult, "item": accountForAPI(latest)}, nil
}

func (s *Server) codexSync(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccountRef        string `json:"account_ref"`
		CPAPoolID         string `json:"cpa_pool_id"`
		RegistrationJobID string `json:"registration_job_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.AccountRef, body.RegistrationJobID = strings.TrimSpace(body.AccountRef), strings.TrimSpace(body.RegistrationJobID)
	var account map[string]any
	var err error
	if body.RegistrationJobID != "" {
		account, err = s.accountByRegistrationJob(body.RegistrationJobID)
	} else {
		account, err = s.accountByRef(body.AccountRef)
	}
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
		return
	}
	result, syncErr := s.syncCodexCredential(r.Context(), account, strings.TrimSpace(body.CPAPoolID))
	latest, _ := s.accountByRef(accountPublicRef(account))
	if syncErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": safeOAuthError(syncErr), "cpa_sync": result, "item": accountForAPI(latest)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cpa_sync": result, "item": accountForAPI(latest)})
}

func (s *Server) setCodexProgress(id, stage, status, message, failure string) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var snapshot codexOAuthProgress
	found := false
	s.codexMu.Lock()
	if item := s.codexProgress[id]; item != nil {
		item.Stage, item.Status, item.Message, item.Error, item.UpdatedAt = stage, status, message, failure, now
		if status == "completed" || status == "failed" {
			item.FinishedAt = now
		}
		snapshot = *item
		found = true
	}
	s.codexMu.Unlock()
	// Persist in call order. This keeps the registration record from ending up
	// at an older stage when several OAuth milestones complete back-to-back.
	if found {
		s.saveRegistrationCodexProgress(&snapshot)
		s.maybeStartCodexEmailAuto(id)
	}
}
func codexProgressJSON(item *codexOAuthProgress) map[string]any {
	return map[string]any{"session_id": item.SessionID, "account_ref": item.AccountRef, "cpa_pool_id": item.CPAPoolID, "registration_job_id": item.RegistrationJobID, "email_code_source": item.EmailCodeSource, "email_code_auto": item.EmailCodeSource == "hme", "stage": item.Stage, "status": item.Status, "message": item.Message, "error": item.Error, "started_at": item.StartedAt, "updated_at": item.UpdatedAt, "finished_at": item.FinishedAt}
}

func (s *Server) accountByRegistrationJob(jobID string) (map[string]any, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, fmt.Errorf("registration_job_id is required")
	}
	items, err := s.store.AccountList()
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if stringValue(item["registration_job_id"]) == jobID {
			return item, nil
		}
	}
	return nil, fmt.Errorf("registered account not found")
}

func (s *Server) saveRegistrationCodexProgress(item *codexOAuthProgress) {
	if item == nil || item.RegistrationJobID == "" || s.freeRegister == nil {
		return
	}
	task := &registerruntime.CodexOAuthTask{SessionID: item.SessionID, Status: item.Status, Stage: item.Stage, Message: item.Message, Error: item.Error, CPAPoolID: item.CPAPoolID, StartedAt: item.StartedAt, UpdatedAt: item.UpdatedAt, FinishedAt: item.FinishedAt}
	if item.Status == "completed" {
		task.CPAStatus = "synced"
	} else if item.Stage == "cpa_sync_failed" {
		task.CPAStatus = "failed"
	} else {
		task.CPAStatus = "pending"
	}
	_ = s.freeRegister.SetCodexOAuth(item.RegistrationJobID, task)
}

func codexCredential(tokens map[string]string, email, accountID string) map[string]any {
	credential := map[string]any{"access_token": tokens["access_token"], "refresh_token": tokens["refresh_token"], "id_token": tokens["id_token"], "email": email, "account_id": accountID, "token_type": tokens["token_type"], "updated_at": time.Now().UTC().Format(time.RFC3339), "status": "authorized"}
	if seconds := intValue(tokens["expires_in"]); seconds > 0 {
		credential["expires_at"] = time.Now().UTC().Add(time.Duration(seconds) * time.Second).Format(time.RFC3339)
	}
	if claims, err := jwtClaims(tokens["access_token"]); err == nil {
		if value := jwtTime(claims["exp"]); value != "" {
			credential["expires_at"] = value
		}
	}
	return credential
}
func codexTokenIdentity(idToken, accessToken string) (string, string) {
	for _, token := range []string{idToken, accessToken} {
		claims, err := jwtClaims(token)
		if err != nil {
			continue
		}
		profile, auth := mapValue(claims["https://api.openai.com/profile"]), mapValue(claims["https://api.openai.com/auth"])
		email := firstNonEmpty(stringValue(claims["email"]), stringValue(profile["email"]))
		accountID := firstNonEmpty(stringValue(auth["chatgpt_account_id"]), stringValue(claims["account_id"]), stringValue(claims["sub"]))
		if email != "" || accountID != "" {
			return email, accountID
		}
	}
	return "", ""
}

func (s *Server) syncCodexCredential(ctx context.Context, account map[string]any, poolID string) (map[string]any, error) {
	credential := mapValue(account["codex_credential"])
	access, refresh := stringValue(credential["access_token"]), stringValue(credential["refresh_token"])
	if access == "" || refresh == "" {
		return nil, fmt.Errorf("Codex 凭据不完整，缺少 access_token 或 refresh_token")
	}
	pool, ok := s.external.getCPA(strings.TrimSpace(poolID))
	if !ok && strings.TrimSpace(poolID) == "" {
		items := s.external.listCPA()
		if len(items) > 0 {
			pool, ok = items[0], true
		}
	}
	if !ok {
		return map[string]any{"status": "not_configured"}, fmt.Errorf("未配置 CPA 连接")
	}
	item := map[string]string{"type": "codex", "email": firstNonEmpty(stringValue(credential["email"]), stringValue(account["email"])), "account_id": stringValue(credential["account_id"]), "access_token": access, "refresh_token": refresh, "id_token": stringValue(credential["id_token"]), "expired": stringValue(credential["expires_at"]), "last_refresh": stringValue(credential["updated_at"])}
	name := "codex-" + safeExportName(firstNonEmpty(item["email"], item["account_id"], accountPublicRef(account)), "account") + ".json"
	_, err := uploadCodexAuthFile(ctx, pool, name, item)
	if err != nil {
		return map[string]any{"status": "failed", "pool_id": pool.ID, "pool_name": pool.Name, "file_name": name}, err
	}
	syncedAt := time.Now().UTC().Format(time.RFC3339)
	credential["cpa_sync_status"], credential["cpa_pool_id"], credential["cpa_file_name"], credential["cpa_synced_at"], credential["cpa_last_error"], credential["cpa_last_error_at"] = "synced", pool.ID, name, syncedAt, nil, nil
	updated := map[string]any{"status": "synced", "pool_id": pool.ID, "pool_name": pool.Name, "file_name": name, "synced_at": syncedAt}
	_, _, updateErr := s.store.UpdateAccount(accountToken(account), map[string]any{"codex_credential": credential})
	return updated, updateErr
}

func uploadCodexAuthFile(ctx context.Context, pool externalCPAPool, filename string, item map[string]string) (map[string]any, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(filename))
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(item)
	if _, err = part.Write(raw); err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	requestBody := append([]byte(nil), body.Bytes()...)
	contentType := writer.FormDataContentType()
	endpoint := strings.TrimRight(pool.BaseURL, "/") + "/v0/management/auth-files"
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+pool.SecretKey)
		req.Header.Set("X-Management-Key", pool.SecretKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", contentType)
		resp, err := remoteClient(true).Do(req)
		if err != nil {
			lastErr = err
		} else {
			rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			var payload map[string]any
			_ = json.Unmarshal(rawResp, &payload)
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return payload, nil
			}
			lastErr = fmt.Errorf("CPA auth-files 上传失败 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rawResp)))
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				break
			}
		}
		if attempt < 2 {
			wait := time.Duration(attempt+1) * 300 * time.Millisecond
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil, lastErr
}

func safeOAuthError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	message = codexSensitiveErrorPattern.ReplaceAllString(message, `${1}[redacted]`)
	message = codexJWTErrorPattern.ReplaceAllString(message, `[redacted-jwt]`)
	if len(message) > 400 {
		message = message[:400]
	}
	return message
}
