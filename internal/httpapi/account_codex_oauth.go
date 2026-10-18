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
)

var codexSensitiveErrorPattern = regexp.MustCompile(`(?i)((?:access|refresh|id)_token["']?\s*[:=]\s*["']?)([^"'\s,}]+)`)
var codexJWTErrorPattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)

type codexOAuthProgress struct {
	SessionID  string
	AccountRef string
	CPAPoolID  string
	Stage      string
	Status     string
	Message    string
	Error      string
	StartedAt  string
	UpdatedAt  string
	FinishedAt string
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
		AccountRef string `json:"account_ref"`
		EmailHint  string `json:"email_hint"`
		CPAPoolID  string `json:"cpa_pool_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.AccountRef, body.EmailHint, body.CPAPoolID = strings.TrimSpace(body.AccountRef), strings.TrimSpace(body.EmailHint), strings.TrimSpace(body.CPAPoolID)
	if body.AccountRef != "" {
		if _, err := s.accountByRef(body.AccountRef); err != nil {
			writeError(w, http.StatusNotFound, "account not found", "not_found")
			return
		}
	}
	result, err := s.codexLogin.Start(body.EmailHint)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error(), "not_configured")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	progress := &codexOAuthProgress{SessionID: result["session_id"], AccountRef: body.AccountRef, CPAPoolID: body.CPAPoolID, Stage: "waiting_authorization", Status: "running", Message: "请在授权页面完成手机验证码；如果上游要求 2FA，再输入 TOTP。完成后粘贴 callback URL。", StartedAt: now, UpdatedAt: now}
	s.codexMu.Lock()
	s.codexProgress[progress.SessionID] = progress
	s.codexMu.Unlock()
	result["stage"], result["message"] = progress.Stage, progress.Message
	writeJSON(w, http.StatusOK, result)
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
		SessionID  string `json:"session_id"`
		Callback   string `json:"callback"`
		AccountRef string `json:"account_ref"`
		CPAPoolID  string `json:"cpa_pool_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.SessionID, body.Callback, body.AccountRef, body.CPAPoolID = strings.TrimSpace(body.SessionID), strings.TrimSpace(body.Callback), strings.TrimSpace(body.AccountRef), strings.TrimSpace(body.CPAPoolID)
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
	s.setCodexProgress(body.SessionID, "exchanging_token", "running", "正在交换 Codex OAuth Token", "")
	tokens, err := s.codexLogin.Finish(r.Context(), body.SessionID, body.Callback)
	if err != nil {
		s.setCodexProgress(body.SessionID, "failed", "failed", "Codex OAuth 失败", safeOAuthError(err))
		writeError(w, http.StatusBadRequest, safeOAuthError(err), "invalid_request_error")
		return
	}
	email, accountID := codexTokenIdentity(tokens["id_token"], tokens["access_token"])
	if body.AccountRef == "" && email != "" {
		if account, lookupErr := s.accountByRef(email); lookupErr == nil {
			body.AccountRef = accountPublicRef(account)
		}
	}
	if body.AccountRef == "" {
		err = fmt.Errorf("account_ref is required; Codex OAuth email %q is not linked to a CFM account", email)
		s.setCodexProgress(body.SessionID, "failed", "failed", "未绑定 CFM 账号", err.Error())
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	account, err := s.accountByRef(body.AccountRef)
	if err != nil {
		s.setCodexProgress(body.SessionID, "failed", "failed", "CFM 账号不存在", err.Error())
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
		return
	}
	credential := codexCredential(tokens, email, accountID)
	credential["status"], credential["cpa_sync_status"] = "authorized", "pending"
	updated, _, err := s.store.UpdateAccount(accountToken(account), map[string]any{"codex_credential": credential})
	if err != nil {
		s.setCodexProgress(body.SessionID, "failed", "failed", "Codex 凭据保存失败", err.Error())
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	s.setCodexProgress(body.SessionID, "syncing_cpa", "running", "Codex 凭据已保存，正在同步 CPA", "")
	syncResult, syncErr := s.syncCodexCredential(r.Context(), updated, body.CPAPoolID)
	if syncErr != nil {
		credential["cpa_sync_status"], credential["cpa_last_error"], credential["cpa_last_error_at"] = "failed", safeOAuthError(syncErr), time.Now().UTC().Format(time.RFC3339)
		_, _, _ = s.store.UpdateAccount(accountToken(updated), map[string]any{"codex_credential": credential})
		s.setCodexProgress(body.SessionID, "cpa_sync_failed", "failed", "Token 已保存，但 CPA 同步失败，可单独重试", safeOAuthError(syncErr))
		latest, _ := s.accountByRef(body.AccountRef)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Codex 登录成功，但 CPA 同步失败；凭据已保留", "cpa_sync": syncResult, "item": accountForAPI(latest)})
		return
	}
	s.setCodexProgress(body.SessionID, "done", "completed", "Codex 登录并同步 CPA 完成", "")
	latest, _ := s.accountByRef(body.AccountRef)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Codex 登录并同步 CPA 完成", "cpa_sync": syncResult, "item": accountForAPI(latest)})
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
		AccountRef string `json:"account_ref"`
		CPAPoolID  string `json:"cpa_pool_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	account, err := s.accountByRef(strings.TrimSpace(body.AccountRef))
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
	s.codexMu.Lock()
	defer s.codexMu.Unlock()
	if item := s.codexProgress[id]; item != nil {
		item.Stage, item.Status, item.Message, item.Error, item.UpdatedAt = stage, status, message, failure, now
		if status == "completed" || status == "failed" {
			item.FinishedAt = now
		}
	}
}
func codexProgressJSON(item *codexOAuthProgress) map[string]any {
	return map[string]any{"session_id": item.SessionID, "account_ref": item.AccountRef, "cpa_pool_id": item.CPAPoolID, "stage": item.Stage, "status": item.Status, "message": item.Message, "error": item.Error, "started_at": item.StartedAt, "updated_at": item.UpdatedAt, "finished_at": item.FinishedAt}
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
