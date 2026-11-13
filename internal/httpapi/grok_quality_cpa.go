package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"github.com/auucoder/gptgrok2api-go/internal/xaiquality"
)

type qualityCPAIdentity struct{ Name, Index string }

func qualityCPAClient() *http.Client {
	client := remoteClient(true)
	client.Timeout = 70 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func qualityCPARequest(ctx context.Context, client *http.Client, pool externalCPAPool, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(pool.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("CPA 地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+pool.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, errors.New("CPA 请求失败或超时")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (3<<20)+1))
	if err != nil || len(raw) > 3<<20 {
		return res.StatusCode, nil, errors.New("CPA 响应无法完整读取")
	}
	return res.StatusCode, raw, nil
}

// Lookup requires the saved pool and exact file name, provider and email. Never
// guess a file from an email or probe the public load-balanced model endpoint.
func qualityCPAFile(ctx context.Context, client *http.Client, pool externalCPAPool, account oauth.Account) (qualityCPAIdentity, bool, error) {
	if account.CPASync == nil || account.CPASync.PoolID != pool.ID || !cpaFileNameValid(account.CPASync.FileName) || strings.TrimSpace(account.Email) == "" {
		return qualityCPAIdentity{}, false, errors.New("缺少已保存的 CPA 目标或文件名，请人工核对")
	}
	name := account.CPASync.FileName
	status, raw, err := qualityCPARequest(ctx, client, pool, "GET", "/v0/management/auth-files?"+url.Values{"name": {name}}.Encode(), nil)
	if err != nil {
		return qualityCPAIdentity{}, false, err
	}
	if status != 200 {
		return qualityCPAIdentity{}, false, fmt.Errorf("读取 CPA 凭据列表返回 HTTP %d", status)
	}
	var body struct {
		Files *[]struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			Type     string `json:"type"`
			Email    string `json:"email"`
			Index    string `json:"auth_index"`
		} `json:"files"`
		Total *int `json:"total"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Files == nil {
		return qualityCPAIdentity{}, false, errors.New("CPA 凭据列表格式无效")
	}
	var found qualityCPAIdentity
	for _, file := range *body.Files {
		if file.Name != name {
			continue
		}
		if found.Name != "" || firstNonEmpty(file.Provider, file.Type) != "xai" || !strings.EqualFold(strings.TrimSpace(file.Email), strings.TrimSpace(account.Email)) {
			return qualityCPAIdentity{}, false, errors.New("CPA 文件身份不匹配或不唯一，请人工核对")
		}
		found = qualityCPAIdentity{Name: name, Index: file.Index}
	}
	if found.Name == "" && body.Total != nil && *body.Total > len(*body.Files) {
		return found, false, errors.New("CPA 返回分页列表，无法确认凭据不存在")
	}
	return found, found.Name != "", nil
}

func (s *Server) qualityCPACaller(ctx context.Context, account oauth.Account) (xaiquality.Caller, *xaiquality.Removal, error) {
	if account.CPASync == nil || account.CPASync.PoolID == "" {
		return nil, nil, errors.New("缺少 CPA 交付记录，不能使用本地旧凭据检测")
	}
	pool, ok := s.external.getCPA(account.CPASync.PoolID)
	if !ok {
		return nil, nil, errors.New("原 CPA 连接不存在，请人工核对")
	}
	client := qualityCPAClient()
	identity, exists, err := qualityCPAFile(ctx, client, pool, account)
	if err != nil {
		return nil, nil, err
	}
	if !exists || identity.Index == "" {
		return nil, nil, errors.New("CPA 凭据不存在或没有 auth_index，无法指定账号检测")
	}
	target := &xaiquality.Removal{PoolID: pool.ID, PoolName: pool.Name, PoolURL: strings.TrimRight(pool.BaseURL, "/"), FileName: identity.Name, AuthIndex: identity.Index}
	call := func(ctx context.Context, payload []byte) (xaiquality.Response, error) {
		current, exists, err := qualityCPAFile(ctx, client, pool, account)
		if err != nil || !exists || current != identity {
			return xaiquality.Response{}, errors.New("CPA 检测目标已变化")
		}
		// CPA substitutes and refreshes its own token. CFM receives no token and
		// inherits the selected credential's CPA proxy instead of its local route.
		body, _ := json.Marshal(map[string]any{"auth_index": identity.Index, "method": "POST", "url": strings.TrimRight(s.cfg.XAICLIBaseURL, "/") + "/responses", "header": provider.XAIQualityHeaders(), "data": string(payload)})
		status, raw, err := qualityCPARequest(ctx, client, pool, "POST", "/v0/management/api-call", body)
		if err != nil {
			return xaiquality.Response{}, err
		}
		if status != 200 {
			return xaiquality.Response{}, fmt.Errorf("CPA 指定账号调用返回 HTTP %d", status)
		}
		var result struct {
			Status int    `json:"status_code"`
			Body   string `json:"body"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Status < 100 {
			return xaiquality.Response{}, errors.New("CPA 调用响应格式无效")
		}
		return xaiquality.Response{Status: result.Status, Body: []byte(result.Body)}, nil
	}
	return call, target, nil
}

// removeQualityCPA only deletes the exact identity used by the completed probe.
// A failed/unsupported delete is an actionable manual-removal label, never a
// reason to clear the quality block or claim the remote file was removed.
func (s *Server) removeQualityCPA(ctx context.Context, account oauth.Account, target xaiquality.Removal) xaiquality.Removal {
	target.Status = "manual_required"
	target.At = time.Now().Unix()
	pool, ok := s.external.getCPA(target.PoolID)
	if !ok {
		target.Error = "原 CPA 连接不存在，请人工删除对应凭据"
		return target
	}
	if target.PoolURL == "" || target.PoolURL != strings.TrimRight(pool.BaseURL, "/") {
		target.Error = "CPA 连接地址已变化，未自动删除，请人工核对原目标"
		return target
	}
	client := qualityCPAClient()
	identity, exists, err := qualityCPAFile(ctx, client, pool, account)
	if err != nil {
		target.Error = err.Error()
		return target
	}
	if !exists {
		target.Status = "removed"
		target.Error = ""
		return target
	}
	if identity.Name != target.FileName || identity.Index == "" || identity.Index != target.AuthIndex {
		target.Error = "CPA 凭据身份已变化，未自动删除，请人工核对"
		return target
	}
	status, _, deleteErr := qualityCPARequest(ctx, client, pool, "DELETE", "/v0/management/auth-files?"+url.Values{"name": {identity.Name}}.Encode(), nil)
	_, exists, readErr := qualityCPAFile(ctx, client, pool, account)
	if readErr == nil && !exists {
		target.Status = "removed"
		target.Error = ""
		return target
	}
	if deleteErr != nil {
		target.Error = "删除结果未知，请在 CPA 人工核对"
	} else if status == 404 || status == 405 || status == 501 {
		target.Error = "此 CPA 不支持凭据删除接口，请人工删除"
	} else if status == 401 || status == 403 {
		target.Error = "CPA 不允许删除凭据，请人工删除"
	} else {
		target.Error = "CPA 删除后未确认凭据消失，请人工核对"
	}
	return target
}
