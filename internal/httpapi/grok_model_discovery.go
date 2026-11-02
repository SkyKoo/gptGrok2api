package httpapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
)

func (s *Server) grokWebAccounts() []accounts.Account {
	rows, _, err := s.grokRuntimeStore.AccountSnapshot()
	if err != nil {
		return nil
	}
	result := []accounts.Account{}
	for _, row := range rows {
		if !boolValue(row["enabled"], true) || accounts.IsAbnormal(row) || stringValue(row["status"]) == "disabled" {
			continue
		}
		token := accountToken(row)
		if token != "" {
			result = append(result, accounts.Account{Token: token, Fields: row})
		}
	}
	return result
}
func (s *Server) refreshGrokWebModels(ctx context.Context, a accounts.Account) error {
	if s.cfg.GrokModesURL == "" {
		return errors.New("Grok Web model directory endpoint is not configured")
	}
	key := "grok-models:" + runtimeTokenID(a.Token)
	s.modelRefreshMu.Lock()
	if s.modelRefreshing[key] {
		s.modelRefreshMu.Unlock()
		return accounts.ErrRefreshDeferred
	}
	s.modelRefreshing[key] = true
	s.modelRefreshMu.Unlock()
	defer func() { s.modelRefreshMu.Lock(); delete(s.modelRefreshing, key); s.modelRefreshMu.Unlock() }()
	// Recheck the persisted attempt after claiming the slot, including failures.
	rows, _, err := s.grokRuntimeStore.AccountSnapshot()
	if err != nil {
		return err
	}
	found := false
	for _, row := range rows {
		if accountToken(row) == a.Token {
			a.Fields = row
			found = true
			break
		}
	}
	if !found || accounts.IsAbnormal(a.Fields) || !boolValue(a.Fields["enabled"], true) {
		return accounts.ErrRefreshDeferred
	}
	before := model.DecodeGrokWebCatalog(a.Fields["grok_web_catalog"])
	if !before.Due(time.Now()) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	next, fetchErr := s.chatProvider.FetchModes(ctx, s.cfg.GrokModesURL, a)
	now := time.Now().UTC()
	if fetchErr != nil {
		next = before
		next.LastError = fetchErr.Error()
	} else {
		next.CheckedAt = now
	}
	next.LastAttemptAt = now
	// Updating an existing token cannot recreate a deleted or reauthorized account.
	_, saveErr := s.grokRuntimeStore.UpdateAccountRuntime(a.Token, map[string]any{"grok_web_catalog": next})
	if fetchErr != nil {
		s.recordGrokCredentialFailure(a.Token, fetchErr)
		return fetchErr
	}
	return saveErr
}
func (s *Server) refreshDueGrokWebModels(ctx context.Context, mode string, limit int, excluded map[string]bool) {
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		return
	}
	items := s.grokWebAccounts()
	if mode != "" {
		for _, a := range items {
			if !excluded[a.Token] && grokWebChatAvailable(a, mode, time.Now()) {
				return
			}
		}
	}
	count := 0
	for _, a := range items {
		if ctx.Err() != nil {
			return
		}
		if !model.DecodeGrokWebCatalog(a.Fields["grok_web_catalog"]).Due(time.Now()) {
			continue
		}
		_ = s.refreshGrokWebModels(ctx, a)
		count++
		if count >= limit {
			return
		}
	}
}

// Directory requests only bootstrap one account when no fresh catalog exists.
// Subsequent maintenance is bounded and handled by the background scheduler.
func (s *Server) ensureGrokWebCatalog(ctx context.Context) {
	for _, a := range s.grokWebAccounts() {
		if model.DecodeGrokWebCatalog(a.Fields["grok_web_catalog"]).Fresh(time.Now()) {
			return
		}
	}
	s.refreshDueGrokWebModels(ctx, "", 1, nil)
}
func (s *Server) grokWebSpecs() []model.Spec {
	latest := map[string]model.GrokWebMode{}
	observed := map[string]time.Time{}
	for _, a := range s.grokWebAccounts() {
		catalog := model.DecodeGrokWebCatalog(a.Fields["grok_web_catalog"])
		if !catalog.Fresh(time.Now()) {
			continue
		}
		for _, m := range catalog.Modes {
			if catalog.CheckedAt.After(observed[m.ID]) {
				latest[m.ID] = m
				observed[m.ID] = catalog.CheckedAt
			}
		}
	}
	result := []model.Spec{}
	for _, spec := range s.catalog {
		mode, ok := model.ResolveGrokWebMode(spec.ID)
		if !ok {
			continue
		}
		if m, ok := latest[mode]; ok {
			spec.Name = m.DisplayName()
		} else if len(latest) > 0 {
			continue // A fresh official directory no longer advertises this mode.
		} else {
			spec.Name += "（目录待确认）"
		}
		result = append(result, spec)
	}
	return result
}
func (s *Server) grokWebSummary() map[string]any {
	result := map[string]any{}
	for _, mode := range []string{"auto", "fast", "expert", "heavy"} {
		verified, available := 0, 0
		for _, a := range s.grokWebAccounts() {
			c := model.DecodeGrokWebCatalog(a.Fields["grok_web_catalog"])
			if c.Fresh(time.Now()) {
				verified++
			}
			if c.Supports(mode, time.Now()) {
				available++
			}
		}
		result[model.GrokWebID(mode)] = map[string]any{"mode_id": mode, "verified_accounts": verified, "available_accounts": available}
	}
	return result
}
func (s *Server) reserveWebChat(ctx context.Context, route model.ChatRoute, excluded map[string]bool) (*accounts.Lease, error) {
	if !route.GrokWeb {
		return s.chatAccountPool(route).Reserve(ctx, route.PoolCandidates, excluded)
	}
	s.refreshDueGrokWebModels(ctx, route.Mode, 2, excluded)
	lease, err := s.grokAccountPool.ReserveMatchingLimit(ctx, route.PoolCandidates, excluded, func(a accounts.Account) bool {
		return grokWebChatAvailable(a, route.Mode, time.Now())
	}, 1)
	if errors.Is(err, accounts.ErrUnavailable) {
		return nil, fmt.Errorf("Grok Web %s 暂无可用账号：请检查官方模式权限、文字额度或等待模型目录刷新: %w", route.Mode, err)
	}
	return lease, err
}

func grokWebAliasIDs() map[string]string {
	aliases := map[string]string{}
	for id, mode := range model.GrokWebAliases() {
		aliases[id] = model.GrokWebID(mode)
	}
	return aliases
}

// A known empty window belongs to its selected Web mode. Unknown counts do
// not mean zero, and a cooling account must not prevent discovery of others.
func grokWebChatAvailable(a accounts.Account, mode string, now time.Time) bool {
	if !boolValue(a.Fields["enabled"], true) || accounts.IsAbnormal(a.Fields) || !model.DecodeGrokWebCatalog(a.Fields["grok_web_catalog"]).Supports(mode, now) {
		return false
	}
	switch stringValue(a.Fields["status"]) {
	case "disabled", "limited", "rate_limited", "cooling", "backoff", "限流":
		return false
	}
	if until, err := time.Parse(time.RFC3339Nano, stringValue(a.Fields["cooldown_until"])); err == nil && now.Before(until) {
		return false
	}
	quota := mapValue(a.Fields[mode+"_quota"])
	if left, known := quota["remaining"]; known && left != nil && intValue(left) == 0 {
		reset := int64(intValue(quota["reset_at"]))
		if reset <= 0 || now.Unix() < reset {
			return false
		}
	}
	return true
}
