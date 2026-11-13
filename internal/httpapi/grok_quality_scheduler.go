package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func qualityNextDue(account oauth.Account, interval time.Duration, now time.Time) int64 {
	if account.Status == "disabled" || account.AccessToken == "" && !oauth.CPAManaged(account) {
		return 0
	}
	if q := account.Quality; q != nil {
		if q.Removal != nil && q.Removal.Status == "removed" && oauth.CPAManaged(account) {
			return 0
		}
		if q.Blocked && q.ConfirmedAt > 0 && q.Removal != nil && q.Removal.Status == "pending" {
			return now.Unix()
		}
		return q.At + int64(interval.Seconds())
	}
	return now.Unix()
}

func (s *Server) grokQualityScheduleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var cfg registerruntime.GrokQualitySchedule
		if !decodeJSON(w, r, &cfg) {
			return
		}
		if err := s.registerStore.SetGrokQualitySchedule(cfg); err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
	} else if r.Method != http.MethodGet {
		writeError(w, 405, "不支持的请求方法", "invalid_request_error")
		return
	}
	cfg := s.registerStore.GrokQualitySchedule()
	accounts, err := s.oauthStore.List()
	if err != nil {
		writeError(w, 500, "无法读取检测状态", "server_error")
		return
	}
	now := time.Now()
	var next, last int64
	for _, account := range accounts {
		if account.Quality != nil && account.Quality.At > last {
			last = account.Quality.At
		}
		if due := qualityNextDue(account, time.Duration(cfg.IntervalHours)*time.Hour, now); due > 0 && (next == 0 || due < next) {
			next = due
		}
	}
	pending, running := 0, 0
	for _, task := range s.taskQueue.List() {
		if task.Kind == "grok_oauth_quality" {
			if task.Status == "queued" {
				pending++
			}
			if task.Status == "running" {
				running++
			}
		}
	}
	if !cfg.Enabled {
		next = 0
	}
	writeJSON(w, 200, map[string]any{"enabled": cfg.Enabled, "interval_hours": cfg.IntervalHours, "last_checked_at": last, "next_due_at": next, "pending": pending, "running": running})
}

// A persisted per-account completion time survives restarts and prevents a
// global timer from repeatedly probing the same first account in the inventory.
func (s *Server) runGrokQualityIfDue(now time.Time) {
	cfg := s.registerStore.GrokQualitySchedule()
	if !cfg.Enabled {
		return
	}
	for _, task := range s.taskQueue.List() {
		if task.Kind == "grok_oauth_quality" && (task.Status == "queued" || task.Status == "running") {
			return
		}
	}
	accounts, err := s.oauthStore.List()
	if err != nil {
		return
	}
	interval := time.Duration(cfg.IntervalHours) * time.Hour
	sort.SliceStable(accounts, func(i, j int) bool {
		return qualityNextDue(accounts[i], interval, now) < qualityNextDue(accounts[j], interval, now)
	})
	for _, account := range accounts {
		due := qualityNextDue(account, interval, now)
		if due == 0 || due > now.Unix() {
			continue
		}
		if _, busy := s.grokOAuthActions.Load(account.ID); busy {
			continue
		}
		if s.qualityAuthorizationBusy(account.Email) {
			continue
		}
		cleanup := account.Quality != nil && account.Quality.Blocked && account.Quality.ConfirmedAt > 0 && account.Quality.Removal != nil && account.Quality.Removal.Status == "pending"
		_, _ = s.enqueueGrokQuality([]string{account.ID}, cleanup)
		return
	}
}

func (s *Server) grokQualityScheduler() {
	for {
		select {
		case <-s.probeStop:
			return
		default:
		}
		if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); os.IsNotExist(err) && s.beginDeploymentBackground() {
			s.runGrokQualityIfDue(time.Now())
			s.endDeploymentBackground()
		}
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-s.probeStop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
