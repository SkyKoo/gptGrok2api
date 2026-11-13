package httpapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
)

func grokProbeInterval(config map[string]any) time.Duration {
	interval := intValue(config["interval_minutes"])
	if interval < 1 {
		interval = 60
	}
	return time.Duration(interval) * time.Minute
}

func (s *Server) grokProbeScheduler() {
	for {
		if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); os.IsNotExist(err) && s.beginDeploymentBackground() {
			s.runGrokProbeIfDue(time.Now())
			s.endDeploymentBackground()
		}
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-s.probeStop:
			timer.Stop()
			return
		case <-s.probeWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Completion owns the next deadline; do not overwrite it with a pre-run copy.
func (s *Server) runGrokProbeIfDue(now time.Time) {
	config := s.registerStore.GrokProbeSchedulerStatus()
	if !boolValue(config["enabled"], false) {
		return
	}
	last, err := time.Parse(time.RFC3339Nano, stringValue(config["last_finished_at"]))
	if err == nil && now.Before(last.Add(grokProbeInterval(config))) {
		next := last.Add(grokProbeInterval(config)).UTC().Format(time.RFC3339)
		if next != stringValue(config["next_run_at"]) {
			_ = s.registerStore.UpdateGrokProbeSchedulerRuntime(false, next, stringValue(config["last_finished_at"]), stringValue(config["error"]))
		}
		return
	}
	s.runGrokProbeCycle()
}

func (s *Server) runGrokProbeCycle() {
	config := s.registerStore.GrokProbeSchedulerStatus()
	_ = s.registerStore.UpdateGrokProbeSchedulerRuntime(true, "", stringValue(config["last_finished_at"]), "")
	var cycleErr error
	defer func() {
		finished := time.Now().UTC()
		message := ""
		if cycleErr != nil {
			message = cycleErr.Error()
		}
		_ = s.registerStore.UpdateGrokProbeSchedulerRuntime(false, finished.Add(grokProbeInterval(config)).Format(time.RFC3339), finished.Format(time.RFC3339), message)
	}()
	archive, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		cycleErr = errors.New("SSO 账号列表读取失败")
		return
	}
	rows, _, err := s.grokRuntimeStore.AccountSnapshot()
	if err != nil {
		cycleErr = errors.New("SSO 调用池读取失败")
		return
	}
	credentials, err := s.oauthStore.List()
	if err != nil {
		cycleErr = errors.New("OAuth 账号列表读取失败")
		return
	}
	// Prefer the runtime route for accounts already in the request pool. Disabled
	// archive entries also prevent a background check of their runtime duplicate.
	byToken := map[string]map[string]any{}
	disabled := map[string]bool{}
	for _, item := range archive {
		if token := strings.TrimSpace(stringValue(item["sso"])); token != "" {
			byToken[token] = item
			disabled[token] = grokAccountDisabled(item)
		}
	}
	for _, row := range rows {
		if token := accountToken(row); token != "" {
			byToken[token] = row
			disabled[token] = disabled[token] || grokAccountDisabled(row)
		}
	}
	var jobs []func(context.Context) error
	for token, row := range byToken {
		if disabled[token] {
			continue
		}
		token, row := token, row
		jobs = append(jobs, func(ctx context.Context) error {
			_, _, err := s.verifyGrokCredential(ctx, token, row)
			return err
		})
	}
	for _, account := range credentials {
		if oauth.CPAManaged(account) || strings.EqualFold(account.Status, "disabled") || (account.AccessToken == "" && account.RefreshToken == "") {
			continue
		}
		id := account.ID
		jobs = append(jobs, func(ctx context.Context) error {
			updated, err := s.refreshOAuthQuota(ctx, id)
			if err == nil && stringValue(updated.Quota["status"]) == "error" {
				return errors.New("OAuth check failed")
			}
			return err
		})
	}
	queue := make(chan func(context.Context) error)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range queue {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err := job(ctx)
				cancel()
				if err != nil && !errors.Is(err, accounts.ErrRefreshDeferred) {
					mu.Lock()
					failures++
					mu.Unlock()
				}
			}
		}()
	}
loop:
	for _, job := range jobs {
		if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
			break
		}
		if !boolValue(s.registerStore.GrokProbeSchedulerStatus()["enabled"], false) {
			break
		}
		select {
		case queue <- job:
		case <-s.probeStop:
			break loop
		}
	}
	close(queue)
	wg.Wait()
	if failures > 0 {
		cycleErr = fmt.Errorf("%d 个账号检查未成功，详见账号检测结果", failures)
	}
}
