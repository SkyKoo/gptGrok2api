package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
)

func TestGrokInventoryJoinsLiveUsageBeforePagination(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	first, _, err := s.registerStore.AddAccount(map[string]any{"id": "a", "email": "ONE@example.test", "sso": "sso-private-one", "password": "private-password", "status": "active", "created_at": "2026-10-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.registerStore.AddAccount(map[string]any{"id": "b", "email": "two@example.test", "sso": "sso-private-two", "status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.registerStore.UpdateRuntime(stringValue(first["id"]), map[string]any{"status": "active", "present": true}, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = s.grokRuntimeStore.AddAccounts(nil, []map[string]any{
		{"access_token": "sso-private-one", "enabled": false, "status": "disabled", "use_count": 7, "fail_count": 2, "last_used_at": "2026-11-02T01:02:03Z", "media_quota": testGrokMediaSnapshot()},
		{"access_token": "sso-private-two", "enabled": true, "status": "active"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.oauthStore.Import(oauth.Account{Email: "one@example.test", AccessToken: "oauth-private-token", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	response := adminRequest(s.Handler(), http.MethodGet, "/api/register/grok/accounts?page=1&page_size=1&keyword=ONE@example.test", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, secret := range []string{"sso-private-one", "sso-private-two", "private-password", "oauth-private-token"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("credential leaked")
		}
	}
	var payload struct {
		Items    []map[string]any `json:"items"`
		Summary  map[string]any   `json:"summary"`
		Total    int              `json:"total"`
		AllTotal int              `json:"all_total"`
	}
	payload.Items = nil
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 || payload.Total != 1 || payload.AllTotal != 2 {
		t.Fatalf("bad pagination: %#v", payload)
	}
	row := payload.Items[0]
	if row["use_count"] != float64(7) || row["fail_count"] != float64(2) || row["last_used_at"] != "2026-11-02T01:02:03Z" {
		t.Fatal("live usage not joined")
	}
	if row["created_at"] != "2026-10-01T00:00:00Z" || row["runtime_status"] != "disabled" || row["oauth"] == nil || row["media_quota"] == nil {
		t.Fatal("missing metadata or stale archive status")
	}
	if payload.Summary["runtime_total"] != float64(2) || payload.Summary["synced"] != float64(2) || payload.Summary["oauth_linked"] != float64(1) {
		t.Fatalf("summary incorrectly filtered: %#v", payload.Summary)
	}
	response = adminRequest(s.Handler(), http.MethodGet, "/api/register/grok/accounts?status=disabled", nil)
	if !strings.Contains(response.Body.String(), stringValue(first["id"])) || strings.Contains(response.Body.String(), stringValue(second["email"])) {
		t.Fatal("runtime filter not applied")
	}
	response = adminRequest(s.Handler(), http.MethodGet, "/api/register/grok/accounts?keyword=two@example.test", nil)
	payload.Items = nil
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, found := payload.Items[0]["use_count"]; found {
		t.Fatal("unknown usage fabricated as zero")
	}
	_, _, err = s.grokRuntimeStore.DeleteAccounts([]string{"sso-private-one"})
	if err != nil {
		t.Fatal(err)
	}
	response = adminRequest(s.Handler(), http.MethodGet, "/api/register/grok/accounts?keyword=ONE@example.test", nil)
	payload.Items = nil
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Items[0]["sync_state"] != "not_synced" {
		t.Fatal("stale archive membership shown")
	}
}

func TestGrokSyncReturnsUsableSummaryWithoutClearingDisabledState(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	item, _, err := s.registerStore.AddAccount(map[string]any{"id": "sync-me", "email": "sync@example.test", "sso": "sync-test-token", "status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "sync-test-token", "status": "disabled", "enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	response := adminRequest(s.Handler(), http.MethodPost, "/api/register/grok/accounts/sync", strings.NewReader(`{"ids":["`+stringValue(item["id"])+`","missing"]}`))
	var payload struct {
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Summary["ok"] != 1 || payload.Summary["fail"] != 1 {
		t.Fatal(response.Body.String())
	}
	rows, err := s.grokRuntimeStore.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	if rows[0]["enabled"] != false || rows[0]["status"] != "disabled" {
		t.Fatal("sync restored disabled account")
	}
}
