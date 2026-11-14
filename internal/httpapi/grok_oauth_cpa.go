package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/xaiquality"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
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

// readXAICredential checks the downloaded identity without exposing secrets to
// callers. Equality is needed when a POST response or final receipt was lost.
func readXAICredential(ctx context.Context, client *http.Client, pool externalCPAPool, name string, account oauth.Account) (bool, error) {
	endpoint := strings.TrimRight(pool.BaseURL, "/") + "/v0/management/auth-files/download?" + url.Values{"name": {name}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, errors.New("CPA 地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+pool.SecretKey)
	res, err := client.Do(req)
	if err != nil {
		return false, errors.New("读取 CPA 凭据确认失败或超时")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return false, fmt.Errorf("读取 CPA 凭据确认返回 HTTP %d", res.StatusCode)
	}
	var row map[string]any
	if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&row) != nil {
		return false, errors.New("CPA 凭据确认格式无效")
	}
	if stringValue(row["type"]) != "xai" || !strings.EqualFold(stringValue(row["email"]), account.Email) {
		return false, errors.New("CPA 凭据渠道或邮箱不匹配")
	}
	return stringValue(row["access_token"]) == account.AccessToken && stringValue(row["refresh_token"]) == account.RefreshToken && stringValue(row["id_token"]) == account.IDToken, nil
}

func postXAICredential(ctx context.Context, client *http.Client, pool externalCPAPool, name string, credential map[string]string) error {
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
		return errors.New("CPA 上传结果未确认；请核对 CPA，未重复上传")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("CPA 上传返回 HTTP %d；请核对 CPA，未重复上传", res.StatusCode)
	}
	return nil
}

func (s *Server) xaiCPATarget(id string) (externalCPAPool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		pools := s.external.listCPA()
		if len(pools) == 1 {
			id = pools[0].ID
		}
	}
	pool, ok := s.external.getCPA(id)
	if !ok {
		return externalCPAPool{}, errors.New("oauth_cpa_required")
	}
	return pool, nil
}

// syncGrokOAuthCPA makes at most one upload attempt for this saved credential.
// Unknown deliveries are recovered through exact readback, never by reposting.
// checkOnly explicitly adopts the matching remote identity as the renewal owner.
func (s *Server) syncGrokOAuthCPA(ctx context.Context, id, requestedPool string, checkOnly bool) (oauth.Account, error) {
	if _, busy := s.grokOAuthActions.LoadOrStore(id, true); busy {
		return oauth.Account{}, errors.New("该账号正在刷新或同步，请稍后重试")
	}
	defer s.grokOAuthActions.Delete(id)
	account, found, err := s.oauthStore.Get(id)
	if err != nil || !found {
		return account, errors.New("无法读取 OAuth 凭据")
	}
	unlock := s.oauthStore.LockIdentity(account.Email)
	defer unlock()
	account, found, err = s.oauthStore.Get(id)
	if err != nil || !found {
		return account, errors.New("无法读取 OAuth 凭据")
	}
	if strings.TrimSpace(account.Email) == "" {
		return account, errors.New("OAuth 凭据缺少邮箱，无法核对 CPA 账号身份")
	}
	if account.Status == "disabled" {
		return account, errors.New("OAuth 账号已禁用")
	}
	if account.Quality != nil && account.Quality.Blocked && (checkOnly || oauth.CPAManaged(account)) {
		return account, errors.New("该账号已标记降智风险，禁止恢复 CPA 交付；请先复测并处理风险")
	}
	receipt := account.CPASync
	target := strings.TrimSpace(requestedPool)
	if receipt != nil && receipt.PoolID != "" {
		if target != "" && target != receipt.PoolID {
			return account, errors.New("本次授权已绑定其他 CPA，请重新授权以选择新目标")
		}
		target = receipt.PoolID
	}
	pool, err := s.xaiCPATarget(target)
	if err != nil {
		return account, errors.New(grokOAuthMessages["oauth_cpa_required"])
	}
	name := xaiCPAFileName(account)
	if receipt != nil && receipt.FileName != "" {
		name = receipt.FileName
	}
	if !cpaFileNameValid(name) {
		return account, errors.New("CPA 文件名无效")
	}
	result := oauth.CPASync{Status: "pending", PoolID: pool.ID, PoolName: pool.Name, FileName: name}
	if receipt != nil {
		result = *receipt
		result.PoolID = pool.ID
		result.PoolName = pool.Name
		result.FileName = name
	}
	result.Managed = oauth.CPAManaged(account)
	save := func() error {
		updated, applied, saveErr := s.oauthStore.UpdateIfDelivery(account, func(current *oauth.Account) { cp := result; current.CPASync = &cp })
		if saveErr != nil {
			return errors.New("CPA 操作状态保存失败，请核对远端文件")
		}
		if !applied {
			return errors.New("同步期间凭据发生变化，请核对 CPA 后再操作")
		}
		account = updated
		return nil
	}
	fail := func(e error, state string) (oauth.Account, error) {
		result.Status = state
		result.Error = e.Error()
		if saveErr := save(); saveErr != nil {
			return account, saveErr
		}
		return account, e
	}
	requireQuality := func(viaCPA bool) error {
		if viaCPA {
			// An existing remote identity owns renewal even if its quality probe
			// fails. Persist that ownership before probing to avoid local tokens.
			result.Managed = true
		}
		if xaiquality.Passed(account.Quality, time.Now()) && (!viaCPA || account.Quality.Source == "cpa") {
			return nil
		}
		if err := save(); err != nil {
			return err
		}
		if result.AuthorizationTaskID != "" {
			if queue, ok := s.taskQueue.(interface{ SetStage(string, string) }); ok {
				queue.SetStage(result.AuthorizationTaskID, "checking_quality")
			}
		}
		checked, err := s.checkGrokQualityLocked(ctx, account)
		account = checked
		if account.CPASync != nil {
			// The probe may have removed a confirmed risky CPA credential.
			// Do not overwrite that receipt with the pre-probe snapshot.
			result = *account.CPASync
		}
		if err != nil {
			return err
		}
		if !xaiquality.Passed(account.Quality, time.Now()) {
			if account.Quality != nil && account.Quality.Status == "error" {
				return errors.New("grok-4.7 检测失败，暂不能判定质量；未新增上传或确认 CPA 交付，请查看检测依据并复测")
			}
			return errors.New("grok-4.7 质量检测未通过，未新增上传或确认 CPA 交付；请查看检测依据并复测")
		}
		return nil
	}
	rejectQuality := func(err error) (oauth.Account, error) {
		if result.Status == "quality_removed" {
			return account, err
		}
		return fail(err, "quality_blocked")
	}
	succeed := func(viaCPA bool) (oauth.Account, error) {
		// Every confirmation path shares this gate, including legacy adoption
		// and recovery after a lost upload response.
		if err := requireQuality(viaCPA); err != nil {
			return rejectQuality(err)
		}
		result.Status = "synced"
		result.Error = ""
		result.Managed = true
		result.ReplaceExisting = false
		result.At = time.Now().Unix()
		result.VerifiedAt = result.At
		if err := save(); err != nil {
			return account, err
		}
		return account, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	client := remoteClient(true)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	exists, err := cpaXAIFile(ctx, client, pool, name, account.Email)
	if err != nil {
		return fail(err, "failed")
	}
	if checkOnly && result.AttemptedAt > 0 && result.VerifiedAt == 0 {
		if exists {
			equal, readErr := readXAICredential(ctx, client, pool, name, account)
			if readErr == nil && equal {
				return succeed(true)
			}
		}
		return fail(errors.New("CPA 当前凭据尚不能证明本次授权已交付，保留待核对状态，未覆盖远端"), "unknown")
	}
	if checkOnly {
		if !exists {
			return fail(errors.New("CPA 未找到该账号的凭据；保留本地副本，请核对或重新授权"), "failed")
		}
		// The explicit check adopts the existing CPA identity, even if CPA has
		// already rotated its tokens. It never overwrites the remote copy.
		return succeed(true)
	}
	if result.AttemptedAt > 0 && result.VerifiedAt == 0 {
		if exists {
			equal, readErr := readXAICredential(ctx, client, pool, name, account)
			if readErr == nil && equal {
				return succeed(true)
			}
		}
		return fail(errors.New("上次上传结果未确认，未重复上传；请核对 CPA 当前凭据"), "unknown")
	}
	if oauth.CPAManaged(account) || (exists && !result.ReplaceExisting) {
		if account.Quality != nil && account.Quality.Blocked {
			return fail(errors.New("该账号已标记降智风险，不能确认或恢复 CPA 交付；请先复测"), "quality_blocked")
		}
		if !exists {
			return fail(errors.New("CPA 托管凭据未找到，未使用本地旧副本覆盖；请核对或重新授权"), "failed")
		}
		return succeed(true)
	}
	if account.AccessToken == "" || account.RefreshToken == "" || account.Status == "invalid" {
		return fail(errors.New("OAuth 凭据不完整或失效，无法同步"), "failed")
	}
	if account.ExpiresAt > 0 && account.ExpiresAt <= time.Now().Unix() {
		return fail(errors.New("待交付的 Access Token 已过期，请重新授权后同步"), "failed")
	}
	if err := requireQuality(false); err != nil {
		return rejectQuality(err)
	}
	if result.AuthorizationTaskID != "" {
		if queue, ok := s.taskQueue.(interface{ SetStage(string, string) }); ok {
			queue.SetStage(result.AuthorizationTaskID, "syncing_cpa")
		}
	}
	result.Status = "uploading"
	result.AttemptedAt = time.Now().Unix()
	result.Managed = true
	result.Error = ""
	if err := save(); err != nil {
		return account, err
	}
	postErr := postXAICredential(ctx, client, pool, name, xaiCPACredential(account, s.cfg.OAuthTokenURL))
	// Even a failed/lost POST response may have committed. Always read back, but
	// never accept an older same-email file as proof of this new delivery.
	exists, readErr := cpaXAIFile(ctx, client, pool, name, account.Email)
	if readErr == nil && exists {
		equal, verifyErr := readXAICredential(ctx, client, pool, name, account)
		if verifyErr == nil && equal {
			return succeed(false)
		}
		if verifyErr != nil {
			readErr = verifyErr
		}
	}
	if postErr != nil {
		return fail(postErr, "unknown")
	}
	if readErr != nil {
		return fail(readErr, "unknown")
	}
	return fail(errors.New("CPA 已接收请求，但本次凭据尚未确认；请核对 CPA，未重复上传"), "unknown")
}

func (s *Server) finishOAuthCPADelivery(ctx context.Context, value map[string]any) {
	if stringValue(value["status"]) != "authorized" {
		return
	}
	id := stringValue(mapValue(value["account"])["id"])
	account, found, err := s.oauthStore.Get(id)
	if err != nil || !found {
		value["cpa_error"] = "无法读取已授权凭据"
		return
	}
	target := ""
	if account.CPASync != nil {
		target = account.CPASync.PoolID
	}
	updated, syncErr := s.syncGrokOAuthCPA(ctx, id, target, false)
	if updated.ID != "" {
		value["account"] = oauth.Public(updated)
	}
	if syncErr != nil {
		value["cpa_error"] = syncErr.Error()
		value["stage"] = "cpa_sync_failed"
	} else {
		value["stage"] = "completed"
	}
}

func (s *Server) grokOAuthCPASync(w http.ResponseWriter, r *http.Request, id string) {
	var request struct {
		PoolID string `json:"cpa_pool_id"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	account, err := s.syncGrokOAuthCPA(r.Context(), strings.TrimSpace(id), request.PoolID, strings.HasSuffix(r.URL.Path, "/check"))
	code := http.StatusOK
	message := ""
	if err != nil {
		code = http.StatusBadGateway
		message = err.Error()
		if account.ID == "" {
			code = http.StatusConflict
		}
	}
	writeJSON(w, code, map[string]any{"ok": err == nil, "account": oauth.Public(account), "cpa_sync": account.CPASync, "error": message})
}
