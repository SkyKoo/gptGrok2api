package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// Pointers preserve the distinction between zero and a field the upstream omitted.
type GrokMediaQuotaWindow struct {
	Available       *bool  `json:"available,omitempty"`
	Remaining       *int64 `json:"remaining,omitempty"`
	WindowSeconds   *int64 `json:"window_seconds,omitempty"`
	NextAvailableAt string `json:"next_available_at,omitempty"`
}

type GrokMediaQuotaSnapshot struct {
	Status     string                          `json:"status"`
	CheckedAt  string                          `json:"checked_at"`
	Error      string                          `json:"error,omitempty"`
	HTTPStatus int                             `json:"http_status,omitempty"`
	RetryAt    string                          `json:"retry_at,omitempty"`
	Quotas     map[string]GrokMediaQuotaWindow `json:"quotas"`
}

// RefreshMedia performs one read-only query. Media errors never imply that the
// text credential is invalid; callers keep the two observations independent.
func (q *GrokQuota) RefreshMedia(parent context.Context, token string, fields map[string]any) GrokMediaQuotaSnapshot {
	result := GrokMediaQuotaSnapshot{Status: "failed", CheckedAt: time.Now().UTC().Format(time.RFC3339), Quotas: map[string]GrokMediaQuotaWindow{}}
	fail := func(reason string) GrokMediaQuotaSnapshot { result.Error = reason; return result }
	if q == nil || strings.TrimSpace(q.URL) == "" {
		return fail("not_configured")
	}
	endpoint, err := url.Parse(q.URL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return fail("not_configured")
	}
	endpoint.Path = "/rest/media/imagine/quota_info"
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	token = strings.TrimSpace(strings.TrimPrefix(token, "sso="))
	if token == "" {
		return fail("missing_credentials")
	}
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	proxyURL := ""
	if q.Proxy != nil {
		proxyURL = q.Proxy.Resolve(fields, false)
	}
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, proxyURL), http.MethodPost, endpoint.String(), bytes.NewBufferString("{}"))
	if err != nil {
		return fail("invalid_request")
	}
	setGrokQuotaHeaders(req, token, fields)
	req.Header.Set("Referer", "https://grok.com/imagine")
	// A media balance check must not reuse a tunnel shared with other requests.
	client, closeClient := newGrokQuotaCheckClient(q.Client, true)
	defer closeClient()
	// Do not forward session cookies through a redirect from this internal API.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fail("timeout")
		}
		return fail("network_error")
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	if retry := strings.TrimSpace(response.Header.Get("Retry-After")); retry != "" {
		if seconds, e := strconv.ParseInt(retry, 10, 32); e == nil && seconds > 0 {
			result.RetryAt = time.Now().Add(time.Duration(seconds) * time.Second).UTC().Format(time.RFC3339)
		} else if at, e := http.ParseTime(retry); e == nil && at.After(time.Now()) {
			result.RetryAt = at.UTC().Format(time.RFC3339)
		}
	}
	if response.StatusCode != http.StatusOK {
		return fail(fmt.Sprintf("HTTP %d", response.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return fail("invalid_response")
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return fail("invalid_response")
	}
	quotas := map[string]GrokMediaQuotaWindow{}
	for _, key := range []string{"image", "imagePro", "imageEdit", "video", "video720p"} {
		value, ok := body[key]
		if !ok || string(value) == "null" {
			continue
		}
		var window struct {
			Available       *bool  `json:"available"`
			Remaining       *int64 `json:"remainingQueries"`
			WindowSeconds   *int64 `json:"windowSizeSeconds"`
			NextAvailableAt string `json:"nextAvailableAt"`
		}
		if json.Unmarshal(value, &window) != nil {
			return fail("invalid_response")
		}
		if window.Remaining != nil && *window.Remaining < 0 || window.WindowSeconds != nil && *window.WindowSeconds < 0 {
			return fail("invalid_response")
		}
		if window.NextAvailableAt != "" {
			if _, err := time.Parse(time.RFC3339Nano, window.NextAvailableAt); err != nil {
				return fail("invalid_response")
			}
		}
		quotas[key] = GrokMediaQuotaWindow{Available: window.Available, Remaining: window.Remaining, WindowSeconds: window.WindowSeconds, NextAvailableAt: window.NextAvailableAt}
	}
	result.Status = "success"
	result.Quotas = quotas
	return result
}
