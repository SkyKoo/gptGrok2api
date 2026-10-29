package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func resinManager(raw string) *Manager {
	m := NewManager("group:resin", nil)
	m.ConfigureTaskGroups([]GroupConfig{{ID: "resin", Enabled: true, Nodes: []NodeConfig{{ID: "gateway", URL: raw, ProxyType: "resin", Enabled: true, TaskConcurrencyLimit: 5}}}})
	return m
}

func TestResinTaskAffinitySurvivesRestartAndAccountUse(t *testing.T) {
	base := "http://CFM-Grok:secret%40token@resin.invalid:2260"
	m := resinManager(base)
	first, err := m.AcquireTask(context.Background(), "group:resin", "grok", "registration", nil, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	second, err := m.AcquireTask(context.Background(), "group:resin", "grok", "registration", nil, "job-2")
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	if first.URL == second.URL || first.Binding.ResinAccount == "" {
		t.Fatal("tasks share an identity")
	}
	raw, _ := json.Marshal(first.Binding)
	if strings.Contains(string(raw), "secret") || strings.Contains(first.Binding.Label(), "secret") {
		t.Fatal("binding leaks credentials")
	}
	var saved Binding
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	restored := resinManager(base)
	retry, err := restored.AcquireTask(context.Background(), "direct", "grok", "registration", &saved, "different-input-id")
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Release()
	if retry.URL != first.URL {
		t.Fatal("restart/retry changed identity")
	}
	fields := map[string]any{"proxy_binding": saved}
	for _, resource := range []bool{false, true} {
		if restored.Resolve(fields, resource) != first.URL {
			t.Fatal("account request changed identity")
		}
	}
	image := restored.AcquireImage(fields)
	defer image.Release(false)
	if image.URL != first.URL {
		t.Fatal("account image changed identity")
	}
	parsed, _ := url.Parse(first.URL)
	token, _ := parsed.User.Password()
	if token != "secret@token" || parsed.User.Username() != "CFM-Grok."+saved.ResinAccount {
		t.Fatal("invalid Resin auth")
	}
	// The same task on another business platform cannot collide.
	other, err := m.AcquireTask(context.Background(), "group:resin", "openai", "registration", nil, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if other.Binding.ResinAccount == saved.ResinAccount {
		t.Fatal("cross-platform collision")
	}
}

func TestResinNewConnectionsKeepProxyAuthorization(t *testing.T) {
	auth := make(chan string, 3)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("expected CONNECT")
		}
		auth <- r.Header.Get("Proxy-Authorization")
		w.WriteHeader(200)
	}))
	defer gateway.Close()
	endpoint, _ := url.Parse(gateway.URL)
	endpoint.User = url.UserPassword("CFM-Grok", "fixture-token")
	m := resinManager(endpoint.String())
	lease, err := m.AcquireTask(context.Background(), "group:resin", "grok", "registration", nil, "signup-1")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	for _, host := range []string{"send.invalid:443", "verify.invalid:443", "profile.invalid:443"} {
		conn, err := DialContext(context.Background(), host, lease.URL)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("CFM-Grok."+lease.Binding.ResinAccount+":fixture-token"))
		if got := <-auth; got != expected {
			t.Fatal("connection changed identity")
		}
	}
}

func TestResinBindingFailsClosedWhenConfigurationChanges(t *testing.T) {
	base := "http://CFM-Grok:fixture-token@resin.invalid:2260"
	m := resinManager(base)
	lease, err := m.AcquireTask(context.Background(), "group:resin", "grok", "registration", nil, "job")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	for _, node := range []NodeConfig{
		{ID: "gateway", URL: base, Enabled: true},
		{ID: "gateway", URL: "http://Other:fixture-token@resin.invalid:2260", ProxyType: "resin", Enabled: true},
		{ID: "gateway", URL: base, ProxyType: "resin", Enabled: false},
	} {
		m.ConfigureTaskGroups([]GroupConfig{{ID: "resin", Enabled: true, Nodes: []NodeConfig{node}}})
		if _, err := m.AcquireTask(context.Background(), "direct", "grok", "registration", &lease.Binding, "job"); err == nil {
			t.Fatal("retry changed route")
		}
		if _, _, err := m.ResolveBound(map[string]any{"proxy_binding": lease.Binding}); err == nil {
			t.Fatal("account changed route")
		}
	}
}

func TestResinIsExplicitAndValidated(t *testing.T) {
	for _, raw := range []string{"http://resin.invalid:2260", "http://P.Account:token@resin.invalid", "socks5://P:token@resin.invalid", "http://P:token@resin.invalid/path", "http://P:@resin.invalid"} {
		if ValidateNodeType("resin", raw) == nil {
			t.Fatalf("accepted invalid Resin configuration: %s", raw)
		}
	}
	m := NewManager("", nil)
	raw := "http://P:token@resin.invalid:2260"
	m.ConfigureTaskGroups([]GroupConfig{{ID: "ordinary", Enabled: true, Nodes: []NodeConfig{{ID: "a", URL: raw, Enabled: true}}}})
	lease, err := m.AcquireTask(context.Background(), "group:ordinary", "grok", "registration", nil, "job")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.URL != raw || lease.Binding.ResinAccount != "" {
		t.Fatal("ordinary proxy modified")
	}
	direct, err := m.AcquireTask(context.Background(), "direct", "grok", "registration", nil, "job")
	if err != nil || direct.URL != "" || direct.Binding.ResinAccount != "" {
		t.Fatal("direct modified")
	}
}

func TestLegacyProxyTypeNormalizationPreservesCapacity(t *testing.T) {
	m := taskManager()
	lease := acquireTest(t, m, "node:shared/a", "grok", nil)
	defer lease.Release()
	groups := taskGroups()
	groups[0].Nodes[0].ProxyType = "standard"
	m.ConfigureTaskGroups(groups)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if next, err := m.AcquireTask(ctx, "node:shared/a", "grok", "registration", nil); !errors.Is(err, context.DeadlineExceeded) {
		if next != nil {
			next.Release()
		}
		t.Fatal("explicit standard type reset live task capacity", err)
	}
}
