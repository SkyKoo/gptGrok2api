package register

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// RegistrationSchedule contains only timing state. Batch options are always
// loaded from the current registration configuration when a timer fires.
type RegistrationSchedule struct {
	Enabled         bool   `json:"enabled"`
	IntervalMinutes int    `json:"interval_minutes"`
	NextRunAt       string `json:"next_run_at"`
	LastAttemptAt   string `json:"last_attempt_at"`
	LastStartedAt   string `json:"last_started_at"`
	LastResult      string `json:"last_result"`
	LastError       string `json:"last_error"`
}

type Schedule struct {
	mu           sync.Mutex
	path         string
	state        RegistrationSchedule
	loadError    error
	runtimeError string
}

func NewSchedule(path string) *Schedule {
	s := &Schedule{path: path, state: RegistrationSchedule{IntervalMinutes: 60}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s
	}
	if err != nil {
		s.loadError = fail("storage", "schedule_state_unreadable")
		return s
	}
	var state RegistrationSchedule
	if json.Unmarshal(raw, &state) != nil || state.IntervalMinutes < 1 || state.IntervalMinutes > 10080 {
		s.loadError = fail("storage", "schedule_state_invalid")
		return s
	}
	if state.Enabled {
		if _, err := time.Parse(time.RFC3339Nano, state.NextRunAt); err != nil {
			s.loadError = fail("storage", "schedule_state_invalid")
			return s
		}
	}
	s.state = state
	return s
}

func (s *Schedule) Snapshot() RegistrationSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state
	if s.loadError != nil {
		state.LastError = safeFailure(s.loadError)
	}
	if s.runtimeError != "" {
		state.LastError = s.runtimeError
	}
	return state
}

func (s *Schedule) Configure(enabled bool, minutes int, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadError != nil {
		return s.loadError
	}
	if minutes < 1 || minutes > 10080 {
		return fail("config", "schedule_interval_must_be_1_to_10080_minutes")
	}
	if s.state.Enabled == enabled && s.state.IntervalMinutes == minutes {
		return nil
	}
	next := s.state
	next.Enabled, next.IntervalMinutes = enabled, minutes
	next.NextRunAt, next.LastError = "", ""
	if enabled {
		next.NextRunAt = now.UTC().Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339Nano)
	}
	if err := s.save(next); err != nil {
		return err
	}
	return nil
}

// RunDue durably claims the timer before starting work. A restart after the
// claim cannot replay the same slot. Missed slots are coalesced into one attempt;
// busy/maintenance/error outcomes wait a full interval rather than piling up.
func (s *Schedule) RunDue(now time.Time, start func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadError != nil {
		return s.loadError
	}
	if !s.state.Enabled {
		return nil
	}
	due, err := time.Parse(time.RFC3339Nano, s.state.NextRunAt)
	if err != nil {
		return fail("storage", "schedule_state_invalid")
	}
	if now.Before(due) {
		return nil
	}
	next := s.state
	next.LastAttemptAt = now.UTC().Format(time.RFC3339Nano)
	next.NextRunAt = now.UTC().Add(time.Duration(next.IntervalMinutes) * time.Minute).Format(time.RFC3339Nano)
	next.LastResult, next.LastError = "pending", ""
	if err := s.save(next); err != nil {
		return err
	}
	if err := start(); err != nil {
		next.LastResult, next.LastError = "failed", safeFailure(err)
		var failure *Failure
		if errors.As(err, &failure) && (failure.Code == "already_running" || failure.Code == "maintenance" || failure.Code == "openai_target_required") {
			next.LastResult = "skipped"
		}
	} else {
		next.LastResult, next.LastStartedAt = "started", next.LastAttemptAt
	}
	return s.save(next)
}

func (s *Schedule) save(next RegistrationSchedule) error {
	if err := writeJSON0600(s.path, next); err != nil {
		s.runtimeError = "storage: schedule_state_write_failed"
		return fail("storage", "schedule_state_write_failed")
	}
	s.state, s.runtimeError = next, ""
	return nil
}
