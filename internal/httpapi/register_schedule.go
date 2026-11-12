package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func (s *Server) registrationScheduleForTarget(target string) *registerruntime.Schedule {
	for _, schedule := range []*registerruntime.Schedule{s.registrationSchedule, s.registrationExtraSchedule, s.adobeRegistrationSchedule} {
		if schedule != nil && schedule.Snapshot().Target == target {
			return schedule
		}
	}
	return nil
}

func (s *Server) registrationScheduleSnapshot(target string) registerruntime.RegistrationSchedule {
	if schedule := s.registrationScheduleForTarget(target); schedule != nil {
		return schedule.Snapshot()
	}
	// Failed migration/load must not launch the wrong channel or hide the error.
	return registerruntime.RegistrationSchedule{Target: target, IntervalMinutes: 60, LastError: "storage: schedule_state_unavailable"}
}

func (s *Server) registrationScheduleAPI(w http.ResponseWriter, r *http.Request) {
	target, ok := s.registrationRequestTarget(w, r)
	if !ok {
		return
	}
	schedule := s.registrationScheduleForTarget(target)
	if schedule == nil {
		writeError(w, 500, "注册定时状态不可用，请检查配置存储", "server_error")
		return
	}
	// registerAPI has already required administrator authentication.
	if r.Method == http.MethodPost {
		var body struct {
			Enabled         *bool `json:"enabled"`
			IntervalMinutes *int  `json:"interval_minutes"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Enabled == nil || body.IntervalMinutes == nil {
			writeError(w, 400, "请设置定时开关和间隔分钟数", "invalid_request_error")
			return
		}
		s.registrationMu.Lock()
		defer s.registrationMu.Unlock()
		if s.registrationClosing {
			writeError(w, 503, "服务正在停止，请稍后重试", "server_error")
			return
		}
		if *body.Enabled {
			config := s.registerStore.ConfigForTarget(target)
			var err error
			if target == "grok" {
				_, err = registerruntime.ParseGrokConfig(config)
			} else if target == "adobe" {
				_, err = registerruntime.ParseAdobeConfig(config)
			} else {
				_, err = registerruntime.ParseFreeConfig(config)
			}
			if err != nil {
				registrationError(w, err)
				return
			}
		}
		if err := schedule.Configure(*body.Enabled, *body.IntervalMinutes, time.Now()); err != nil {
			registrationError(w, err)
			return
		}
	} else if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	writeJSON(w, 200, map[string]any{"schedule": schedule.Snapshot()})
}

func (s *Server) registrationScheduler() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.runRegistrationSchedule(time.Now())
		select {
		case <-s.registrationStop:
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) runRegistrationSchedule(now time.Time) {
	if !s.beginDeploymentBackground() {
		return
	}
	defer s.endDeploymentBackground()
	// Serialize timer claims with manual start/stop and batch configuration writes.
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if s.registrationClosing {
		return
	}
	for _, schedule := range []*registerruntime.Schedule{s.registrationSchedule, s.registrationExtraSchedule, s.adobeRegistrationSchedule} {
		if schedule == nil {
			continue
		}
		target := schedule.Snapshot().Target
		if target != "openai" && target != "grok" && target != "adobe" {
			continue
		}
		_ = schedule.RunDue(now, func() error {
			if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
				return &registerruntime.Failure{Stage: "task", Code: "maintenance"}
			}
			config := s.registerStore.ConfigForTarget(target)
			var err error
			if target == "grok" {
				_, err = s.grokRegister.Start(config)
			} else if target == "adobe" {
				_, err = s.adobeRegister.Start(config)
			} else {
				_, err = s.freeRegister.Start(config)
			}
			return err
		}) // Each channel durably claims only its own timer slot.
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		return
	}
	// Existing per-engine compensation rules remain independent of the viewed tab.
	_, _ = s.freeRegister.RunCompensation(s.registerStore.ConfigForTarget("openai"), now)
	_, _ = s.grokRegister.RunCompensation(s.registerStore.ConfigForTarget("grok"), now)
	if s.adobeRegister != nil {
		_, _ = s.adobeRegister.RunCompensation(s.registerStore.ConfigForTarget("adobe"), now)
	}
}
