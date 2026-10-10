package httpapi

import (
	"encoding/json"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDynamicModelRoutesBothAPIsAndKeepsRequestedModel(t *testing.T) {
	s, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		if v["model"] != "gpt-future" {
			t.Errorf("model silently changed: %v", v["model"])
		}
		if r.Header.Get("Authorization") != "Bearer second" {
			t.Error("selected an account without requested model")
		}
		fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["ok"]}}}`)
		fmt.Fprintln(w, "data: [DONE]")
	})
	rows, _ := s.store.AccountList()
	a := rows[1]
	err := s.discovery.Record(accounts.Identity(accounts.Account{Token: accountToken(a), Fields: a}), "auto", map[string]any{"models": []any{map[string]any{"slug": "gpt-future", "title": "Future"}, map[string]any{"slug": "research", "title": "Research"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		body := multimodalRequestBody(t, path == "/v1/responses", true, false)
		body = strings.ReplaceAll(body, `"auto"`, `"gpt-future"`)
		got := invokeMultimodal(s, path, body)
		if got.Code != 200 || !strings.Contains(got.Body.String(), "ok") {
			t.Fatalf("%s %d %s", path, got.Code, got.Body.String())
		}
	}
	var publicIDs, adminIDs []string
	for _, path := range []string{"/v1/models", "/v1/models/gpt-future", "/api/model-catalog"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer admin-secret")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "gpt-future") {
			t.Fatalf("directory inconsistent %s: %d", path, w.Code)
		}
		var payload map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &payload)
		if path == "/v1/models" {
			for _, raw := range payload["data"].([]any) {
				publicIDs = append(publicIDs, raw.(map[string]any)["id"].(string))
			}
		}
		if path == "/api/model-catalog" {
			for _, raw := range payload["all_models"].([]any) {
				adminIDs = append(adminIDs, raw.(string))
			}
		}
		for _, secret := range []string{"Bearer second", "access_token", "account_ref"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("identity leaked in catalog")
			}
		}
	}
	sort.Strings(publicIDs)
	sort.Strings(adminIDs)
	if !reflect.DeepEqual(publicIDs, adminIDs) {
		t.Fatalf("public/admin catalogs differ: %v %v", publicIDs, adminIDs)
	}
	if _, ok := s.resolveChatModel("research"); ok {
		t.Fatal("research enabled as ordinary chat")
	}
	if _, ok := s.resolveChatModel("gpt-invented"); ok {
		t.Fatal("invented model accepted")
	}
	if _, ok := s.resolveChatModel("gpt-5-3"); !ok {
		t.Fatal("verified compatibility entry lost")
	}
}
