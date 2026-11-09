package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestGrokSweepChecksSSOAndOAuthWithoutGeneration(t *testing.T) {
	var ssoCalls, billingCalls, status atomic.Int32
	status.Store(401)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/rate-limits":
			ssoCalls.Add(1)
			w.WriteHeader(int(status.Load()))
			if status.Load() == 200 {
				w.Write([]byte(`{"remainingQueries":0,"totalQueries":20}`))
			}
		case "/billing":
			billingCalls.Add(1)
			w.Write([]byte(`{"config":{"monthlyLimit":0,"used":0,"onDemandCap":0}}`))
		default:
			t.Errorf("periodic check made an unexpected request: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.GrokRateLimitsURL = upstream.URL + "/rest/rate-limits"
	s := New(cfg)
	s.xaiProbe = provider.NewXAIProbe(upstream.URL, upstream.URL+"/token", upstream.Client())
	_, _, _, _ = s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "sweep-sso", "status": "active", "enabled": true, "media_quota": testGrokMediaSnapshot()}, {"access_token": "manual-disabled", "status": "disabled", "enabled": false}})
	_, _ = s.oauthStore.Import(oauth.Account{ID: "oauth-check", AccessToken: "oauth-token", Status: "active"})
	_, _ = s.registerStore.SetGrokProbeSchedulerEnabled(true)
	s.runGrokProbeIfDue(time.Now())
	config := s.registerStore.GrokProbeSchedulerStatus()
	finished, err := time.Parse(time.RFC3339, stringValue(config["last_finished_at"]))
	if err != nil || stringValue(config["error"]) == "" || ssoCalls.Load() != 1 || billingCalls.Load() != 1 {
		t.Fatalf("first sweep: %+v sso=%d oauth=%d", config, ssoCalls.Load(), billingCalls.Load())
	}
	next, _ := time.Parse(time.RFC3339, stringValue(config["next_run_at"]))
	if !next.Equal(finished.Add(time.Hour)) {
		t.Fatal("deadline not anchored to completion")
	}
	s.runGrokProbeIfDue(finished.Add(30 * time.Second))
	again := s.registerStore.GrokProbeSchedulerStatus()
	if again["last_finished_at"] != config["last_finished_at"] || again["next_run_at"] != config["next_run_at"] || again["error"] != config["error"] || ssoCalls.Load() != 1 {
		t.Fatal("scheduler overwrote completion or repeated too soon")
	}
	if _, err := s.grokAccountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: "image"}); !errors.Is(err, accounts.ErrUnavailable) {
		t.Fatal("invalid credentials still leased")
	}
	status.Store(200)
	_ = s.registerStore.UpdateGrokProbeSchedulerRuntime(false, "", time.Now().Add(-2*time.Hour).Format(time.RFC3339), "")
	s.runGrokProbeIfDue(time.Now())
	if ssoCalls.Load() != 2 {
		t.Fatal("abnormal SSO was not rechecked")
	}
	lease, err := s.grokAccountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: "video", Model: "grok-imagine-video"})
	if err != nil {
		t.Fatal("explicit verification did not restore valid media account", err)
	}
	s.grokAccountPool.Release(lease)
	items, _ := s.oauthStore.List()
	if items[0].Status != "active" || items[0].Probe != nil || items[0].Quota["credential_status"] != "valid" {
		t.Fatal("zero billing invented model or media unavailability")
	}
	if s.registerStore.GrokProbeSchedulerStatus()["error"] != "" {
		t.Fatal("successful sweep retained old errors")
	}
}

func TestGrokCredentialCheckCannotUndoConcurrentDisable(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-finish
		w.Write([]byte(`{"remainingQueries":10,"totalQueries":20}`))
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.GrokRateLimitsURL = upstream.URL
	s := New(cfg)
	_, _, _, _ = s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "race-sso", "status": "invalid", "enabled": true}})
	done := make(chan error, 1)
	go func() { _, _, err := s.verifyGrokCredential(context.Background(), "race-sso", nil); done <- err }()
	<-started
	_, _, _ = s.grokRuntimeStore.UpdateAccount("race-sso", map[string]any{"enabled": false, "status": "disabled"})
	close(finish)
	if err := <-done; !errors.Is(err, accounts.ErrRefreshDeferred) {
		t.Fatal("stale check was accepted", err)
	}
	rows, _, _ := s.grokRuntimeStore.AccountSnapshot()
	if rows[0]["status"] != "disabled" || rows[0]["grok_credential_check"] != nil {
		t.Fatal("concurrent disable overwritten")
	}
}
