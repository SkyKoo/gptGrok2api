package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

type accountTwoFAProgress struct {
	ID         string `json:"progress_id"`
	AccountRef string `json:"account_ref"`
	Email      string `json:"email"`
	Stage      string `json:"stage"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
	Pending    bool   `json:"pending"`
}

type accountTwoFATask struct {
	accountTwoFAProgress
	token, userID string
	code          chan string
	cancel        context.CancelFunc
}

// authStartMu serializes admission only. Progress locks are never held while
// waiting for a network request or OTP. No public task contains credentials.
func (s *Server) twoFAActive(ref string) bool {
	s.twofaMu.Lock()
	defer s.twofaMu.Unlock()
	for _, t := range s.twofaTasks {
		if (t.AccountRef == ref || strings.EqualFold(t.Email, ref)) && t.Status == "running" {
			return true
		}
	}
	return false
}

func (s *Server) twoFAConflictingAuth(ref, email string) bool {
	if s.cpaSyncActive(ref, email) {
		return true
	}
	s.reloginMu.RLock()
	for _, t := range s.reloginProgress {
		if t.AccountRef == ref && !t.Done {
			s.reloginMu.RUnlock()
			return true
		}
	}
	s.reloginMu.RUnlock()
	s.codexMu.RLock()
	defer s.codexMu.RUnlock()
	for _, t := range s.codexProgress {
		started, _ := time.Parse(time.RFC3339Nano, t.StartedAt)
		if (t.AccountRef == ref || strings.EqualFold(t.Email, email)) && t.Status == "running" && time.Since(started) < s.codexLogin.TTL {
			return true
		}
	}
	return false
}

func (s *Server) accountTwoFAAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.store.AccountList()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取账号失败", "internal_error")
			return
		}
		items := []accountTwoFAProgress{}
		s.twofaMu.Lock()
		for _, row := range rows {
			state := mapValue(row["twofa_setup"])
			id := stringValue(state["progress_id"])
			if id == "" {
				continue
			}
			if task := s.twofaTasks[id]; task != nil && stringValue(state["status"]) != "completed" {
				items = append(items, task.accountTwoFAProgress)
				continue
			}
			// A process restart cannot reconstruct the authenticated HTTP session.
			status, stage, message := stringValue(state["status"]), stringValue(state["stage"]), stringValue(state["message"])
			if status == "running" {
				status, stage, message = "interrupted", "interrupted", "服务重启中断，请重试；待激活密钥会保留"
			}
			items = append(items, accountTwoFAProgress{ID: id, AccountRef: accountPublicRef(row), Email: stringValue(row["email"]), Status: status, Stage: stage, Message: message, Error: stringValue(state["error"]), StartedAt: stringValue(state["started_at"]), UpdatedAt: stringValue(state["updated_at"]), Pending: len(mapValue(row["twofa_pending"])) > 0})
		}
		s.twofaMu.Unlock()
		sort.Slice(items, func(i, j int) bool { return items[i].StartedAt > items[j].StartedAt })
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccountRef string `json:"account_ref"`
		ProgressID string `json:"progress_id"`
		Action     string `json:"action"`
		Code       string `json:"code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Action == "code" || body.Action == "cancel" {
		s.twofaMu.Lock()
		task := s.twofaTasks[body.ProgressID]
		if task == nil || task.Status != "running" {
			s.twofaMu.Unlock()
			writeError(w, http.StatusConflict, "任务已结束或已过期", "invalid_request_error")
			return
		}
		if body.Action == "cancel" {
			task.cancel()
			s.twofaMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		code := strings.TrimSpace(body.Code)
		if task.Stage != "waiting_email_code" || !twoFAValidCode(code) {
			s.twofaMu.Unlock()
			writeError(w, http.StatusConflict, "当前阶段不接受验证码，或验证码不是 6 位数字", "invalid_request_error")
			return
		}
		task.Stage = "verifying_email_code"
		task.code <- code
		s.twofaMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if body.Action != "" {
		writeError(w, http.StatusBadRequest, "未知操作", "invalid_request_error")
		return
	}
	s.authStartMu.Lock()
	defer s.authStartMu.Unlock()
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		writeError(w, http.StatusConflict, "服务维护中", "invalid_request_error")
		return
	}
	account, err := s.accountByRef(strings.TrimSpace(body.AccountRef))
	if err != nil {
		writeError(w, http.StatusNotFound, "账号不存在", "not_found")
		return
	}
	if !isOpenAIAccount(accounts.Account{Token: accountToken(account), Fields: account}) || stringValue(account["email"]) == "" {
		writeError(w, http.StatusBadRequest, "需要有邮箱的 ChatGPT 账号", "invalid_request_error")
		return
	}
	ref := accountPublicRef(account)
	s.twofaMu.Lock()
	active := 0
	for _, t := range s.twofaTasks {
		if t.Status != "running" {
			continue
		}
		active++
		if t.AccountRef == ref {
			summary := t.accountTwoFAProgress
			s.twofaMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"progress": summary})
			return
		}
	}
	s.twofaMu.Unlock()
	if firstNonEmpty(stringValue(account["two_factor_secret"]), stringValue(account["totp_secret"])) != "" {
		writeError(w, http.StatusConflict, "账号已有 2FA 密钥，请勿重复开启", "invalid_request_error")
		return
	}
	if active >= 2 || s.twoFAConflictingAuth(ref, stringValue(account["email"])) {
		writeError(w, http.StatusConflict, "2FA 任务繁忙，或该账号正在登录 / OAuth 授权", "invalid_request_error")
		return
	}
	profile, err := s.accountReloginProfile(account)
	if err != nil || profile == nil || stringValue(mapValue(account["fp"])["oai-device-id"]) == "" {
		writeError(w, http.StatusBadRequest, "缺少浏览器环境或设备标识。已开启 2FA 的账号请在编辑账号中补填原 2FA Secret，无需重复开启；尚未开启的账号请先完成重新登录", "invalid_request_error")
		return
	}
	proxy := s.proxyManager.Resolve(account, false)
	flow, err := registerruntime.NewWebRegistrar(strings.TrimSuffix(strings.TrimRight(s.cfg.OpenAIBaseURL, "/"), "/backend-api"), s.cfg.OpenAIAuthBaseURL, proxy, *profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号网络环境不可用", "invalid_request_error")
		return
	}
	if err = flow.SetDeviceID(stringValue(mapValue(account["fp"])["oai-device-id"])); err != nil {
		flow.Close()
		writeError(w, http.StatusBadRequest, "账号设备标识不可用", "invalid_request_error")
		return
	}
	flow.RestoreTwoFACookies(account["web_cookies"], accountToken(account), stringValue(account["web_cookies_token_hash"]))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t := &accountTwoFATask{accountTwoFAProgress: accountTwoFAProgress{ID: newChatID(), AccountRef: ref, Email: stringValue(account["email"]), Status: "running", Stage: "queued", Message: "等待重认证", StartedAt: now, UpdatedAt: now, Pending: len(mapValue(account["twofa_pending"])) > 0}, token: accountToken(account), userID: stringValue(account["user_id"]), code: make(chan string, 1), cancel: cancel}
	if err = s.persistTwoFA(t, nil); err != nil {
		cancel()
		flow.Close()
		writeError(w, http.StatusInternalServerError, "无法保存 2FA 任务", "internal_error")
		return
	}
	s.twofaMu.Lock()
	if s.twofaTasks == nil {
		s.twofaTasks = map[string]*accountTwoFATask{}
	}
	// Keep one task per account; completed summaries are durably stored on it.
	for id, old := range s.twofaTasks {
		if old.AccountRef == ref && old.Status != "running" {
			delete(s.twofaTasks, id)
		}
	}
	s.twofaTasks[t.ID] = t
	summary := t.accountTwoFAProgress
	s.twofaMu.Unlock()
	go s.runAccountTwoFA(ctx, t, account, flow)
	writeJSON(w, http.StatusOK, map[string]any{"progress": summary})
}

func twoFAValidCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (s *Server) persistTwoFA(t *accountTwoFATask, extra map[string]any) error {
	state := map[string]any{"progress_id": t.ID, "status": t.Status, "stage": t.Stage, "message": t.Message, "error": t.Error, "started_at": t.StartedAt, "updated_at": t.UpdatedAt}
	updates := map[string]any{"twofa_setup": state}
	for k, v := range extra {
		updates[k] = v
	}
	_, _, err := s.store.UpdateAccountIf(t.token, updates, func(row map[string]any) bool {
		stateID := stringValue(mapValue(row["twofa_setup"])["progress_id"])
		return accountPublicRef(row) == t.AccountRef && stringValue(row["email"]) == t.Email && stringValue(row["user_id"]) == t.userID && (stateID == "" || stateID == t.ID || t.Stage == "queued") && firstNonEmpty(stringValue(row["two_factor_secret"]), stringValue(row["totp_secret"])) == ""
	})
	return err
}

func (s *Server) twoFAStage(t *accountTwoFATask, stage, message string, extra map[string]any) error {
	s.twofaMu.Lock()
	defer s.twofaMu.Unlock()
	t.Stage, t.Message, t.UpdatedAt = stage, message, time.Now().UTC().Format(time.RFC3339Nano)
	err := s.persistTwoFA(t, extra)
	if pending, ok := extra["twofa_pending"]; err == nil && ok {
		t.Pending = len(mapValue(pending)) > 0
	}
	return err
}

func (s *Server) finishTwoFA(t *accountTwoFATask, err error) {
	s.twofaMu.Lock()
	defer s.twofaMu.Unlock()
	t.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err == nil {
		t.Status, t.Stage, t.Message = "completed", "completed", "2FA 已开启，密钥已保存"
		t.Pending = false
		return
	}
	t.Status, t.Message = "failed", "开启失败；可查看原因后重试"
	var failure *registerruntime.Failure
	switch {
	case errors.Is(err, context.Canceled):
		t.Status, t.Message, t.Error = "cancelled", "任务已取消；待激活密钥会保留", "task_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		t.Error = "task_timeout"
	case errors.As(err, &failure):
		t.Error = failure.Error()
	default:
		t.Error = "account_changed_or_storage_error"
	}
	if t.Pending {
		t.Message = "待激活密钥已保留，结果需核对；重试只续跑激活，不重新申请密钥"
	}
	_ = s.persistTwoFA(t, nil)
}

func (s *Server) runAccountTwoFA(ctx context.Context, t *accountTwoFATask, account map[string]any, flow *registerruntime.WebRegistrar) {
	defer t.cancel()
	defer flow.Close()
	var resultErr error
	defer func() { s.finishTwoFA(t, resultErr) }()
	step := func(stage, message string, extra map[string]any) bool {
		resultErr = s.twoFAStage(t, stage, message, extra)
		return resultErr == nil
	}
	if !step("reauth", "正在发起邮箱重认证", nil) {
		return
	}
	after, err := flow.BeginTwoFA(ctx, t.Email)
	if err != nil {
		resultErr = err
		return
	}
	if !step("waiting_email_code", "等待邮箱验证码；可在此手动输入", nil) {
		return
	}
	// The HME worker only submits during the waiting stage. Manual input and
	// automatic delivery compete for the same one-shot channel under twofaMu.
	mailCtx, mailCancel := context.WithCancel(ctx)
	defer mailCancel()
	go s.twoFAAutoCode(mailCtx, t, account, after)
	var code string
	select {
	case <-ctx.Done():
		resultErr = ctx.Err()
		return
	case code = <-t.code:
	}
	mailCancel()
	if !step("verifying_email_code", "正在验证邮箱验证码", nil) {
		return
	}
	token, err := flow.VerifyTwoFAEmail(ctx, t.Email, t.userID, code)
	if err != nil {
		resultErr = err
		return
	}
	pending := mapValue(account["twofa_pending"])
	secret, id := stringValue(pending["secret"]), stringValue(pending["session_id"])
	if len(pending) > 0 && (secret == "" || id == "") {
		resultErr = errors.New("invalid_pending_enrollment")
		return
	}
	if secret == "" {
		if !step("enroll", "正在申请 TOTP 密钥", nil) {
			return
		}
		secret, id, err = flow.EnrollTwoFA(ctx, token)
		if err != nil {
			resultErr = err
			return
		}
		// Save synchronously before the irreversible activation request. Activation
		// is never retried automatically, including on timeout or process restart.
		if !step("activate", "密钥已保存，正在激活 2FA", map[string]any{"twofa_pending": map[string]any{"secret": secret, "session_id": id}}) {
			return
		}
	} else if !step("activate", "正在使用保存的密钥续跑激活", nil) {
		return
	}
	if err = ctx.Err(); err != nil {
		resultErr = err
		return
	}
	if err = flow.ActivateTwoFA(ctx, token, secret, id); err != nil {
		resultErr = err
		return
	}
	// The secret moves from pending to the normal credential field atomically.
	if !step("completed", "2FA 已开启，密钥已保存", map[string]any{"two_factor_secret": secret, "twofa_pending": nil, "twofa_setup": map[string]any{"progress_id": t.ID, "status": "completed", "stage": "completed", "message": "2FA 已开启，密钥已保存", "started_at": t.StartedAt, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}}) {
		return
	}
}

func (s *Server) twoFAAutoCode(ctx context.Context, t *accountTwoFATask, account map[string]any, after time.Time) {
	if s.registerStore == nil {
		return
	}
	box, ok := codexHMEMailbox(account)
	if !ok || !strings.EqualFold(box.Email, t.Email) {
		return
	}
	cfg, err := registerruntime.ParseFreeConfig(s.registerStore.Get())
	if err != nil || box.AccountID != cfg.HME.AccountID {
		return
	}
	mail, err := registerruntime.NewHME(cfg.HME, cfg.PollInterval)
	if err != nil {
		return
	}
	defer mail.Close()
	ctx, cancel := context.WithTimeout(ctx, cfg.MailTimeout)
	defer cancel()
	if preparer, ok := any(mail).(registerruntime.MailSessionPreparer); ok {
		if err = preparer.Prepare(ctx, box); err != nil {
			return
		}
	}
	code, err := mail.WaitCode(ctx, box, after)
	if err != nil || !twoFAValidCode(code) {
		return
	}
	s.twofaMu.Lock()
	defer s.twofaMu.Unlock()
	if t.Status == "running" && t.Stage == "waiting_email_code" && ctx.Err() == nil {
		t.Stage = "verifying_email_code"
		t.code <- code
	}
}

// accountTwoFACode generates a code locally; it never contacts the upstream or
// treats a pending enrollment as an active credential.
func (s *Server) accountTwoFACode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccountRef string `json:"account_ref"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.AccountRef) == "" {
		writeError(w, http.StatusBadRequest, "account_ref is required", "invalid_request_error")
		return
	}
	rows, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账号失败", "server_error")
		return
	}
	tokens, _ := resolveAccountRefTokens(rows, []string{body.AccountRef})
	if len(tokens) == 0 {
		writeError(w, http.StatusNotFound, "账号不存在", "not_found")
		return
	}
	for _, row := range rows {
		if accountToken(row) != tokens[0] {
			continue
		}
		secret := firstNonEmpty(stringValue(row["two_factor_secret"]), stringValue(row["totp_secret"]))
		if secret == "" {
			writeError(w, http.StatusConflict, "账号未配置已确认的 2FA 密钥", "invalid_request_error")
			return
		}
		now := time.Now()
		code, err := registerruntime.TOTPCode(secret, now)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "2FA 密钥格式无效，请在编辑账号中核对", "invalid_request_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code": code, "server_time": now.UnixMilli(), "expires_at": (now.Unix()/30 + 1) * 30000,
		})
		return
	}
}
