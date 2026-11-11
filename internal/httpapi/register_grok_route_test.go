package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func TestRegisteredGrokDailyRouteFollowsDefaultAndSurvivesSync(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Egress") != "default" {
			t.Error("daily request bypassed the configured default proxy")
		}
		if !strings.Contains(r.Header.Get("Cookie"), "sso=route-test-session") {
			t.Error("daily request lost account credentials")
		}
		switch r.URL.Path {
		case "/rate-limits":
			fmt.Fprint(w, `{"remainingQueries":30,"totalQueries":30}`)
		case "/chat":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: [DONE]\n")
		case "/video.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			fmt.Fprint(w, "fixture-video")
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer up.Close()
	var firstCalls, secondCalls atomic.Int32
	forward := func(counter *atomic.Int32) *httptest.Server {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			req := r.Clone(r.Context())
			req.RequestURI = ""
			req.Header.Set("X-Test-Egress", "default")
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Error(err)
				w.WriteHeader(502)
				return
			}
			defer resp.Body.Close()
			for key, values := range resp.Header {
				w.Header()[key] = values
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		}))
		t.Cleanup(proxy.Close)
		return proxy
	}
	first, second := forward(&firstCalls), forward(&secondCalls)
	cfg := adminTestConfig(t.TempDir())
	cfg.ProxyURL = first.URL
	cfg.GrokRateLimitsURL = up.URL + "/rate-limits"
	cfg.MediaChatURL = up.URL + "/chat"
	s := New(cfg)
	signup := map[string]any{
		"sso": "route-test-session", "cookie_header": "sso=route-test-session; sso-rw=route-test-session",
		"proxy":           "http://signup-user:signup-secret@127.0.0.1:3",
		"proxy_binding":   map[string]any{"reference": "http://signup-user:signup-secret@127.0.0.1:3", "platform": "grok", "purpose": "registration"},
		"browser_profile": map[string]any{"user-agent": "registration-profile"},
	}
	original, _ := json.Marshal(signup)
	if err := s.importRegisteredGrokAccount(context.Background(), "job-inherit", signup); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := json.Marshal(signup)
	if string(unchanged) != string(original) {
		t.Fatal("import changed registration task route or credentials")
	}
	archive, _ := s.registerStore.ExportAccounts(nil)
	if len(archive) != 1 || archive[0]["proxy_policy"] != "inherit" || archive[0]["proxy_binding"] != nil || boolValue(archive[0]["enabled"], true) {
		t.Fatal("new account must follow the default and still await verification")
	}
	if err := s.verifyRegisteredGrokAccount(context.Background(), "job-inherit", signup); err != nil {
		t.Fatal(err)
	}
	runtime, _ := s.grokRuntimeStore.AccountList()
	if len(runtime) != 1 || runtime[0]["proxy_policy"] != "inherit" || runtime[0]["proxy_binding"] != nil || !boolValue(runtime[0]["enabled"], false) {
		t.Fatal("verified account did not enter runtime with inherited route")
	}
	if firstCalls.Load() != 1 {
		t.Fatal("registration verification did not use the daily default")
	}
	s.proxyManager.SetDefault(second.URL, nil)
	for _, resource := range []bool{false, true} {
		if s.proxyManager.Resolve(runtime[0], resource) != second.URL {
			t.Fatal("daily request did not follow the changed default")
		}
	}
	a := accounts.Account{Token: "route-test-session", Fields: runtime[0]}
	response, err := s.mediaProvider.StreamChat(context.Background(), a, map[string]any{"message": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	body, mime, err := s.mediaProvider.Fetch(context.Background(), a, up.URL+"/video.mp4")
	if err != nil || mime != "video/mp4" || string(body) != "fixture-video" {
		t.Fatalf("proxied resource fetch failed: mime=%s err=%v", mime, err)
	}
	if secondCalls.Load() != 2 {
		t.Fatal("media generation or download did not use the changed default")
	}
	// A later explicit choice remains authoritative during registration recovery.
	id := stringValue(archive[0]["id"])
	reqBody, _ := json.Marshal(map[string]any{"platform": "grok", "account_id": id, "reference": "http://127.0.0.1:4", "reason": "explicit daily route"})
	req := httptest.NewRequest(http.MethodPost, "/api/proxy/binding", strings.NewReader(string(reqBody)))
	req.Header.Set("Authorization", "Bearer admin-secret")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatalf("rebind status %d", res.Code)
	}
	if err := s.importRegisteredGrokAccount(context.Background(), "job-inherit", signup); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.syncRegisteredGrokRuntimeAccount(archive[0], "route-test-session"); err != nil {
		t.Fatal(err)
	}
	runtime, _ = s.grokRuntimeStore.AccountList()
	binding := proxyruntime.ParseBinding(runtime[0]["proxy_binding"])
	if binding == nil || binding.Reference != "http://127.0.0.1:4" || len(runtime[0]["proxy_binding_history"].([]any)) != 1 {
		t.Fatal("recovery replaced explicit daily route/history")
	}
}
