package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/config"
)

func sessionImportTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	root := t.TempDir()
	return New(config.Config{Version: "test", RootDir: root, DataDir: filepath.Join(root, "data"), AccountsPath: filepath.Join(root, "data/accounts.json"), AuthKeysPath: filepath.Join(root, "data/keys.json"), ConfigPath: filepath.Join(root, "config.json"), StaticDir: filepath.Join(root, "web_dist"), AdminKey: "fixture-admin", APIKey: "fixture-api", OpenAIBaseURL: upstream, OpenAIOAuthURL: upstream + "/token"})
}

func sessionImportCall(t *testing.T, s *Server, session map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"session": session})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/import-session", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer fixture-admin")
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, req)
	var result map[string]any
	if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-session-token", "fixture-secret", "injected-secret"} {
		if strings.Contains(out.Body.String(), secret) {
			t.Fatal("import response leaked a credential")
		}
	}
	if out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("import response must not be cached")
	}
	return out.Code, result
}

func TestSessionImportPersistsVerifiesAndFindsAccount(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-session-token" {
			t.Error("session AT not used")
		}
		w.Header().Set("Content-Type", "application/json")
		if fail.Load() {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"fixture-session-token unavailable"}}`)
			return
		}
		switch r.URL.Path {
		case "/backend-api/me":
			fmt.Fprint(w, `{"email":"person@example.test","id":"user-session"}`)
		case "/backend-api/conversation/init":
			fmt.Fprint(w, `{"limits_progress":[{"feature_name":"image_gen","remaining":7}]}`)
		case "/backend-api/accounts/check/v4-2023-04-27":
			fmt.Fprint(w, `{"accounts":{"default":{"account":{"plan_type":"plus"}}}}`)
		default:
			t.Error("unexpected upstream route")
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	s := sessionImportTestServer(t, upstream.URL)
	seeds := []map[string]any{}
	for i := 0; i < 21; i++ {
		seeds = append(seeds, map[string]any{"access_token": fmt.Sprintf("old-%d", i), "email": fmt.Sprintf("old-%d@example.test", i)})
	}
	s.store.AddAccounts(nil, seeds)
	session := map[string]any{"accessToken": "fixture-session-token", "user": map[string]any{"email": "person@example.test", "id": "user-session"}, "expires": "2027-01-01T00:00:00Z", "refresh_token": "injected-secret", "two_factor_secret": "injected-secret", "proxy": "injected-secret", "enabled": false}
	status, result := sessionImportCall(t, s, session)
	if status != 200 || result["added"] != float64(1) || result["refreshed"] != float64(1) || result["refresh_error"] != "" || calls.Load() != 3 {
		t.Fatalf("import did not verify: status=%d result=%v calls=%d", status, result, calls.Load())
	}
	rows, _ := s.store.AccountList()
	account := rows[21]
	if len(rows) != 22 || account["email"] != "person@example.test" || account["user_id"] != "user-session" || account["type"] != "plus" || intValue(account["quota"]) != 7 || account["enabled"] != true {
		t.Fatal("identity, plan, quota or enabled state missing")
	}
	for _, key := range []string{"refresh_token", "two_factor_secret", "proxy", "user"} {
		if account[key] != nil {
			t.Fatalf("unexpected imported field %s", key)
		}
	}
	disk, err := os.ReadFile(s.cfg.AccountsPath)
	if err != nil || !bytes.Contains(disk, []byte("person@example.test")) {
		t.Fatal("identity not durable")
	}
	for _, keyword := range []string{"person@example.test", stringValue(result["account_ref"])} {
		req := httptest.NewRequest("GET", "/api/accounts?page=1&page_size=20&keyword="+url.QueryEscape(keyword), nil)
		req.Header.Set("Authorization", "Bearer fixture-admin")
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		var list map[string]any
		json.Unmarshal(out.Body.Bytes(), &list)
		if out.Code != 200 || list["total"] != float64(1) || len(list["items"].([]any)) != 1 {
			t.Fatal("cannot find imported account on first filtered page")
		}
	}
	// Re-import the same token fills legacy identity and preserves management data.
	s.store.UpdateAccount("fixture-session-token", map[string]any{"email": nil, "user_id": nil, "two_factor_secret": "fixture-secret", "group_id": "saved-group", "proxy": "direct", "enabled": false})
	status, result = sessionImportCall(t, s, session)
	if status != 200 || result["added"] != float64(0) || result["updated"] != float64(1) || result["refreshed"] != float64(1) {
		t.Fatal("duplicate import did not update")
	}
	rows, _ = s.store.AccountList()
	account = rows[21]
	if len(rows) != 22 || account["email"] != "person@example.test" || account["two_factor_secret"] != "fixture-secret" || account["group_id"] != "saved-group" || account["proxy"] != "direct" || account["enabled"] != false {
		t.Fatal("duplicate import lost account settings")
	}
	// A transient upstream failure is a partial success, never a deletion.
	fail.Store(true)
	status, result = sessionImportCall(t, s, session)
	if status != 200 || result["updated"] != float64(1) || result["refreshed"] != float64(0) || result["refresh_error"] == "" {
		t.Fatal("refresh failure falsely reported success")
	}
	rows, _ = s.store.AccountList()
	if len(rows) != 22 || rows[21]["two_factor_secret"] != "fixture-secret" {
		t.Fatal("failed verification lost saved account")
	}
}

func TestSessionImportRejectsInvalidInputAndPreservesUnverifiedAccount(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"expired"}`)
	}))
	defer upstream.Close()
	s := sessionImportTestServer(t, upstream.URL)
	for _, session := range []map[string]any{nil, {}, {"accessToken": 123}, {"accessToken": "fixture-session-token", "email": "one@example.test", "user": map[string]any{"email": "two@example.test"}}} {
		code, _ := sessionImportCall(t, s, session)
		if code != 400 {
			t.Fatal("invalid session accepted")
		}
	}
	rows, _ := s.store.AccountList()
	if len(rows) != 0 || calls.Load() != 0 {
		t.Fatal("invalid input produced writes or upstream calls")
	}
	code, result := sessionImportCall(t, s, map[string]any{"accessToken": "fixture-session-token", "user": map[string]any{"email": "person@example.test", "id": "user-session"}})
	if code != 200 || result["added"] != float64(1) || result["refreshed"] != float64(0) || result["refresh_error"] == "" {
		t.Fatal("invalid token result not explicit")
	}
	rows, _ = s.store.AccountList()
	if len(rows) != 1 || rows[0]["email"] != "person@example.test" || accountStatusCategory(rows[0]) != "abnormal" {
		t.Fatal("unverified account not retained and blocked")
	}
	before := calls.Load()
	code, _ = sessionImportCall(t, s, map[string]any{"accessToken": "fixture-session-token", "email": "other@example.test"})
	if code != 409 || calls.Load() != before {
		t.Fatal("saved identity conflict not rejected before upstream")
	}
	rows, _ = s.store.AccountList()
	if rows[0]["email"] != "person@example.test" {
		t.Fatal("conflict modified saved account")
	}
	for _, auth := range []string{"", "Bearer fixture-api"} {
		req := httptest.NewRequest("POST", "/api/accounts/import-session", strings.NewReader(`{"session":{"accessToken":"fixture-session-token"}}`))
		req.Header.Set("Authorization", auth)
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		if out.Code != 401 {
			t.Fatal("non-admin import allowed")
		}
	}
}

// A browser Session must be checked with its own token, even when the saved
// record has OAuth credentials. Missing identity is learned from the upstream.
func TestSessionImportDiscoversIdentityWithoutOAuthFallback(t *testing.T) {
	var reject atomic.Bool
	var oauthCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			oauthCalls.Add(1)
			http.Error(w, "unexpected OAuth refresh", 500)
			return
		}
		if reject.Load() {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"expired"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			fmt.Fprint(w, `{"email":"person@example.test","id":"user-session"}`)
		case "/backend-api/conversation/init":
			fmt.Fprint(w, `{"limits_progress":[]}`)
		case "/backend-api/accounts/check/v4-2023-04-27":
			fmt.Fprint(w, `{"accounts":{"default":{"account":{"plan_type":"free"}}}}`)
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	s := sessionImportTestServer(t, upstream.URL)
	code, result := sessionImportCall(t, s, map[string]any{"access_token": "fixture-session-token"})
	if code != 200 || result["refreshed"] != float64(1) || result["email"] != "person@example.test" {
		t.Fatalf("token-only identity discovery failed: %d %v", code, result)
	}
	s.store.UpdateAccount("fixture-session-token", map[string]any{"source_type": "oauth", "refresh_token": "fixture-secret", "id_token": "fixture-secret"})
	reject.Store(true)
	code, result = sessionImportCall(t, s, map[string]any{"access_token": "fixture-session-token"})
	if code != 200 || result["refreshed"] != float64(0) || result["refresh_error"] == "" || oauthCalls.Load() != 0 {
		t.Fatal("Session validation fell back to OAuth or hid invalid token")
	}
	rows, _ := s.store.AccountList()
	if len(rows) != 1 || rows[0]["refresh_token"] != "fixture-secret" || rows[0]["id_token"] != "fixture-secret" {
		t.Fatal("Session validation changed saved OAuth credentials")
	}
}
