package register

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduleTimingPersistenceAndNoCatchUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	s := NewSchedule(path)
	calls := 0
	start := func() error { calls++; return nil }
	if err := s.RunDue(now, start); err != nil || calls != 0 {
		t.Fatal("default schedule started")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("default read wrote state")
	}
	if err := s.Configure(true, 30, now); err != nil {
		t.Fatal(err)
	}
	first := s.Snapshot().NextRunAt
	if err := s.Configure(true, 30, now.Add(time.Minute)); err != nil || s.Snapshot().NextRunAt != first {
		t.Fatal("no-op save moved timer")
	}
	if err := s.RunDue(now.Add(29*time.Minute), start); err != nil || calls != 0 {
		t.Fatal("started early")
	}
	s = NewSchedule(path)
	late := now.Add(24 * time.Hour)
	if err := s.RunDue(late, func() error {
		persisted := NewSchedule(path).Snapshot()
		next, _ := time.Parse(time.RFC3339Nano, persisted.NextRunAt)
		if !next.After(late) || persisted.LastResult != "pending" {
			t.Fatal("attempt not claimed before start")
		}
		return start()
	}); err != nil {
		t.Fatal(err)
	}
	s = NewSchedule(path)
	if err := s.RunDue(late, start); err != nil || calls != 1 {
		t.Fatal("replayed missed slots")
	}
	if s.Snapshot().LastResult != "started" || s.Snapshot().LastStartedAt == "" {
		t.Fatal("missing outcome")
	}
	if err := s.Configure(true, 60, late); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDue(late.Add(30*time.Minute), start); err != nil || calls != 1 {
		t.Fatal("interval change not applied")
	}
	if err := s.Configure(false, 60, late); err != nil {
		t.Fatal(err)
	}
	if err := NewSchedule(path).RunDue(late.Add(2*time.Hour), start); err != nil || calls != 1 {
		t.Fatal("disabled timer fired")
	}
}

func TestScheduleSkipsAndFailuresWaitForNextInterval(t *testing.T) {
	now := time.Now().UTC()
	for _, code := range []string{"already_running", "maintenance", "openai_target_required", "history_full_archive_completed_jobs_first"} {
		t.Run(code, func(t *testing.T) {
			s := NewSchedule(filepath.Join(t.TempDir(), "schedule.json"))
			if err := s.Configure(true, 1, now); err != nil {
				t.Fatal(err)
			}
			calls := 0
			start := func() error { calls++; return fail("task", code) }
			due := now.Add(time.Minute)
			if err := s.RunDue(due, start); err != nil {
				t.Fatal(err)
			}
			state := s.Snapshot()
			want := "skipped"
			if strings.HasPrefix(code, "history_full") {
				want = "failed"
			}
			if state.LastResult != want || !strings.Contains(state.LastError, code) {
				t.Fatalf("wrong outcome: %+v", state)
			}
			if err := s.RunDue(due.Add(time.Second), start); err != nil || calls != 1 {
				t.Fatal("retried before interval")
			}
		})
	}
	s := NewSchedule(filepath.Join(t.TempDir(), "schedule.json"))
	_ = s.Configure(true, 1, now)
	_ = s.RunDue(now.Add(time.Minute), func() error { return errors.New("PRIVATE-PASSWORD") })
	if strings.Contains(s.Snapshot().LastError, "PRIVATE") {
		t.Fatal("unsafe error leaked")
	}
}

func TestScheduleConcurrentTicksStartOnce(t *testing.T) {
	s := NewSchedule(filepath.Join(t.TempDir(), "schedule.json"))
	now := time.Now()
	if err := s.Configure(true, 1, now); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.RunDue(now.Add(time.Minute), func() error { calls.Add(1); return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate starts: %d", calls.Load())
	}
}

func TestScheduleStorageFailureNeverStartsOrOverwritesCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	now := time.Now()
	s := NewSchedule(path)
	for _, minutes := range []int{0, -1, 10081} {
		if err := s.Configure(true, minutes, now); err == nil {
			t.Fatal("accepted invalid interval")
		}
	}
	if err := s.Configure(true, 1, now); err != nil {
		t.Fatal(err)
	}
	s.path = t.TempDir() // Atomic rename cannot replace this directory.
	called := false
	if err := s.RunDue(now.Add(time.Minute), func() error { called = true; return nil }); err == nil || called {
		t.Fatal("started without durable claim")
	}
	if !strings.Contains(s.Snapshot().LastError, "write_failed") {
		t.Fatal("missing storage failure")
	}
	corrupt := []byte("{broken")
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	s = NewSchedule(path)
	if s.Snapshot().Enabled || s.Configure(true, 1, now) == nil {
		t.Fatal("accepted unreadable state")
	}
	_ = s.RunDue(now, func() error { called = true; return nil })
	after, _ := os.ReadFile(path)
	if string(after) != string(corrupt) || called {
		t.Fatal("corrupt state lost or work started")
	}
}
