package httpapi

import (
	"context"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (s *Server) openAIAccountIDs() []string {
	rows, _, _ := s.store.AccountSnapshot()
	ids := []string{}
	for _, row := range rows {
		a := accounts.Account{Token: accountToken(row), Fields: row}
		if isOpenAIAccount(a) && boolValue(row["enabled"], true) {
			ids = append(ids, accounts.Identity(a))
		}
	}
	return ids
}
func (s *Server) modelSpecs() []model.Spec {
	specs := []model.Spec{}
	for _, m := range s.catalog {
		if m.OwnedBy != "openai" || m.Capability&model.Chat == 0 {
			specs = append(specs, m)
		}
	}
	return append(s.discovery.Specs(s.openAIAccountIDs()), specs...)
}
func (s *Server) resolveChatModel(id string) (model.ChatRoute, bool) {
	id = strings.TrimSpace(id)
	if m, ok := model.Find(s.modelSpecs(), id); ok && m.Enabled && m.OwnedBy == "openai" && m.Capability&model.Chat != 0 {
		return model.ChatRoute{OpenAI: true, PoolCandidates: []string{"basic", "super", "heavy"}}, true
	}
	return model.ResolveChat(id)
}
func (s *Server) supportsChatModel(id string) func(accounts.Account) bool {
	return func(a accounts.Account) bool {
		return isOpenAIAccount(a) && s.discovery.Supports(accounts.Identity(a), id)
	}
}
func (s *Server) refreshAccountModels(ctx context.Context, a accounts.Account) error {
	id := accounts.Identity(a)
	s.modelRefreshMu.Lock()
	if s.modelRefreshing[id] {
		s.modelRefreshMu.Unlock()
		return accounts.ErrRefreshDeferred
	}
	s.modelRefreshing[id] = true
	s.modelRefreshMu.Unlock()
	defer func() { s.modelRefreshMu.Lock(); delete(s.modelRefreshing, id); s.modelRefreshMu.Unlock() }()
	// Persist a non-secret stable key before writing the sidecar cache, so a token
	// rotation cannot orphan the association even if the process stops midway.
	if a.Fields["cfm_account_id"] == nil {
		fields, _, e := s.store.UpdateAccount(a.Token, map[string]any{"cfm_account_id": id})
		if e != nil {
			return e
		}
		a.Fields = fields
	}
	raw, err := s.openAIAccountClient().FetchModels(ctx, a)
	return s.discovery.Record(id, stringValue(a.Fields["default_model_slug"]), raw, err)
}
func (s *Server) refreshDueModels() {
	rows, _, err := s.store.AccountSnapshot()
	if err != nil {
		return
	}
	count := 0
	for _, row := range rows {
		if _, e := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(e) {
			return
		}
		a := accounts.Account{Token: accountToken(row), Fields: row}
		if !isOpenAIAccount(a) || !boolValue(row["enabled"], true) || accountStatusCategory(row) == "abnormal" {
			continue
		}
		if !s.discovery.Due(accounts.Identity(a), false) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = s.refreshAccountModels(ctx, a)
		cancel()
		count++
		if count >= 2 {
			return
		}
	}
}
func (s *Server) refreshModelsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		s.modelRefreshMu.Lock()
		n := len(s.modelRefreshing)
		s.modelRefreshMu.Unlock()
		writeJSON(w, 200, map[string]any{"active": n, "discovery": s.discovery.Summary(s.openAIAccountIDs())})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	if _, e := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(e) {
		writeError(w, 503, "maintenance in progress", "server_error")
		return
	}
	var body struct {
		Refs []string `json:"account_refs"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Refs) < 1 || len(body.Refs) > 10 {
		writeError(w, 400, "provide 1–10 account_refs", "invalid_request_error")
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 2)
	success, failed, deferred := 0, 0, 0
	for _, ref := range uniqueAccountRefs(body.Refs) {
		row, e := s.accountByRef(ref)
		if e != nil {
			mu.Lock()
			failed++
			mu.Unlock()
			continue
		}
		a := accounts.Account{Token: accountToken(row), Fields: row}
		if !isOpenAIAccount(a) {
			mu.Lock()
			failed++
			mu.Unlock()
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(a accounts.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			e := s.refreshAccountModels(ctx, a)
			cancel()
			mu.Lock()
			if errors.Is(e, accounts.ErrRefreshDeferred) {
				deferred++
			} else if e == nil {
				success++
			} else {
				failed++
			}
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	writeJSON(w, 200, map[string]any{"refreshed": success, "failed": failed, "deferred": deferred, "discovery": s.discovery.Summary(s.openAIAccountIDs())})
}
