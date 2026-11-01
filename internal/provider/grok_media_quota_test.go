package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func TestGrokMediaQuotaPreservesUnknownAndZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/rest/media/imagine/quota_info" || string(body) != "{}" || r.Header.Get("Cookie") != "sso=test; sso-rw=test" {
			t.Errorf("unexpected quota request")
		}
		_, _ = w.Write([]byte(`{"imagePro":{"available":true,"remainingQueries":0,"nextAvailableAt":"2026-11-02T11:59:19.975576146Z","windowSizeSeconds":86400},"video720p":{"available":true,"remainingQueries":1},"imageEdit":{"available":false},"video":null,"unknown":{"secret":"do-not-expose"}}`))
	}))
	defer server.Close()
	q := NewGrokQuota(server.URL+"/rest/rate-limits", server.Client(), nil)
	got := q.RefreshMedia(context.Background(), "test", nil)
	if got.Status != "success" || len(got.Quotas) != 3 || got.Error != "" {
		t.Fatalf("snapshot: %#v", got)
	}
	if _, ok := got.Quotas["image"]; ok {
		t.Fatal("absent image category must remain unknown")
	}
	if got.Quotas["imagePro"].Remaining == nil || *got.Quotas["imagePro"].Remaining != 0 || got.Quotas["imagePro"].NextAvailableAt == "" {
		t.Fatal("explicit zero/reset lost")
	}
	if got.Quotas["imageEdit"].Remaining != nil || got.Quotas["imageEdit"].Available == nil || *got.Quotas["imageEdit"].Available {
		t.Fatal("missing count or explicit false lost")
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "do-not-expose") {
		t.Fatal("unknown upstream field exposed")
	}
}

func TestGrokMediaQuotaFailuresAreUnknownAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"forbidden", 403, "cookie-secret invalid-credentials", "HTTP 403"},
		{"unauthorized", 401, "cookie-secret", "HTTP 401"},
		{"limited", 429, "cookie-secret", "HTTP 429"},
		{"html", 200, "<html>cookie-secret</html>", "invalid_response"},
		{"bad count", 200, `{"image":{"remainingQueries":"cookie-secret"}}`, "invalid_response"},
		{"negative", 200, `{"image":{"remainingQueries":-1}}`, "invalid_response"},
		{"bad date", 200, `{"image":{"nextAvailableAt":"cookie-secret"}}`, "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			got := NewGrokQuota(server.URL, server.Client(), nil).RefreshMedia(context.Background(), "secret", nil)
			if got.Status != "failed" || got.Error != tc.want || len(got.Quotas) != 0 {
				t.Fatalf("%#v", got)
			}
		})
	}
}

type mediaQuotaTransport func(*http.Request) (*http.Response, error)

func (f mediaQuotaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGrokMediaQuotaUsesAccountProxyAndRedactsTransportError(t *testing.T) {
	client := &http.Client{Transport: mediaQuotaTransport(func(r *http.Request) (*http.Response, error) {
		if proxyruntime.URLFromContext(r.Context()) != "http://account-proxy:8080" {
			t.Error("account proxy was not preserved")
		}
		return nil, errors.New("proxy-password-cookie-secret")
	})}
	q := NewGrokQuota("https://grok.com/rest/rate-limits", client, proxyruntime.NewManager("http://default-proxy:8080", nil))
	got := q.RefreshMedia(context.Background(), "test", map[string]any{"proxy": "http://account-proxy:8080"})
	if got.Error != "network_error" || len(got.Quotas) != 0 {
		t.Fatalf("%#v", got)
	}
}

func TestGrokMediaQuotaDoesNotFollowRedirects(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("session was forwarded to redirect destination") }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer source.Close()
	got := NewGrokQuota(source.URL, source.Client(), nil).RefreshMedia(context.Background(), "secret", nil)
	if got.Error != "HTTP 302" {
		t.Fatalf("%#v", got)
	}
}
