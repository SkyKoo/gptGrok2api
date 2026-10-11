package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func (s *Server) registrationScheduleAPI(w http.ResponseWriter, r *http.Request) {
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
			if _, err := registerruntime.ParseFreeConfig(s.registerStore.Get()); err != nil {
				registrationError(w, err)
				return
			}
		}
		if err := s.registrationSchedule.Configure(*body.Enabled, *body.IntervalMinutes, time.Now()); err != nil {
			registrationError(w, err)
			return
		}
	} else if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	writeJSON(w, 200, map[string]any{"schedule": s.registrationSchedule.Snapshot()})
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
	// Serialize timer claims with manual start/stop and batch configuration writes.
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if s.registrationClosing {
		return
	}
	_ = s.registrationSchedule.RunDue(now, func() error {
		if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
			return &registerruntime.Failure{Stage: "task", Code: "maintenance"}
		}
		_, err := s.freeRegister.Start(s.registerStore.Get())
		return err
	}) // Storage/launch failures are exposed by the persisted schedule snapshot.
}
