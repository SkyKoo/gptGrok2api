package register

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScopedMailboxSourceMigrationAndRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "register.json")
	if err := os.WriteFile(path, []byte(`{"target":"grok","mail":{"source_id":"legacy-mail"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(path, filepath.Join(dir, "accounts.json"))
	for _, target := range []string{"openai", "grok"} {
		if object(s.ConfigForTarget(target)["mail"])["source_id"] != "legacy-mail" {
			t.Fatal("legacy source lost", target)
		}
	}
	if _, err := s.UpdateForTarget("openai", map[string]any{"mail": map[string]any{"source_id": "openai-mail"}, "total": 6}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateForTarget("grok", map[string]any{"mail": map[string]any{"source_id": "grok-mail"}, "total": 2}); err != nil {
		t.Fatal(err)
	}
	s = New(path, filepath.Join(dir, "accounts.json"))
	for _, target := range []string{"openai", "grok"} {
		if object(s.ConfigForTarget(target)["mail"])["source_id"] != target+"-mail" {
			t.Fatal("source did not persist", target)
		}
	}
	if s.Get()["target"] != "grok" {
		t.Fatal("scoped write changed default target")
	}
	// Empty is meaningful: clearing one channel's selection must not restore the old ID.
	if _, err := s.UpdateForTarget("openai", map[string]any{"mail": map[string]any{"source_id": ""}}); err != nil {
		t.Fatal(err)
	}
	if object(s.ConfigForTarget("openai")["mail"])["source_id"] != "" || object(s.ConfigForTarget("grok")["mail"])["source_id"] != "grok-mail" {
		t.Fatal("empty source incorrectly inherited")
	}
}
func TestScheduleOwnerMigrationPersistsWithoutResettingTimer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	s := NewSchedule(path)
	if err := s.Configure(true, 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	if err := s.BindTarget("grok"); err != nil {
		t.Fatal(err)
	}
	s = NewSchedule(path)
	if err := s.BindTarget("openai"); err != nil {
		t.Fatal(err)
	}
	after := s.Snapshot()
	if after.Target != "grok" || after.NextRunAt != before.NextRunAt || after.Enabled != before.Enabled {
		t.Fatal("migration redirected/reset old timer")
	}
}
