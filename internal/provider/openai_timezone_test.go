package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

func timezoneLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	l, e := time.LoadLocation(name)
	if e != nil {
		t.Fatal(e)
	}
	return l
}
func TestOpenAITimezoneParsing(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"timezone":{"id":"America/Phoenix"}}`, "America/Phoenix"},
		{`{"timezone":"Asia/Kolkata"}`, "Asia/Kolkata"},
		{`{"timeZones":["Australia/Lord_Howe"]}`, "Australia/Lord_Howe"},
		{`{"timezone":"UTC"}`, "UTC"},
		{`{"success":false,"timezone":"UTC"}`, ""}, {`{"timezone":"Local"}`, ""}, {`{"timezone":"../etc/passwd"}`, ""}, {`{"timezone":"not/a/zone"}`, ""}, {`not json`, ""},
	} {
		l := parseOpenAITimezone([]byte(tc.raw))
		got := ""
		if l != nil {
			got = l.String()
		}
		if got != tc.want {
			t.Errorf("%s: %s", tc.raw, got)
		}
	}
}
func TestOpenAITimezoneCacheIsolationRefreshAndFailure(t *testing.T) {
	now := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
	calls := 0
	fail := false
	phoenix := timezoneLocation(t, "America/Phoenix")
	tokyo := timezoneLocation(t, "Asia/Tokyo")
	r := newOpenAITimezones(func(_ context.Context, p string) (*time.Location, error) {
		calls++
		if fail {
			return nil, errors.New("offline")
		}
		if p == "proxy-b" {
			return tokyo, nil
		}
		return phoenix, nil
	})
	r.now = func() time.Time { return now }
	ctx := context.Background()
	if r.resolve(ctx, "proxy-a") != phoenix || r.resolve(ctx, "proxy-a") != phoenix || calls != 1 {
		t.Fatal("cache miss")
	}
	if r.resolve(ctx, "proxy-b") != tokyo || calls != 2 {
		t.Fatal("egress cache not isolated")
	}
	now = now.Add(openAITimezoneTTL + time.Second)
	fail = true
	if r.resolve(ctx, "proxy-a") != phoenix || calls != 3 {
		t.Fatal("stale valid zone lost")
	}
	r.resolve(ctx, "proxy-a")
	if calls != 3 {
		t.Fatal("failure caused lookup storm")
	}
	now = now.Add(openAITimezoneRetry + time.Second)
	fail = false
	if r.resolve(ctx, "proxy-a") != phoenix || calls != 4 {
		t.Fatal("refresh never recovered")
	}
	fail = true
	if r.resolve(ctx, "new-proxy") != fallbackOpenAILocation {
		t.Fatal("missing cold fallback")
	}
	fail = false
	now = now.Add(openAITimezoneRetry + time.Second)
	if r.resolve(ctx, "new-proxy") != phoenix {
		t.Fatal("fallback persisted as successful result")
	}
}
func TestOpenAITimezoneCoalescesConcurrentRequestsAndHonorsCancellation(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	loc := timezoneLocation(t, "America/Phoenix")
	r := newOpenAITimezones(func(ctx context.Context, _ string) (*time.Location, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return loc, nil
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.resolve(context.Background(), "same") != loc {
				t.Error("unexpected zone")
			}
		}()
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r.resolve(ctx, "same") != fallbackOpenAILocation {
		t.Fatal("cancelled waiter did not exit")
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("parallel lookups=%d", calls.Load())
	}
	// Cancelling the lookup owner must not cache a failure for the next caller.
	var ownerCalls atomic.Int32
	ownerStarted, ownerDone := make(chan struct{}), make(chan struct{})
	r2 := newOpenAITimezones(func(ctx context.Context, _ string) (*time.Location, error) {
		if ownerCalls.Add(1) == 1 {
			close(ownerStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return loc, nil
	})
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	go func() { defer close(ownerDone); r2.resolve(ownerCtx, "same") }()
	<-ownerStarted
	ownerCancel()
	<-ownerDone
	if r2.resolve(context.Background(), "same") != loc || ownerCalls.Load() != 2 {
		t.Fatal("cancelled owner poisoned cache")
	}
}
func TestOpenAITimezoneCacheBounded(t *testing.T) {
	loc := timezoneLocation(t, "UTC")
	r := newOpenAITimezones(func(context.Context, string) (*time.Location, error) { return loc, nil })
	for i := 0; i < openAITimezoneCacheLimit+5; i++ {
		r.resolve(context.Background(), fmt.Sprint(i))
	}
	if len(r.entries) > openAITimezoneCacheLimit {
		t.Fatal("unbounded cache")
	}
}
func TestOpenAITimezoneLookupKeepsProxyAndNoCredentials(t *testing.T) {
	var received atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Oai-Device-Id") != "" {
			t.Error("account headers sent to IP service")
		}
		if r.URL.Host != "geo.invalid" {
			t.Errorf("unexpected destination %s", r.URL.Host)
		}
		fmt.Fprint(w, `{"timezone":{"id":"America/Phoenix"}}`)
	}))
	defer proxy.Close()
	client := newOpenAITimezoneHTTPClient()
	defer client.CloseIdleConnections()
	l, e := fetchOpenAITimezone(context.Background(), proxy.URL, client, []string{"http://geo.invalid/"})
	if e != nil || l.String() != "America/Phoenix" || received.Load() != 1 {
		t.Fatalf("proxy ignored: %v", e)
	}
	var direct atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1); fmt.Fprint(w, `{"timezone":"UTC"}`) }))
	defer target.Close()
	_, e = fetchOpenAITimezone(context.Background(), "unsupported://proxy.invalid", client, []string{target.URL, target.URL})
	if e == nil || direct.Load() != 0 {
		t.Fatal("failed proxy fell back to direct")
	}
	// The direct route must ignore process-level proxy environment settings.
	req, _ := http.NewRequest("GET", target.URL, nil)
	if p, e := client.Transport.(*http.Transport).Proxy(req); e != nil || p != nil {
		t.Fatal("direct route overridden")
	}
}
func TestOpenAITimezoneLookupFallbackAndCancelledRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/bad" {
			fmt.Fprint(w, `{"timezone":"invalid"}`)
			return
		}
		fmt.Fprint(w, `{"timezone":"Asia/Kolkata"}`)
	}))
	defer server.Close()
	l, e := fetchOpenAITimezone(context.Background(), "", server.Client(), []string{server.URL + "/bad", server.URL + "/good"})
	if e != nil || l.String() != "Asia/Kolkata" || calls.Load() != 2 {
		t.Fatalf("fallback failed: %v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e = fetchOpenAITimezone(ctx, "", server.Client(), []string{server.URL})
	if e == nil || calls.Load() != 2 {
		t.Fatal("cancelled request performed network I/O")
	}
}
func TestOpenAITimezoneDSTAndSentinelConsistency(t *testing.T) {
	for _, tc := range []struct {
		zone, at string
		offset   int
		fragment string
	}{
		{"America/Phoenix", "2026-10-15T05:20:00Z", 420, "GMT-0700"},
		{"America/New_York", "2026-03-08T06:59:59Z", 300, "GMT-0500"},
		{"America/New_York", "2026-03-08T07:00:01Z", 240, "GMT-0400"},
		{"Asia/Kolkata", "2026-10-15T05:20:00Z", -330, "GMT+0530"},
		{"Australia/Lord_Howe", "2026-10-15T05:20:00Z", -660, "GMT+1100"},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		s := openAITimezoneSnapshot{location: timezoneLocation(t, tc.zone), at: at}
		ctx := context.WithValue(context.Background(), openAITimezoneKey{}, s)
		p := map[string]any{}
		applyOpenAITimezone(ctx, p)
		if p["timezone"] != tc.zone || s.offset() != tc.offset || !strings.Contains(s.date(), tc.fragment) {
			t.Fatalf("bad timezone snapshot %+v %s", p, s.date())
		}
		tokens := []string{buildLegacyRequirementsToken("ua", nil, "build", s), buildProofToken("seed", "ff", "ua", nil, "build", s)}
		for _, token := range tokens {
			raw, e := base64.StdEncoding.DecodeString(token[7:])
			if e != nil {
				t.Fatal(e)
			}
			var fields []any
			if json.Unmarshal(raw, &fields) != nil {
				t.Fatal("invalid token")
			}
			if fields[1] != s.date() || int64(fields[17].(float64)) != at.UnixMilli() {
				t.Fatal("Sentinel date and epoch diverged")
			}
		}
	}
}
func TestOpenAITimezoneSnapshotPinnedButChangesWithEgress(t *testing.T) {
	phoenix, tokyo := timezoneLocation(t, "America/Phoenix"), timezoneLocation(t, "Asia/Tokyo")
	next := phoenix
	r := newOpenAITimezones(func(context.Context, string) (*time.Location, error) { return next, nil })
	client := NewOpenAIImage("http://upstream.invalid", nil, nil, time.Second)
	client.timezones = r
	ctx := client.withTimezone(proxyruntime.WithURL(context.Background(), "a"), accounts.Account{})
	next = tokyo
	original := openAITimezoneFrom(ctx)
	if got := openAITimezoneFrom(withOpenAITimezone(ctx, r, "a")); got != original {
		t.Fatal("in-flight timezone changed")
	}
	if got := openAITimezoneFrom(client.withTimezone(proxyruntime.WithURL(ctx, "b"), accounts.Account{})); got.location != tokyo {
		t.Fatal("new egress reused old timezone")
	}
}

// The browser transport previously skipped status validation entirely.
type timezoneBrowserStub struct {
	tlsclient.HttpClient
	do func(*fhttp.Request) (*fhttp.Response, error)
}

func (s timezoneBrowserStub) Do(r *fhttp.Request) (*fhttp.Response, error) { return s.do(r) }

type timezoneTrackedBody struct {
	io.Reader
	closed bool
}

func (b *timezoneTrackedBody) Close() error { b.closed = true; return nil }
func TestOpenAIImageBrowserHTTPErrorPreserved(t *testing.T) {
	body := &timezoneTrackedBody{Reader: strings.NewReader(`{"detail":"Model not found"}`)}
	p := NewOpenAIImage("https://chatgpt.com", nil, nil, time.Second)
	p.browsers[""] = &browserHTTP{client: timezoneBrowserStub{do: func(*fhttp.Request) (*fhttp.Response, error) {
		return &fhttp.Response{StatusCode: 404, Header: make(fhttp.Header), Body: body}, nil
	}}}
	_, _, err := p.start(context.Background(), accounts.Account{Token: "test"}, openAIRequirements{}, "conduit", "draw circle", "gpt-5-6", "auto", "auto", nil)
	var up *protocol.UpstreamError
	if !errors.As(err, &up) || up.Status != 404 || up.Endpoint != "/backend-api/f/conversation" || up.Body != `{"detail":"Model not found"}` || !body.closed {
		t.Fatalf("original error lost: %v", err)
	}
	if !IsModelRejected(err) {
		t.Fatal("model rejection no longer classifiable")
	}
}
func TestOpenAITimezoneAppliedToChatImageAndAccounts(t *testing.T) {
	var date string
	var conversations, offsets int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&p)
		}
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<html></html>`)
		case "/backend-api/sentinel/chat-requirements/prepare":
			raw, _ := base64.StdEncoding.DecodeString(p["p"].(string)[7:])
			var fields []any
			_ = json.Unmarshal(raw, &fields)
			date = fields[1].(string)
			if time.Since(time.UnixMilli(int64(fields[17].(float64)))).Abs() > 5*time.Second {
				t.Error("Sentinel reused stale task clock")
			}
			if !strings.Contains(date, "GMT-0700") {
				t.Errorf("bad Sentinel date %s", date)
			}
			fmt.Fprint(w, `{"prepare_token":"test"}`)
		case "/backend-api/sentinel/chat-requirements/finalize":
			fmt.Fprint(w, `{"token":"test"}`)
		case "/backend-api/f/conversation/prepare", "/backend-api/f/conversation", "/backend-api/conversation":
			conversations++
			if p["timezone"] != "America/Phoenix" || p["timezone_offset_min"] != float64(420) {
				t.Errorf("wrong conversation zone %+v", p)
			}
			if strings.HasSuffix(r.URL.Path, "/prepare") {
				fmt.Fprint(w, `{"conduit_token":"test"}`)
			} else {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"detail":"Model not found"}`)
			}
		case "/backend-api/me":
			fmt.Fprint(w, `{"email":"test@example.invalid","id":"test"}`)
		case "/backend-api/conversation/init":
			offsets++
			if p["timezone_offset_min"] != float64(420) {
				t.Error("quota offset mismatched")
			}
			fmt.Fprint(w, `{"limits_progress":[]}`)
		case "/backend-api/accounts/check/v4-2023-04-27":
			offsets++
			if r.URL.Query().Get("timezone_offset_min") != "420" {
				t.Error("account offset mismatched")
			}
			fmt.Fprint(w, `{"accounts":{}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	var lookups int
	resolver := newOpenAITimezones(func(context.Context, string) (*time.Location, error) {
		lookups++
		return timezoneLocation(t, "America/Phoenix"), nil
	})
	p := NewOpenAIImage(upstream.URL, upstream.Client(), nil, time.Second)
	p.timezones = resolver
	a := accounts.Account{Token: "test", Fields: map[string]any{"access_token": "test", "source_type": "chatgpt_web"}}
	oldCtx := p.withTimezone(context.Background(), a)
	oldSnapshot := openAITimezoneFrom(oldCtx)
	oldSnapshot.at = time.Now().Add(-time.Hour)
	oldCtx = context.WithValue(oldCtx, openAITimezoneKey{}, oldSnapshot)
	_, _, _ = NewOpenAIChat(p).Complete(oldCtx, a, protocol.ChatRequest{Model: "auto", Messages: []protocol.Message{{Role: "user", Content: "hi"}}})
	_, err := p.Generate(context.Background(), a, "circle", "gpt-5-6", "auto", "auto", nil)
	if !IsModelRejected(err) {
		t.Fatalf("image error lost: %v", err)
	}
	// Export helpers consume the same immutable request snapshot.
	ctx := p.withTimezone(context.Background(), a)
	_, _ = p.prepareEditable(ctx, a, openAIRequirements{}, "ppt", nil)
	_, _ = p.startEditable(ctx, a, openAIRequirements{}, "test", "ppt", nil)
	c := NewOpenAIAccountClient(upstream.URL, "", upstream.Client(), nil)
	c.timezones = resolver
	if _, err := c.RefreshAccount(context.Background(), a.Fields); err != nil {
		t.Fatal(err)
	}
	if conversations != 5 || offsets != 2 || lookups != 1 {
		t.Fatalf("unexpected calls conversation=%d account=%d lookup=%d", conversations, offsets, lookups)
	}
}

func TestAuthenticationTimezoneSharesEgressCache(t *testing.T) {
	old := sharedOpenAITimezones
	defer func() { sharedOpenAITimezones = old }()
	var selected []string
	phoenix := timezoneLocation(t, "America/Phoenix")
	tokyo := timezoneLocation(t, "Asia/Tokyo")
	sharedOpenAITimezones = newOpenAITimezones(func(_ context.Context, proxy string) (*time.Location, error) {
		selected = append(selected, proxy)
		if proxy != "" {
			return tokyo, nil
		}
		return phoenix, nil
	})
	ctx := context.Background()
	if ResolveOpenAITimezone(ctx, "https://chat.example.test", "direct") != nil || len(selected) != 0 {
		t.Fatal("custom origin contacted geolocation")
	}
	if ResolveOpenAITimezone(ctx, "https://chatgpt.com", "direct") != phoenix {
		t.Fatal("direct lookup failed")
	}
	// Simulate a subsequent chat/image request through its existing entry point.
	if openAITimezonesFor("https://chatgpt.com/backend-api").resolve(ctx, "") != phoenix || len(selected) != 1 {
		t.Fatal("authentication and provider did not share cache")
	}
	proxy := "http://proxy.example.test:8080"
	if ResolveOpenAITimezone(ctx, "https://chatgpt.com", proxy) != tokyo || len(selected) != 2 || selected[1] != proxy {
		t.Fatal("proxy egress not forwarded or isolated")
	}
}
