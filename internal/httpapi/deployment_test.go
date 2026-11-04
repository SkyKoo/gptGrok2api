package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestDeploymentPausePreservesUserSettingsAndLetsRequestsFinish(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AuthKeysPath = filepath.Join(root, "keys.json")
	cfg.RegisterPath = filepath.Join(root, "register.json")
	cfg.GrokAccountsPath = filepath.Join(root, "grok_accounts.json")
	cfg.QueuePath = filepath.Join(root, "tasks.json")
	s := NewWithWorkers(cfg, false)
	defer s.ShutdownRegistration(context.Background())
	if _, err := s.registerStore.Update(map[string]any{"openai_survival": map[string]any{"enabled": true}, "grok_probe_scheduler": map[string]any{"enabled": true}}); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.registerStore.Get())
	s.PauseDeployment(false)
	if !s.beginDeploymentBackground() {
		t.Fatal("background not admitted")
	}
	s.PauseDeployment(true)
	if s.beginDeploymentBackground() {
		t.Fatal("new timer admitted while paused")
	}
	if s.DeploymentStatus()["background"] != 1 {
		t.Fatal("existing background work lost")
	}
	s.endDeploymentBackground()
	after, _ := json.Marshal(s.registerStore.Get())
	if string(before) != string(after) {
		t.Fatal("deployment rewrote user schedule settings")
	}
	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatal("paused workers blocked business HTTP", res.Code)
	}
}

func TestDeploymentSealWaitsForDetachedWorkAndIsIrreversible(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = root
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AuthKeysPath = filepath.Join(root, "keys.json")
	cfg.RegisterPath = filepath.Join(root, "register.json")
	cfg.GrokAccountsPath = filepath.Join(root, "grok_accounts.json")
	cfg.QueuePath = filepath.Join(root, "tasks.json")
	s := NewWithWorkers(cfg, false)
	release := make(chan struct{})
	s.launchDeploymentWork(func() { <-release })
	if s.SealDeployment(context.Background()) == nil {
		t.Fatal("detached work sealed")
	}
	close(release)
	end := time.Now().Add(time.Second)
	for s.DeploymentStatus()["detached"] != 0 && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	if err := s.SealDeployment(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.PauseDeployment(false)
	if s.beginDeploymentBackground() {
		t.Fatal("retired runtime restarted timer")
	}
}
