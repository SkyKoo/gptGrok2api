package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGrokMediaQuotaRefreshPersistsAndClearsStaleDataIndependently(t *testing.T) {
	var denied atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/media/imagine/quota_info" {
			if denied.Load() {
				http.Error(w, "private-cookie invalid-credentials", 403)
				return
			}
			_, _ = w.Write([]byte(`{"imagePro":{"available":true,"remainingQueries":4},"video720p":{"available":true,"remainingQueries":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"remainingQueries":30,"totalQueries":30}`))
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.GrokRateLimitsURL = upstream.URL + "/rest/rate-limits"
	s := New(cfg)
	account, _, err := s.registerStore.AddAccount(map[string]any{"sso": "test-media-secret", "email": "test@example.test", "enabled": true, "status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	id := stringValue(account["id"])
	refresh := func() {
		t.Helper()
		r := adminRequest(s.Handler(), "POST", "/api/register/grok/accounts/runtime/refresh", strings.NewReader(`{"ids":["`+id+`"]}`))
		if r.Code != 200 {
			t.Fatalf("refresh: %d %s", r.Code, r.Body.String())
		}
	}
	list := func() map[string]any {
		t.Helper()
		r := adminRequest(s.Handler(), "GET", "/api/register/grok/accounts", nil)
		if strings.Contains(r.Body.String(), "test-media-secret") || strings.Contains(r.Body.String(), "private-cookie") {
			t.Fatal("credential leaked")
		}
		var body struct {
			Items []map[string]any `json:"items"`
		}
		if json.Unmarshal(r.Body.Bytes(), &body) != nil || len(body.Items) != 1 {
			t.Fatal("list failed")
		}
		return body.Items[0]
	}
	refresh()
	item := list()
	media := item["media_quota"].(map[string]any)
	if media["status"] != "success" || len(media["quotas"].(map[string]any)) != 2 {
		t.Fatalf("media missing: %#v", media)
	}
	// A later media denial cannot retain the old 4/1 counters or disable text.
	denied.Store(true)
	refresh()
	item = list()
	media = item["media_quota"].(map[string]any)
	if media["status"] != "failed" || media["error"] != "HTTP 403" || len(media["quotas"].(map[string]any)) != 0 {
		t.Fatalf("stale media survived: %#v", media)
	}
	if item["status"] != "active" || item["quota"].(map[string]any)["fast"].(map[string]any)["remaining"] != float64(30) {
		t.Fatal("media error changed text status/quota")
	}
	rows, _ := s.grokRuntimeStore.AccountList()
	if len(rows) != 1 || rows[0]["media_quota"] == nil {
		t.Fatal("runtime media observation missing")
	}
	_, _ = s.registerStore.SetDisabled([]string{id}, true)
	_, _ = s.grokRuntimeStore.UpdateAccountRuntime("test-media-secret", map[string]any{"enabled": false, "status": "disabled"})
	denied.Store(false)
	refresh()
	item = list()
	if item["status"] != "disabled" {
		t.Fatal("media refresh re-enabled manually disabled archive")
	}
	rows, _ = s.grokRuntimeStore.AccountList()
	if rows[0]["enabled"] != false {
		t.Fatal("refresh re-enabled runtime")
	}
}
