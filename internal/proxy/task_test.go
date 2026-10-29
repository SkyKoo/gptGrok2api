package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func taskGroups() []GroupConfig {
	return []GroupConfig{{ID: "shared", Enabled: true, Nodes: []NodeConfig{
		{ID: "a", URL: "http://a.invalid:8080", Enabled: true}, {ID: "b", URL: "http://b.invalid:8080", Enabled: true},
	}}}
}
func taskManager() *Manager {
	m := NewManager("http://fallback.invalid:8080", nil)
	m.ConfigureTaskGroups(taskGroups())
	return m
}
func acquireTest(t *testing.T, m *Manager, selection, platform string, saved *Binding) *TaskLease {
	t.Helper()
	l, e := m.AcquireTask(context.Background(), selection, platform, "registration", saved)
	if e != nil {
		t.Fatal(e)
	}
	return l
}

func TestTasksSharePoolAndPersistAffinityAcrossRestart(t *testing.T) {
	m := taskManager()
	a := acquireTest(t, m, "group:shared", "openai", nil)
	b := acquireTest(t, m, "group:shared", "grok", nil)
	if a.Binding.NodeID != "a" || b.Binding.NodeID != "b" {
		t.Fatal("tasks did not share round-robin cursor")
	}
	a.Release()
	b.Release()
	restored := taskManager()
	retry := acquireTest(t, restored, "group:nonexistent", "openai", &a.Binding)
	defer retry.Release()
	if retry.URL != a.URL {
		t.Fatal("retry changed proxy")
	}
	fields := map[string]any{"proxy_binding": a.Binding, "proxy": "http://other.invalid:8080"}
	for _, resource := range []bool{false, true} {
		if got := restored.Resolve(fields, resource); got != a.URL {
			t.Fatal("account route changed", got)
		}
	}
	image := restored.AcquireImage(fields)
	defer image.Release(false)
	if image.URL != a.URL {
		t.Fatal("image route changed")
	}
	if stable := restored.AcquireStableImage(fields, a.URL); stable != nil {
		t.Fatal("bound account received alternate node")
	}
}
func TestTaskCapacityCancellationAndRefresh(t *testing.T) {
	m := taskManager()
	a := acquireTest(t, m, "node:shared/a", "openai", nil)
	m.ConfigureTaskGroups(taskGroups())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, e := m.AcquireTask(ctx, "node:shared/a", "grok", "registration", nil); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("capacity lost during refresh", e)
	}
	done := make(chan *TaskLease, 1)
	go func() {
		l, _ := m.AcquireTask(context.Background(), "node:shared/a", "grok", "registration", nil)
		done <- l
	}()
	a.Release()
	a.Release()
	select {
	case l := <-done:
		if l == nil {
			t.Fatal("waiter failed")
		}
		l.Release()
	case <-time.After(time.Second):
		t.Fatal("waiter stuck")
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, e := m.AcquireTask(cancelled, "group:shared", "grok", "registration", nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestBindingsFailClosedForDisabledDeletedChangedNodes(t *testing.T) {
	for _, mode := range []string{"disabled", "deleted", "changed"} {
		t.Run(mode, func(t *testing.T) {
			m := taskManager()
			l := acquireTest(t, m, "node:shared/a", "grok", nil)
			l.Release()
			groups := taskGroups()
			switch mode {
			case "disabled":
				groups[0].Nodes[0].Enabled = false
			case "deleted":
				groups[0].Nodes = groups[0].Nodes[1:]
			case "changed":
				groups[0].Nodes[0].URL = "http://changed.invalid:8080"
			}
			m.ConfigureTaskGroups(groups)
			if _, e := m.AcquireTask(context.Background(), "group:shared", "grok", "registration", &l.Binding); !errors.Is(e, ErrBindingUnavailable) {
				t.Fatal(e)
			}
			fields := map[string]any{"proxy_binding": l.Binding}
			if got := m.Resolve(fields, false); got != blockedProxyURL {
				t.Fatal("fallback used", got)
			}
			if got := m.AcquireImage(fields); got.URL != blockedProxyURL {
				t.Fatal("image fallback used")
			}
		})
	}
	m := taskManager()
	for _, s := range []string{"group:missing", "node:missing/a", "node:broken"} {
		if _, e := m.AcquireTask(context.Background(), s, "grok", "registration", nil); e == nil {
			t.Fatal("unknown reference accepted")
		}
	}
}
func TestTaskCooldownScopedAndRestored(t *testing.T) {
	m := taskManager()
	a := acquireTest(t, m, "node:shared/a", "grok", nil)
	a.Release()
	a.Binding.BlockedUntil = time.Now().Add(time.Minute)
	m.BlockTaskScope(a.Binding, a.Binding.BlockedUntil)
	if _, e := m.AcquireTask(context.Background(), "node:shared/a", "grok", "registration", nil); e == nil {
		t.Fatal("blocked node admitted")
	}
	other := acquireTest(t, m, "node:shared/a", "openai", nil)
	other.Release()
	if got := m.Resolve(map[string]any{"proxy_binding": a.Binding}, false); got != a.URL {
		t.Fatal("signup restriction blocked normal account use")
	}
	restarted := taskManager()
	if _, e := restarted.AcquireTask(context.Background(), "group:shared", "grok", "registration", &a.Binding); e == nil {
		t.Fatal("saved restriction ignored")
	}
}
func TestTaskDefaultBindingAndDirect(t *testing.T) {
	m := NewManager("", []string{"http://a.invalid:8080", "http://b.invalid:8080"})
	l := acquireTest(t, m, "", "openai", nil)
	other := acquireTest(t, m, "", "openai", nil)
	if l.URL == other.URL {
		t.Fatal("default pool did not rotate")
	}
	retry := acquireTest(t, m, "direct", "openai", &l.Binding)
	if retry.URL != l.URL {
		t.Fatal("default binding lost")
	}
	m.SetDefault("http://new.invalid:8080", nil)
	if _, _, e := m.ResolveBound(map[string]any{"proxy_binding": l.Binding}); e == nil {
		t.Fatal("removed default fell back")
	}
	direct := acquireTest(t, m, "direct", "openai", nil)
	if direct.URL != "" || m.Resolve(map[string]any{"proxy_binding": direct.Binding}, true) != "" {
		t.Fatal("direct ignored")
	}
}
func TestBoundTransportRoutesAndFailsWithoutContactingFallback(t *testing.T) {
	var hit atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Add(1); w.WriteHeader(204) }))
	defer endpoint.Close()
	m := NewManager(endpoint.URL, nil)
	g := taskGroups()
	g[0].Nodes[0].URL = endpoint.URL
	m.ConfigureTaskGroups(g)
	l := acquireTest(t, m, "node:shared/a", "grok", nil)
	l.Release()
	client := &http.Client{Transport: NewTransport(nil)}
	req, _ := http.NewRequest("GET", "http://upstream.invalid/test", nil)
	req = req.WithContext(WithURL(req.Context(), m.Resolve(map[string]any{"proxy_binding": l.Binding}, false)))
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if hit.Load() != 1 {
		t.Fatal("proxy not used")
	}
	m.ConfigureTaskGroups(nil)
	req = req.WithContext(WithURL(req.Context(), m.Resolve(map[string]any{"proxy_binding": l.Binding}, false)))
	if res, e = client.Do(req); e == nil {
		res.Body.Close()
		t.Fatal("invalid binding allowed request")
	}
	if hit.Load() != 1 {
		t.Fatal("fallback contacted")
	}
}

func TestOptionalProxyModesAcrossRegistrationChatAndImages(t *testing.T) {
	for _, mode := range []string{"direct", "group:shared", "http://single.invalid:8080"} {
		t.Run(mode, func(t *testing.T) {
			m := taskManager()
			m.SetDefault(mode, nil)
			m.ConfigureImageGroups("", taskGroups())
			l := acquireTest(t, m, "", "openai", nil)
			defer l.Release()
			chat := m.Resolve(nil, false)
			image, e := m.AcquireImageContext(context.Background(), nil)
			if e != nil {
				t.Fatal(e)
			}
			defer image.Release(false)
			switch mode {
			case "direct":
				if l.URL != "" || chat != "" || image.URL != "" {
					t.Fatal("direct used proxy")
				}
			case "group:shared":
				if l.Binding.GroupID != "shared" || chat == "" || image.GroupID != "shared" {
					t.Fatal("group mode was not applied")
				}
			default:
				if l.URL != mode || chat != mode || image.URL != mode {
					t.Fatal("single proxy not honored")
				}
			}
			direct := map[string]any{"proxy": "direct"}
			if m.Resolve(direct, false) != "" || m.AcquireImage(direct).URL != "" {
				t.Fatal("explicit account direct overridden by default")
			}
			fixed := acquireTest(t, m, "direct", "grok", nil)
			if fixed.URL != "" {
				t.Fatal("explicit signup direct overridden")
			}
		})
	}
	m := taskManager()
	if m.Resolve(map[string]any{"proxy": "group:shared"}, false) != "http://a.invalid:8080" {
		t.Fatal("chat account group ignored")
	}
}
func TestDirectTransportIgnoresEnvironmentProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer upstream.Close()
	unwanted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyCalls.Add(1); w.WriteHeader(502) }))
	defer unwanted.Close()
	base := http.DefaultTransport.(*http.Transport).Clone()
	u, _ := url.Parse(unwanted.URL)
	base.Proxy = http.ProxyURL(u)
	client := &http.Client{Transport: NewTransport(base)}
	req, _ := http.NewRequest("GET", upstream.URL, nil)
	req = req.WithContext(WithURL(req.Context(), ""))
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 204 || proxyCalls.Load() != 0 {
		t.Fatal("direct request used inherited proxy")
	}
}

func TestExplicitMissingGroupNeverBecomesDirect(t *testing.T) {
	m := taskManager()
	m.SetDefault("group:missing", nil)
	if _, err := m.AcquireTask(context.Background(), "", "grok", "registration", nil); err == nil {
		t.Fatal("signup lost missing group")
	}
	if m.Resolve(nil, false) != blockedProxyURL {
		t.Fatal("chat lost missing group")
	}
	if _, err := m.AcquireImageContext(context.Background(), nil); err == nil {
		t.Fatal("image silently used another route")
	}
	if m.AcquireImage(nil).URL != blockedProxyURL {
		t.Fatal("image fell back")
	}
}
