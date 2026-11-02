package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
)

func callGrokWeb(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	method := "POST"
	if body == "" {
		method = "GET"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, req)
	return out
}
func TestGrokWebDirectoryAndRoutingUseOfficialAccountCapabilities(t *testing.T) {
	var metadata, chat atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/modes" {
			metadata.Add(1)
			availability := `"requiresUpgrade":{"minimumSubscriptionTier":"TIER_SUPERGROK_LITE"}`
			if strings.Contains(r.Header.Get("Cookie"), "sso=paid") {
				availability = `"available":{}`
			}
			fmt.Fprintf(w, `{"modes":[{"id":"fast","title":"快速","description":"快速回复","availability":{"available":{}}},{"id":"expert","title":"专家","description":"深度思考 · Grok 4.7","availability":{%s}}]}`, availability)
			return
		}
		chat.Add(1)
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		if payload["modeId"] != "expert" || payload["modelName"] != nil || !strings.Contains(r.Header.Get("Cookie"), "sso=paid") {
			t.Errorf("incorrect request or selected account: %#v", payload)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"result":{"response":{"token":"official route","messageTag":"final"}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()
	cfg := chatTestConfig(t, t.TempDir(), up.URL+"/chat")
	cfg.GrokModesURL = up.URL + "/rest/modes"
	s := New(cfg)
	t.Cleanup(func() { s.grokRuntimeStore.FlushAccounts() })
	_, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "free", "pool": "basic"}, {"access_token": "paid", "pool": "basic"}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := callGrokWeb(t, s, "/api/model-catalog", "")
	if catalog.Code != 200 || !strings.Contains(catalog.Body.String(), "Grok 4.7") || chat.Load() != 0 {
		t.Fatalf("bad directory: %d %s", catalog.Code, catalog.Body.String())
	}
	var directory map[string]any
	json.Unmarshal(catalog.Body.Bytes(), &directory)
	for _, id := range directory["chat_models"].([]any) {
		if strings.HasPrefix(id.(string), "grok-4") {
			t.Fatalf("legacy fabricated version listed: %s", id)
		}
	}
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"grok-web-expert","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/chat/completions", `{"model":"grok-4.20-expert","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"grok-4.20-expert","input":"hi"}`},
		{"/v1/messages", `{"model":"grok-web-expert","messages":[{"role":"user","content":"hi"}]}`},
	} {
		out := callGrokWeb(t, s, tc.path, tc.body)
		if out.Code != 200 || !strings.Contains(out.Body.String(), "official route") || !strings.Contains(out.Body.String(), "grok-web-expert") || strings.Contains(out.Body.String(), `"model":"grok-4.20`) {
			t.Fatalf("%s failed %d %s", tc.path, out.Code, out.Body.String())
		}
	}
	if metadata.Load() != 2 || chat.Load() != 4 {
		t.Fatalf("unexpected calls: modes %d chat %d", metadata.Load(), chat.Load())
	}
	public := callGrokWeb(t, s, "/v1/models", "")
	if !strings.Contains(public.Body.String(), "Grok 4.7") || strings.Contains(public.Body.String(), "sso=") {
		t.Fatal("catalog lost description or leaked credential")
	}
	// A used-up expert window must not borrow the same account's Fast quota.
	s.grokRuntimeStore.UpdateAccountRuntime("paid", map[string]any{"expert_quota": map[string]any{"remaining": 0, "reset_at": time.Now().Add(time.Hour).Unix()}, "fast_quota": map[string]any{"remaining": 99}})
	out := callGrokWeb(t, s, "/v1/chat/completions", `{"model":"grok-web-expert","messages":[{"role":"user","content":"hi"}]}`)
	if out.Code != 429 || chat.Load() != 4 {
		t.Fatalf("used exhausted mode: %d %s", out.Code, out.Body.String())
	}
}

func TestGrokWebUnknownOrUnavailableModesNeverGenerate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"upgrade", 200, `{"modes":[{"id":"expert","availability":{"requiresUpgrade":{}}}]}`},
		{"missing_availability", 200, `{"modes":[{"id":"expert"}]}`},
		{"unavailable", 200, `{"modes":[{"id":"expert","availability":{"unavailable":{}}}]}`},
		{"invalid_json", 200, `<!doctype html>SECRET`},
		{"refused", 403, `SECRET`},
		{"invalid_credentials", 401, `SECRET`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queried, generated atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/modes" {
					queried.Add(1)
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				generated.Add(1)
			}))
			defer up.Close()
			cfg := chatTestConfig(t, t.TempDir(), up.URL+"/chat")
			cfg.GrokModesURL = up.URL + "/modes"
			s := New(cfg)
			t.Cleanup(func() { s.grokRuntimeStore.FlushAccounts() })
			s.grokRuntimeStore.AddAccounts([]string{"one"}, nil)
			for n := 0; n < 2; n++ {
				out := callGrokWeb(t, s, "/v1/chat/completions", `{"model":"grok-web-expert","messages":[{"role":"user","content":"hi"}]}`)
				if out.Code != 429 || strings.Contains(out.Body.String(), "SECRET") {
					t.Fatalf("unsafe directory failure %d %s", out.Code, out.Body.String())
				}
			}
			if queried.Load() != 1 || generated.Load() != 0 {
				t.Fatalf("unexpected calls: metadata=%d generated=%d", queried.Load(), generated.Load())
			}
			rows, _ := s.grokRuntimeStore.AccountList()
			if accounts.IsAbnormal(rows[0]) != (tc.status == 401) {
				t.Fatal("metadata error wrongly changed account credential state")
			}
		})
	}
}

func TestGrokWebStaleSnapshotsLoseVersionAndCannotOverrideDisabled(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"modes":[{"id":"fast","title":"快速","description":"Grok future","availability":{"available":{}}}]}`)
	}))
	defer up.Close()
	cfg := chatTestConfig(t, t.TempDir(), up.URL)
	cfg.GrokModesURL = up.URL
	s := New(cfg)
	t.Cleanup(func() { s.grokRuntimeStore.FlushAccounts() })
	c, _ := model.ParseGrokWebCatalog([]byte(`{"modes":[{"id":"fast","title":"快速","description":"Grok obsolete","availability":{"available":{}}}]}`))
	c.CheckedAt = time.Now().Add(-7 * time.Hour)
	s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "test", "grok_web_catalog": c}})
	for _, spec := range s.modelSpecs() {
		if strings.Contains(spec.Name, "obsolete") {
			t.Fatal("stale version still displayed")
		}
	}
	done := make(chan error, 1)
	go func() { done <- s.refreshGrokWebModels(context.Background(), accounts.Account{Token: "test"}) }()
	<-started
	if err := s.refreshGrokWebModels(context.Background(), accounts.Account{Token: "test"}); err != accounts.ErrRefreshDeferred {
		t.Fatal("concurrent directory query was not excluded")
	}
	s.grokRuntimeStore.UpdateAccountRuntime("test", map[string]any{"enabled": false, "status": "disabled"})
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(s.grokWebAccounts()) != 0 {
		t.Fatal("late directory fetch re-enabled account")
	}
}

func TestGrokWebRetryDiscoversAnAccountAfterFirstIsExcluded(t *testing.T) {
	var generated, queried atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/modes" {
			queried.Add(1)
			fmt.Fprint(w, `{"modes":[{"id":"fast","availability":{"available":{}}}]}`)
			return
		}
		generated.Add(1)
		if strings.Contains(r.Header.Get("Cookie"), "sso=first") {
			w.WriteHeader(429)
			return
		}
		fmt.Fprintln(w, `data: {"result":{"response":{"token":"second account","messageTag":"final"}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()
	cfg := chatTestConfig(t, t.TempDir(), up.URL+"/chat")
	cfg.GrokModesURL = up.URL + "/modes"
	s := New(cfg)
	t.Cleanup(func() { s.grokRuntimeStore.FlushAccounts() })
	c, _ := model.ParseGrokWebCatalog([]byte(`{"modes":[{"id":"fast","availability":{"available":{}}}]}`))
	c.CheckedAt = time.Now()
	s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "first", "grok_web_catalog": c}, {"access_token": "second"}})
	out := callGrokWeb(t, s, "/v1/chat/completions", `{"model":"grok-web-fast","messages":[{"role":"user","content":"hi"}]}`)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "second account") || generated.Load() != 2 || queried.Load() != 1 {
		t.Fatalf("retry failed %d %s metadata=%d generated=%d", out.Code, out.Body.String(), queried.Load(), generated.Load())
	}
}

func TestGrokWebQuotaUnknownAndExpiredAreNotZero(t *testing.T) {
	c, _ := model.ParseGrokWebCatalog([]byte(`{"modes":[{"id":"fast","availability":{"available":{}}}]}`))
	c.CheckedAt = time.Now()
	a := accounts.Account{Fields: map[string]any{"grok_web_catalog": c}}
	for _, q := range []map[string]any{{}, {"remaining": nil}, {"remaining": -1}, {"remaining": 0, "reset_at": time.Now().Add(-time.Minute).Unix()}} {
		a.Fields["fast_quota"] = q
		if !grokWebChatAvailable(a, "fast", time.Now()) {
			t.Fatalf("unknown/expired quota treated as exhausted: %v", q)
		}
	}
	a.Fields["cooldown_until"] = time.Now().Add(time.Minute).Format(time.RFC3339Nano)
	if grokWebChatAvailable(a, "fast", time.Now()) {
		t.Fatal("cooldown did not affect mode availability")
	}
}
