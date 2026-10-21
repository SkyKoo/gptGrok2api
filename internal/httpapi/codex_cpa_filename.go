package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

// 与 CPA 的浏览器 OAuth 命名来源保持一致，优先读取 ID Token 中的账户和套餐。
// JWT 仅作本地元数据解析，不进行在线校验或 Token 刷新。
func codexCPAAccountPlan(credential map[string]any) (string, string) {
	accountID, plan := "", ""
	savedID := cpaString(credential, "account_id")
	for _, key := range []string{"id_token", "access_token"} {
		claims, err := jwtClaims(cpaString(credential, key))
		if err != nil {
			continue
		}
		// 旧凭据可能把用户 sub 记为 account_id；它不能用作 CPA 账户哈希。
		if savedID != "" && savedID == cpaString(claims, "sub") {
			savedID = ""
		}
		auth := mapValue(claims["https://api.openai.com/auth"])
		accountID = firstNonEmpty(accountID, cpaString(auth, "chatgpt_account_id"))
		plan = firstNonEmpty(plan, cpaString(auth, "chatgpt_plan_type"))
	}
	return firstNonEmpty(accountID, savedID), firstNonEmpty(plan, cpaString(credential, "plan_type"), "free")
}

func codexCPASyncFileName(account, credential map[string]any, poolID string) (string, error) {
	// 已有文件继续复用原名，避免改命名规则或套餐变化时留下重复凭据。
	existing := ""
	if cpaString(credential, "cpa_source_pool_id") == poolID {
		existing = cpaString(credential, "cpa_source_file_name")
		if existing == "" {
			return "", fmt.Errorf("原始 CPA 文件名无效，请重新导入")
		}
	} else if cpaString(credential, "cpa_pool_id") == poolID {
		existing = cpaString(credential, "cpa_file_name")
	}
	if existing != "" {
		if !cpaFileNameValid(existing) {
			return "", fmt.Errorf("已保存的 CPA 文件名无效，请核对后重试")
		}
		return existing, nil
	}
	return codexCPANewFileName(account, credential)
}

func codexCPANewFileName(account, credential map[string]any) (string, error) {
	accountID, plan := codexCPAAccountPlan(credential)
	parts := strings.FieldsFunc(plan, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	plan = strings.ToLower(strings.Join(parts, "-"))
	if plan == "" {
		plan = "free"
	}
	email := firstNonEmpty(cpaString(credential, "email"), cpaString(account, "email"))
	if email == "" {
		email = safeExportName(firstNonEmpty(accountID, accountPublicRef(account)), "account")
	}
	name := "codex-"
	if accountID != "" {
		digest := sha256.Sum256([]byte(accountID))
		name += hex.EncodeToString(digest[:])[:8] + "-"
	}
	name += email + "-" + plan + ".json"
	if !cpaFileNameValid(name) {
		return "", fmt.Errorf("邮箱或套餐无法生成有效的 CPA 文件名，请核对账号资料")
	}
	return name, nil
}

// 只迁移 CFM 生成的旧名称；CPA 导入文件始终保留来源名称。
// 用户先删除远端旧文件，再同步时才使用新名称，不自动删除远端文件。
func codexCPAResolveSyncFileName(ctx context.Context, account, credential map[string]any, pool externalCPAPool) (string, error) {
	name, err := codexCPASyncFileName(account, credential, pool.ID)
	if err != nil {
		return "", err
	}
	if cpaString(credential, "cpa_source_pool_id") != "" || cpaString(credential, "cpa_source_file_name") != "" || cpaString(credential, "cpa_pool_id") != pool.ID {
		return name, nil
	}
	email := firstNonEmpty(cpaString(credential, "email"), cpaString(account, "email"))
	legacy := "codex-" + safeExportName(firstNonEmpty(email, cpaString(credential, "account_id"), accountPublicRef(account)), "account") + ".json"
	if name != legacy {
		return name, nil
	}
	newName, err := codexCPANewFileName(account, credential)
	if err != nil {
		return "", err
	}
	if newName == name {
		return name, nil
	}
	exists, err := codexCPAFileExists(ctx, pool, name)
	if err != nil {
		return "", err
	}
	if exists {
		return name, nil
	}
	exists, err = codexCPAFileExists(ctx, pool, newName)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("旧文件已删除，但新名称的 CPA 文件已存在，请先导入核对，避免覆盖远端凭据")
	}
	return newName, nil
}

func codexCPAFileExists(ctx context.Context, pool externalCPAPool, name string) (bool, error) {
	endpoint := strings.TrimRight(pool.BaseURL, "/") + "/v0/management/auth-files/download?" + url.Values{"name": []string{name}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("CPA 地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+pool.SecretKey)
	req.Header.Set("Accept", "application/json")
	client := remoteClient(true)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("无法确认 CPA 旧文件状态，请稍后重试")
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("无法确认 CPA 文件状态 HTTP %d，请核对连接后重试", res.StatusCode)
	}
}
