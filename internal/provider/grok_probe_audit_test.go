package provider

import (
	"bytes"
	"context"
	proxy "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
)

type auditRoundTrip func(*http.Request) (*http.Response, error)

func (f auditRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGrokProbeAuditSafeMetadata(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	calls := 0
	transport := &grokProbeAuditTransport{base: auditRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Cookie") != "sso=private-token" {
			t.Fatal("request changed")
		}
		return &http.Response{StatusCode: 403, Proto: "HTTP/2.0", Header: http.Header{"Content-Type": []string{"text/html; secret=value"}, "Cf-Mitigated": []string{"challenge"}}, Body: io.NopCloser(strings.NewReader("sensitive-body"))}, nil
	})}
	req, _ := http.NewRequestWithContext(proxy.WithURL(context.Background(), "http://private-user:private-password@proxy.example:8080"), "POST", "https://upstream.example/private", nil)
	req.Header.Set("Cookie", "sso=private-token")
	response, err := transport.RoundTrip(req)
	if err != nil || response.StatusCode != 403 || calls != 1 {
		t.Fatal("result changed")
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "sensitive-body" {
		t.Fatal("body consumed")
	}
	output := logs.String()
	for _, value := range []string{"private-token", "private-user", "private-password", "proxy.example", "upstream.example", "secret=value", "sensitive-body"} {
		if strings.Contains(output, value) {
			t.Fatalf("leaked %s", value)
		}
	}
	for _, value := range []string{"status=403", "protocol=HTTP/2.0", "content_type=text/html", "challenge=true", "request_fp=", "route_fp="} {
		if !strings.Contains(output, value) {
			t.Fatalf("missing %s", value)
		}
	}
}
