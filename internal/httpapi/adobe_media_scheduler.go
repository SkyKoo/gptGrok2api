package httpapi

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

func (s *Server) refreshAdobeMaintenanceSnapshot(ctx context.Context, items []adobe.Account, now time.Time) {
	items = append([]adobe.Account(nil), items...)
	sort.SliceStable(items, func(i, j int) bool {
		return adobe.QuotaRefreshDueAt(items[i], now).Before(adobe.QuotaRefreshDueAt(items[j], now))
	})
	attempts := 0
	for _, a := range items {
		if ctx.Err() != nil || !s.adobeReconciliationAllowed() {
			return
		}
		if !adobe.MaintenanceDue(a, now) {
			continue
		}
		fixed, err := s.adobeAccountContext(ctx, a)
		if err != nil {
			continue
		}
		_, attempted, _ := s.adobeStore.RefreshDueWithPermissions(fixed, s.adobeClient, a.ID, time.Now())
		if attempted {
			attempts++
		}
		if attempts >= 4 {
			return
		}
	}
}

// Warm only recent demand after quota maintenance. The short ready window is a
// local reuse policy, not an assertion about Adobe's upstream session lifetime.
func (s *Server) warmRecentAdobeMediaSessions(ctx context.Context) {
	if ctx.Err() != nil || s.adobeStore == nil || !s.adobeReconciliationAllowed() {
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
	type candidate struct {
		account adobe.Account
		ctx     context.Context
	}
	now := time.Now()
	ready := 0
	candidates := []candidate{}
	for _, a := range items {
		if !adobe.Eligible(a, now) || strings.TrimSpace(a.ProfileID) == "" || !adobeHasKnownMediaCapacity(a, now) {
			continue
		}
		fixed, err := s.adobeAccountContext(ctx, a)
		if err != nil {
			continue
		}
		if s.adobeStore.MediaSessionReady(fixed, a, time.Minute) {
			ready++
			if ready >= 2 {
				return
			}
		}
		if a.LastUsedAt <= 0 || a.LastUsedAt > now.Unix() || a.LastUsedAt < now.Add(-10*time.Minute).Unix() || !s.adobeStore.MediaPreparationDue(fixed, a) {
			continue
		}
		candidates = append(candidates, candidate{account: a, ctx: fixed})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].account.LastUsedAt > candidates[j].account.LastUsedAt
	})
	if len(candidates) == 0 || ctx.Err() != nil || !s.adobeReconciliationAllowed() {
		return
	}
	// Store rechecks eligibility/due under the shared preparation guard. A
	// foreground collection that wins the race makes this a no-op or busy result.
	selected := candidates[0]
	_, _ = s.adobeStore.WarmMediaSession(selected.ctx, selected.account.ID, selected.account.Revision)
}

// refreshAllAdobeMediaSessions checks every account that can be used for media
// generation and warms at most one missing or expired session. The browser
// worker supports one session at a time, so the next candidate is left for the
// next scheduler tick. WarmMediaSession rechecks the account gate, readiness
// cache and failure backoff before it starts any browser work, so a foreground
// request or a previous failed refresh wins safely when it races this scan.
func (s *Server) refreshAllAdobeMediaSessions(ctx context.Context) {
	if ctx.Err() != nil || s.adobeStore == nil || !s.adobeReconciliationAllowed() {
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

	type candidate struct {
		account adobe.Account
		ctx     context.Context
	}
	now := time.Now()
	candidates := make([]candidate, 0, len(items))
	for _, a := range items {
		if !adobe.Eligible(a, now) || strings.TrimSpace(a.ProfileID) == "" {
			continue
		}
		fixed, resolveErr := s.adobeAccountContext(ctx, a)
		if resolveErr != nil || !s.adobeStore.MediaPreparationDue(fixed, a) {
			continue
		}
		candidates = append(candidates, candidate{account: a, ctx: fixed})
	}
	// Prefer accounts used most recently. The scan only decides which account is
	// due; the refresh below deliberately starts one browser session per tick.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].account.LastUsedAt > candidates[j].account.LastUsedAt
	})
	if len(candidates) == 0 || ctx.Err() != nil || !s.adobeReconciliationAllowed() {
		return
	}
	// Do not start a second browser session in this tick. A failed attempt is
	// recorded by the store and its backoff removes the same account from the
	// next scan, allowing the following candidate to proceed.
	item := candidates[0]
	_, _ = s.adobeStore.WarmMediaSession(item.ctx, item.account.ID, item.account.Revision)
}

func adobeHasKnownMediaCapacity(a adobe.Account, now time.Time) bool {
	generation := a.Quota.Generation
	if generation == nil || generation.Status != "known" {
		return false
	}
	checked, err := time.Parse(time.RFC3339Nano, generation.CheckedAt)
	if err != nil || checked.After(now) || now.Sub(checked) > 5*time.Minute {
		return false
	}
	for _, entitlement := range generation.Models {
		if !entitlement.Authorized || entitlement.PoolID == "" {
			continue
		}
		if _, supported := adobe.FindModel(entitlement.Model); !supported {
			continue
		}
		for _, pool := range generation.Pools {
			if pool.ID != entitlement.PoolID || pool.Available == nil {
				continue
			}
			available := *pool.Available > 0
			if entitlement.Cost != nil {
				available = *pool.Available >= *entitlement.Cost
			}
			if available {
				return true
			}
		}
	}
	return false
}
