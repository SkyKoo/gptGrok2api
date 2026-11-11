package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestAutomaticAccountRoutesFollowChangedDefaultOnTheWire(t *testing.T) {
	server := func(route string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, route)
		}))
	}
	upstream, first, second := server("direct"), server("first"), server("second")
	defer upstream.Close()
	defer first.Close()
	defer second.Close()
	cases := map[string]map[string]any{
		"registered direct":    {"proxy": "direct", "proxy_binding": Binding{Reference: "direct", Platform: "openai", Purpose: "registration"}},
		"registered old proxy": {"proxy": "http://old.invalid", "proxy_binding": Binding{Reference: "http://old.invalid", Platform: "openai", Purpose: "registration"}},
		"Grok import policy":   {"proxy": "direct", "source_type": "grok_protocol", "proxy_binding": Binding{Reference: "direct", Platform: "grok", Purpose: "account"}},
		"Grok old bulk policy": {"proxy": "direct", "proxy_binding": Binding{Reference: "direct", Platform: "grok", Purpose: "account"}, "proxy_binding_history": []any{map[string]any{"reason": legacyGrokDirectReason}}},
		"explicit inheritance": {"proxy_policy": AccountProxyInherit, "proxy": "direct", "proxy_url": "http://old.invalid", "proxyUrl": "http://older.invalid", "proxy_binding": "invalid old metadata"},
		"unconfigured import":  {},
	}
	client := &http.Client{Transport: NewTransport(http.DefaultTransport), Timeout: time.Second}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			before, _ := json.Marshal(fields)
			manager := NewManager(first.URL, nil)
			for _, route := range []struct{ url, body string }{{first.URL, "first"}, {second.URL, "second"}, {"direct", "direct"}} {
				manager.SetDefault(route.url, nil)
				image, err := manager.AcquireImageContext(context.Background(), fields)
				if err != nil {
					t.Fatal(err)
				}
				legacyImage := manager.AcquireImage(fields)
				urls := []string{manager.Resolve(fields, false), manager.Resolve(fields, true), image.URL, legacyImage.URL}
				for _, endpoint := range urls {
					req, _ := http.NewRequestWithContext(WithURL(context.Background(), endpoint), http.MethodGet, upstream.URL, nil)
					response, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil || string(body) != route.body {
						t.Fatalf("request took %q instead of %q: %v", body, route.body, err)
					}
				}
				image.Release(false)
				legacyImage.Release(false)
			}
			after, _ := json.Marshal(fields)
			if string(before) != string(after) {
				t.Fatal("runtime policy rewrote stored account metadata")
			}
		})
	}
}

func TestManualAndUnknownAccountRoutesRemainOverrides(t *testing.T) {
	binding := Binding{Reference: "direct", Platform: "grok", Purpose: "account"}
	for name, fields := range map[string]map[string]any{
		"explicit direct":            {"proxy_policy": AccountProxyExplicit, "proxy": "direct"},
		"imported direct":            {"proxy": "direct"},
		"unknown account binding":    {"proxy_binding": binding},
		"manual history":             {"source_type": "grok_protocol", "proxy_binding": binding, "proxy_binding_history": []any{map[string]any{"reason": "Keep this account direct"}}},
		"similar bulk reason":        {"source_type": "grok_protocol", "proxy_binding": binding, "proxy_binding_history": []any{map[string]any{"reason": legacyGrokDirectReason + " "}}},
		"manual history before bulk": {"proxy_binding": binding, "proxy_binding_history": []any{map[string]any{"reason": "Manual override"}, map[string]any{"reason": legacyGrokDirectReason}}},
		"malformed history":          {"source_type": "grok_protocol", "proxy_binding": binding, "proxy_binding_history": map[string]any{}},
		"explicit old registration":  {"proxy_policy": AccountProxyExplicit, "proxy_binding": Binding{Reference: "direct", Purpose: "registration"}},
		"unknown future policy":      {"proxy_policy": "future-policy", "proxy_binding": Binding{Reference: "direct", Purpose: "registration"}},
	} {
		t.Run(name, func(t *testing.T) {
			if AccountInheritsDefault(fields) || IsLegacyAutomaticAccountRoute(fields) {
				t.Fatal("manual or unknown route classified as automatic")
			}
			manager := NewManager("http://default.invalid", nil)
			if got := manager.Resolve(fields, false); got != "" {
				t.Fatal("explicit direct changed to", got)
			}
		})
	}
	manager := NewManager("http://default.invalid", nil)
	for _, fields := range []map[string]any{
		{"proxy_binding": map[string]any{"purpose": "registration"}},
		{"proxy_policy": AccountProxyExplicit, "proxy_binding": Binding{Reference: "node:deleted/node", Purpose: "registration"}},
	} {
		if manager.Resolve(fields, false) != blockedProxyURL {
			t.Fatal("unknown or unavailable binding fell through to default")
		}
	}
}

func TestInheritedImageDefaultGroupAndStableRetry(t *testing.T) {
	manager := taskManager()
	groups := taskGroups()
	for i := range groups[0].Nodes {
		groups[0].Nodes[i].RuntimeSuccesses = imageNodeStableSuccess
	}
	manager.ConfigureImageGroups("group:shared", groups)
	fields := map[string]any{"proxy_policy": AccountProxyInherit, "proxy": "direct", "proxy_binding": Binding{Reference: "node:deleted/node"}}
	for _, mode := range []string{"group:shared", "direct"} {
		manager.SetDefault(mode, nil)
		lease, err := manager.AcquireImageContext(context.Background(), fields)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "direct" && lease.URL != "" || mode == "group:shared" && lease.GroupID != "shared" {
			t.Fatalf("inherited image %s lost selected default: %+v", mode, lease)
		}
		lease.Release(false)
	}
	manager.SetDefault("group:shared", nil)
	if lease := manager.AcquireStableImage(fields, groups[0].Nodes[0].URL); lease == nil || lease.URL != groups[0].Nodes[1].URL {
		t.Fatal("inherited account retained a stale binding during stable retry")
	} else {
		lease.Release(false)
	}
}

func TestDailyInheritanceDoesNotChangeSavedRegistrationLease(t *testing.T) {
	manager := taskManager()
	registration := acquireTest(t, manager, "node:shared/a", "openai", nil)
	registration.Release()
	fields := map[string]any{"proxy_binding": registration.Binding}
	manager.ConfigureTaskGroups(nil)
	manager.SetDefault("http://new-default.invalid", nil)
	if manager.Resolve(fields, false) != "http://new-default.invalid" {
		t.Fatal("daily account did not adopt new default")
	}
	if lease, err := manager.AcquireTask(context.Background(), "direct", "openai", "registration", &registration.Binding); err == nil {
		lease.Release()
		t.Fatal("saved registration silently changed its unavailable route")
	}
}

func TestRebindInheritanceClearsAliasesAndPreservesAudit(t *testing.T) {
	old := map[string]any{"proxy": "direct", "proxy_url": "http://old.invalid", "proxyUrl": "http://older.invalid", "proxy_binding": Binding{Reference: "direct", Purpose: "account"}, "proxy_binding_history": []any{map[string]any{"reason": "Original manual decision"}}}
	before, _ := json.Marshal(old)
	updates := RebindFields(old, Binding{Reference: AccountProxyInherit, BoundAt: "now"}, "Follow the shared default")
	if updates["proxy_policy"] != AccountProxyInherit || updates["proxy_binding"] != nil || !AccountInheritsDefault(updates) {
		t.Fatal("inheritance not stored explicitly")
	}
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
		if updates[key] != "" {
			t.Fatal("legacy alias not cleared", key)
		}
	}
	if len(updates["proxy_binding_history"].([]any)) != 2 {
		t.Fatal("binding audit was lost")
	}
	after, _ := json.Marshal(old)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rebind mutated source account")
	}
	explicit := RebindFields(updates, Binding{Reference: "direct", Platform: "grok", Purpose: "account"}, "Only this account should be direct")
	if explicit["proxy_policy"] != AccountProxyExplicit || AccountInheritsDefault(explicit) {
		t.Fatal("new manual override classified as automatic")
	}
}
