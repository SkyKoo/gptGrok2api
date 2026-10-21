package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

const cpaPreviewLimit = 20

type cpaBinding struct {
	Name          string `json:"name"`
	AccountRef    string `json:"account_ref"`
	SourceVersion string `json:"source_version"`
	TargetVersion string `json:"target_version"`
	Replace       bool   `json:"replace"`
}

// 版本只用于检测预览后的变化；凭据始终在后端下载，不接受浏览器传入Token。
func cpaVersion(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func cpaTargetVersion(row map[string]any) string {
	return cpaVersion(map[string]any{"token": accountToken(row), "email": row["email"], "account_id": row["account_id"], "chatgpt_account_id": row["chatgpt_account_id"], "user_id": row["user_id"], "type": row["type"], "source_type": row["source_type"], "codex": row["codex_credential"]})
}

func cpaFileNameValid(name string) bool {
	return name != "" && len(name) <= 255 && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\") && strings.IndexFunc(name, unicode.IsControl) < 0 && strings.HasSuffix(strings.ToLower(name), ".json")
}

func cpaString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

// 只规范化Codex授权文件。JWT声明用于一致性检查，不代表已在线验证Token。
func parseCPACredential(payload map[string]any) (map[string]any, error) {
	if !strings.EqualFold(cpaString(payload, "type"), "codex") {
		return nil, errors.New("仅支持 type 为 codex 的授权文件")
	}
	access, refresh, id := cpaString(payload, "access_token"), cpaString(payload, "refresh_token"), cpaString(payload, "id_token")
	if access == "" || refresh == "" {
		return nil, errors.New("Codex 凭据不完整，缺少 Access Token 或 Refresh Token")
	}
	email, accountID := cpaString(payload, "email"), cpaString(payload, "account_id")
	expiry := firstNonEmpty(cpaString(payload, "expired"), cpaString(payload, "expires_at"))
	for _, token := range []string{access, id} {
		if token == "" {
			continue
		}
		claims, err := jwtClaims(token)
		if err != nil {
			continue
		}
		profile, auth := mapValue(claims["https://api.openai.com/profile"]), mapValue(claims["https://api.openai.com/auth"])
		claimEmail := firstNonEmpty(cpaString(claims, "email"), cpaString(profile, "email"))
		claimID := firstNonEmpty(cpaString(auth, "chatgpt_account_id"), cpaString(claims, "account_id"))
		if email != "" && claimEmail != "" && !strings.EqualFold(email, claimEmail) {
			return nil, errors.New("CPA 文件邮箱与 Token 身份不一致")
		}
		if accountID != "" && claimID != "" && accountID != claimID {
			return nil, errors.New("CPA 文件账户 ID 与 Token 身份不一致")
		}
		email, accountID = firstNonEmpty(email, claimEmail), firstNonEmpty(accountID, claimID)
		if expiry == "" && token == access {
			expiry = jwtTime(claims["exp"])
		}
	}
	if email == "" && accountID == "" {
		return nil, errors.New("凭据缺少可核对的邮箱或账户 ID")
	}
	if expiry != "" {
		if _, err := time.Parse(time.RFC3339, expiry); err != nil {
			return nil, errors.New("凭据到期时间格式无效")
		}
	}
	_, plan := codexCPAAccountPlan(map[string]any{"access_token": access, "id_token": id, "plan_type": cpaString(payload, "plan_type")})
	return map[string]any{"plan_type": plan, "access_token": access, "refresh_token": refresh, "id_token": id, "email": email, "account_id": accountID, "expires_at": expiry, "updated_at": cpaString(payload, "last_refresh"), "token_type": firstNonEmpty(cpaString(payload, "token_type"), "Bearer"), "status": "authorized"}, nil
}

func downloadCPACredential(ctx context.Context, pool externalCPAPool, name string) (map[string]any, error) {
	if !cpaFileNameValid(name) {
		return nil, errors.New("授权文件名无效")
	}
	endpoint := strings.TrimRight(pool.BaseURL, "/") + "/v0/management/auth-files/download?" + url.Values{"name": []string{name}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("CPA 地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+pool.SecretKey)
	req.Header.Set("Accept", "application/json")
	client := remoteClient(true)
	// 不向重定向目的地发送管理密钥。
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("读取 CPA 文件失败或超时，请重新加载后重试")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("读取 CPA 文件失败 HTTP %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("CPA 文件读取失败或超过大小限制")
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil, errors.New("CPA 文件不是有效的 JSON 对象")
	}
	return parseCPACredential(payload)
}

func cpaIdentityConflict(row, credential map[string]any) string {
	if !isOpenAIAccount(accounts.Account{Token: accountToken(row), Fields: row}) {
		return "只能绑定 ChatGPT 账号"
	}
	email, sourceEmail := cpaString(row, "email"), cpaString(credential, "email")
	if email != "" && sourceEmail != "" && !strings.EqualFold(email, sourceEmail) {
		return "邮箱不一致，不能绑定到该账号"
	}
	existing := mapValue(row["codex_credential"])
	targetID := firstNonEmpty(cpaString(existing, "account_id"), cpaString(row, "chatgpt_account_id"))
	sourceID := cpaString(credential, "account_id")
	if targetID != "" && sourceID != "" && targetID != sourceID {
		return "Codex 账户 ID 不一致，不能绑定到该账号"
	}
	return ""
}

func cpaCredentialsEqual(a, b map[string]any) bool {
	if len(a) == 0 {
		return false
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "email", "account_id", "expires_at"} {
		if cpaString(a, key) != cpaString(b, key) {
			return false
		}
	}
	return true
}

func (s *Server) cpaCredentialPreview(w http.ResponseWriter, r *http.Request, pool externalCPAPool) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Names []string `json:"names"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	names := uniqueStrings(body.Names)
	if len(names) == 0 || len(names) > cpaPreviewLimit {
		writeError(w, 400, "请选择 1～20 个文件进行预览", "invalid_request_error")
		return
	}
	rows, err := s.store.AccountList()
	if err != nil {
		writeError(w, 500, "读取已有账号失败", "server_error")
		return
	}
	refCounts := map[string]int{}
	for _, row := range rows {
		refCounts[accountPublicRef(row)]++
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	result := []map[string]any{}
	for _, name := range names {
		item := map[string]any{"name": name, "error": "", "candidates": []any{}, "suggested_ref": ""}
		credential, err := downloadCPACredential(ctx, pool, name)
		if err != nil {
			item["error"] = err.Error()
			result = append(result, item)
			continue
		}
		item["source_version"], item["email"], item["account_id"], item["expires_at"] = cpaVersion(credential), credential["email"], credential["account_id"], credential["expires_at"]
		item["has_access_token"], item["has_refresh_token"], item["has_id_token"] = true, true, cpaString(credential, "id_token") != ""
		candidates := []map[string]any{}
		matches := []string{}
		for _, row := range rows {
			if !isOpenAIAccount(accounts.Account{Token: accountToken(row), Fields: row}) {
				continue
			}
			ref := accountPublicRef(row)
			conflict := cpaIdentityConflict(row, credential)
			if refCounts[ref] != 1 {
				conflict = "账号引用重复，请先为账号完善唯一 ID"
			}
			if ref == "" {
				continue
			}
			existing := mapValue(row["codex_credential"])
			candidates = append(candidates, map[string]any{"account_ref": ref, "email": stringValue(row["email"]), "target_version": cpaTargetVersion(row), "has_credential": len(existing) > 0, "identical": cpaCredentialsEqual(existing, credential), "conflict": conflict})
			sourceEmail := cpaString(credential, "email")
			sameEmail := sourceEmail != "" && strings.EqualFold(cpaString(row, "email"), sourceEmail)
			sameID := cpaString(credential, "account_id") != "" && cpaString(existing, "account_id") == cpaString(credential, "account_id")
			if conflict == "" && (sameEmail || sameID) {
				matches = append(matches, ref)
			}
		}
		if len(matches) == 1 {
			item["suggested_ref"] = matches[0]
		}
		item["candidates"] = candidates
		result = append(result, item)
	}
	writeJSON(w, 200, map[string]any{"items": result})
}

// 每次确认只绑定一个文件。前端逐条显示结果，失败不会重放或回滚其他成功项。
func (s *Server) cpaCredentialBind(w http.ResponseWriter, r *http.Request, pool externalCPAPool) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body cpaBinding
	if !decodeJSON(w, r, &body) {
		return
	}
	if !cpaFileNameValid(body.Name) || body.AccountRef == "" || body.SourceVersion == "" || body.TargetVersion == "" {
		writeError(w, 400, "请先预览并选择已有 CFM 账号", "invalid_request_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	credential, err := downloadCPACredential(ctx, pool, body.Name)
	if err != nil {
		writeError(w, 502, err.Error(), "upstream_error")
		return
	}
	if cpaVersion(credential) != body.SourceVersion {
		writeError(w, 409, "CPA 文件已变化，请重新预览", "conflict")
		return
	}
	s.authStartMu.Lock()
	defer s.authStartMu.Unlock()
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		writeError(w, 409, "服务维护中，请稍后重试", "conflict")
		return
	}
	rows, err := s.store.AccountList()
	if err != nil {
		writeError(w, 500, "读取账号失败", "server_error")
		return
	}
	var target map[string]any
	matches := 0
	for _, row := range rows {
		if accountPublicRef(row) == body.AccountRef {
			target = row
			matches++
		}
	}
	if matches != 1 {
		writeError(w, 409, "目标账号不存在或引用不唯一，请重新预览", "conflict")
		return
	}
	if message := cpaIdentityConflict(target, credential); message != "" {
		writeError(w, 409, message, "conflict")
		return
	}
	if s.twoFAActive(body.AccountRef) || s.twoFAConflictingAuth(body.AccountRef, cpaString(target, "email")) {
		writeError(w, 409, "账号正在登录、授权或设置 2FA，请完成后重新预览", "conflict")
		return
	}
	existing := mapValue(target["codex_credential"])
	if cpaCredentialsEqual(existing, credential) && cpaString(existing, "cpa_source_pool_id") == pool.ID && cpaString(existing, "cpa_source_file_name") == body.Name {
		writeJSON(w, 200, map[string]any{"status": "skipped", "message": "相同凭据已绑定，无需重复导入"})
		return
	}
	if cpaTargetVersion(target) != body.TargetVersion {
		writeError(w, 409, "目标账号或 Codex 凭据已变化，请重新预览", "conflict")
		return
	}
	if len(existing) > 0 && !body.Replace && !cpaCredentialsEqual(existing, credential) {
		writeJSON(w, 200, map[string]any{"status": "skipped", "message": "已有 Codex 凭据，未选择替换"})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	credential["cpa_source_pool_id"], credential["cpa_source_file_name"], credential["imported_at"] = pool.ID, body.Name, now
	credential["cpa_pool_id"], credential["cpa_file_name"], credential["cpa_sync_status"] = pool.ID, body.Name, "imported"
	if cpaString(credential, "updated_at") == "" {
		credential["updated_at"] = now
	}
	_, _, err = s.store.UpdateAccountIf(accountToken(target), map[string]any{"codex_credential": credential}, func(current map[string]any) bool { return cpaTargetVersion(current) == body.TargetVersion })
	if errors.Is(err, store.ErrAccountChanged) || errors.Is(err, os.ErrNotExist) {
		writeError(w, 409, "目标账号已变化，请重新预览", "conflict")
		return
	}
	if err != nil {
		writeError(w, 500, "保存绑定失败，未完成导入", "server_error")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "bound", "message": "已绑定完整 Codex 凭据", "account_ref": accountPublicRef(target)})
}

// 同步期间只占用该账号，避免慢 CPA 请求阻塞其他账号的编辑和登录。
func (s *Server) cpaSyncActive(ref, email string) bool {
	_, byRef := s.cpaSyncing.Load("ref:" + ref)
	_, byEmail := s.cpaSyncing.Load("email:" + strings.ToLower(email))
	return byRef || (email != "" && byEmail)
}
