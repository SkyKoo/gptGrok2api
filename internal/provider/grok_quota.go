package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

var ErrGrokInvalidCredentials = errors.New("grok credentials are invalid")

// GrokQuotaError exposes a stable reason and HTTP status without response
// bodies, request URLs, proxy passwords or Cookie values.
type GrokQuotaError struct {
	Kind   string
	Status int
}

func (e *GrokQuotaError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("grok rate-limits HTTP %d (%s)", e.Status, e.Kind)
	}
	return "grok rate-limits " + e.Kind
}

type QuotaWindow struct {
	Remaining     int   `json:"remaining"`
	Total         int   `json:"total"`
	WindowSeconds int   `json:"window_seconds"`
	ResetAt       int64 `json:"reset_at"`
}

type GrokQuota struct {
	URL    string
	Client *http.Client
	Proxy  *proxyruntime.Manager
}

func NewGrokQuota(url string, client *http.Client, proxy *proxyruntime.Manager) *GrokQuota {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GrokQuota{URL: strings.TrimRight(strings.TrimSpace(url), "/"), Client: client, Proxy: proxy}
}

var grokQuotaModes = map[string]string{
	"auto": "auto", "fast": "fast", "expert": "expert", "heavy": "heavy", "grok_4_3": "grok-420-computer-use-sa",
}

func (q *GrokQuota) Refresh(ctx context.Context, account accounts.Account) (map[string]QuotaWindow, error) {
	return q.RefreshToken(ctx, account.Token, account.Fields)
}

func (q *GrokQuota) RefreshToken(ctx context.Context, token string, fields map[string]any) (map[string]QuotaWindow, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "sso="))
	if token == "" {
		return nil, errors.New("sso token is required")
	}
	type result struct {
		mode string
		win  QuotaWindow
		err  error
	}
	results := make(chan result, len(grokQuotaModes))
	var wg sync.WaitGroup
	for mode, modelName := range grokQuotaModes {
		wg.Add(1)
		go func(mode, modelName string) {
			defer wg.Done()
			win, err := q.fetch(ctx, token, fields, modelName)
			results <- result{mode: mode, win: win, err: err}
		}(mode, modelName)
	}
	wg.Wait()
	close(results)
	quotas := map[string]QuotaWindow{}
	var firstErr error
	for item := range results {
		if item.err != nil {
			if errors.Is(item.err, ErrGrokInvalidCredentials) {
				return nil, item.err
			}
			if firstErr == nil {
				firstErr = item.err
			}
			continue
		}
		quotas[item.mode] = item.win
	}
	if len(quotas) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return quotas, nil
}

func (q *GrokQuota) ProbeFast(ctx context.Context, token string, fields map[string]any) (QuotaWindow, error) {
	// An explicit identity check must not inherit a long-lived connection
	// shared with unrelated traffic. Keep the exact route, headers and timeout;
	// this is one request, never an automatic retry after a 403 or challenge.
	probe := *q
	client, closeClient := newGrokQuotaCheckClient(q.Client, false)
	defer closeClient()
	probe.Client = client
	return probe.fetch(ctx, strings.TrimSpace(strings.TrimPrefix(token, "sso=")), fields, "fast")
}

// Explicit checks own their connection pools while keeping the configured route
// and client policy. Custom transports are borrowed and must not be closed.
func newGrokQuotaCheckClient(source *http.Client, media bool) (*http.Client, func()) {
	if source == nil {
		source = http.DefaultClient
	}
	client := *source
	owned := true
	switch transport := client.Transport.(type) {
	case *proxyruntime.Transport:
		client.Transport = transport.Clone()
	case *http.Transport:
		client.Transport = transport.Clone()
	case nil:
		client.Transport = http.DefaultTransport.(*http.Transport).Clone()
	default:
		owned = false
	}
	transport := client.Transport
	cleanup := func() {}
	if owned {
		cleanup = func() {
			if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
		}
	}
	client.Transport = &grokProbeAuditTransport{base: transport, media: media}
	return &client, cleanup
}

func (q *GrokQuota) fetch(ctx context.Context, token string, fields map[string]any, modelName string) (QuotaWindow, error) {
	body, err := json.Marshal(map[string]string{"modelName": modelName})
	if err != nil {
		return QuotaWindow{}, err
	}
	proxyURL := ""
	if q.Proxy != nil {
		proxyURL = q.Proxy.Resolve(fields, false)
	}
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, proxyURL), http.MethodPost, q.URL, bytes.NewReader(body))
	if err != nil {
		return QuotaWindow{}, &GrokQuotaError{Kind: "request_invalid"}
	}
	setGrokQuotaHeaders(req, token, fields)
	response, err := q.Client.Do(req)
	if err != nil {
		kind := "transport_failed"
		var network net.Error
		if errors.Is(err, context.Canceled) {
			kind = "cancelled"
		} else if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
			kind = "timeout"
		}
		return QuotaWindow{}, &GrokQuotaError{Kind: kind}
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode == http.StatusUnauthorized || isGrokInvalidBody(string(raw)) {
		return QuotaWindow{}, ErrGrokInvalidCredentials
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		kind := "upstream_rejected"
		if response.StatusCode == 403 {
			kind = "access_denied"
		} else if response.StatusCode == 429 {
			kind = "rate_limited"
		} else if response.StatusCode >= 500 {
			kind = "upstream_unavailable"
		}
		return QuotaWindow{}, &GrokQuotaError{Kind: kind, Status: response.StatusCode}
	}
	if readErr != nil {
		return QuotaWindow{}, &GrokQuotaError{Kind: "response_read_failed", Status: response.StatusCode}
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return QuotaWindow{}, &GrokQuotaError{Kind: "response_invalid", Status: response.StatusCode}
	}
	remaining, ok := quotaNumber(value["remainingQueries"])
	if !ok {
		return QuotaWindow{}, &GrokQuotaError{Kind: "quota_missing", Status: response.StatusCode}
	}
	total, ok := quotaNumber(value["totalQueries"])
	if !ok {
		total = remaining
	}
	window, ok := quotaNumber(value["windowSizeSeconds"])
	if !ok || window <= 0 {
		window = 72000
	}
	return QuotaWindow{Remaining: maxInt(0, remaining), Total: maxInt(0, total), WindowSeconds: window, ResetAt: time.Now().Add(time.Duration(window) * time.Second).UnixMilli()}, nil
}

func setGrokQuotaHeaders(req *http.Request, token string, fields map[string]any) {
	cookie := stringValue(fields["cookie_header"])
	if cookie == "" {
		cookie = "sso=" + token + "; sso-rw=" + token
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "https://grok.com")
	req.Header.Set("Referer", "https://grok.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/136 Safari/537.36")
}

func isGrokInvalidBody(value string) bool {
	value = strings.ToLower(value)
	for _, marker := range []string{"invalid-credentials", "bad-credentials", "session not found", "token revoked", "token expired", "account suspended", "blocked-user"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func quotaNumber(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	default:
		var parsed int
		_, err := fmt.Sscanf(fmt.Sprint(value), "%d", &parsed)
		return parsed, err == nil
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// grokProbeAuditTransport records bounded metadata for explicit validation.
// Fingerprints compare requests without logging cookies, URLs or proxy credentials.
type grokProbeAuditTransport struct {
	base  http.RoundTripper
	media bool
}

func (t *grokProbeAuditTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	headers, _ := json.Marshal(req.Header)
	fingerprint := sha256.Sum256(append([]byte(req.Method+"\n"+req.URL.String()+"\n"), headers...))
	route := sha256.Sum256([]byte(proxyruntime.URLFromContext(req.Context())))
	response, err := t.base.RoundTrip(req)
	status, protocol, contentType, challenge := 0, "none", "other", false
	if response != nil {
		status = response.StatusCode
		switch response.Proto {
		case "HTTP/1.1", "HTTP/2.0":
			protocol = response.Proto
		}
		rawType := strings.ToLower(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0])
		switch rawType {
		case "application/json", "text/html", "text/plain":
			contentType = rawType
		}
		challenge = strings.EqualFold(response.Header.Get("Cf-Mitigated"), "challenge")
	}
	label := "grok_verification"
	if t.media {
		label = "grok_media_quota"
	}
	log.Printf("%s request_fp=%x route_fp=%x status=%d protocol=%s content_type=%s challenge=%t elapsed_ms=%d", label, fingerprint[:8], route[:6], status, protocol, contentType, challenge, time.Since(started).Milliseconds())
	return response, err
}

func (t *grokProbeAuditTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
