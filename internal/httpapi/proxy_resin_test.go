package httpapi

import (
	"context"
	"github.com/auucoder/gptgrok2api-go/internal/auth"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"github.com/auucoder/gptgrok2api-go/internal/store"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestResinGroupConfigurationPersistsAndReachesRuntime(t *testing.T) {
	for _, key := range []string{"GO_PROXY_URL", "GO_PROXY_POOL", "GO_RESOURCE_PROXY_URL", "GO_RESOURCE_PROXY_POOL"} {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	s := &Server{auth: auth.New("", "admin-secret", "", false, repository), cfg: testConfig(), store: repository, proxyManager: proxyruntime.NewManager("", nil)}
	call := func(body string, want int) {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/proxy/groups", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		res := httptest.NewRecorder()
		s.proxyGroups(res, req)
		if res.Code != want {
			t.Fatalf("got %d want %d: %s", res.Code, want, res.Body.String())
		}
	}
	call(`{"id":"resin","enabled":true,"nodes":[{"id":"one","enabled":true,"proxy_type":"resin","url":"http://CFM:fixture-token@resin.invalid:2260"}]}`, 200)
	// A fresh manager loads persisted type metadata via config parsing.
	s.proxyManager = proxyruntime.NewManager("", nil)
	if err := s.refreshProxyRuntime(); err != nil {
		t.Fatal(err)
	}
	lease, err := s.proxyManager.AcquireTask(context.Background(), "group:resin", "grok", "registration", nil, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if !strings.Contains(lease.URL, "CFM.task-") || lease.Binding.ResinPlatform != "CFM" {
		t.Fatal("Resin metadata was lost")
	}
	call(`{"id":"resin","nodes":[{"id":"one","proxy_type":"resin","url":"http://resin.invalid:2260"}]}`, 400)
	call(`{"id":"resin","nodes":[{"id":"one","proxy_type":"unknown","url":"http://resin.invalid:2260"}]}`, 400)
	retry, err := s.proxyManager.AcquireTask(context.Background(), "direct", "grok", "registration", &lease.Binding, "job-1")
	if err != nil {
		t.Fatal("invalid update replaced working config", err)
	}
	retry.Release()
	if retry.URL != lease.URL {
		t.Fatal("invalid update changed binding")
	}
}

func TestRegistrationResinChannelPersistsAcrossRestartWithoutGroup(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	handler := s.Handler()
	response := adminRequest(handler, "POST", "/api/register", strings.NewReader(`{"proxy":"resin:registration","resin_proxy_url":"http://P:fixture-token@resin.invalid:2260","openai_survival":{"enabled":false},"grok_probe_scheduler":{"enabled":false}}`))
	if response.Code != 200 {
		t.Fatalf("save channel: %d", response.Code)
	}
	lease, err := s.proxyManager.AcquireTask(context.Background(), proxyruntime.RegistrationResinChannel, "grok", "registration", nil, "job-one")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if lease.Binding.ResinAccount == "" || lease.Binding.GroupID != "" {
		t.Fatal("channel required a group")
	}
	if err := s.ShutdownRegistration(context.Background()); err != nil {
		t.Fatal(err)
	}
	s = New(cfg)
	defer s.ShutdownRegistration(context.Background())
	// Choosing direct for new jobs preserves the channel for existing accounts.
	response = adminRequest(s.Handler(), "POST", "/api/register", strings.NewReader(`{"proxy":"direct"}`))
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	again, err := s.proxyManager.AcquireTask(context.Background(), "direct", "grok", "registration", &lease.Binding, "job-one")
	if err != nil || again.URL != lease.URL {
		t.Fatal("restarted channel lost account", err)
	}
	got, bound, err := s.proxyManager.ResolveBound(map[string]any{"proxy_binding": lease.Binding})
	if err != nil || !bound || got != lease.URL {
		t.Fatal("account lost saved channel", err)
	}
	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest("GET", "/api/register", nil))
	if unauth.Code != 401 || strings.Contains(unauth.Body.String(), "fixture-token") {
		t.Fatal("channel config exposed")
	}
}
