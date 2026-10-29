package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func routingFixture() *ProxyRouting {
	m := proxyruntime.NewManager("http://fallback.invalid:8080", nil)
	m.ConfigureTaskGroups([]proxyruntime.GroupConfig{{ID: "shared", Enabled: true, Nodes: []proxyruntime.NodeConfig{{ID: "a", URL: "http://a.invalid:8080", Enabled: true}, {ID: "b", URL: "http://b.invalid:8080", Enabled: true}}}})
	return &ProxyRouting{Manager: m}
}

type routedFreeFlow struct {
	t        *testing.T
	selected string
	get      func() RegistrationJob
}

func (f *routedFreeFlow) Close() {}
func (f *routedFreeFlow) Register(ctx context.Context, b Mailbox, c FreeConfig, w func(context.Context, time.Time) (string, error), p Progress) (map[string]any, error) {
	j := f.get()
	if c.Proxy != f.selected || j.ProxyBinding == nil {
		f.t.Error("flow not using journaled factory route")
	}
	return map[string]any{"email": b.Email, "access_token": "test-token"}, nil
}

type routedGrokFlow struct {
	t        *testing.T
	selected string
	get      func() RegistrationJob
}

func (f *routedGrokFlow) Close() {}
func (f *routedGrokFlow) Register(ctx context.Context, b Mailbox, c GrokConfig, w func(context.Context, time.Time) (string, error), p Progress) (map[string]any, error) {
	j := f.get()
	if c.Proxy != f.selected || j.ProxyBinding == nil {
		f.t.Error("Grok flow not using journaled factory route")
	}
	return map[string]any{"email": b.Email, "sso": "test-sso"}, nil
}
func TestRegistrationRoutesBothPlatformsThroughSharedPool(t *testing.T) {
	t.Run("pool", func(t *testing.T) { testRegistrationRoutesBothPlatforms(t, false) })
	t.Run("resin-channel", func(t *testing.T) { testRegistrationRoutesBothPlatforms(t, true) })
}
func testRegistrationRoutesBothPlatforms(t *testing.T, channel bool) {
	routing := routingFixture()
	selection := "group:shared"
	channelURL := "http://P:secret@resin.invalid:2260"
	if channel {
		selection = proxyruntime.RegistrationResinChannel
		routing = &ProxyRouting{Manager: proxyruntime.NewManager("", nil)}
		routing.Manager.ConfigureResinChannel(selection, channelURL)
	}
	mail := &fakeMail{}
	var free *FreeEngine
	free = NewFreeEngine(filepath.Join(t.TempDir(), "tasks.json"), func(c FreeConfig) (MailSource, RegistrationFlow, error) {
		if channel && !strings.Contains(c.Proxy, "P.task-") {
			t.Error("GPT factory missing Resin Account")
		}
		free.mu.Lock()
		j := free.jobs[len(free.jobs)-1]
		free.mu.Unlock()
		if j.ProxyBinding == nil || mail.calls.Load() != 0 {
			t.Error("binding not persisted before factory/mail")
		}
		return mail, &routedFreeFlow{t: t, selected: c.Proxy, get: func() RegistrationJob { v, _ := free.getJob(j.ID); return v }}, nil
	}, func(ctx context.Context, id string, a map[string]any) error {
		b := proxyruntime.ParseBinding(a["proxy_binding"])
		if b == nil || (channel && (b.Reference != selection || b.ResinAccount == "")) || (!channel && b.NodeID != "a") {
			t.Error("GPT import lost binding")
		}
		return nil
	}, nil)
	free.SetProxyRouting(routing)
	cfg := freeTestConfig()
	cfg["proxy"] = selection
	cfg["resin_proxy_url"] = channelURL
	id, e := free.Start(cfg)
	if e != nil {
		t.Fatal(e)
	}
	waitEngine(t, free)
	j, _ := free.getJob(id)
	if len(j.Account) == 0 {
		t.Fatal(j.Error)
	}
	var grok *GrokEngine
	grok = NewGrokEngine(filepath.Join(t.TempDir(), "tasks.json"), func(c GrokConfig) (MailSource, GrokRegistrationFlow, error) {
		if channel && !strings.Contains(c.Proxy, "P.task-") {
			t.Error("Grok factory missing Resin Account")
		}
		grok.mu.Lock()
		job := grok.jobs[len(grok.jobs)-1]
		grok.mu.Unlock()
		return &fakeMail{}, &routedGrokFlow{t: t, selected: c.Proxy, get: func() RegistrationJob { v, _ := grok.getJob(job.ID); return v }}, nil
	}, func(ctx context.Context, id string, a map[string]any) error {
		b := proxyruntime.ParseBinding(a["proxy_binding"])
		if b == nil || (channel && (b.Reference != selection || b.ResinAccount == "")) || (!channel && b.NodeID != "b") {
			t.Error("Grok import lost binding or shared pool cursor")
		}
		return nil
	}, nil)
	grok.SetProxyRouting(routing)
	gc := grokConfigFixture()
	gc["proxy"] = selection
	gc["resin_proxy_url"] = channelURL
	gc["total"] = 1
	gid, e := grok.Start(gc)
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for grok.Snapshot()["running"] == true && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	gj, _ := grok.getJob(gid)
	if len(gj.Account) == 0 {
		t.Fatal("Grok failed", gj.Error)
	}
}
func TestRoutingRecoveryUsesSavedBindingAndRejectsMissingNode(t *testing.T) {
	routing := routingFixture()
	job := RegistrationJob{ID: "job"}
	get := func(string) (RegistrationJob, error) { return job, nil }
	change := func(_ string, f func(*RegistrationJob)) error { f(&job); return nil }
	selection := "group:shared"
	release, e := routing.acquire(context.Background(), job.ID, "grok", &selection, get, change)
	if e != nil {
		t.Fatal(e)
	}
	release()
	first := selection
	routing = routingFixture()
	selection = "direct"
	release, e = routing.acquire(context.Background(), job.ID, "grok", &selection, get, change)
	if e != nil {
		t.Fatal(e)
	}
	release()
	if selection != first {
		t.Fatal("retry changed exit")
	}
	routing.Manager.ConfigureTaskGroups(nil)
	selection = "group:shared"
	if _, e = routing.acquire(context.Background(), job.ID, "grok", &selection, get, change); e == nil {
		t.Fatal("missing node allowed")
	}
}
func TestRoutingFailureReleasesLeaseAndRestoresScopedCooldown(t *testing.T) {
	routing := routingFixture()
	job := RegistrationJob{ID: "job"}
	get := func(string) (RegistrationJob, error) { return job, nil }
	selection := "group:shared"
	if _, e := routing.acquire(context.Background(), job.ID, "grok", &selection, get, func(string, func(*RegistrationJob)) error { return errors.New("disk failure") }); e == nil {
		t.Fatal("journal failure ignored")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lease, e := routing.Manager.AcquireTask(ctx, "node:shared/a", "grok", "registration", nil)
	if e != nil {
		t.Fatal("journal failure leaked capacity", e)
	}
	lease.Release()
	job.ProxyBinding = &lease.Binding
	routing.recordFailure(&job, fail("send_code", "email_signup_unavailable"))
	next := routingFixture()
	next.restore([]RegistrationJob{job})
	if _, e = next.Manager.AcquireTask(ctx, "node:shared/a", "grok", "registration", nil); e == nil {
		t.Fatal("cooldown not restored")
	}
	other, e := next.Manager.AcquireTask(ctx, "node:shared/a", "openai", "registration", nil)
	if e != nil {
		t.Fatal("unrelated platform blocked", e)
	}
	other.Release()
}

func TestSavedRegistrationEnginesKeepOriginalProxy(t *testing.T) {
	for _, platform := range []string{"openai", "grok"} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%v", platform, missing), func(t *testing.T) {
				routing := routingFixture()
				lease, err := routing.Manager.AcquireTask(context.Background(), "node:shared/a", platform, "registration", nil)
				if err != nil {
					t.Fatal(err)
				}
				lease.Release()
				profile := browserProfileBase(1440, 900, 8, 4294967296)
				job := RegistrationJob{ID: "saved", Status: "registration_pending", Stage: "wait_code", ProxyBinding: &lease.Binding, BrowserProfile: &profile, Mailbox: Mailbox{Email: "existing@example.test", AccountID: "acc-test"}}
				path := filepath.Join(t.TempDir(), "tasks.json")
				raw, _ := json.Marshal([]RegistrationJob{job})
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				routing = routingFixture()
				if missing {
					routing.Manager.ConfigureTaskGroups(nil)
				}
				calls := 0
				mail := &grokSavedMailStub{}
				var get func(string) (RegistrationJob, error)
				var snapshot func() map[string]any
				if platform == "openai" {
					engine := NewFreeEngine(path, func(cfg FreeConfig) (MailSource, RegistrationFlow, error) {
						calls++
						if cfg.Proxy != lease.URL {
							t.Error("compensation changed route")
						}
						return mail, &fakeFlow{}, nil
					}, nil, nil)
					engine.SetProxyRouting(routing)
					cfg := freeTestConfig()
					cfg["proxy"] = "direct"
					err = engine.RetryRegistration(cfg, job.ID)
					get = engine.getJob
					snapshot = engine.Snapshot
				} else {
					job.Mailbox.AccountID = "hme-account"
					raw, _ = json.Marshal([]RegistrationJob{job})
					if err = os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
					engine := NewGrokEngine(path, func(cfg GrokConfig) (MailSource, GrokRegistrationFlow, error) {
						calls++
						if cfg.Proxy != lease.URL {
							t.Error("saved Grok route changed")
						}
						return mail, &grokSavedFlowStub{}, nil
					}, nil, nil)
					engine.SetProxyRouting(routing)
					cfg := grokConfigFixture()
					cfg["proxy"] = "direct"
					err = engine.RetryRegistration(cfg, job.ID)
					get = engine.getJob
					snapshot = engine.Snapshot
				}
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(3 * time.Second)
				for snapshot()["running"] == true && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if snapshot()["running"] == true {
					t.Fatal("retry did not finish")
				}
				saved, _ := get(job.ID)
				if missing {
					if calls != 0 || mail.prepared.Load() != 0 || saved.Error != "proxy: binding_unavailable" {
						t.Fatal("missing binding allowed upstream work", saved.Error)
					}
				} else {
					if calls != 1 || mail.prepared.Load() != 1 || proxyruntime.ParseBinding(saved.Account["proxy_binding"]) == nil {
						t.Fatal("saved registration lost route or mailbox", saved.Error)
					}
				}
			})
		}
	}
}

func resinRoutingFixture() *ProxyRouting {
	m := proxyruntime.NewManager("", nil)
	m.ConfigureTaskGroups([]proxyruntime.GroupConfig{{ID: "shared", Enabled: true, Nodes: []proxyruntime.NodeConfig{{ID: "a", URL: "http://CFM:fixture-secret@resin.invalid:2260", ProxyType: "resin", Enabled: true}}}})
	return &ProxyRouting{Manager: m}
}

func TestSavedResinRegistrationEnginesKeepOriginalProxy(t *testing.T) {
	for _, channel := range []bool{false, true} {
		t.Run(fmt.Sprintf("channel=%v", channel), func(t *testing.T) { testSavedResinRegistrationEngines(t, channel) })
	}
}

func testSavedResinRegistrationEngines(t *testing.T, channel bool) {
	fixture := func() *ProxyRouting {
		if !channel {
			return resinRoutingFixture()
		}
		m := proxyruntime.NewManager("", nil)
		m.ConfigureResinChannel(proxyruntime.RegistrationResinChannel, "http://CFM:fixture-secret@resin.invalid:2260")
		return &ProxyRouting{Manager: m}
	}
	selection := "node:shared/a"
	if channel {
		selection = proxyruntime.RegistrationResinChannel
	}
	for _, platform := range []string{"openai", "grok"} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%v", platform, missing), func(t *testing.T) {
				routing := fixture()
				lease, err := routing.Manager.AcquireTask(context.Background(), selection, platform, "registration", nil)
				if err != nil {
					t.Fatal(err)
				}
				lease.Release()
				profile := browserProfileBase(1440, 900, 8, 4294967296)
				job := RegistrationJob{ID: "saved", Status: "registration_pending", Stage: "wait_code", ProxyBinding: &lease.Binding, BrowserProfile: &profile, Mailbox: Mailbox{Email: "existing@example.test", AccountID: "acc-test"}}
				path := filepath.Join(t.TempDir(), "tasks.json")
				raw, _ := json.Marshal([]RegistrationJob{job})
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				routing = fixture()
				if missing {
					routing.Manager.ConfigureTaskGroups(nil)
					routing.Manager.ConfigureResinChannel(proxyruntime.RegistrationResinChannel, "")
				}
				calls := 0
				mail := &grokSavedMailStub{}
				var get func(string) (RegistrationJob, error)
				var snapshot func() map[string]any
				if platform == "openai" {
					engine := NewFreeEngine(path, func(cfg FreeConfig) (MailSource, RegistrationFlow, error) {
						calls++
						if cfg.Proxy != lease.URL || !strings.Contains(cfg.Proxy, "CFM."+lease.Binding.ResinAccount+":") {
							t.Error("compensation changed route")
						}
						return mail, &fakeFlow{}, nil
					}, nil, nil)
					engine.SetProxyRouting(routing)
					cfg := freeTestConfig()
					cfg["proxy"] = "direct"
					err = engine.RetryRegistration(cfg, job.ID)
					get = engine.getJob
					snapshot = engine.Snapshot
				} else {
					job.Mailbox.AccountID = "hme-account"
					raw, _ = json.Marshal([]RegistrationJob{job})
					if err = os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
					engine := NewGrokEngine(path, func(cfg GrokConfig) (MailSource, GrokRegistrationFlow, error) {
						calls++
						if cfg.Proxy != lease.URL || !strings.Contains(cfg.Proxy, "CFM."+lease.Binding.ResinAccount+":") {
							t.Error("saved Grok route changed")
						}
						return mail, &grokSavedFlowStub{}, nil
					}, nil, nil)
					engine.SetProxyRouting(routing)
					cfg := grokConfigFixture()
					cfg["proxy"] = "direct"
					err = engine.RetryRegistration(cfg, job.ID)
					get = engine.getJob
					snapshot = engine.Snapshot
				}
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(3 * time.Second)
				for snapshot()["running"] == true && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if snapshot()["running"] == true {
					t.Fatal("retry did not finish")
				}
				saved, _ := get(job.ID)
				if missing {
					if calls != 0 || mail.prepared.Load() != 0 || saved.Error != "proxy: binding_unavailable" {
						t.Fatal("missing binding allowed upstream work", saved.Error)
					}
				} else {
					if calls != 1 || mail.prepared.Load() != 1 || proxyruntime.ParseBinding(saved.Account["proxy_binding"]) == nil || saved.ProxyBinding.ResinAccount != lease.Binding.ResinAccount {
						t.Fatal("saved registration lost route or mailbox", saved.Error)
					}
				}
			})
		}
	}
}

func TestResinTaskJournalIdentityAndSafeLogs(t *testing.T) {
	routing := resinRoutingFixture()
	job := RegistrationJob{ID: "task-specific-id"}
	get := func(string) (RegistrationJob, error) { return job, nil }
	change := func(_ string, f func(*RegistrationJob)) error { f(&job); return nil }
	selection := "group:shared"
	release, err := routing.acquire(context.Background(), job.ID, "grok", &selection, get, change)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if job.ProxyBinding == nil || job.ProxyBinding.ResinAccount == "" {
		t.Fatal("identity was not journaled")
	}
	original := selection
	encoded, _ := json.Marshal(job)
	if strings.Contains(string(encoded), "fixture-secret") || !strings.Contains(string(encoded), "注册代理身份已固定") {
		t.Fatal("unsafe or missing task identity log")
	}
	// A new engine/manager must reuse the journal even if user selected direct.
	routing = resinRoutingFixture()
	selection = "direct"
	release, err = routing.acquire(context.Background(), job.ID, "grok", &selection, get, change)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if selection != original {
		t.Fatal("retry changed identity")
	}
	encoded, _ = json.Marshal(job)
	if strings.Contains(string(encoded), "fixture-secret") || !strings.Contains(string(encoded), "复用任务原代理身份") {
		t.Fatal("unsafe or missing retry log")
	}
}
