package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func mailboxSourceFromProvider(raw map[string]any, index int) mailboxstore.Source {
	provider := cloneMap(raw)
	typeName := strings.TrimSpace(strings.ToLower(stringValue(provider["type"])))
	if typeName == "" {
		typeName = "unknown"
	}
	id := strings.TrimSpace(stringValue(provider["id"]))
	if id == "" {
		id = fmt.Sprintf("%s-%d", typeName, index+1)
		provider["id"] = id
	}
	label := firstNonEmpty(stringValue(provider["label"]), fmt.Sprintf("邮箱来源 %d", index+1))
	capabilities := mailboxSourceCapabilities(typeName, provider)
	return mailboxstore.Source{ID: id, Type: typeName, Label: label, Enabled: boolValue(provider["enable"], true), Config: provider, Capabilities: capabilities, Status: stringValue(capabilities["status"])}
}

// Source capabilities describe the current adapter, not an administrator
// setting. Derive them from the saved source config so sources created before
// a new registration target do not retain an obsolete capability snapshot.
func mailboxSourceCapabilities(sourceType string, config map[string]any) map[string]any {
	ready := strings.EqualFold(strings.TrimSpace(sourceType), "icloud_hme") &&
		strings.TrimSpace(stringValue(config["api_base"])) != "" &&
		strings.TrimSpace(stringValue(config["admin_password"])) != "" &&
		strings.TrimSpace(stringValue(config["account_id"])) != ""
	status := "config_only"
	if ready {
		status = "available"
	}
	return map[string]any{"openai": ready, "grok": ready, "adobe": ready, "create": ready, "status": status}
}

func (s *Server) syncMailboxSources(ctx context.Context) error {
	if s.mailboxStore == nil {
		return nil
	}
	existing, err := s.mailboxStore.ListSources(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, source := range existing {
		known[source.ID] = true
	}
	mail := cloneMap(mapValue(s.registerStore.Get()["mail"]))
	providers, _ := mail["providers"].([]any)
	migratedIDs := make([]string, 0, len(providers))
	configChanged := false
	for index, raw := range providers {
		provider := mapValue(raw)
		source := mailboxSourceFromProvider(provider, index)
		if strings.TrimSpace(stringValue(mapValue(raw)["id"])) != source.ID {
			provider["id"] = source.ID
			configChanged = true
		}
		if !known[source.ID] {
			if _, err := s.mailboxStore.UpsertSource(ctx, source); err != nil {
				return err
			}
			known[source.ID] = true
		}
		migratedIDs = append(migratedIDs, source.ID)
	}
	if configChanged {
		mail["providers"] = providers
		if _, err := s.registerStore.Update(map[string]any{"mail": mail}); err != nil {
			return err
		}
	}
	// 导入完整来源目录后再判断历史账号是否唯一。
	for _, id := range migratedIDs {
		if _, err := s.mailboxStore.BackfillSourceID(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func mailboxSourcePublic(source mailboxstore.Source) map[string]any {
	capabilities := mailboxSourceCapabilities(source.Type, source.Config)
	config := cloneMap(source.Config)
	if _, ok := config["admin_password"]; ok {
		config["admin_password_set"] = stringValue(config["admin_password"]) != ""
		delete(config, "admin_password")
	}
	return map[string]any{
		"id": source.ID, "type": source.Type, "label": source.Label, "enable": source.Enabled,
		"status": capabilities["status"], "last_error": source.LastError, "capabilities": capabilities,
		"config": config, "last_sync_at": source.LastSyncAt, "created_at": source.CreatedAt, "updated_at": source.UpdatedAt,
	}
}

func newMailboxSourceID(sourceType string, taken map[string]bool) string {
	prefix := strings.ToLower(strings.TrimSpace(sourceType))
	if prefix == "" {
		prefix = "source"
	}
	for attempt := 0; attempt < 8; attempt++ {
		var raw [6]byte
		if _, err := rand.Read(raw[:]); err == nil {
			candidate := prefix + "-" + hex.EncodeToString(raw[:])
			if !taken[candidate] {
				return candidate
			}
		}
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func validateHMESourceConfig(apiBase, accountID, password string, requireComplete bool) string {
	if apiBase == "" && accountID == "" && password == "" && !requireComplete {
		return ""
	}
	if apiBase != "" || requireComplete {
		u, err := url.Parse(apiBase)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "API Base 无效，请填写 http:// 或 https:// 地址"
		}
	}
	if requireComplete {
		if accountID == "" {
			return "请填写上游账号 ID"
		}
		if password == "" {
			return "请填写管理员密码"
		}
		if err := (registerruntime.HMEConfig{BaseURL: apiBase, Password: password, AccountID: accountID}).Validate(); err != nil {
			return "iCloud 邮箱上游配置无效"
		}
	}
	return ""
}

func (s *Server) iCloudMailboxSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !s.mailboxDBReady(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		sources, err := s.mailboxStore.ListSources(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
			return
		}
		items := make([]map[string]any, 0, len(sources))
		for _, source := range sources {
			items = append(items, mailboxSourcePublic(source))
		}
		writeJSON(w, http.StatusOK, map[string]any{"sources": items})
	case http.MethodPost:
		const conflictMessage = "相同 API Base 和上游账号 ID 的邮箱来源已存在"
		var request struct {
			Source map[string]any `json:"source"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.Source == nil {
			writeError(w, http.StatusBadRequest, "缺少邮箱来源配置", "invalid_request_error")
			return
		}
		s.registrationMu.Lock()
		defer s.registrationMu.Unlock()
		existingSources, err := s.mailboxStore.ListSources(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
			return
		}
		takenIDs := make(map[string]bool, len(existingSources))
		var existing mailboxstore.Source
		found := false
		id := strings.TrimSpace(stringValue(request.Source["id"]))
		if len(id) > 128 {
			writeError(w, http.StatusBadRequest, "来源 ID 过长", "invalid_request_error")
			return
		}
		for _, item := range existingSources {
			takenIDs[item.ID] = true
			if id != "" && item.ID == id {
				existing = item
				found = true
			}
		}
		creating := !found
		rawConfig := mapValue(request.Source["config"])
		var sourceType string
		if creating {
			sourceType = strings.ToLower(strings.TrimSpace(firstNonEmpty(stringValue(request.Source["type"]), stringValue(rawConfig["type"]), "icloud_hme")))
			if sourceType != "icloud_hme" {
				writeError(w, http.StatusBadRequest, "当前仅支持新增 iCloud 邮箱上游来源", "invalid_request_error")
				return
			}
		} else {
			sourceType = strings.ToLower(strings.TrimSpace(firstNonEmpty(stringValue(request.Source["type"]), existing.Type, stringValue(existing.Config["type"]))))
			if sourceType == "" {
				sourceType = "unknown"
			}
		}

		config := map[string]any{}
		if !creating {
			config = cloneMap(existing.Config)
		}
		delete(config, "admin_password_set")
		oldAPIBase := strings.TrimRight(strings.TrimSpace(stringValue(existing.Config["api_base"])), "/")
		oldPassword, _ := existing.Config["admin_password"].(string)
		passwordProvided := false
		for key, value := range rawConfig {
			if key == "admin_password_set" {
				continue
			}
			if key == "admin_password" {
				rawPassword, _ := value.(string)
				if rawPassword == "" {
					continue
				}
				passwordProvided = true
				config[key] = rawPassword
				continue
			}
			config[key] = value
		}

		enabledDefault := true
		if !creating {
			enabledDefault = existing.Enabled
		}
		enabled := boolValue(request.Source["enable"], enabledDefault)
		label := strings.TrimSpace(firstNonEmpty(stringValue(request.Source["label"]), existing.Label, stringValue(config["label"])))

		var replay mailboxstore.Source
		if sourceType == "icloud_hme" {
			apiBase := strings.TrimRight(strings.TrimSpace(stringValue(config["api_base"])), "/")
			accountID := strings.TrimSpace(stringValue(config["account_id"]))
			config["api_base"] = apiBase
			config["account_id"] = accountID
			config["delete_hme_mailbox_on_account_delete"] = boolValue(config["delete_hme_mailbox_on_account_delete"], false)
			if !creating && !passwordProvided && oldPassword != "" && oldAPIBase != "" && apiBase != oldAPIBase {
				writeError(w, http.StatusBadRequest, "修改 API Base 时请重新填写管理员密码", "invalid_request_error")
				return
			}
			password, _ := config["admin_password"].(string)
			if msg := validateHMESourceConfig(apiBase, accountID, password, creating || enabled); msg != "" {
				writeError(w, http.StatusBadRequest, msg, "invalid_request_error")
				return
			}
			if apiBase != "" && accountID != "" {
				for _, other := range existingSources {
					if other.ID == id || !strings.EqualFold(other.Type, "icloud_hme") {
						continue
					}
					otherBase := strings.TrimRight(strings.TrimSpace(stringValue(other.Config["api_base"])), "/")
					otherAccount := strings.TrimSpace(stringValue(other.Config["account_id"]))
					if !strings.EqualFold(otherBase, apiBase) || otherAccount != accountID {
						continue
					}
					if !creating || id != "" || replay.ID != "" {
						writeError(w, http.StatusConflict, conflictMessage, "mailbox_source_conflict")
						return
					}
					replay = other
				}
			}
		}

		if replay.ID != "" {
			id = replay.ID
			if label == "" {
				label = replay.Label
			}
		} else if id == "" {
			id = newMailboxSourceID(sourceType, takenIDs)
		}
		if label == "" {
			label = fmt.Sprintf("邮箱来源 %d", len(existingSources)+1)
		}
		config["id"] = id
		config["type"] = sourceType
		config["label"] = label
		config["enable"] = enabled
		delete(config, "admin_password_set")
		saved := replay
		if replay.ID != "" {
			// 仅相同的创建请求可以沿用已保存的来源并补写注册配置。
			if !reflect.DeepEqual(config, replay.Config) {
				writeError(w, http.StatusConflict, conflictMessage, "mailbox_source_conflict")
				return
			}
		} else {
			source := mailboxSourceFromProvider(config, len(existingSources))
			saved, err = s.mailboxStore.UpsertSource(r.Context(), source)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
				return
			}
		}

		current := s.registerStore.Get()
		mail := cloneMap(mapValue(current["mail"]))
		delete(mail, "source_id")
		providers, _ := mail["providers"].([]any)
		updated := false
		wasEnabled := false
		for index, raw := range providers {
			provider := mapValue(raw)
			if strings.TrimSpace(stringValue(provider["id"])) != id {
				continue
			}
			wasEnabled = strings.EqualFold(stringValue(provider["type"]), "icloud_hme") && boolValue(provider["enable"], true)
			mail["providers"] = append(append([]any{}, providers[:index]...), append([]any{saved.Config}, providers[index+1:]...)...)
			updated = true
			break
		}
		if !updated {
			mail["providers"] = append(providers, saved.Config)
		}
		// 按注册配置判断是否首次启用，重试也能固定原来隐式使用的来源。
		previousSingleHMEID := ""
		if sourceType == "icloud_hme" && enabled && !wasEnabled {
			enabledOthers := make([]string, 0, 2)
			for _, other := range existingSources {
				if other.ID != id && other.Enabled && strings.EqualFold(other.Type, "icloud_hme") {
					enabledOthers = append(enabledOthers, other.ID)
				}
			}
			if len(enabledOthers) == 1 {
				previousSingleHMEID = enabledOthers[0]
			}
		}
		updates := map[string]any{"mail": mail}
		if previousSingleHMEID != "" {
			profiles := mapValue(current["target_settings"])
			targetUpdates := map[string]any{}
			for _, target := range []string{"openai", "grok", "adobe"} {
				if strings.TrimSpace(stringValue(mapValue(profiles[target])["mail_source_id"])) == "" {
					targetUpdates[target] = map[string]any{"mail_source_id": previousSingleHMEID}
				}
			}
			if len(targetUpdates) > 0 {
				updates["target_settings"] = targetUpdates
			}
		}
		if _, updateErr := s.registerStore.Update(updates); updateErr != nil {
			writeError(w, http.StatusInternalServerError, updateErr.Error(), "server_error")
			return
		}
		if _, err := s.mailboxStore.BackfillSourceID(r.Context(), saved.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"source": mailboxSourcePublic(saved)})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) mailboxDBReady(w http.ResponseWriter) bool {
	if s.mailboxStore != nil {
		return true
	}
	message := "邮箱数据库未配置"
	if s.mailboxInitErr != nil {
		message = "邮箱数据库不可用"
	}
	writeError(w, http.StatusServiceUnavailable, message, "mailbox_database_unavailable")
	return false
}

func (s *Server) iCloudBridgeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if !s.requireAdmin(w, r) {
		return
	}
	response := map[string]any{"enabled": s.mailboxStore != nil, "reachable": false, "database_configured": strings.TrimSpace(s.cfg.MailboxDatabaseURL) != "", "hme_configured": false}
	if s.mailboxManager != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		response["reachable"] = s.mailboxStore != nil && s.mailboxStore.Ping(ctx) == nil
		response["hme_configured"] = s.mailboxManager.HasEnabledSourceType(ctx, "icloud_hme")
		cancel()
	}
	if s.mailboxInitErr != nil {
		response["error"] = "mailbox_database_unavailable"
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) iCloudMailboxStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if !s.requireAdmin(w, r) || !s.mailboxDBReady(w) {
		return
	}
	counts, err := s.mailboxStore.Counts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "enabled": true, "database": "postgres", "counts": counts})
}

func (s *Server) iCloudMailboxes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if !s.requireAdmin(w, r) || !s.mailboxDBReady(w) {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	views, total, err := s.mailboxStore.List(r.Context(), page, pageSize, mailboxstore.MailboxListFilter{
		Search:   r.URL.Query().Get("search"),
		Status:   r.URL.Query().Get("status"),
		SourceID: r.URL.Query().Get("source_id"),
		Platform: r.URL.Query().Get("platform"),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
		return
	}
	items := make([]map[string]any, 0, len(views))
	for _, view := range views {
		items = append(items, map[string]any{
			"id": view.ID, "email": view.Email, "source_id": view.SourceID, "source_type": view.SourceType,
			"source_account_id": view.SourceAccountID, "source_mailbox_id": view.SourceMailboxID,
			"status": view.Status, "api_active": true, "icloud_active": true,
			"openai_claimed": view.OpenAIClaimed, "grok_claimed": view.GrokClaimed, "adobe_claimed": view.AdobeClaimed,
			"openai_reserved": view.OpenAIReserved, "grok_reserved": view.GrokReserved, "adobe_reserved": view.AdobeReserved,
			"openai_disabled": view.OpenAIDisabled, "grok_disabled": view.GrokDisabled, "adobe_disabled": view.AdobeDisabled,
			"openai_retired": view.OpenAIRetired, "grok_retired": view.GrokRetired, "adobe_retired": view.AdobeRetired,
			"last_error": view.LastError, "created_at": view.CreatedAt, "updated_at": view.UpdatedAt,
		})
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 20
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"mailboxes": items, "pagination": map[string]any{"page": page, "page_size": pageSize, "total": total, "total_all": total, "total_pages": totalPages}})
}

// iCloudMailboxAction handles mutations for one canonical mailbox asset. The
// list endpoint remains GET-only; status changes and explicit upstream deletion
// use separate actions so provider requests stay behind the resource manager.
func (s *Server) iCloudMailboxAction(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !s.mailboxDBReady(w) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/icloud/mailboxes/"), "/")
	if len(parts) > 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "mailbox action not found", "not_found")
		return
	}
	mailboxID, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(mailboxID) == "" {
		writeError(w, http.StatusBadRequest, "invalid mailbox id", "invalid_request_error")
		return
	}
	var updated mailboxstore.Mailbox
	if len(parts) == 1 {
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		if s.mailboxManager == nil {
			writeError(w, http.StatusServiceUnavailable, "邮箱资源管理模块不可用", "mailbox_provider_unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		updated, err = s.mailboxManager.DeleteAsset(ctx, mailboxID, "admin")
	} else if parts[1] == "status" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		var request struct {
			Status   string `json:"status"`
			Platform string `json:"platform"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		updated, err = s.mailboxStore.SetPlatformStatus(r.Context(), mailboxID, request.Platform, request.Status)
	} else {
		writeError(w, http.StatusNotFound, "mailbox action not found", "not_found")
		return
	}
	if err != nil {
		status, code, message := http.StatusInternalServerError, "mailbox_database_error", "邮箱操作未完成，请刷新后查看操作日志"
		switch {
		case errors.Is(err, mailboxstore.ErrMailboxNotFound):
			status, code, message = http.StatusNotFound, "mailbox_not_found", "邮箱资产不存在"
		case errors.Is(err, mailboxstore.ErrMailboxRegistrationRetired):
			status, code, message = http.StatusConflict, "mailbox_registration_retired", err.Error()
		case errors.Is(err, mailboxstore.ErrMailboxInUse), errors.Is(err, mailboxstore.ErrMailboxBusy):
			status, code, message = http.StatusConflict, "mailbox_in_use", err.Error()
		case errors.Is(err, mailboxstore.ErrInvalidMailboxPlatform), errors.Is(err, mailboxstore.ErrInvalidMailboxStatus), errors.Is(err, mailboxstore.ErrMailboxStateImmutable):
			status, code, message = http.StatusBadRequest, "invalid_request_error", err.Error()
		case updated.Status == "cleanup_pending":
			status, code, message = http.StatusBadGateway, "mailbox_provider_error", "上游删除未完成，邮箱已保留为待清理，可查看日志并重试"
		}
		writeError(w, status, message, code)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "status": updated.Status, "mailbox": map[string]any{
		"id": updated.ID, "email": updated.Email, "source_id": updated.SourceID, "source_type": updated.SourceType,
		"source_account_id": updated.SourceAccountID, "source_mailbox_id": updated.SourceMailboxID,
		"status": updated.Status, "last_error": updated.LastError, "created_at": updated.CreatedAt, "updated_at": updated.UpdatedAt,
	}})
}

func (s *Server) iCloudMailboxEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if !s.requireAdmin(w, r) || !s.mailboxDBReady(w) {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	events, total, err := s.mailboxStore.Events(r.Context(), page, pageSize, r.URL.Query().Get("mailbox_id"), r.URL.Query().Get("platform"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "total": total, "page": page, "page_size": pageSize})
}

func hmeConfigFromSource(source mailboxstore.Source) (registerruntime.HMEConfig, bool) {
	if !strings.EqualFold(source.Type, "icloud_hme") || !source.Enabled {
		return registerruntime.HMEConfig{}, false
	}
	config := source.Config
	cfg := registerruntime.HMEConfig{SourceID: source.ID, BaseURL: strings.TrimRight(stringValue(config["api_base"]), "/"), Password: stringValue(config["admin_password"]), AccountID: stringValue(config["account_id"])}
	return cfg, cfg.BaseURL != "" && cfg.Password != "" && cfg.AccountID != ""
}

func mailboxAccount(item map[string]any) (registerruntime.Mailbox, string, bool) {
	raw := mapValue(item["registration_mailbox"])
	if raw == nil || strings.TrimSpace(stringValue(raw["email"])) == "" {
		return registerruntime.Mailbox{}, "", false
	}
	provider := strings.ToLower(strings.TrimSpace(stringValue(raw["provider"])))
	if provider != "icloud_hme" && provider != "hme" {
		return registerruntime.Mailbox{}, "", false
	}
	box := registerruntime.Mailbox{ID: stringValue(raw["id"]), Email: stringValue(raw["email"]), AccountID: stringValue(raw["account_id"]), SourceID: stringValue(raw["source_id"]), SourceType: "hme", SourceMailboxID: firstNonEmpty(stringValue(raw["source_mailbox_id"]), stringValue(raw["id"]))}
	ref := firstNonEmpty(stringValue(item["id"]), stringValue(item["account_id"]), stringValue(item["user_id"]), stringValue(item["email"]))
	return box, ref, true
}

func (s *Server) iCloudMailboxReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if !s.requireAdmin(w, r) || !s.mailboxDBReady(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result := map[string]any{"aliases": 0, "openai_claims": 0, "grok_claims": 0, "adobe_claims": 0, "missing": []string{}, "errors": []string{}}
	sources, sourcesErr := s.mailboxStore.ListSources(ctx)
	if sourcesErr != nil {
		result["errors"] = append(result["errors"].([]string), sourcesErr.Error())
	} else {
		hmeSources := make([]mailboxstore.Source, 0, len(sources))
		for _, source := range sources {
			if source.Enabled && strings.EqualFold(source.Type, "icloud_hme") {
				hmeSources = append(hmeSources, source)
			}
		}
		if s.mailboxManager == nil || len(hmeSources) == 0 {
			result["errors"] = append(result["errors"].([]string), "邮箱上游未配置")
		} else {
			for _, source := range hmeSources {
				sourceName := firstNonEmpty(source.Label, source.ID)
				session, err := s.mailboxManager.Open(ctx, source.ID, time.Second)
				if err != nil {
					result["errors"] = append(result["errors"].([]string), sourceName+"："+err.Error())
					continue
				}
				aliases, listErr := session.List(ctx)
				session.Close()
				if listErr != nil {
					result["errors"] = append(result["errors"].([]string), sourceName+"："+listErr.Error())
					continue
				}
				for _, alias := range aliases {
					if _, err := s.mailboxStore.Upsert(ctx, alias, "provider_reconciled"); err != nil {
						result["errors"] = append(result["errors"].([]string), sourceName+"："+err.Error())
						continue
					}
					result["aliases"] = result["aliases"].(int) + 1
				}
			}
		}
	}
	released, repairErr := s.reconcileDeletedMailboxClaims(ctx)
	result["released_claims"] = released
	if repairErr != nil {
		result["errors"] = append(result["errors"].([]string), "历史占用核对失败："+repairErr.Error())
	}
	reconcile := func(platform string, accounts []map[string]any, counter string) {
		for _, account := range accounts {
			box, ref, ok := mailboxAccount(account)
			if !ok {
				continue
			}
			canonical, err := s.mailboxStore.Upsert(ctx, mailboxstoreMailbox(box), "account_reconciled")
			if err != nil {
				result["errors"] = append(result["errors"].([]string), err.Error())
				continue
			}
			box.ID = canonical.ID
			box.SourceID = canonical.SourceID
			box.SourceMailboxID = canonical.SourceMailboxID
			box.AccountID = canonical.SourceAccountID
			jobID := firstNonEmpty(stringValue(account["registration_job_id"]), ref)
			if err := s.mailboxStore.EnsureClaim(ctx, platform, jobID, mailboxstoreMailbox(box)); err != nil {
				result["errors"] = append(result["errors"].([]string), err.Error())
				continue
			}
			if err := s.mailboxStore.Commit(ctx, platform, jobID, ref, mailboxstoreMailbox(box)); err != nil {
				result["errors"] = append(result["errors"].([]string), err.Error())
				continue
			}
			result[counter] = result[counter].(int) + 1
		}
	}
	openAI, err := s.store.AccountList()
	if err != nil {
		result["errors"] = append(result["errors"].([]string), err.Error())
	} else {
		reconcile("openai", openAI, "openai_claims")
	}
	grok, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		result["errors"] = append(result["errors"].([]string), err.Error())
	} else {
		reconcile("grok", grok, "grok_claims")
	}
	if s.adobeStore != nil {
		accounts, err := s.adobeStore.List()
		if err != nil {
			result["errors"] = append(result["errors"].([]string), "Adobe 账号读取失败")
		} else {
			for _, a := range accounts {
				// Only the upstream list above creates/synchronizes Adobe assets.
				// Imported metadata must not overwrite an existing asset or claim.
				_, err := s.linkAdobeMailbox(ctx, a.ID, true)
				if errors.Is(err, errAdobeMailboxMetadataMissing) || errors.Is(err, adobe.ErrNotFound) {
					continue
				}
				if err != nil {
					message := "Adobe 邮箱关联失败，已保留原占用；可重试核对"
					switch {
					case errors.Is(err, mailboxstore.ErrMailboxNotFound):
						message = "Adobe 邮箱资产未匹配，请先核对该邮箱来源，再关联账号"
					case errors.Is(err, mailboxstore.ErrMailboxInUse):
						message = "Adobe 邮箱已有其他账号或注册任务占用，未覆盖"
					case errors.Is(err, mailboxstore.ErrMailboxStateImmutable):
						message = "Adobe 邮箱已删除或待清理，未关联"
					case errors.Is(err, errAdobeMailboxMetadataMismatch):
						message = "Adobe 账号邮箱来源与现有资产不一致，未改写"
					case errors.Is(err, errAdobeMailboxSourceUnsupported):
						message = "Adobe 邮箱来源暂不支持关联"
					}
					result["errors"] = append(result["errors"].([]string), a.ID+"："+message)
					continue
				}
				result["adobe_claims"] = result["adobe_claims"].(int) + 1
			}
		}
	}
	if s.grokRegister != nil {
		if err := s.grokRegister.ReconcileMailboxReservations(ctx); err != nil {
			result["errors"] = append(result["errors"].([]string), "失败注册预留清理未完成")
		}
	}
	if s.adobeRegister != nil {
		if err := s.adobeRegister.ReconcileMailboxReservations(ctx); err != nil {
			result["errors"] = append(result["errors"].([]string), "Adobe 失败注册预留清理未完成")
		}
	}
	success := len(result["errors"].([]string)) == 0
	result["success"] = success
	switch {
	case success:
		result["outcome"] = "success"
	case result["aliases"].(int)+result["openai_claims"].(int)+result["grok_claims"].(int)+result["adobe_claims"].(int)+released > 0:
		result["outcome"] = "partial"
	default:
		result["outcome"] = "failed"
	}
	writeJSON(w, http.StatusOK, result)
}

func mailboxstoreMailbox(box registerruntime.Mailbox) mailboxstore.Mailbox {
	return mailboxstore.Mailbox{ID: box.ID, Email: box.Email, SourceID: box.SourceID, SourceType: "hme", SourceAccountID: box.AccountID, SourceMailboxID: firstNonEmpty(box.SourceMailboxID, box.ID, box.Email)}
}
