package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func (s *Server) adobeAccountsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.adobeStore == nil || s.adobeClient == nil {
		writeError(w, 503, "Adobe 账号服务不可用", "not_configured")
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/adobe/accounts"), "/")
	switch {
	case path == "" && r.Method == http.MethodGet:
		items, err := s.adobeStore.PublicList()
		if err != nil {
			writeError(w, 500, "Adobe 账号读取失败", "storage_error")
			return
		}
		all := len(items)
		filtered := make([]map[string]any, 0, len(items))
		keyword := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("keyword")))
		status := r.URL.Query().Get("status")
		for _, a := range items {
			if keyword != "" && !strings.Contains(strings.ToLower(stringValue(a["id"])+" "+stringValue(a["email"])+" "+stringValue(a["account_id"])), keyword) {
				continue
			}
			if status != "" && status != "all" && stringValue(a["status"]) != status {
				continue
			}
			filtered = append(filtered, a)
		}
		total := len(filtered)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		if page < 1 {
			page = 1
		}
		if size < 1 {
			size = 20
		}
		if size > 200 {
			size = 200
		}
		start := (page - 1) * size
		if start > total {
			start = total
		}
		end := start + size
		if end > total {
			end = total
		}
		writeJSON(w, 200, map[string]any{"items": filtered[start:end], "total": total, "all_total": all, "page": page, "page_size": size})
	case path == "import" && r.Method == http.MethodPost:
		var b struct {
			Accounts []adobe.Account `json:"accounts"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		if !decodeJSON(w, r, &b) {
			return
		}
		if len(b.Accounts) == 0 || len(b.Accounts) > 100 {
			writeError(w, 400, "每批导入 1–100 个 Adobe 账号", "invalid_request_error")
			return
		}
		for i := range b.Accounts {
			b.Accounts[i].Enabled = true
		}
		items, err := s.adobeStore.Import(b.Accounts)
		if err != nil {
			writeError(w, 400, adobe.SafeError(err), "adobe_import_failed")
			return
		}
		out := []map[string]any{}
		for _, a := range items {
			out = append(out, adobe.Public(a))
		}
		writeJSON(w, 200, map[string]any{"items": out, "imported": len(out)})
	case path == "refresh" && r.Method == http.MethodPost:
		var b struct {
			IDs         []string `json:"ids"`
			Credentials bool     `json:"credentials"`
		}
		if !decodeJSON(w, r, &b) {
			return
		}
		ids := uniqueStrings(b.IDs)
		if len(ids) == 0 || len(ids) > 100 {
			writeError(w, 400, "请选择 1–100 个 Adobe 账号", "invalid_request_error")
			return
		}
		out := []map[string]any{}
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			a, err := s.refreshAdobeAccount(ctx, id, b.Credentials)
			cancel()
			row := map[string]any{"id": id, "ok": err == nil}
			if err != nil {
				row["error"] = adobe.SafeError(err)
			}
			if a.ID != "" {
				row["account"] = adobe.Public(a)
			}
			out = append(out, row)
		}
		writeJSON(w, 200, map[string]any{"results": out})
	case path == "disabled" && r.Method == http.MethodPost:
		var b struct {
			IDs      []string `json:"ids"`
			Disabled bool     `json:"disabled"`
		}
		if !decodeJSON(w, r, &b) {
			return
		}
		if len(b.IDs) == 0 {
			writeError(w, 400, "请选择 Adobe 账号", "invalid_request_error")
			return
		}
		if err := s.adobeStore.SetEnabled(uniqueStrings(b.IDs), !b.Disabled); err != nil {
			writeError(w, 500, "Adobe 账号更新失败", "storage_error")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case path == "" && r.Method == http.MethodDelete:
		var b struct {
			IDs []string `json:"ids"`
		}
		if !decodeJSON(w, r, &b) {
			return
		}
		if len(b.IDs) == 0 {
			writeError(w, 400, "请选择 Adobe 账号", "invalid_request_error")
			return
		}
		before := time.Now()
		deleted, err := s.adobeStore.Delete(uniqueStrings(b.IDs))
		if err != nil {
			writeError(w, 409, adobe.SafeError(err), "adobe_delete_failed")
			return
		}
		rows := []map[string]any{}
		for _, a := range deleted {
			rows = append(rows, map[string]any{"id": a.ID, "account_id": a.AccountID, "email": a.Email, "platform": "adobe", "source_type": "adobe", "registration_mailbox": a.RegistrationMailbox})
		}
		cleanup := s.cleanupHMEMailboxes(r.Context(), rows, before)
		writeJSON(w, 200, map[string]any{"deleted": len(deleted), "hme_cleanup": cleanup})
	default:
		writeError(w, 404, "Adobe 账号接口不存在", "not_found")
	}
}

func (s *Server) refreshAdobeAccount(ctx context.Context, id string, credentials bool) (adobe.Account, error) {
	a, err := s.adobeStore.Get(id)
	if err != nil {
		return a, err
	}
	ctx, err = s.adobeAccountContext(ctx, a)
	if err != nil {
		return a, err
	}
	return s.adobeStore.Refresh(ctx, s.adobeClient, id, credentials)
}
func (s *Server) ensureAdobeAccounts(ctx context.Context) error {
	if s.adobeStore == nil || s.adobeClient == nil {
		return adobe.ErrNoAccount
	}
	items, err := s.adobeStore.List()
	if err != nil {
		return err
	}
	for _, a := range items {
		if adobe.Eligible(a, time.Now()) {
			return nil
		}
	}
	count := 0
	for _, a := range items {
		if !a.Enabled || a.InFlight != "" || a.SettlementUntil > time.Now().Unix() || (a.NextRefreshAt > time.Now().Unix() && a.LastError != "") {
			continue
		}
		fresh, e := s.refreshAdobeAccount(ctx, a.ID, false)
		count++
		if e == nil && adobe.Eligible(fresh, time.Now()) {
			return nil
		}
		if count >= 4 {
			break
		}
	}
	return adobe.ErrNoAccount
}

func adobeAccountFromMap(fields map[string]any) (adobe.Account, error) {
	var a adobe.Account
	raw, err := json.Marshal(fields)
	if err != nil {
		return a, err
	}
	err = json.Unmarshal(raw, &a)
	return a, err
}
func (s *Server) importRegisteredAdobeAccount(ctx context.Context, jobID string, fields map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a, err := adobeAccountFromMap(fields)
	if err != nil {
		return err
	}
	a.Enabled = false
	a.RegistrationJobID = jobID
	if s.mailboxStore != nil && len(a.RegistrationMailbox) > 0 {
		var box registerruntime.Mailbox
		raw, _ := json.Marshal(a.RegistrationMailbox)
		if err = json.Unmarshal(raw, &box); err != nil {
			return err
		}
		if err = s.mailboxStore.Commit(ctx, "adobe", jobID, "adobe:"+a.AccountID, mailboxFromRegister(box)); err != nil {
			return err
		}
	}
	// Recovery of an already imported registration must not replace a refreshed
	// credential or re-disable an account whose verification already completed.
	if existing, e := s.adobeStore.Get("adobe:" + a.AccountID); e == nil && existing.RegistrationJobID == jobID {
		return nil
	}
	_, err = s.adobeStore.Import([]adobe.Account{a})
	return err
}
func (s *Server) verifyRegisteredAdobeAccount(ctx context.Context, jobID string, fields map[string]any) error {
	a, err := s.adobeStore.Get("adobe:" + stringValue(fields["account_id"]))
	if err != nil {
		return err
	}
	if a.RegistrationJobID != jobID {
		return errors.New("adobe_registration_owner_mismatch")
	}
	a, err = s.refreshAdobeAccount(ctx, a.ID, false)
	if err != nil {
		return &registerruntime.Failure{Stage: "verify", Code: "adobe_verification_failed"}
	}
	if a.Quota.Status != "known" {
		return &registerruntime.Failure{Stage: "verify", Code: "adobe_quota_unknown"}
	}
	return s.adobeStore.SetEnabled([]string{a.ID}, true)
}

// adobeAccountContext preserves the account egress for quota, submission,
// polling and download; unavailable bindings never fall through to defaults.
func (s *Server) adobeAccountContext(ctx context.Context, a adobe.Account) (context.Context, error) {
	fields := map[string]any{"proxy": a.Proxy}
	if a.ProxyBinding != nil {
		fields["proxy_binding"] = a.ProxyBinding
	}
	if s.proxyManager == nil {
		return ctx, nil
	}
	if endpoint, bound, err := s.proxyManager.ResolveBound(fields); bound || err != nil {
		if err != nil {
			return ctx, &adobe.Error{Stage: "routing", Code: "proxy_unavailable"}
		}
		return proxyruntime.WithURL(ctx, endpoint), nil
	}
	if a.Proxy == "" {
		fields["proxy"] = "direct"
	}
	return proxyruntime.WithURL(ctx, s.proxyManager.Resolve(fields, false)), nil
}
func (s *Server) adobeMaintenanceScheduler() {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	select {
	case <-s.probeStop:
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if s.beginDeploymentBackground() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			s.refreshDueAdobeAccounts(ctx)
			cancel()
			s.endDeploymentBackground()
		}
		select {
		case <-s.probeStop:
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) refreshDueAdobeAccounts(ctx context.Context) {
	s.reconcileAdobeMediaReservations()
	if s.adobeStore == nil {
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		return
	}
	cfg, err := s.store.Config()
	if err != nil || !boolValue(mapValue(cfg["adobe_maintenance"])["enabled"], true) {
		return
	}
	items, err := s.adobeStore.List()
	if err != nil {
		return
	}
	now := time.Now()
	count := 0
	for _, a := range items {
		if !a.Enabled || a.InFlight != "" || a.SettlementUntil > now.Unix() || (a.NextRefreshAt > now.Unix() && a.LastError != "") {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, a.Quota.CheckedAt)
		interval := time.Hour
		if a.LastUsedAt > now.Add(-time.Hour).Unix() {
			interval = 5 * time.Minute
		}
		due := a.ExpiresAt <= now.Add(5*time.Minute).Unix() || a.Quota.Status == "stale" || at.IsZero() || now.Sub(at) >= interval
		if !due {
			continue
		}
		_, _ = s.refreshAdobeAccount(ctx, a.ID, false)
		count++
		if count >= 4 || ctx.Err() != nil {
			return
		}
	}
}
