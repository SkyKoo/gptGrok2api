package provider

import (
	"context"
	"fmt"
	proxy "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestGrokMediaQuotaOwnsHTTP2ConnectionsAndPreservesSession(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprint(wrapped), func(t *testing.T) {
			var mu sync.Mutex
			var remotes []string
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				remotes = append(remotes, r.RemoteAddr)
				mu.Unlock()
				if r.ProtoMajor != 2 {
					t.Error("HTTP2 was not exercised")
				}
				if r.Method != "POST" || r.Header.Get("Cookie") != "sso=session; sso-rw=write-session" {
					t.Error("session request changed")
				}
				if r.URL.Path == "/rest/media/imagine/quota_info" {
					body, _ := io.ReadAll(r.Body)
					if string(body) != "{}" || r.Header.Get("Referer") != "https://grok.com/imagine" {
						t.Error("media contract changed")
					}
					fmt.Fprint(w, `{"imagePro":{"available":true,"remainingQueries":4},"video720p":{"available":true,"remainingQueries":1}}`)
				} else {
					fmt.Fprint(w, `{"remainingQueries":30,"totalQueries":30}`)
				}
			}))
			upstream.EnableHTTP2 = true
			upstream.StartTLS()
			defer upstream.Close()
			client := upstream.Client()
			if wrapped {
				client.Transport = proxy.NewTransport(client.Transport)
			}
			defer client.CloseIdleConnections()
			q := NewGrokQuota(upstream.URL+"/rest/rate-limits", client, proxy.NewManager("direct", nil))
			fields := map[string]any{"cookie_header": "sso=session; sso-rw=write-session"}
			if _, err := q.fetch(context.Background(), "session", fields, "fast"); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				got := q.RefreshMedia(context.Background(), "session", fields)
				if got.Status != "success" || *got.Quotas["imagePro"].Remaining != 4 {
					t.Fatalf("media query failed: %#v", got)
				}
			}
			if _, err := q.fetch(context.Background(), "session", fields, "fast"); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(remotes) != 4 || remotes[0] != remotes[3] || remotes[0] == remotes[1] || remotes[0] == remotes[2] || remotes[1] == remotes[2] {
				t.Fatalf("unexpected connection ownership: %v", remotes)
			}
		})
	}
}
