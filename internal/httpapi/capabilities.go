package httpapi

import (
	"context"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (s *Server) quotaContext(ctx context.Context, l *accounts.Lease) context.Context {
	return provider.WithQuotaObserver(ctx, func(feature string) { s.accountPool.MarkSent(l, feature) })
}
func (s *Server) quotaRetrySafe(l *accounts.Lease, err error) bool {
	status := upstreamStatus(err)
	return !s.accountPool.Sent(l, "reason") || status == http.StatusUnauthorized || status == http.StatusTooManyRequests
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
	return s.accountPool.RefreshCapabilities(a, func(current accounts.Account) (map[string]any, error) {
		return s.openAIAccountClient().FetchQuotas(ctx, current)
	})
}
func (s *Server) capabilityScheduler() {
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
		if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); os.IsNotExist(err) {
			s.refreshDueCapabilities()
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
	count := 0
	for _, row := range items {
		if _, e := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(e) {
			return
		}
		a := accounts.Account{Token: accountToken(row), Fields: row}
		if !isOpenAIAccount(a) || !boolValue(row["enabled"], true) {
			continue
		}
		if row["quota_last_used_at"] == nil && row["quota_refresh_needed"] != true {
			continue
		}
		observed, _ := time.Parse(time.RFC3339, stringValue(row["quota_observed_at"]))
		attempt, _ := time.Parse(time.RFC3339, stringValue(row["quota_last_attempt_at"]))
		if time.Since(attempt) < 30*time.Second {
			continue
		}
		due := row["quota_refresh_needed"] == true || time.Since(observed) > 5*time.Minute
		for _, q := range accounts.DecodeQuotas(row["capability_quotas"]) {
			if q.Remaining != nil && *q.Remaining == 0 {
				if reset, e := time.Parse(time.RFC3339, q.ResetAt); e == nil && time.Now().After(reset) {
					due = true
				}
			}
		}
		if !due {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = s.refreshAccountQuotas(ctx, a)
		cancel()
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
	refreshed, failed := 0, 0
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
		if err != nil {
			failed++
		} else {
			refreshed++
		}
	}
	writeJSON(w, 200, map[string]any{"refreshed": refreshed, "failed": failed})
}
