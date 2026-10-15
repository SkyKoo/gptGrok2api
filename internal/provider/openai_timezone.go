package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // Minimal production images may not contain an IANA zone database.

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

const openAITimezoneTTL = 10 * time.Minute
const openAITimezoneRetry = time.Minute
const openAITimezoneTimeout = 4500 * time.Millisecond
const openAITimezoneCacheLimit = 256

var fallbackOpenAILocation = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	} // Embedded tzdata makes this a build invariant.
	return loc
}()
var sharedOpenAITimezones = newOpenAITimezones(lookupOpenAITimezone)

// ResolveOpenAITimezone shares the chat/image egress cache with authentication
// flows. A custom origin returns nil without geolocation traffic. The caller
// must pass the same explicit proxy used by its authentication client.
func ResolveOpenAITimezone(ctx context.Context, baseURL, proxyURL string) *time.Location {
	resolver := openAITimezonesFor(baseURL)
	if resolver == nil {
		return nil
	}
	if proxyURL == "direct" {
		proxyURL = ""
	}
	return resolver.resolve(ctx, proxyURL)
}

type openAITimezoneEntry struct {
	location *time.Location
	retryAt  time.Time
	lastUsed time.Time
	loading  chan struct{}
}
type openAITimezoneResolver struct {
	mu      sync.Mutex
	entries map[[32]byte]*openAITimezoneEntry
	lookup  func(context.Context, string) (*time.Location, error)
	now     func() time.Time
}

func newOpenAITimezones(lookup func(context.Context, string) (*time.Location, error)) *openAITimezoneResolver {
	return &openAITimezoneResolver{entries: make(map[[32]byte]*openAITimezoneEntry), lookup: lookup, now: time.Now}
}

// Cache by selected egress, not account. A failed proxy lookup never falls back
// to a direct network lookup. Proxy credentials and public IPs are not logged.
func (r *openAITimezoneResolver) resolve(ctx context.Context, proxyURL string) *time.Location {
	if r == nil {
		return fallbackOpenAILocation
	}
	key := sha256.Sum256([]byte(proxyURL))
	for {
		if ctx.Err() != nil {
			return fallbackOpenAILocation
		}
		r.mu.Lock()
		now := r.now()
		entry := r.entries[key]
		if entry == nil {
			if len(r.entries) >= openAITimezoneCacheLimit {
				var oldestKey [32]byte
				var oldest *openAITimezoneEntry
				for k, e := range r.entries {
					if e.loading == nil && (oldest == nil || e.lastUsed.Before(oldest.lastUsed)) {
						oldestKey, oldest = k, e
					}
				}
				if oldest == nil {
					r.mu.Unlock()
					return fallbackOpenAILocation
				}
				delete(r.entries, oldestKey)
			}
			entry = &openAITimezoneEntry{}
			r.entries[key] = entry
		}
		entry.lastUsed = now
		if now.Before(entry.retryAt) {
			loc := entry.location
			r.mu.Unlock()
			if loc != nil {
				return loc
			}
			return fallbackOpenAILocation
		}
		if done := entry.loading; done != nil {
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return fallbackOpenAILocation
			case <-done:
				continue
			}
		}
		entry.loading = make(chan struct{})
		r.mu.Unlock()
		lookupCtx, cancel := context.WithTimeout(ctx, openAITimezoneTimeout)
		loc, err := r.lookup(lookupCtx, proxyURL)
		cancel()
		r.mu.Lock()
		if err == nil && loc != nil {
			entry.location = loc
			entry.retryAt = r.now().Add(openAITimezoneTTL)
		} else if ctx.Err() == nil {
			entry.retryAt = r.now().Add(openAITimezoneRetry)
		}
		// Keep the last valid zone during outages. Never persist the fallback as a
		// successful observation, and don't poison the cache on caller cancellation.
		loc = entry.location
		close(entry.loading)
		entry.loading = nil
		r.mu.Unlock()
		if ctx.Err() == nil {
			zone, source := fallbackOpenAILocation.String(), "fallback"
			if loc != nil {
				zone, source = loc.String(), "stale_cache"
			}
			if err == nil && loc != nil {
				source = "ip_lookup"
			}
			log.Printf("[openai-timezone] egress=%x timezone=%s source=%s", key[:4], zone, source)
		}
		if loc != nil {
			return loc
		}
		return fallbackOpenAILocation
	}
}

var openAITimezoneEndpoints = []string{"https://ipwho.is/", "https://ipapi.co/json/", "https://freeipapi.com/api/json"}

func newOpenAITimezoneHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Explicit selection prevents HTTP_PROXY/HTTPS_PROXY from changing direct
	// egress. These requests use a separate client with no account cookies/tokens.
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		value := proxyruntime.URLFromContext(req.Context())
		if value == "" {
			return nil, nil
		}
		p, err := url.Parse(value)
		if err != nil || p.Host == "" {
			return nil, errors.New("invalid timezone proxy")
		}
		switch p.Scheme {
		case "http", "https", "socks5", "socks5h":
			return p, nil
		case "socks":
			p.Scheme = "socks5"
			return p, nil
		}
		return nil, errors.New("unsupported timezone proxy")
	}
	transport.MaxIdleConns = 16
	transport.MaxIdleConnsPerHost = 2
	transport.IdleConnTimeout = time.Minute
	return &http.Client{Transport: transport, Timeout: openAITimezoneTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var openAITimezoneHTTP = newOpenAITimezoneHTTPClient()

func lookupOpenAITimezone(ctx context.Context, proxyURL string) (*time.Location, error) {
	return fetchOpenAITimezone(ctx, proxyURL, openAITimezoneHTTP, openAITimezoneEndpoints)
}
func fetchOpenAITimezone(ctx context.Context, proxyURL string, client *http.Client, endpoints []string) (*time.Location, error) {
	for _, endpoint := range endpoints {
		if ctx.Err() != nil {
			break
		}
		attempt, cancel := context.WithTimeout(proxyruntime.WithURL(ctx, proxyURL), 1500*time.Millisecond)
		req, err := http.NewRequestWithContext(attempt, http.MethodGet, endpoint, nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Accept", "application/json")
		response, err := client.Do(req)
		if err != nil {
			cancel()
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		_ = response.Body.Close()
		cancel()
		if response.StatusCode != http.StatusOK || readErr != nil || len(raw) > 64<<10 {
			continue
		}
		if loc := parseOpenAITimezone(raw); loc != nil {
			return loc, nil
		}
	}
	return nil, errors.New("egress timezone lookup unavailable")
}
func parseOpenAITimezone(raw []byte) *time.Location {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m["success"] == false {
		return nil
	}
	zone, _ := m["timezone"].(string)
	if item, ok := m["timezone"].(map[string]any); ok {
		zone, _ = item["id"].(string)
	}
	if zone == "" {
		if a, ok := m["timeZones"].([]any); ok && len(a) > 0 {
			zone, _ = a[0].(string)
		}
	}
	if zone == "" {
		zone, _ = m["timeZone"].(string)
	}
	if zone == "" || zone == "Local" || len(zone) > 100 || strings.Contains(zone, "..") {
		return nil
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil
	}
	return loc
}

type openAITimezoneKey struct{}
type openAITimezoneSnapshot struct {
	location *time.Location
	at       time.Time
	egress   [32]byte
}

func (s openAITimezoneSnapshot) offset() int {
	_, seconds := s.at.In(s.location).Zone()
	return -seconds / 60
}
func (s openAITimezoneSnapshot) date() string {
	local := s.at.In(s.location)
	return local.Format("Mon Jan 02 2006 15:04:05 GMT-0700 (MST)")
}
func openAITimezoneFrom(ctx context.Context) openAITimezoneSnapshot {
	if s, ok := ctx.Value(openAITimezoneKey{}).(openAITimezoneSnapshot); ok {
		return s
	}
	return openAITimezoneSnapshot{location: fallbackOpenAILocation, at: time.Now()}
}
func currentOpenAITimezone(ctx context.Context) openAITimezoneSnapshot {
	s := openAITimezoneFrom(ctx)
	s.at = time.Now()
	return s
}

func applyOpenAITimezone(ctx context.Context, payload map[string]any) {
	s := currentOpenAITimezone(ctx)
	payload["timezone"] = s.location.String()
	payload["timezone_offset_min"] = s.offset()
}
func withOpenAITimezone(ctx context.Context, r *openAITimezoneResolver, proxyURL string) context.Context {
	key := sha256.Sum256([]byte(proxyURL))
	if s, ok := ctx.Value(openAITimezoneKey{}).(openAITimezoneSnapshot); ok && s.egress == key {
		return ctx
	}
	loc := r.resolve(ctx, proxyURL)
	return context.WithValue(ctx, openAITimezoneKey{}, openAITimezoneSnapshot{location: loc, at: time.Now(), egress: key})
}
func (o *OpenAIImage) withTimezone(ctx context.Context, account accounts.Account) context.Context {
	proxyURL, selected := proxyruntime.URLSelectionFromContext(ctx)
	if !selected && o.Proxy != nil {
		proxyURL = o.Proxy.Resolve(account.Fields, false)
	}
	ctx = proxyruntime.WithURL(ctx, proxyURL)
	return withOpenAITimezone(ctx, o.timezones, proxyURL)
}
func (c *OpenAIAccountClient) withTimezone(ctx context.Context, account map[string]any) context.Context {
	proxyURL, selected := proxyruntime.URLSelectionFromContext(ctx)
	if !selected {
		proxyURL = c.ProxyURL(account)
	}
	ctx = proxyruntime.WithURL(ctx, proxyURL)
	return withOpenAITimezone(ctx, c.timezones, proxyURL)
}
func openAITimezonesFor(baseURL string) *openAITimezoneResolver {
	if browserEligible(baseURL) {
		return sharedOpenAITimezones
	}
	return nil
}
