package httpapi

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type adobeSubmittedQuota struct {
	accountID, taskKey, endpoint string
	revision                     uint64
}

type adobeSubmittedQuotaQueue struct {
	mu      sync.Mutex
	pending map[string]adobeSubmittedQuota
	wake    chan struct{}
}

func (q *adobeSubmittedQuotaQueue) enqueue(job adobeSubmittedQuota) {
	q.mu.Lock()
	q.pending[job.accountID] = job
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *adobeSubmittedQuotaQueue) next() (adobeSubmittedQuota, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, job := range q.pending {
		delete(q.pending, id)
		return job, true
	}
	return adobeSubmittedQuota{}, false
}

func (s *Server) queueAdobeSubmittedQuota(ctx context.Context, a adobe.Account, key string) {
	if endpoint, ok := proxyruntime.URLSelectionFromContext(ctx); ok {
		s.adobeSubmittedQuotas.enqueue(adobeSubmittedQuota{a.ID, key, endpoint, a.Revision})
	}
}

func (s *Server) runAdobeSubmittedQuota(job adobeSubmittedQuota) {
	if !s.adobeReconciliationAllowed() || !s.beginDeploymentBackground() {
		return
	}
	defer s.endDeploymentBackground()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	ctx = proxyruntime.WithURL(ctx, job.endpoint)
	_, _ = s.adobeStore.QuerySubmittedQuota(ctx, s.adobeClient, job.accountID, job.taskKey, job.revision)
}

func (s *Server) adobeSubmittedQuotaScheduler() {
	for {
		select {
		case <-s.probeStop:
			return
		case <-s.adobeSubmittedQuotas.wake:
		}
		for {
			job, ok := s.adobeSubmittedQuotas.next()
			if !ok {
				break
			}
			select {
			case <-s.probeStop:
				return
			default:
				s.runAdobeSubmittedQuota(job)
			}
		}
	}
}

func (s *Server) refreshAdobeSettledQuotas(ctx context.Context, items []adobe.Account, now time.Time) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].SettlementUntil < items[j].SettlementUntil })
	reads := 0
	for _, a := range items {
		if ctx.Err() != nil || !s.adobeReconciliationAllowed() || reads >= 4 {
			return
		}
		if !adobe.SettledQuotaDue(a, now) {
			continue
		}
		fixed, err := s.adobeAccountContext(ctx, a)
		if err != nil {
			if _, saveErr := s.adobeStore.RecordQuotaQueryFailure(a, "proxy_unavailable", time.Now()); saveErr == nil {
				reads++
			}
			continue
		}
		_, err = s.adobeStore.QuerySettledQuota(fixed, s.adobeClient, a)
		if !errors.Is(err, adobe.ErrBusy) && !errors.Is(err, adobe.ErrResolutionConflict) {
			reads++
		}
	}
}

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
		if !adobe.PeriodicQuotaDue(a, now) {
			continue
		}
		fixed, err := s.adobeAccountContext(ctx, a)
		if err != nil {
			if _, saveErr := s.adobeStore.RecordQuotaQueryFailure(a, "proxy_unavailable", time.Now()); saveErr == nil {
				attempts++
			}
		} else {
			_, attempted, _ := s.adobeStore.RefreshDueQuota(fixed, s.adobeClient, a.ID, time.Now())
			if attempted {
				attempts++
			}
		}
		if attempts >= 4 {
			return
		}
	}
}

func (s *Server) refreshAdobeLoginSnapshot(ctx context.Context, items []adobe.Account, now time.Time) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].ExpiresAt < items[j].ExpiresAt })
	for _, a := range items {
		if ctx.Err() != nil || !s.adobeReconciliationAllowed() {
			return
		}
		if !adobe.AutomaticReloginDue(a, now) {
			continue
		}
		if !s.adobeAutoReloginEnabled() {
			return
		}
		fixed, err := s.adobeAccountContext(ctx, a)
		if err != nil {
			_, _ = s.adobeStore.RecordQuotaQueryFailure(a, "proxy_unavailable", time.Now())
			continue
		}
		if _, attempted, _ := s.adobeStore.RefreshDueLogin(fixed, s.adobeClient, a.ID, time.Now()); attempted {
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
	if err != nil || !boolValue(mapValue(cfg["adobe_maintenance"])["enabled"], true) || !boolValue(cfg[adobeAutoReloginSetting], true) {
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
	if err != nil || !boolValue(mapValue(cfg["adobe_maintenance"])["enabled"], true) || !boolValue(cfg[adobeAutoReloginSetting], true) {
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
		if !adobe.Eligible(a, now) || strings.TrimSpace(a.ProfileID) == "" || !adobeHasKnownMediaCapacity(a, now) {
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
	if err != nil || checked.After(now) || now.Sub(checked) > adobe.GenerationObservationMaxAge {
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
