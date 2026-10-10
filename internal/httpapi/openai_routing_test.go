package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
)

func discoverTestModel(t *testing.T, s *Server, slug string) {
	t.Helper()
	rows, _ := s.store.AccountList()
	for _, a := range rows {
		if e := s.discovery.Record(accounts.Identity(accounts.Account{Token: accountToken(a), Fields: a}), "auto", map[string]any{"models": []any{map[string]any{"slug": slug, "title": slug}}}, nil); e != nil {
			t.Fatal(e)
		}
	}
}
func TestRoutingConfigValidationAndMaintenance(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	if got := imageTaskCall(s, "POST", "/api/openai/routing", "api-secret", `{"image_conversation_model":"auto"}`); got.Code != 401 && got.Code != 403 {
		t.Fatal("ordinary token can change routing")
	}
	get := func() string {
		m, e := s.imageConversationModel()
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	if get() != "gpt-5-3" {
		t.Fatal("default changed")
	}
	_, _ = s.store.UpdateConfig("preserved", true)
	for _, slug := range []string{"gpt-invented", "gpt-image-2", "grok-4.3-console", "research"} {
		w := adminRequest(s.Handler(), "POST", "/api/openai/routing", strings.NewReader(`{"image_conversation_model":"`+slug+`"}`))
		if w.Code != 400 {
			t.Fatalf("invalid selection accepted: %s %d", slug, w.Code)
		}
	}
	w := adminRequest(s.Handler(), "POST", "/api/openai/routing", strings.NewReader(`{"image_conversation_model":"auto"}`))
	if w.Code != 200 || get() != "auto" {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	if w := adminRequest(s.Handler(), "POST", "/api/settings", strings.NewReader(`{"openai_routing":{"image_conversation_model":"gpt-invented"},"log_level":"INFO"}`)); w.Code != 200 {
		t.Fatalf("general settings failed: %s", w.Body.String())
	}
	if get() != "auto" {
		t.Fatal("stale general settings replaced model route")
	}
	cfg, _ := s.store.Config()
	if cfg["preserved"] != true {
		t.Fatal("unrelated configuration lost")
	}
	if e := os.MkdirAll(s.cfg.DataDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(s.cfg.DataDir, "cfm-maintenance"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/images/generations", "/api/image-tasks/edits", "/api/accounts/models/refresh", "/api/openai/routing"} {
		w := adminRequest(s.Handler(), "POST", path, strings.NewReader(`{}`))
		if w.Code != 503 || w.Header().Get("Retry-After") == "" {
			t.Fatalf("maintenance admitted %s: %d", path, w.Code)
		}
	}
	if w := adminRequest(s.Handler(), "GET", "/api/openai/routing", nil); w.Code != 200 {
		t.Fatal("status unavailable during drain")
	}
	if w := adminRequest(s.Handler(), "POST", "/api/register/openai/survival", strings.NewReader(`{"enabled":false}`)); w.Code != 200 {
		t.Fatalf("cannot pause survival %d %s", w.Code, w.Body.String())
	}
}
func TestModelRejectionRetriesSameModelAndLogsActualModel(t *testing.T) {
	var calls atomic.Int32
	var rejectedToken string
	s, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		if v["model"] != "gpt-6" {
			t.Error("silent cross-model fallback")
		}
		if calls.Add(1) == 1 {
			rejectedToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"code":"model_not_found"}}`)
			return
		}
		fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"metadata":{"model_slug":"gpt-6-mini"},"content":{"parts":["ok"]}}}`)
		fmt.Fprintln(w, "data: [DONE]")
	})
	s.cfg.ChatMaxRetries = 1
	discoverTestModel(t, s, "gpt-6")
	got := invokeMultimodal(s, "/v1/chat/completions", strings.ReplaceAll(multimodalRequestBody(t, false, false, false), `"auto"`, `"gpt-6"`))
	if got.Code != 200 || calls.Load() != 2 {
		t.Fatalf("retry failed %d %s calls=%d", got.Code, got.Body.String(), calls.Load())
	}
	rows, _ := s.store.AccountList()
	for _, a := range rows {
		if accountToken(a) == rejectedToken {
			id := accounts.Identity(accounts.Account{Token: rejectedToken, Fields: a})
			if s.discovery.Supports(id, "gpt-6") || !s.discovery.Supports(id, "auto") {
				t.Fatal("model rejection not scoped")
			}
		}
	}
	logs := s.loadCallLogs()
	raw, _ := json.Marshal(logs)
	for _, field := range []string{`"requested_model":"gpt-6"`, `"sent_model":"gpt-6"`, `"upstream_model":"gpt-6-mini"`, `same_model_account_retry`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("missing routing evidence %s: %s", field, raw)
		}
	}
}
