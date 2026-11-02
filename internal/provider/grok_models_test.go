package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func TestGrokModesUsesAccountRouteAndNeverGenerates(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/rest/modes" || r.Header.Get("Cookie") != "sso=test; sso-rw=test" {
			t.Error("wrong modes request or credentials")
		}
		fmt.Fprint(w, `{"modes":[{"id":"fast","title":"快速","availability":{"available":{}}}]}`)
	}))
	defer upstream.Close()
	client := &http.Client{Transport: proxyruntime.NewTransport(http.DefaultTransport)}
	g := NewGrokChat(upstream.URL+"/must-not-generate", client, 0)
	g.SetProxyManager(proxyruntime.NewManager("http://127.0.0.1:1", nil))
	account := accounts.Account{Token: "test", Fields: map[string]any{"proxy": "direct"}}
	c, err := g.FetchModes(context.Background(), upstream.URL+"/rest/modes", account)
	if err != nil || len(c.Modes) != 1 || calls.Load() != 1 {
		t.Fatalf("directory failed: %v %#v", err, c)
	}
}
func TestGrokModesRefusalsNeverRetryRedirectOrLeak(t *testing.T) {
	for _, status := range []int{301, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirect")
				w.WriteHeader(status)
				fmt.Fprint(w, "SECRET_TOKEN upstream body")
			}))
			defer up.Close()
			g := NewGrokChat(up.URL, up.Client(), 0)
			_, err := g.FetchModes(context.Background(), up.URL, accounts.Account{Token: "SECRET_TOKEN"})
			if err == nil || calls.Load() != 1 {
				t.Fatalf("expected single refusal: %v %d", err, calls.Load())
			}
			if errors.Is(err, ErrGrokInvalidCredentials) != (status == 401) {
				t.Fatalf("wrong credential failure classification: %v", err)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatal("secret disclosed")
			}
		})
	}
}
