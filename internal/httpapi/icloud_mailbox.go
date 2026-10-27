package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

type mailboxCoordinatorAdapter struct{ store *mailboxstore.Store }

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
	capabilities := map[string]any{"openai": false, "grok": false, "create": false, "status": "config_only"}
	if typeName == "icloud_hme" && strings.TrimSpace(stringValue(provider["api_base"])) != "" && strings.TrimSpace(stringValue(provider["admin_password"])) != "" && strings.TrimSpace(stringValue(provider["account_id"])) != "" {
		capabilities = map[string]any{"openai": true, "grok": true, "create": true, "status": "available"}
	}
	return mailboxstore.Source{ID: id, Type: typeName, Label: label, Enabled: boolValue(provider["enable"], true), Config: provider, Capabilities: capabilities, Status: stringValue(capabilities["status"])}
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
	configChanged := false
	for index, raw := range providers {
		provider := mapValue(raw)
		source := mailboxSourceFromProvider(provider, index)
		if strings.TrimSpace(stringValue(mapValue(raw)["id"])) != source.ID {
			provider["id"] = source.ID
			configChanged = true
		}
		if known[source.ID] {
			_, _ = s.mailboxStore.BackfillSourceID(ctx, source.ID, source.Type, stringValue(source.Config["account_id"]))
			continue
		}
		if _, err := s.mailboxStore.UpsertSource(ctx, source); err != nil {
			return err
		}
		_, _ = s.mailboxStore.BackfillSourceID(ctx, source.ID, source.Type, stringValue(source.Config["account_id"]))
	}
	if configChanged {
		mail["providers"] = providers
		if _, err := s.registerStore.Update(map[string]any{"mail": mail}); err != nil {
			return err
		}
	}
	return nil
}

func mailboxSourcePublic(source mailboxstore.Source) map[string]any {
	config := cloneMap(source.Config)
	if _, ok := config["admin_password"]; ok {
		config["admin_password_set"] = stringValue(config["admin_password"]) != ""
		delete(config, "admin_password")
	}
	return map[string]any{
		"id": source.ID, "type": source.Type, "label": source.Label, "enable": source.Enabled,
		"status": source.Status, "last_error": source.LastError, "capabilities": source.Capabilities,
		"config": config, "last_sync_at": source.LastSyncAt, "created_at": source.CreatedAt, "updated_at": source.UpdatedAt,
	}
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
		var request struct {
			Source map[string]any `json:"source"`
		}
		if !decodeJSON(w, r, &request) || request.Source == nil {
			return
		}
		id := strings.TrimSpace(stringValue(request.Source["id"]))
		if id == "" {
			writeError(w, http.StatusBadRequest, "source id is required", "invalid_request_error")
			return
		}
		existing, err := s.mailboxStore.GetSource(r.Context(), id)
		if err != nil && err != sql.ErrNoRows {
			writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
			return
		}
		config := cloneMap(existing.Config)
		for key, value := range mapValue(request.Source["config"]) {
			if key == "admin_password" && strings.TrimSpace(stringValue(value)) == "" {
				continue
			}
			config[key] = value
		}
		config["id"] = id
		config["type"] = firstNonEmpty(stringValue(request.Source["type"]), stringValue(existing.Type), stringValue(config["type"]))
		config["label"] = firstNonEmpty(stringValue(request.Source["label"]), stringValue(existing.Label), stringValue(config["label"]))
		config["enable"] = boolValue(request.Source["enable"], existing.Enabled)
		source := mailboxSourceFromProvider(config, 0)
		source.ID = id
		if _, err := s.mailboxStore.UpsertSource(r.Context(), source); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
			return
		}
		current := s.registerStore.Get()
		mail := cloneMap(mapValue(current["mail"]))
		providers, _ := mail["providers"].([]any)
		updated := false
		for index, raw := range providers {
			provider := mapValue(raw)
			if strings.TrimSpace(stringValue(provider["id"])) != id {
				continue
			}
			mail["providers"] = append(append([]any{}, providers[:index]...), append([]any{source.Config}, providers[index+1:]...)...)
			updated = true
			break
		}
		if !updated {
			mail["providers"] = append(providers, source.Config)
		}
		if _, err := s.registerStore.Update(map[string]any{"mail": mail}); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"source": mailboxSourcePublic(source)})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func mailboxAsset(box registerruntime.Mailbox) mailboxstore.Mailbox {
	sourceID := strings.TrimSpace(box.SourceMailboxID)
	if sourceID == "" {
		sourceID = strings.TrimSpace(box.ID)
	}
	return mailboxstore.Mailbox{ID: strings.TrimSpace(box.ID), Email: box.Email, SourceID: strings.TrimSpace(box.SourceID), SourceType: firstNonEmpty(stringValue(box.SourceType), "hme"), SourceAccountID: box.AccountID, SourceMailboxID: sourceID}
}

func providerMailbox(item mailboxstore.Mailbox) registerruntime.Mailbox {
	return registerruntime.Mailbox{ID: item.ID, Email: item.Email, AccountID: item.SourceAccountID, SourceID: item.SourceID, SourceType: item.SourceType, SourceMailboxID: item.SourceMailboxID}
}

func (a mailboxCoordinatorAdapter) Reserve(ctx context.Context, platform, jobID, sourceID string, acquire func(context.Context) (registerruntime.Mailbox, error), prepare func(context.Context, registerruntime.Mailbox) error) (registerruntime.Mailbox, error) {
	item, err := a.store.Reserve(ctx, platform, jobID, sourceID, func(ctx context.Context) (mailboxstore.Mailbox, error) {
		box, err := acquire(ctx)
		if err != nil {
			return mailboxstore.Mailbox{}, err
		}
		return mailboxAsset(box), nil
	}, func(ctx context.Context, item mailboxstore.Mailbox) error {
		return prepare(ctx, providerMailbox(item))
	})
	if err != nil {
		return registerruntime.Mailbox{}, err
	}
	return providerMailbox(item), nil
}

func (a mailboxCoordinatorAdapter) EnsureClaim(ctx context.Context, platform, jobID string, box registerruntime.Mailbox) error {
	return a.store.EnsureClaim(ctx, platform, jobID, mailboxAsset(box))
}

func (a mailboxCoordinatorAdapter) Commit(ctx context.Context, platform, jobID, accountID string, box registerruntime.Mailbox) error {
	return a.store.Commit(ctx, platform, jobID, accountID, mailboxAsset(box))
}

func (a mailboxCoordinatorAdapter) Release(ctx context.Context, platform, jobID string, box registerruntime.Mailbox, reason string) error {
	return a.store.Release(ctx, platform, jobID, mailboxAsset(box), reason)
}

func (s *Server) mailboxCoordinator() registerruntime.MailboxClaimCoordinator {
	if s.mailboxStore == nil {
		return nil
	}
	return mailboxCoordinatorAdapter{store: s.mailboxStore}
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
	if s.mailboxStore != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		response["reachable"] = s.mailboxStore.Ping(ctx) == nil
		cancel()
	}
	if _, ok := s.mailboxHMEConfig(); ok {
		response["hme_configured"] = true
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
	views, total, err := s.mailboxStore.List(r.Context(), page, pageSize, r.URL.Query().Get("search"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "mailbox_database_error")
		return
	}
	items := make([]map[string]any, 0, len(views))
	for _, view := range views {
		status := view.Status
		if status == "active" && view.OpenAIClaimed && view.GrokClaimed {
			status = "active"
		}
		items = append(items, map[string]any{
			"id": view.ID, "email": view.Email, "source_id": view.SourceID, "source_type": view.SourceType,
			"source_account_id": view.SourceAccountID, "source_mailbox_id": view.SourceMailboxID,
			"status": status, "api_active": true, "icloud_active": true,
			"openai_claimed": view.OpenAIClaimed, "grok_claimed": view.GrokClaimed,
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

func (s *Server) mailboxHMEConfigForSource(sourceID string) (registerruntime.HMEConfig, bool) {
	if s.mailboxStore != nil && strings.TrimSpace(sourceID) != "" {
		source, err := s.mailboxStore.GetSource(context.Background(), sourceID)
		if err == nil {
			return hmeConfigFromSource(source)
		}
	}
	mail := mapValue(s.registerStore.Get()["mail"])
	providers, _ := mail["providers"].([]any)
	for index, raw := range providers {
		provider := mapValue(raw)
		if strings.TrimSpace(sourceID) != "" && strings.TrimSpace(stringValue(provider["id"])) != strings.TrimSpace(sourceID) {
			continue
		}
		source := mailboxSourceFromProvider(provider, index)
		if cfg, ok := hmeConfigFromSource(source); ok {
			return cfg, true
		}
	}
	return registerruntime.HMEConfig{}, false
}

func (s *Server) mailboxHMEConfig() (registerruntime.HMEConfig, bool) {
	if s.mailboxStore != nil {
		if sources, err := s.mailboxStore.ListSources(context.Background()); err == nil {
			for _, source := range sources {
				if cfg, ok := hmeConfigFromSource(source); ok {
					return cfg, true
				}
			}
		}
	}
	mail := mapValue(s.registerStore.Get()["mail"])
	providers, _ := mail["providers"].([]any)
	for index, raw := range providers {
		provider := mapValue(raw)
		source := mailboxSourceFromProvider(provider, index)
		if cfg, ok := hmeConfigFromSource(source); ok {
			return cfg, true
		}
	}
	return registerruntime.HMEConfig{}, false
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
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	result := map[string]any{"aliases": 0, "openai_claims": 0, "grok_claims": 0, "missing": []string{}, "errors": []string{}}
	if cfg, ok := s.mailboxHMEConfig(); ok {
		hme, err := registerruntime.NewHME(cfg, time.Second)
		if err != nil {
			result["errors"] = append(result["errors"].([]string), err.Error())
		} else {
			aliases, listErr := hme.ListMailboxes(ctx)
			hme.Close()
			if listErr != nil {
				result["errors"] = append(result["errors"].([]string), listErr.Error())
			} else {
				for _, alias := range aliases {
					if _, err := s.mailboxStore.Upsert(ctx, mailboxstore.Mailbox{ID: alias.ID, Email: alias.Email, SourceID: alias.SourceID, SourceType: "hme", SourceAccountID: alias.AccountID, SourceMailboxID: alias.SourceMailboxID}, "provider_reconciled"); err != nil {
						result["errors"] = append(result["errors"].([]string), err.Error())
						continue
					}
					result["aliases"] = result["aliases"].(int) + 1
				}
			}
		}
	} else {
		result["errors"] = append(result["errors"].([]string), "iCloud 邮箱上游未配置")
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
	result["success"] = len(result["errors"].([]string)) == 0
	writeJSON(w, http.StatusOK, result)
}

func mailboxstoreMailbox(box registerruntime.Mailbox) mailboxstore.Mailbox {
	return mailboxstore.Mailbox{ID: box.ID, Email: box.Email, SourceID: box.SourceID, SourceType: "hme", SourceAccountID: box.AccountID, SourceMailboxID: firstNonEmpty(box.SourceMailboxID, box.ID, box.Email)}
}
