package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func (s *Server) refreshGrokMediaQuota(ctx context.Context, token string, fields map[string]any) provider.GrokMediaQuotaSnapshot {
	snapshot, _ := s.observeGrokMediaQuota(ctx, token, fields)
	return snapshot
}

func (s *Server) observeGrokMediaQuota(ctx context.Context, token string, fields map[string]any) (provider.GrokMediaQuotaSnapshot, bool) {
	rows, _ := s.grokRuntimeStore.AccountList()
	for _, row := range rows {
		if accountToken(row) == token {
			fields = row
			break
		}
	}
	snapshot := provider.GrokMediaQuotaSnapshot{Status: "deferred", Quotas: map[string]provider.GrokMediaQuotaWindow{}}
	var queryErr error
	attempted := false
	err := s.grokAccountPool.RefreshGrokMedia(accounts.Account{Token: token, Fields: fields}, func(current accounts.Account) (map[string]any, error) {
		attempted = true
		snapshot = s.grokQuota.RefreshMedia(ctx, current.Token, current.Fields)
		if snapshot.Status != "success" {
			queryErr = fmt.Errorf("media quota query failed")
		}
		return map[string]any{"media_quota": snapshot}, queryErr
	})
	if errors.Is(err, accounts.ErrRefreshDeferred) {
		// A manual/page refresh must not replace the active generation's observation.
		raw, _ := json.Marshal(fields["media_quota"])
		_ = json.Unmarshal(raw, &snapshot)
		return snapshot, false
	}
	if snapshot.Status == "deferred" {
		return snapshot, false
	}
	if err != nil && err != queryErr {
		return provider.GrokMediaQuotaSnapshot{Status: "failed", Error: "persistence_failed", Quotas: map[string]provider.GrokMediaQuotaWindow{}}, attempted
	}
	// Runtime persistence completed while the pool excluded new media leases.
	items, loadErr := s.registerStore.ExportAccounts(nil)
	if loadErr == nil {
		for _, item := range items {
			saved := firstNonEmpty(stringValue(item["sso"]), stringValue(item["access_token"]), stringValue(item["token"]))
			if strings.TrimSpace(saved) == strings.TrimSpace(token) {
				_, _ = s.registerStore.UpdateAccountFields(stringValue(item["id"]), map[string]any{"media_quota": snapshot})
			}
		}
	}
	return snapshot, attempted
}

func (s *Server) ensureGrokMediaQuotas(parent context.Context, intents ...accounts.Intent) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	s.refreshGrokMediaQuotas(ctx, true, intents...)
}
func (s *Server) refreshDueGrokMediaQuotas(ctx context.Context) {
	s.refreshGrokMediaQuotas(ctx, false)
}
func (s *Server) refreshGrokMediaQuotas(ctx context.Context, onDemand bool, intents ...accounts.Intent) {
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		return
	}
	rows, _, err := s.grokRuntimeStore.AccountSnapshot()
	if err != nil {
		return
	}
	// AccountSnapshot shares an immutable slice with Store; sort a copy.
	rows = append([]map[string]any(nil), rows...)
	now := time.Now()
	sort.SliceStable(rows, func(i, j int) bool {
		return accounts.GrokMediaCheckPlan(rows[i], now, onDemand).At.Before(accounts.GrokMediaCheckPlan(rows[j], now, onDemand).At)
	})
	count := 0
	for _, row := range rows {
		if len(intents) > 0 && s.grokAccountPool.HasGrokMediaCapacity(intents[0]) {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
			return
		}
		if !boolValue(row["enabled"], true) || accounts.IsAbnormal(row) {
			continue
		}
		switch strings.ToLower(stringValue(row["status"])) {
		case "invalid", "disabled", "expired", "异常", "失效", "禁用":
			continue
		}
		token := accountToken(row)
		if token == "" || accounts.GrokMediaCheckPlan(row, time.Now(), onDemand).At.After(time.Now()) {
			continue
		}
		_, attempted := s.observeGrokMediaQuota(ctx, token, row)
		if attempted {
			count++
		}
		if count >= 4 {
			return
		}
	}
}
func (s *Server) grokMediaQuotaScheduler() {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	select {
	case <-s.probeStop:
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		if boolValue(s.registerStore.GrokProbeSchedulerStatus()["enabled"], false) && s.beginDeploymentBackground() {
			s.refreshDueGrokMediaQuotas(ctx)
			s.endDeploymentBackground()
		}
		cancel()
		select {
		case <-s.probeStop:
			return
		case <-ticker.C:
		}
	}
}
