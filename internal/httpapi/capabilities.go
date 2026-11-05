package httpapi

import (
	"context"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (s *Server) quotaContext(ctx context.Context, l *accounts.Lease) context.Context {
	return provider.WithQuotaObserver(ctx, func(feature string) { s.accountPool.MarkSent(l, feature) })
}
func (s *Server) quotaRetrySafe(l *accounts.Lease, err error) bool {
	status := upstreamStatus(err)
	return !s.accountPool.Sent(l, "reason") || status == http.StatusUnauthorized || status == http.StatusTooManyRequests || provider.IsModelRejected(err)
}
func maskCount(masks []*provider.ImageMask) int {
	for _, m := range masks {
		if m != nil {
			return 1
		}
	}
	return 0
}
func (s *Server) refreshAccountQuotas(ctx context.Context, a accounts.Account) error {
	_, err := s.observeAccountQuotas(ctx, a, false)
	return err
}

func (s *Server) observeAccountQuotas(ctx context.Context, a accounts.Account, scheduled bool) (bool, error) {
	attempted := false
	err := s.accountPool.RefreshCapabilities(a, func(current accounts.Account) (map[string]any, error) {
		if scheduled && accounts.QuotaCheckPlan(current.Fields, time.Now()).At.After(time.Now()) {
			return nil, accounts.ErrRefreshDeferred
		}
		attempted = true
		fields, err := s.openAIAccountClient().FetchQuotas(ctx, current)
		return fields, quotaRefreshError(err)
	})
	s.recoverInvalidAccount(a.Fields, err)
	return attempted, err
}

func quotaRefreshError(err error) error {
	if err == nil {
		return nil
	}
	kind := "network"
	retry := time.Duration(0)
	var detail *provider.AccountEndpointError
	if errors.As(err, &detail) {
		retry = time.Duration(detail.RetryAfterSeconds) * time.Second
		switch {
		case detail.Status == 401:
			kind = "auth"
		case detail.Status == 429:
			kind = "rate_limit"
		case detail.Status >= 500:
			kind = "upstream"
		case detail.Status > 0:
			kind = "invalid_response"
		}
	}
	return &accounts.QuotaRefreshError{Err: err, Kind: kind, RetryAfter: retry}
}

func (s *Server) capabilityScheduler() { s.runCapabilitySchedule(s.refreshDueCapabilities) }
func (s *Server) modelCapabilityScheduler() {
	s.runCapabilitySchedule(func() {
		s.refreshDueModels()
		s.refreshDueGrokWebModels(context.Background(), "", 2, nil)
	})
}
func (s *Server) runCapabilitySchedule(work func()) {
	// Startup grace gives operators time to verify a rollout against a quiet data
	// directory. The maintenance marker pauses this scheduler during deployments.
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	select {
	case <-s.probeStop:
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); os.IsNotExist(err) && s.beginDeploymentBackground() {
			work()
			s.endDeploymentBackground()
		}
		select {
		case <-s.probeStop:
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) refreshDueCapabilities() {
	items, _, err := s.store.AccountSnapshot()
	if err != nil {
		return
	}
	now := time.Now()
	// Sort a copy: Store snapshots are immutable shared views.
	items = append([]map[string]any(nil), items...)
	sort.SliceStable(items, func(i, j int) bool {
		return accounts.QuotaCheckPlan(items[i], now).At.Before(accounts.QuotaCheckPlan(items[j], now).At)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	count := 0
	for _, row := range items {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-s.probeStop:
			return
		default:
		}
		if _, e := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(e) {
			return
		}
		a := accounts.Account{Token: accountToken(row), Fields: row}
		if !isOpenAIAccount(a) || !boolValue(row["enabled"], true) {
			continue
		}
		if mapValue(row["relogin_validation"])["pending"] == true {
			if s.retryAccountValidation(row, time.Now()) {
				count++
			}
			if count >= 4 {
				break
			}
			continue
		}
		if s.skipAccountRecoveryProbe(row) || accounts.QuotaCheckPlan(row, time.Now()).At.After(time.Now()) {
			continue
		}
		attemptCtx, stop := context.WithTimeout(ctx, 20*time.Second)
		attempted, err := s.observeAccountQuotas(attemptCtx, a, true)
		stop()
		if errors.Is(err, accounts.ErrRefreshDeferred) {
			// A busy account did not query upstream; stale results after a query
			// still count against the round's request budget.
			_, _ = s.store.UpdateAccountRuntime(a.Token, map[string]any{"quota_deferred_until": time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339)})
			if !attempted {
				continue
			}
		}
		count++
		if count >= 4 {
			break
		}
	}
}
func (s *Server) refreshQuotasAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"active": s.accountPool.CapabilityRefreshCount()})
		return
	}
	if _, e := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(e) {
		writeError(w, 503, "maintenance in progress", "server_error")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
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
	refreshed, failed, deferred := 0, 0, 0
	for _, ref := range uniqueAccountRefs(body.Refs) {
		row, err := s.accountByRef(ref)
		if err != nil {
			failed++
			continue
		}
		a := accounts.Account{Token: accountToken(row), Fields: row}
		if !isOpenAIAccount(a) {
			failed++
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		err = s.refreshAccountQuotas(ctx, a)
		cancel()
		if errors.Is(err, accounts.ErrRefreshDeferred) {
			deferred++
		} else if err != nil {
			failed++
		} else {
			refreshed++
		}
	}
	writeJSON(w, 200, map[string]any{"refreshed": refreshed, "failed": failed, "deferred": deferred})
}
