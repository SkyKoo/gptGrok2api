package httpapi

import (
	"context"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (s *Server) survivalSnapshot() map[string]any {
	config := s.registerStore.Get()
	survival := mapValue(config["openai_survival"])
	if len(survival) == 0 {
		survival = map[string]any{"enabled": true, "interval_minutes": 60, "concurrency": 4, "refresh_codex_rt": true}
	}
	s.survivalMu.RLock()
	status := cloneMap(s.survivalStatus)
	s.survivalMu.RUnlock()
	for key, value := range survival {
		status[key] = value
	}
	return status
}

func (s *Server) runOpenAISurvival(w http.ResponseWriter) {
	s.survivalMu.Lock()
	if s.survivalRunning {
		s.survivalMu.Unlock()
		writeJSON(w, 200, map[string]any{"started": false, "survival": s.survivalSnapshot()})
		return
	}
	s.survivalRunning = true
	s.survivalStatus["running"] = true
	s.survivalStatus["last_started_at"] = time.Now().UTC().Format(time.RFC3339)
	s.survivalStatus["last_error"] = ""
	s.survivalMu.Unlock()
	s.launchDeploymentWork(s.executeOpenAISurvival)
	writeJSON(w, 202, map[string]any{"started": true, "survival": s.survivalSnapshot()})
}

func (s *Server) executeOpenAISurvival() {
	config := mapValue(s.registerStore.Get()["openai_survival"])
	concurrency := intValue(config["concurrency"])
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 8 {
		concurrency = 8
	}
	refreshCredentials := boolValue(config["refresh_codex_rt"], true)
	items, err := s.store.AccountList()
	if err != nil {
		s.finishSurvival(map[string]any{"error": err.Error()})
		return
	}
	var mu sync.Mutex
	counts := map[string]int{}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, account := range items {
		account := account
		if strings.TrimSpace(accountToken(account)) == "" || s.skipAccountRecoveryProbe(account) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			status := s.survivalProbe(account, refreshCredentials)
			mu.Lock()
			counts[status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	confirmed := 0
	errorsCount := 0
	for status, count := range counts {
		if isHealthyOpenAIPlan(status) {
			confirmed += count
		}
		if status == "error" || status == "token_dead" || status == "no_token" {
			errorsCount += count
		}
	}
	s.finishSurvival(map[string]any{"total": sumCounts(counts), "confirmed": confirmed, "errors": errorsCount, "statuses": counts})
}

func (s *Server) openAISurvivalScheduler() {
	// Match the Python service's startup grace period so account imports and
	// configuration updates settle before the first remote probe.
	timer := time.NewTimer(60 * time.Second)
	select {
	case <-s.probeStop:
		timer.Stop()
		return
	case <-timer.C:
	case <-s.survivalWake:
		timer.Stop()
	}
	for {
		config := mapValue(s.registerStore.Get()["openai_survival"])
		enabled := boolValue(config["enabled"], true)
		interval := intValue(config["interval_minutes"])
		if interval < 15 {
			interval = 60
		}
		if enabled {
			last := ""
			s.survivalMu.RLock()
			last = stringValue(s.survivalStatus["last_finished_at"])
			s.survivalMu.RUnlock()
			due := last == ""
			if parsed, err := time.Parse(time.RFC3339, last); err == nil {
				due = time.Since(parsed) >= time.Duration(interval)*time.Minute
			}
			if due {
				s.runOpenAISurvivalInternal()
			}
			next := time.Now().UTC().Add(time.Duration(interval) * time.Minute).Format(time.RFC3339)
			s.survivalMu.Lock()
			s.survivalStatus["next_run_at"] = next
			s.survivalMu.Unlock()
		}
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-s.probeStop:
			timer.Stop()
			return
		case <-s.survivalWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Server) runOpenAISurvivalInternal() {
	if !s.beginDeploymentBackground() {
		return
	}
	defer s.endDeploymentBackground()
	s.survivalMu.Lock()
	if s.survivalRunning {
		s.survivalMu.Unlock()
		return
	}
	s.survivalRunning = true
	s.survivalStatus["running"] = true
	s.survivalStatus["last_started_at"] = time.Now().UTC().Format(time.RFC3339)
	s.survivalStatus["last_error"] = ""
	s.survivalMu.Unlock()
	s.launchDeploymentWork(s.executeOpenAISurvival)
}

func (s *Server) survivalProbe(account map[string]any, refreshCredentials bool) string {
	token := accountToken(account)
	if token == "" {
		return "no_token"
	}
	now := time.Now()
	// Survival also reads quotas; do not let it bypass the quota Retry-After.
	if retry, err := time.Parse(time.RFC3339, stringValue(account["quota_retry_at"])); err == nil && retry.After(now) {
		return "deferred"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	status := "error"
	active := account
	err := s.accountPool.RefreshAccountCapabilities(accounts.Account{Token: token, Fields: account}, "survival", func(current accounts.Account) (accounts.CapabilityObservation, error) {
		active = current.Fields
		if retry, err := time.Parse(time.RFC3339, stringValue(current.Fields["quota_retry_at"])); err == nil && retry.After(time.Now()) {
			return accounts.CapabilityObservation{}, accounts.ErrRefreshDeferred
		}
		// RefreshAccount already handles credentials when needed. Use one attempt,
		// not a second immediate retry of a failed quota/identity observation.
		candidate := current.Fields
		if !refreshCredentials {
			candidate = cloneMap(current.Fields)
			delete(candidate, "refresh_token")
		}
		result, err := s.refreshValidatedAccount(ctx, candidate)
		if err != nil {
			if errors.Is(err, provider.ErrInvalidAccessToken) {
				status = "token_dead"
			}
			fields := accountRefreshFailureUpdates(err)
			fields["survival_status"] = status
			fields["survival_last_probe_status"] = status
			fields["survival_last_checked_at"] = time.Now().UTC().Format(time.RFC3339)
			fields["survival_check_error"] = safeRefreshError(err)
			return accounts.CapabilityObservation{Fields: fields}, quotaRefreshError(err)
		}
		fields := cloneMap(result.Fields)
		status = firstNonEmpty(strings.ToLower(stringValue(fields["type"])), "free")
		for k, v := range map[string]any{"survival_status": status, "survival_last_probe_status": status, "survival_last_checked_at": time.Now().UTC().Format(time.RFC3339), "survival_alive": true, "survival_plan_type": status, "survival_check_error": nil} {
			fields[k] = v
		}
		return accounts.CapabilityObservation{Fields: fields, AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken, VerifiedAccount: true}, nil
	})
	if errors.Is(err, accounts.ErrRefreshDeferred) {
		return "deferred"
	}
	if err != nil {
		s.recoverInvalidAccount(active, err)
		if status != "token_dead" {
			return "error"
		}
	}
	return status
}

func (s *Server) finishSurvival(summary map[string]any) {
	s.survivalMu.Lock()
	defer s.survivalMu.Unlock()
	s.survivalRunning = false
	s.survivalStatus["running"] = false
	s.survivalStatus["last_finished_at"] = time.Now().UTC().Format(time.RFC3339)
	if value, ok := summary["error"]; ok {
		s.survivalStatus["last_error"] = value
	} else {
		s.survivalStatus["last_error"] = ""
		s.survivalStatus["last_summary"] = summary
	}
}

func isHealthyOpenAIPlan(status string) bool {
	switch status {
	case "free", "plus", "pro", "team", "k12":
		return true
	}
	return false
}
func sumCounts(values map[string]int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}
