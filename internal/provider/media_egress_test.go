package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func TestMediaAttemptKeepsProxyAcrossDefaultChange(t *testing.T) {
	for _, separateResource := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared exit", true: "separate resource"}[separateResource], func(t *testing.T) {
			var firstHits, secondHits atomic.Int32
			proxy := func(label string, hits *atomic.Int32) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if r.URL.Host != "media.example.test" {
						t.Error("request bypassed the simulated proxy")
					}
					w.Header().Set("X-Test-Egress", label)
					if r.URL.Path == "/post" {
						_, _ = io.WriteString(w, `{"post":{"id":"fixture"}}`)
					} else {
						_, _ = io.WriteString(w, label)
					}
				}))
			}
			first, second := proxy("first", &firstHits), proxy("second", &secondHits)
			defer first.Close()
			defer second.Close()
			manager := proxyruntime.NewManager(first.URL, nil)
			if separateResource {
				manager.SetResource(second.URL, nil)
			}
			client := &http.Client{Transport: proxyruntime.NewTransport(http.DefaultTransport), Timeout: time.Second}
			media := NewMedia(client, "http://media.example.test/generate", "http://media.example.test/post", "", "", time.Second)
			media.SetProxyManager(manager)
			account := accounts.Account{Token: "fixture", Fields: map[string]any{"proxy_policy": proxyruntime.AccountProxyInherit}}
			ctx := manager.WithAccountEgress(context.Background(), account.Fields)
			if _, err := media.CreatePost(ctx, account, "video", "", "fixture"); err != nil {
				t.Fatal(err)
			}
			manager.SetDefault(second.URL, nil)
			manager.SetResource(first.URL, nil)
			response, err := media.StreamChat(ctx, account, map[string]any{"prompt": "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.Header.Get("X-Test-Egress") != "first" {
				t.Fatal("generation switched from its initial proxy")
			}
			raw, _, err := media.Fetch(ctx, account, "http://media.example.test/asset")
			want := "first"
			if separateResource {
				want = "second"
			}
			if err != nil || string(raw) != want {
				t.Fatalf("download changed route: %q, %v", raw, err)
			}
			next := manager.WithAccountEgress(context.Background(), account.Fields)
			response, err = media.StreamChat(next, account, map[string]any{"prompt": "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.Header.Get("X-Test-Egress") != "second" || firstHits.Load()+secondHits.Load() != 4 {
				t.Fatal("next generation did not use the changed default exactly once")
			}
		})
	}
}
