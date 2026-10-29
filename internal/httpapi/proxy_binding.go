package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// proxyBindingAPI is an explicit admin action, never a retry/failover hook.
func (s *Server) proxyBindingAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var input struct {
		Platform  string `json:"platform"`
		AccountID string `json:"account_id"`
		Reference string `json:"reference"`
		Reason    string `json:"reason"`
	}
	if r.Method == http.MethodGet {
		input.Platform = r.URL.Query().Get("platform")
		input.AccountID = r.URL.Query().Get("account_id")
	} else if r.Method == http.MethodPost {
		if !decodeJSON(w, r, &input) {
			return
		}
	} else {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	if input.Platform != "openai" && input.Platform != "grok" {
		writeError(w, 400, "platform must be openai or grok", "invalid_request_error")
		return
	}
	if strings.TrimSpace(input.AccountID) == "" {
		writeError(w, 400, "account_id is required", "invalid_request_error")
		return
	}
	s.proxyBindingMu.Lock()
	defer s.proxyBindingMu.Unlock()
	var account map[string]any
	var err error
	if input.Platform == "grok" {
		var rows []map[string]any
		rows, err = s.registerStore.ExportAccounts([]string{input.AccountID})
		if len(rows) > 0 {
			account = rows[0]
		}
	} else {
		var rows []map[string]any
		rows, err = s.store.AccountList()
		for _, row := range rows {
			if accountRefForToken(rows, accountToken(row)) == input.AccountID || stringValue(row["id"]) == input.AccountID {
				account = row
				break
			}
		}
	}
	if err != nil {
		writeError(w, 500, "account read failed", "server_error")
		return
	}
	if account == nil {
		writeError(w, 404, "account not found", "not_found")
		return
	}
	if r.Method == http.MethodPost {
		input.Reason = strings.TrimSpace(input.Reason)
		if input.Reason == "" || len(input.Reason) > 500 {
			writeError(w, 400, "填写切换原因（1–500 字节）", "invalid_request_error")
			return
		}
		if input.Reference != "direct" && !strings.HasPrefix(input.Reference, "node:") {
			u, parseErr := url.Parse(input.Reference)
			if parseErr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				writeError(w, 400, "请选择明确的代理节点、单个代理 URL 或直连", "invalid_request_error")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		lease, leaseErr := s.proxyManager.AcquireTask(ctx, input.Reference, input.Platform, "account", nil)
		if leaseErr != nil {
			writeError(w, 409, "所选代理节点不可用，绑定未修改", "proxy_unavailable")
			return
		}
		lease.Release()
		if input.Platform == "grok" {
			account, err = s.registerStore.BindProxy(input.AccountID, lease.Binding, input.Reason)
			if err == nil {
				token := stringValue(account["sso"])
				if token != "" {
					updates := map[string]any{"proxy": account["proxy"], "proxy_binding": account["proxy_binding"], "proxy_binding_history": account["proxy_binding_history"]}
					_, _, err = s.grokRuntimeStore.UpdateAccount(token, updates)
					if errors.Is(err, os.ErrNotExist) {
						err = nil
					}
				}
			}
		} else {
			account, err = s.store.BindProxy(accountToken(account), lease.Binding, input.Reason)
		}
		if err != nil {
			writeError(w, 500, "绑定保存或运行时同步失败，请刷新查询当前绑定后再处理", "server_error")
			return
		}
	}
	binding := proxyruntime.ParseBinding(account["proxy_binding"])
	label := "未绑定"
	if binding != nil {
		label = binding.Label()
	}
	_, _, resolveErr := s.proxyManager.ResolveBound(account)
	writeJSON(w, 200, map[string]any{"platform": input.Platform, "account_id": input.AccountID, "binding": publicProxyBinding(binding), "label": label, "available": !errors.Is(resolveErr, proxyruntime.ErrBindingUnavailable), "history": account["proxy_binding_history"]})
}
func publicProxyBinding(b *proxyruntime.Binding) any {
	if b == nil {
		return nil
	}
	copy := *b
	if !strings.HasPrefix(copy.Reference, "node:") && !strings.HasPrefix(copy.Reference, "default:") && copy.Reference != "direct" {
		copy.Reference = "custom:" + copy.EndpointHash
	}
	return copy
}
