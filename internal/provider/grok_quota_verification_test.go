package provider

import (
	"context"
	"errors"
	"fmt"
	proxy "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestGrokExplicitVerificationUsesIndependentConnectionAndPreservesRequest(t *testing.T) {
	var mu sync.Mutex
	var remotes []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes = append(remotes, r.RemoteAddr)
		mu.Unlock()
		if r.Method != "POST" || r.Header.Get("Cookie") != "sso=private; sso-rw=private" {
			t.Error("changed verification request")
		}
		fmt.Fprint(w, `{"remainingQueries":30,"totalQueries":30}`)
	}))
	defer upstream.Close()
	client := &http.Client{Transport: proxy.NewTransport(http.DefaultTransport)}
	defer client.CloseIdleConnections()
	q := NewGrokQuota(upstream.URL, client, proxy.NewManager("direct", nil))
	if _, e := q.fetch(context.Background(), "private", nil, "fast"); e != nil {
		t.Fatal(e)
	}
	if _, e := q.ProbeFast(context.Background(), "private", nil); e != nil {
		t.Fatal(e)
	}
	if _, e := q.fetch(context.Background(), "private", nil, "fast"); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(remotes) != 3 || remotes[0] == remotes[1] || remotes[0] != remotes[2] {
		t.Fatalf("connection ownership incorrect: %v", remotes)
	}
}

func TestGrokQuotaFailuresKeepStatusAndDoNotRetryOrLeakBodies(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, kind string
	}{
		{403, "private token SECRET", "access_denied"}, {429, "private token SECRET", "rate_limited"},
		{503, "private token SECRET", "upstream_unavailable"}, {200, "private token SECRET", "response_invalid"},
		{200, `{"private":"SECRET"}`, "quota_missing"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			count := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer up.Close()
			_, err := NewGrokQuota(up.URL, up.Client(), nil).ProbeFast(context.Background(), "SECRET", nil)
			var failure *GrokQuotaError
			if !errors.As(err, &failure) || failure.Kind != tc.kind || failure.Status != tc.status || count != 1 {
				t.Fatalf("bad result: %v, requests=%d", err, count)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatal("leaked upstream body")
			}
			if errors.Is(err, ErrGrokInvalidCredentials) {
				t.Fatal("transient failure marked invalid")
			}
		})
	}
}
