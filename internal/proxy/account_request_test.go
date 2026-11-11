package proxy

import (
	"context"
	"testing"
)

func TestAccountAttemptPinsDefaultAndResourceSelections(t *testing.T) {
	for _, resource := range []string{"", "direct", "http://resource.invalid"} {
		t.Run(resource, func(t *testing.T) {
			manager := NewManager("", []string{"http://first.invalid", "http://second.invalid"})
			manager.SetResource(resource, nil)
			fields := map[string]any{"proxy_policy": AccountProxyInherit}
			ctx := manager.WithAccountEgress(context.Background(), fields)
			primary := URLFromContext(manager.RequestContext(ctx, fields, false))
			wantResource := primary
			if resource != "" {
				wantResource = normalizeURL(resource)
			}
			manager.SetDefault("http://changed.invalid", nil)
			manager.SetResource("http://changed-resource.invalid", nil)
			for _, selection := range []struct {
				resource bool
				want     string
			}{{false, "http://first.invalid"}, {true, wantResource}} {
				if actual := URLFromContext(manager.RequestContext(ctx, fields, selection.resource)); actual != selection.want {
					t.Fatalf("in-flight route changed: got %q want %q", actual, selection.want)
				}
			}
			next := manager.WithAccountEgress(context.Background(), fields)
			if URLFromContext(manager.RequestContext(next, fields, false)) != "http://changed.invalid" {
				t.Fatal("next attempt did not adopt changed default")
			}
			explicit := WithURL(ctx, "")
			if endpoint, selected := URLSelectionFromContext(manager.RequestContext(explicit, fields, true)); !selected || endpoint != "" {
				t.Fatal("explicit direct request lost precedence over the attempt snapshot")
			}
		})
	}
}

func TestAccountAttemptPinsOneAccountGroupNodeAndSupportsNilManager(t *testing.T) {
	manager := taskManager()
	manager.SetResource("http://resource.invalid", nil)
	fields := map[string]any{"proxy": "group:shared", "proxy_policy": AccountProxyExplicit}
	ctx := manager.WithAccountEgress(context.Background(), fields)
	request := URLFromContext(manager.RequestContext(ctx, fields, false))
	resource := URLFromContext(manager.RequestContext(ctx, fields, true))
	if request != "http://a.invalid:8080" || resource != request {
		t.Fatal("one account attempt used multiple group nodes")
	}
	var absent *Manager
	ctx = absent.WithAccountEgress(context.Background(), nil)
	if endpoint, selected := URLSelectionFromContext(absent.RequestContext(ctx, nil, true)); !selected || endpoint != "" {
		t.Fatal("nil manager did not provide an explicit direct request")
	}
}
