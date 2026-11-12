package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func (s *Server) registerAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/register")
	switch {
	case (path == "" || path == "/") && r.Method == http.MethodGet:
		s.registerConfig(w, r)
	case (path == "" || path == "/") && r.Method == http.MethodPost:
		s.updateRegisterConfig(w, r)
	case path == "/schedule":
		s.registrationScheduleAPI(w, r)
	case path == "/start" && r.Method == http.MethodPost:
		s.setRegisterEnabled(w, r, true)
	case path == "/stop" && r.Method == http.MethodPost:
		s.setRegisterEnabled(w, r, false)
	case path == "/reset" && r.Method == http.MethodPost:
		s.resetRegister(w, r)
	case path == "/runtime" && r.Method == http.MethodGet:
		s.registerRuntimeStatus(w, r)
	case path == "/openai/jobs" && r.Method == http.MethodGet:
		writeRegistrationJSON(w, http.StatusOK, s.freeRegister.SnapshotPage(positiveInt(r.URL.Query().Get("page"), 1)))
	case path == "/adobe/jobs" && r.Method == http.MethodGet:
		writeRegistrationJSON(w, http.StatusOK, s.adobeRegister.SnapshotPage(positiveInt(r.URL.Query().Get("page"), 1)))
	case path == "/adobe/retry-result" && r.Method == http.MethodPost:
		s.retryAdobeRegistration(w, r)
	case path == "/adobe/retry-registration" && r.Method == http.MethodPost:
		s.retryAdobeRegistrationTask(w, r)
	case path == "/grok/jobs" && r.Method == http.MethodGet:
		writeRegistrationJSON(w, http.StatusOK, s.grokRegister.SnapshotPage(positiveInt(r.URL.Query().Get("page"), 1)))
	case path == "/openai/retry-result" && r.Method == http.MethodPost:
		s.retryFreeRegistration(w, r)
	case path == "/openai/retry-registration" && r.Method == http.MethodPost:
		s.retryFreeRegistrationTask(w, r)
	case path == "/grok/retry-result" && r.Method == http.MethodPost:
		s.retryGrokRegistration(w, r)
	case path == "/grok/retry-registration" && r.Method == http.MethodPost:
		s.retryGrokRegistrationTask(w, r)
	case path == "/openai/codex/start" && r.Method == http.MethodPost:
		s.codexOAuthStart(w, r)
	case path == "/openai/codex/finish" && r.Method == http.MethodPost:
		s.codexOAuthFinish(w, r)
	case path == "/checkout-retries/stop" && r.Method == http.MethodPost:
		s.stopCheckoutRetries(w)
	case path == "/checkout-history/clear" && r.Method == http.MethodPost:
		s.clearCheckoutHistory(w)
	case path == "/outlook-pool/reset" && r.Method == http.MethodPost:
		s.resetOutlookPool(w, r)
	case path == "/outlook-pool/retry-selected" && r.Method == http.MethodPost:
		s.retryOutlookPool(w, r)
	case path == "/gptmail/status" && r.Method == http.MethodPost:
		s.gptMailStatus(w, r, false)
	case path == "/gptmail/refresh-key" && r.Method == http.MethodPost:
		s.gptMailStatus(w, r, true)
	case path == "/openai/survival" && r.Method == http.MethodGet:
		s.openAISurvival(w)
	case path == "/openai/survival" && r.Method == http.MethodPost:
		s.updateOpenAISurvival(w, r)
	case path == "/openai/survival/run" && r.Method == http.MethodPost:
		s.runOpenAISurvival(w)
	case path == "/grok/accounts" && r.Method == http.MethodGet:
		s.listRegisteredGrokAccounts(w, r)
	case strings.HasPrefix(path, "/grok/accounts/") && strings.HasSuffix(path, "/credentials") && r.Method == http.MethodGet:
		s.registeredGrokCredentials(w, strings.TrimSuffix(strings.TrimPrefix(path, "/grok/accounts/"), "/credentials"))
	case path == "/grok/probe-polling" && r.Method == http.MethodPost:
		s.setGrokProbePolling(w, r)
	case path == "/grok/accounts/runtime/snapshot" && r.Method == http.MethodPost:
		s.refreshGrokRuntimeSnapshot(w)
	case path == "/grok/accounts/sync" && r.Method == http.MethodPost:
		s.syncRegisteredGrokAccounts(w, r)
	case path == "/grok/accounts/oauth/authorize" && r.Method == http.MethodPost:
		s.authorizeRegisteredGrokAccounts(w, r)
	case path == "/grok/accounts/runtime/refresh" && r.Method == http.MethodPost:
		s.refreshRegisteredGrokRuntime(w, r)
	case path == "/grok/accounts/runtime/verify" && r.Method == http.MethodPost:
		s.verifyRegisteredGrokRuntime(w, r)
	case path == "/grok/accounts/runtime/disabled" && r.Method == http.MethodPost:
		s.disableRegisteredGrokAccounts(w, r)
	case path == "/grok/accounts" && r.Method == http.MethodDelete:
		s.deleteRegisteredGrokAccounts(w, r)
	case path == "/grok/accounts/export" && r.Method == http.MethodGet:
		s.exportRegisteredGrokAccounts(w, r, nil)
	case path == "/grok/accounts/export" && r.Method == http.MethodPost:
		s.exportSelectedGrokAccounts(w, r)
	case path == "/grok/accounts/export-sso" && r.Method == http.MethodGet:
		s.exportGrokSSO(w, nil)
	case path == "/grok/accounts/export-sso" && r.Method == http.MethodPost:
		s.exportSelectedGrokSSO(w, r)
	case path == "/events" && r.Method == http.MethodGet:
		s.registerEvents(w, r)
	default:
		writeError(w, http.StatusNotFound, "register endpoint not found", "not_found")
	}
}

func (s *Server) refreshRegistrationResinChannel() {
	if s.proxyManager != nil {
		value := s.registerStore.Get()
		profiles := mapValue(value["target_settings"])
		for _, target := range []string{"openai", "grok", "adobe"} {
			s.proxyManager.ConfigureResinChannel(proxyruntime.RegistrationResinChannel+":"+target, stringValue(mapValue(profiles[target])["resin_proxy_url"]))
		}
		legacy := mapValue(profiles[stringValue(value["legacy_resin_target"])])
		s.proxyManager.ConfigureResinChannel(proxyruntime.RegistrationResinChannel, stringValue(legacy["resin_proxy_url"]))
	}
}

func (s *Server) registrationRequestTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	values, explicit := r.URL.Query()["target"]
	target := strings.ToLower(strings.TrimSpace(stringValue(s.registerStore.Get()["target"])))
	if explicit {
		if len(values) != 1 {
			writeError(w, 400, "请指定唯一注册渠道", "invalid_request_error")
			return "", false
		}
		target = strings.ToLower(strings.TrimSpace(values[0]))
	}
	if target != "openai" && target != "grok" && target != "adobe" {
		writeError(w, 400, "不支持的注册渠道", "invalid_request_error")
		return "", false
	}
	return target, true
}

func (s *Server) registerConfig(w http.ResponseWriter, r *http.Request) {
	target, ok := s.registrationRequestTarget(w, r)
	if !ok {
		return
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"register": s.registrationSnapshotForTarget(target)})
}

func (s *Server) updateRegisterConfig(w http.ResponseWriter, r *http.Request) {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	var updates map[string]any
	if !decodeJSON(w, r, &updates) {
		return
	}
	target, ok := s.registrationRequestTarget(w, r)
	if !ok {
		return
	}
	_, scoped := r.URL.Query()["target"]
	running := s.freeRegister.Snapshot()["running"] == true
	if target == "grok" {
		running = s.grokRegister.Snapshot()["running"] == true
	} else if target == "adobe" {
		running = s.adobeRegistrationSnapshot()["running"] == true
	}
	if (!scoped && (s.freeRegister.Snapshot()["running"] == true || s.grokRegister.Snapshot()["running"] == true || s.adobeRegistrationSnapshot()["running"] == true)) || (scoped && running) {
		writeError(w, http.StatusConflict, "请先停止本渠道注册，再修改该渠道配置", "registration_running")
		return
	}
	for _, key := range []string{"target_runtime", "enabled", "stats", "logs", "jobs", "runtime_error", "compensation_error", "registration_schedule", "jobs_total", "jobs_page", "jobs_page_size"} {
		delete(updates, key)
	}
	s.mergeAdobeWorkerKey(updates)
	var err error
	if scoped {
		_, err = s.registerStore.UpdateForTarget(target, updates)
	} else {
		s.mergeHMEPassword(updates)
		_, err = s.registerStore.Update(updates)
		target = stringValue(s.registerStore.Get()["target"])
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	s.refreshRegistrationResinChannel()
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"register": s.registrationSnapshotForTarget(target)})
}

func (s *Server) setRegisterEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if enabled && s.registrationClosing {
		writeError(w, 503, "服务正在停止，请稍后重试", "server_error")
		return
	}
	target, ok := s.registrationRequestTarget(w, r)
	if !ok {
		return
	}
	config := s.registerStore.ConfigForTarget(target)
	var err error
	if target == "openai" {
		if enabled {
			_, err = s.freeRegister.Start(config)
		} else {
			s.freeRegister.Stop()
		}
	} else if target == "adobe" {
		if enabled {
			_, err = s.adobeRegister.Start(config)
		} else {
			s.adobeRegister.Stop()
		}
	} else {
		if enabled {
			_, err = s.grokRegister.Start(config)
		} else {
			s.grokRegister.Stop()
		}
	}
	if err != nil {
		registrationError(w, err)
		return
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"register": s.registrationSnapshotForTarget(target)})
}

func (s *Server) resetRegister(w http.ResponseWriter, r *http.Request) {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	target, ok := s.registrationRequestTarget(w, r)
	if !ok {
		return
	}
	if target == "openai" {
		removed, err := s.freeRegister.ClearFinished()
		if err != nil {
			registrationError(w, err)
			return
		}
		writeRegistrationJSON(w, http.StatusOK, map[string]any{"register": s.registrationSnapshotForTarget(target), "removed": removed})
		return
	}
	if target == "adobe" {
		removed, err := s.adobeRegister.ClearFinished()
		if err != nil {
			registrationError(w, err)
			return
		}
		writeRegistrationJSON(w, 200, map[string]any{"register": s.registrationSnapshotForTarget(target), "removed": removed})
		return
	}
	if target == "grok" {
		removed, err := s.grokRegister.ClearFinished()
		if err != nil {
			registrationError(w, err)
			return
		}
		writeRegistrationJSON(w, http.StatusOK, map[string]any{"register": s.registrationSnapshotForTarget(target), "removed": removed})
		return
	}
	value, err := s.registerStore.Reset()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"register": value})
}

func (s *Server) registerAction(w http.ResponseWriter, message string) {
	writeRegistrationJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "runtime": "go", "error": message})
}

func (s *Server) resetOutlookPool(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope string `json:"scope"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	scope := strings.ToLower(strings.TrimSpace(body.Scope))
	if scope == "" {
		scope = "all"
	}
	if scope != "all" && scope != "unused" && scope != "failed" && scope != "retryable" && scope != "invalid" {
		writeError(w, 400, "invalid Outlook pool reset scope", "invalid_request_error")
		return
	}
	value, err := s.registerStore.ResetOutlookPoolState(scope)
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"register": value, "runtime_available": false, "runtime_error": "Outlook token/Graph registration worker is not configured in Go runtime"})
}

func (s *Server) retryOutlookPool(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string   `json:"provider_id"`
		MailboxIDs []string `json:"mailbox_ids"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ProviderID) == "" || len(uniqueStrings(body.MailboxIDs)) == 0 {
		writeError(w, 400, "provider_id and mailbox_ids are required", "invalid_request_error")
		return
	}
	value, err := s.registerStore.QueueOutlookRetry(body.ProviderID, uniqueStrings(body.MailboxIDs))
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	writeRegistrationJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "register": value, "runtime_available": false, "runtime_error": "Outlook token/Graph registration worker is not configured in Go runtime"})
}

func (s *Server) gptMailStatus(w http.ResponseWriter, r *http.Request, refreshKey bool) {
	var body struct {
		Provider map[string]any `json:"provider"`
		Force    *bool          `json:"force"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	config := s.registerStore.Get()
	mail := mapValue(config["mail"])
	providerConfig := body.Provider
	if providerConfig == nil {
		providerConfig = firstGPTMailConfig(mail)
	}
	if providerConfig == nil {
		writeError(w, http.StatusBadRequest, "未找到 GPTMail 邮箱来源", "invalid_request_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	var result map[string]any
	var err error
	if refreshKey {
		force := body.Force == nil || *body.Force
		result, err = s.gptMail.RefreshPublicKey(ctx, providerConfig, force)
	} else {
		force := body.Force != nil && *body.Force
		result, err = s.gptMail.Status(ctx, providerConfig, force)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "upstream_error")
		return
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"status": result})
}

func firstGPTMailConfig(mail map[string]any) map[string]any {
	providers, _ := mail["providers"].([]any)
	for _, raw := range providers {
		item, ok := raw.(map[string]any)
		if ok && strings.EqualFold(stringValue(item["type"]), "gptmail") {
			return cloneMap(item)
		}
	}
	return nil
}

func (s *Server) stopCheckoutRetries(w http.ResponseWriter) {
	value, err := s.registerStore.StopCheckoutRetries()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"register": value})
}

func (s *Server) clearCheckoutHistory(w http.ResponseWriter) {
	value, err := s.registerStore.ClearCheckoutHistory()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeRegistrationJSON(w, http.StatusOK, value)
}

func (s *Server) setGrokProbePolling(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	status, err := s.registerStore.SetGrokProbeSchedulerEnabled(request.Enabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	select {
	case s.probeWake <- struct{}{}:
	default:
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"probe_scheduler": status})
}

// refreshGrokRuntimeSnapshot imports the local registration archive and then
// probes each eligible SSO against the real Grok rate-limits endpoint.
func (s *Server) refreshGrokRuntimeSnapshot(w http.ResponseWriter) {
	items, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	result := map[string]any{
		"total":           len(items),
		"eligible":        0,
		"added":           0,
		"skipped":         0,
		"missing_sso":     0,
		"disabled":        0,
		"quota_refreshed": 0,
		"quota_failed":    0,
		"errors":          []any{},
	}
	type quotaJob struct {
		item  map[string]any
		token string
	}
	jobs := make([]quotaJob, 0, len(items))
	for _, item := range items {
		token := firstNonEmpty(stringValue(item["sso"]), stringValue(item["access_token"]), stringValue(item["token"]))
		status := strings.ToLower(strings.TrimSpace(stringValue(item["status"])))
		if token == "" {
			result["missing_sso"] = intValue(result["missing_sso"]) + 1
			continue
		}
		if status == "disabled" || !boolValue(item["enabled"], true) {
			result["disabled"] = intValue(result["disabled"]) + 1
			continue
		}
		if status != "" && status != "active" && status != "正常" {
			result["skipped"] = intValue(result["skipped"]) + 1
			continue
		}
		result["eligible"] = intValue(result["eligible"]) + 1
		added, skipped, addErr := s.syncRegisteredGrokRuntimeAccount(item, token)
		if addErr != nil {
			errors, _ := result["errors"].([]any)
			errors = append(errors, map[string]any{"id": stringValue(item["id"]), "error": addErr.Error()})
			result["errors"] = errors
			continue
		}
		result["added"] = intValue(result["added"]) + added
		result["skipped"] = intValue(result["skipped"]) + skipped
		jobs = append(jobs, quotaJob{item: item, token: token})
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	var resultMu sync.Mutex
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			_, _, quotaErr := s.refreshGrokAccountQuotas(ctx, job.token, job.item)
			cancel()
			resultMu.Lock()
			defer resultMu.Unlock()
			if quotaErr != nil {
				result["quota_failed"] = intValue(result["quota_failed"]) + 1
				errorItems, _ := result["errors"].([]any)
				errorItems = append(errorItems, map[string]any{"id": stringValue(job.item["id"]), "stage": "quota", "error": safeRefreshError(quotaErr)})
				result["errors"] = errorItems
				s.recordGrokCredentialFailure(job.token, quotaErr)
				_, _ = s.grokRuntimeStore.UpdateAccountRuntime(job.token, map[string]any{"last_refresh_error": safeRefreshError(quotaErr), "last_refresh_error_at": time.Now().UTC().Format(time.RFC3339)})
				return
			}
			result["quota_refreshed"] = intValue(result["quota_refreshed"]) + 1
		}()
	}
	wg.Wait()
	errors, _ := result["errors"].([]any)
	writeRegistrationJSON(w, http.StatusOK, map[string]any{
		"ok":            len(errors) == 0,
		"refreshed":     true,
		"refreshing":    false,
		"error":         "",
		"summary":       result,
		"quota_refresh": map[string]any{"available": true, "refreshed": result["quota_refreshed"], "failed": result["quota_failed"]},
	})
}

func (s *Server) registerRuntimeStatus(w http.ResponseWriter, r *http.Request) {
	selected, ok := s.registrationRequestTarget(w, r)
	if !ok {
		return
	}
	config := s.registerStore.ConfigForTarget(selected)
	target := stringValue(config["target"])
	if target == "" {
		target = "openai"
	}
	if target == "openai" {
		state := s.freeRegister.Snapshot()
		_, err := registerruntime.ParseFreeConfig(config)
		state["ready"] = err == nil && state["error"] == nil
		state["target"], state["driver"] = "openai", "go_chatgpt_web"
		if err != nil {
			state["configuration_error"] = err.Error()
		}
		writeRegistrationJSON(w, http.StatusOK, map[string]any{"runtime": state})
		return
	}
	if target == "grok" {
		state := s.grokRegister.Snapshot()
		_, err := registerruntime.ParseGrokConfig(config)
		state["ready"] = err == nil && state["error"] == nil
		state["target"], state["driver"] = "grok", "go_grok_protocol"
		if err != nil {
			state["configuration_error"] = err.Error()
		}
		writeRegistrationJSON(w, http.StatusOK, map[string]any{"runtime": state})
		return
	}
	if target == "adobe" {
		state := s.adobeRegister.Snapshot()
		_, err := registerruntime.ParseAdobeConfig(config)
		state["ready"] = err == nil && state["error"] == nil
		state["target"], state["driver"] = "adobe", "cloakbrowser_adobe"
		if err != nil {
			state["configuration_error"] = err.Error()
		}
		writeRegistrationJSON(w, 200, map[string]any{"runtime": state})
		return
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"runtime": s.registerRuntime.Status(target)})
}

func (s *Server) openAISurvival(w http.ResponseWriter) {
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"survival": s.survivalSnapshot()})
}

func (s *Server) updateOpenAISurvival(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if !decodeJSON(w, r, &updates) {
		return
	}
	value, err := s.registerStore.Update(map[string]any{"openai_survival": updates})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	select {
	case s.survivalWake <- struct{}{}:
	default:
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"survival": value["openai_survival"]})
}

func (s *Server) listRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	items, allTotal, baseSummary, err := s.registerStore.ListAccounts("", "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	summary, err := s.attachGrokAccountRuntime(items, baseSummary)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法读取 Grok 调用池状态", "server_error")
		return
	}
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if grokAccountListMatches(item, r.URL.Query().Get("keyword"), r.URL.Query().Get("status")) {
			filtered = append(filtered, item)
		}
	}
	items = filtered
	page := positiveInt(r.URL.Query().Get("page"), 1)
	pageSize := positiveInt(r.URL.Query().Get("page_size"), 100)
	if pageSize > 500 {
		pageSize = 500
	}
	start := (page - 1) * pageSize
	if start > len(items) {
		start = len(items)
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	runtimeAvailable := s.grokQuota != nil && strings.TrimSpace(s.grokQuota.URL) != ""
	runtimeError := ""
	if !runtimeAvailable {
		runtimeError = "Grok runtime quota endpoint is not configured"
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"count": len(items), "total": len(items), "all_total": allTotal, "page": page, "page_size": pageSize, "items": items[start:end], "summary": summary, "runtime_available": runtimeAvailable, "runtime_error": runtimeError, "probe_scheduler": s.registerStore.GrokProbeSchedulerStatus()})
}

func (s *Server) registeredGrokCredentials(w http.ResponseWriter, id string) {
	item, found, err := s.registerStore.Credentials(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "Grok 账号不存在或已删除", "not_found")
		return
	}
	if strings.TrimSpace(item["password"]) == "" {
		writeError(w, http.StatusConflict, "该 Grok 账号未保存登录密码", "conflict")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeRegistrationJSON(w, http.StatusOK, item)
}

func decodeIDs(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var request struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &request) {
		return nil, false
	}
	set := map[string]bool{}
	ids := make([]string, 0, len(request.IDs))
	for _, id := range request.IDs {
		if id = strings.TrimSpace(id); id != "" && !set[id] {
			set[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required", "invalid_request_error")
		return nil, false
	}
	return ids, true
}

func (s *Server) syncRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	items, err := s.registerStore.GetAccounts(ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	added, skipped := 0, 0
	results := make([]map[string]any, 0, len(ids))
	for _, item := range items {
		token := strings.TrimSpace(stringValue(item["sso"]))
		if token == "" {
			results = append(results, map[string]any{"id": item["id"], "ok": false, "error": "账号未保存 SSO 登录态"})
			continue
		}
		count, _, addErr := s.syncRegisteredGrokRuntimeAccount(item, token)
		if addErr != nil {
			results = append(results, map[string]any{"id": item["id"], "ok": false, "error": "同步失败，请检查账号是否异常或禁用并先验证登录态"})
			continue
		}
		results = append(results, map[string]any{"id": item["id"], "ok": true, "sync_state": "synced"})
		if count > 0 {
			added += count
		} else {
			skipped++
		}
	}
	syncState := "sync_failed"
	if added+skipped == len(ids) {
		syncState = "synced"
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"ok": added+skipped == len(ids), "sync_state": syncState, "added": added, "skipped": skipped, "verification_pending": len(ids) - added - skipped, "summary": map[string]int{"total": len(ids), "ok": added + skipped, "fail": len(ids) - added - skipped}, "results": results})
}

// syncRegisteredGrokRuntimeAccount keeps the shared request pool aware that
// this token is a Grok SSO account. Without the source marker, JWT-shaped SSO
// values can be mistaken for OpenAI sessions and be invalidated by GPT-only
// account checks. Grok media routes then cannot find the account either.
func (s *Server) syncRegisteredGrokRuntimeAccount(item map[string]any, token string) (int, int, error) {
	s.proxyBindingMu.Lock()
	defer s.proxyBindingMu.Unlock()
	// Background refresh may carry an older account snapshot. Fetch routing
	// metadata again under the binding lock before syncing the runtime pool.
	if id := stringValue(item["id"]); id != "" {
		rows, err := s.registerStore.ExportAccounts([]string{id})
		if err != nil {
			return 0, 0, err
		}
		if len(rows) > 0 {
			item = rows[0]
		}
	}
	return s.syncRegisteredGrokRuntimeAccountLocked(item, token)
}
func (s *Server) syncRegisteredGrokRuntimeAccountLocked(item map[string]any, token string) (int, int, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, 0, errors.New("registered Grok account missing SSO")
	}
	if accounts.IsAbnormal(item) || !boolValue(item["enabled"], true) || stringValue(item["status"]) == "disabled" {
		return 0, 0, errors.New("Grok account requires successful verification before runtime sync")
	}
	fields := map[string]any{
		"access_token": token,
		"source_type":  "grok_protocol",
		"platform":     "grok",
		"pool":         "basic",
		"status":       "正常",
		"enabled":      true,
	}
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl", "proxy_policy", "proxy_binding", "proxy_binding_history", "browser_profile", "fp", "web_cookies", "registration_job_id"} {
		if value, ok := item[key]; ok {
			fields[key] = value
		}
	}
	for _, key := range []string{"email", "cookie_header", "sso_rw"} {
		if value := stringValue(item[key]); value != "" {
			fields[key] = value
		}
	}
	if id := stringValue(item["id"]); id != "" {
		fields["grok_account_id"] = id
	}
	added, skipped, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{fields})
	if err != nil {
		return 0, 0, err
	}
	updates := cloneMap(fields)
	// Clear GPT-side validation markers left by earlier imports of the same
	// SSO token. A successful Grok quota refresh is the authoritative check.
	for _, key := range []string{
		"last_account_validation_error", "last_error_kind", "last_error_status", "last_error_message", "last_error_at",
		"last_refresh_error", "last_refresh_error_at", "last_refresh_warning", "last_refresh_warning_at",
		"status_reason_code", "relogin_blocked", "survival_check_error", "survival_status", "survival_last_probe_status",
	} {
		updates[key] = nil
	}
	if _, _, err := s.grokRuntimeStore.UpdateAccountIf(token, updates, func(current map[string]any) bool {
		return !accounts.IsAbnormal(current) && boolValue(current["enabled"], true) && stringValue(current["status"]) != "disabled"
	}); err != nil {
		if !errors.Is(err, store.ErrAccountChanged) {
			return 0, 0, err
		}
		// Routing/profile synchronization remains available for disabled or
		// abnormal accounts, but it must never change their eligibility.
		metadata := cloneMap(fields)
		delete(metadata, "status")
		delete(metadata, "enabled")
		if _, _, err := s.grokRuntimeStore.UpdateAccount(token, metadata); err != nil {
			return 0, 0, err
		}
	}
	// Remove legacy copies created before Grok had its own runtime store. GPT
	// account management must never see or validate a Grok SSO token.
	_, _, _ = s.store.DeleteAccounts([]string{token})
	return added, skipped, nil
}

func (s *Server) authorizeRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	results := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		task, reused, err := s.startGrokOAuth(id)
		if err != nil {
			results = append(results, map[string]any{"id": id, "ok": false, "status": "failed", "error": grokOAuthMessages[grokOAuthErrorCode(err)]})
			continue
		}
		results = append(results, map[string]any{"id": id, "ok": true, "status": task.Status, "job_id": task.ID, "reused": reused})
	}
	queued, reused, failed := 0, 0, 0
	for _, result := range results {
		if result["ok"] == true {
			if result["reused"] == true {
				reused++
			} else {
				queued++
			}
		} else {
			failed++
		}
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"summary": map[string]int{"total": len(results), "queued": queued, "reused": reused, "skipped": 0, "failed": failed}, "results": results})
}

func (s *Server) disableRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		IDs      []string `json:"ids"`
		Disabled bool     `json:"disabled"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	ids := uniqueIDs(request.IDs)
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required", "invalid_request_error")
		return
	}
	count, err := s.registerStore.SetDisabled(ids, request.Disabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"disabled": request.Disabled, "summary": map[string]int{"total": len(ids), "ok": count, "fail": len(ids) - count}})
}

func (s *Server) deleteRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	accounts, err := s.registerStore.ExportAccounts(ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	deletedBefore := time.Now()
	removed, err := s.registerStore.Delete(ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	for _, account := range accounts {
		account["platform"] = "grok"
	}
	hmeCleanup := s.cleanupHMEMailboxes(r.Context(), accounts, deletedBefore)
	writeRegistrationJSON(w, http.StatusOK, map[string]any{"removed": removed, "count": removed, "upstream_deleted": 0, "hme_cleanup": hmeCleanup})
}

func (s *Server) exportRegisteredGrokAccounts(w http.ResponseWriter, r *http.Request, ids []string) {
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "cpa" {
		s.exportCPA(w, ids)
		return
	}
	items, err := s.registerStore.ExportAccounts(ids)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	payload := map[string]any{"exported_at": time.Now().UTC(), "proxies": []any{}, "accounts": items}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="grok-accounts-sub2api.json"`)
	_, _ = w.Write(append(raw, '\n'))
}

func (s *Server) exportSelectedGrokAccounts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		IDs    []string `json:"ids"`
		Format string   `json:"format"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if strings.EqualFold(request.Format, "cpa") {
		s.exportCPA(w, request.IDs)
		return
	}
	s.exportRegisteredGrokAccounts(w, r, request.IDs)
}

func (s *Server) exportCPA(w http.ResponseWriter, ids []string) {
	items, err := s.registerStore.ExportAccounts(ids)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for index, item := range items {
		name := fmt.Sprintf("grok-account-%d.json", index+1)
		writer, createErr := archive.Create(name)
		if createErr != nil {
			writeError(w, http.StatusInternalServerError, createErr.Error(), "server_error")
			return
		}
		raw, _ := json.MarshalIndent(item, "", "  ")
		_, _ = writer.Write(append(raw, '\n'))
	}
	if err := archive.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="grok-accounts-cpa.zip"`)
	_, _ = w.Write(buffer.Bytes())
}

func (s *Server) exportGrokSSO(w http.ResponseWriter, ids []string) {
	items, err := s.registerStore.ExportAccounts(ids)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if token := strings.TrimSpace(stringValue(item["sso"])); token != "" {
			lines = append(lines, token)
		}
	}
	if len(lines) == 0 {
		writeError(w, http.StatusBadRequest, "暂无可导出的 Grok SSO 账号", "invalid_request_error")
		return
	}
	sort.Strings(lines)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="grok-sso.txt"`)
	_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
}

func (s *Server) exportSelectedGrokSSO(w http.ResponseWriter, r *http.Request) {
	ids, ok := decodeIDs(w, r)
	if !ok {
		return
	}
	s.exportGrokSSO(w, ids)
}

func (s *Server) registerEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := ""
	for {
		payload := s.registrationSnapshot()
		raw, _ := json.Marshal(payload)
		if string(raw) != last {
			writeSSE(w, payload)
			last = string(raw)
			if flusher != nil {
				flusher.Flush()
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func uniqueIDs(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
