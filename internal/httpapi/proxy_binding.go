package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
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
	if input.Platform != "openai" && input.Platform != "grok" && input.Platform != "adobe" {
		writeError(w, 400, "platform must be openai, grok or adobe", "invalid_request_error")
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
	} else if input.Platform == "adobe" {
		if s.adobeStore == nil {
			writeError(w, 503, "Adobe 账号服务不可用", "not_configured")
			return
		}
		var saved adobe.Account
		saved, err = s.adobeStore.Get(input.AccountID)
		if err == nil {
			account = saved.RoutingFields()
		} else if errors.Is(err, adobe.ErrNotFound) {
			err = nil
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
		input.Reference = strings.TrimSpace(input.Reference)
		input.Reason = strings.TrimSpace(input.Reason)
		if input.Reason == "" || len(input.Reason) > 500 {
			writeError(w, 400, "填写切换原因（1–500 字节）", "invalid_request_error")
			return
		}
		if input.Reference != "inherit" && input.Reference != "direct" && !strings.HasPrefix(input.Reference, "node:") {
			u, parseErr := url.Parse(input.Reference)
			if parseErr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				writeError(w, 400, "请选择跟随默认出口、代理节点、单个代理 URL 或直连", "invalid_request_error")
				return
			}
		}
		binding := proxyruntime.Binding{Reference: "inherit", Platform: input.Platform, Purpose: "account", BoundAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if input.Reference != "inherit" {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			lease, leaseErr := s.proxyManager.AcquireTask(ctx, input.Reference, input.Platform, "account", nil)
			if leaseErr != nil {
				writeError(w, 409, "所选代理节点不可用，绑定未修改", "proxy_unavailable")
				return
			}
			binding = lease.Binding
			lease.Release()
		}
		if input.Platform == "grok" {
			account, err = s.registerStore.BindProxy(input.AccountID, binding, input.Reason)
			if err == nil {
				token := stringValue(account["sso"])
				if token != "" {
					updates := map[string]any{}
					for _, key := range []string{"proxy", "proxy_url", "proxyUrl", "proxy_policy", "proxy_binding", "proxy_binding_history"} {
						updates[key] = account[key]
					}
					_, _, err = s.grokRuntimeStore.UpdateAccount(token, updates)
					if errors.Is(err, os.ErrNotExist) {
						err = nil
					}
				}
			}
		} else if input.Platform == "adobe" {
			var saved adobe.Account
			saved, err = s.adobeStore.BindProxy(input.AccountID, binding, input.Reason)
			if err == nil {
				account = saved.RoutingFields()
			}
		} else {
			account, err = s.store.BindProxy(accountToken(account), binding, input.Reason)
		}
		if err != nil {
			writeError(w, 500, "绑定保存或运行时同步失败，请刷新查询当前绑定后再处理", "server_error")
			return
		}
	}
	effective := proxyruntime.AccountRoutingFields(account)
	binding := proxyruntime.ParseBinding(effective["proxy_binding"])
	policy, label := "explicit", "自定义代理"
	if proxyruntime.AccountInheritsDefault(account) {
		policy, label = "inherit", "跟随默认出口"
	} else if binding != nil {
		label = binding.Label()
	} else if ref := firstNonEmpty(stringValue(effective["proxy"]), stringValue(effective["proxy_url"]), stringValue(effective["proxyUrl"])); ref == "direct" {
		label = "direct"
	} else if strings.HasPrefix(ref, "group:") {
		label = "代理组 " + strings.TrimPrefix(ref, "group:")
	}
	_, _, resolveErr := s.proxyManager.ResolveBound(effective)
	writeJSON(w, 200, map[string]any{"platform": input.Platform, "account_id": input.AccountID, "policy": policy, "binding": publicProxyBinding(binding), "label": label, "available": resolveErr == nil, "history": account["proxy_binding_history"]})
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
