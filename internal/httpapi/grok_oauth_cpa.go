package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

func xaiCPAFileName(account oauth.Account) string {
	value := strings.TrimSpace(account.Email)
	if value == "" {
		value = strings.TrimSpace(account.Subject)
	}
	if value == "" {
		value = account.ID
	}
	var safe strings.Builder
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("@._-", c) {
			safe.WriteRune(c)
		} else {
			safe.WriteByte('-')
		}
	}
	return "xai-" + strings.Trim(safe.String(), "-") + ".json"
}
func xaiCPACredential(account oauth.Account, tokenEndpoint string) map[string]string {
	expiry := ""
	if account.ExpiresAt > 0 {
		expiry = time.Unix(account.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	return map[string]string{"type": "xai", "auth_kind": "oauth", "email": account.Email, "sub": account.Subject, "access_token": account.AccessToken, "refresh_token": account.RefreshToken, "id_token": account.IDToken, "token_type": firstNonEmpty(account.TokenType, "Bearer"), "expired": expiry, "last_refresh": time.Unix(account.UpdatedAt, 0).UTC().Format(time.RFC3339), "token_endpoint": firstNonEmpty(account.TokenEndpoint, tokenEndpoint)}
}

// cpaXAIFile checks provider and identity before overwriting an existing auth file.
func cpaXAIFile(ctx context.Context, client *http.Client, pool externalCPAPool, name, email string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(pool.BaseURL, "/")+"/v0/management/auth-files", nil)
	if err != nil {
		return false, errors.New("CPA 地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+pool.SecretKey)
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return false, errors.New("读取 CPA 文件列表失败或超时")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return false, fmt.Errorf("读取 CPA 文件列表返回 HTTP %d", res.StatusCode)
	}
	var body struct {
		Files *[]map[string]any `json:"files"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&body) != nil || body.Files == nil {
		return false, errors.New("CPA 文件列表格式无效")
	}
	for _, file := range *body.Files {
		if stringValue(file["name"]) != name {
			continue
		}
		provider := firstNonEmpty(stringValue(file["provider"]), stringValue(file["type"]))
		if provider != "xai" || email == "" || !strings.EqualFold(strings.TrimSpace(stringValue(file["email"])), strings.TrimSpace(email)) {
			return false, errors.New("CPA 同名文件的渠道或邮箱不匹配，未覆盖")
		}
		return true, nil
	}
	return false, nil
}
func uploadXAICredential(ctx context.Context, pool externalCPAPool, name string, credential map[string]string) error {
	client := remoteClient(true)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := cpaXAIFile(ctx, client, pool, name, credential["email"]); err != nil {
		return err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return errors.New("无法创建 CPA 凭据文件")
	}
	if err = json.NewEncoder(part).Encode(credential); err != nil {
		return errors.New("无法编码 CPA 凭据")
	}
	if writer.Close() != nil {
		return errors.New("无法完成 CPA 凭据文件")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(pool.BaseURL, "/")+"/v0/management/auth-files", &body)
	if err != nil {
		return errors.New("CPA 地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+pool.SecretKey)
	req.Header.Set("X-Management-Key", pool.SecretKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := client.Do(req)
	if err != nil {
		return errors.New("CPA 上传结果未确认，请先检查远端文件；未自动重试")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("CPA 上传返回 HTTP %d；未自动重试", res.StatusCode)
	}
	exists, err := cpaXAIFile(ctx, client, pool, name, credential["email"])
	if err != nil {
		return fmt.Errorf("CPA 已接收文件，但确认失败：%w", err)
	}
	if !exists {
		return errors.New("CPA 已接收文件，但尚未识别为该邮箱的 xAI 凭据")
	}
	return nil
}

func (s *Server) grokOAuthCPASync(w http.ResponseWriter, r *http.Request, id string) {
	var request struct {
		PoolID string `json:"cpa_pool_id"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	id = strings.TrimSpace(id)
	if _, busy := s.grokOAuthActions.LoadOrStore(id, true); busy {
		writeError(w, 409, "该账号正在刷新或同步，请稍后重试", "conflict")
		return
	}
	defer s.grokOAuthActions.Delete(id)
	account, found, err := s.oauthStore.Get(id)
	if err != nil {
		writeError(w, 500, "无法读取 OAuth 凭据", "server_error")
		return
	}
	if !found {
		writeError(w, 404, "OAuth 账号不存在", "not_found")
		return
	}
	if strings.TrimSpace(account.Email) == "" {
		writeError(w, 400, "OAuth 凭据缺少邮箱，无法核对 CPA 账号身份，请重新授权", "invalid_request_error")
		return
	}
	if account.AccessToken == "" || account.RefreshToken == "" || account.Status == "disabled" || account.Status == "invalid" {
		writeError(w, 400, "OAuth 凭据不完整、失效或已禁用，无法同步", "invalid_request_error")
		return
	}
	if account.ExpiresAt > 0 && account.ExpiresAt <= time.Now().Unix() {
		writeError(w, 400, "Access Token 已过期，请先刷新额度以更新凭据，再同步 CPA", "invalid_request_error")
		return
	}
	pool, ok := s.external.getCPA(strings.TrimSpace(request.PoolID))
	if !ok {
		writeError(w, 400, "请选择已配置的 CPA 连接", "invalid_request_error")
		return
	}
	name := xaiCPAFileName(account)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	result := &oauth.CPASync{Status: "synced", PoolID: pool.ID, PoolName: pool.Name, FileName: name, At: time.Now().Unix()}
	err = uploadXAICredential(ctx, pool, name, xaiCPACredential(account, s.cfg.OAuthTokenURL))
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
	}
	updated, applied, saveErr := s.oauthStore.UpdateIfCredential(account, func(current *oauth.Account) { current.CPASync = result })
	if saveErr != nil {
		writeError(w, 500, "CPA 操作已结束，但同步状态保存失败，请检查远端文件", "server_error")
		return
	}
	if !applied {
		writeError(w, 409, "同步期间凭据发生变化，未记录为同步成功；请核对 CPA 后重新同步", "conflict")
		return
	}
	status := 200
	if err != nil {
		status = 502
	}
	writeJSON(w, status, map[string]any{"ok": err == nil, "account": oauth.Public(updated), "cpa_sync": result, "error": result.Error})
}
