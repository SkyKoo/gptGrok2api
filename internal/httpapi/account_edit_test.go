package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountEditPublicRefPersistsExistingTwoFASeed(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir, cfg.DataDir = root, root
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "keys.json")
	s := New(cfg)
	_, _, rows, err := s.store.AddAccounts(nil, []map[string]any{{"access_token": "fixture-edit-token", "email": "before@example.test", "refresh_token": "fixture-oauth-refresh", "source_type": "session_json"}})
	if err != nil {
		t.Fatal(err)
	}
	ref := accountPublicRef(rows[0])
	request := func(server *Server, method, path, body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		res := httptest.NewRecorder()
		server.Handler().ServeHTTP(res, req)
		if res.Code != 200 {
			t.Fatalf("%s returned %d", path, res.Code)
		}
		var reply map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	const seed = "JBSWY3DPEHPK3PXP"
	body, _ := json.Marshal(map[string]any{"access_token": ref, "two_factor_secret": seed, "email": "after@example.test"})
	reply := request(s, "POST", "/api/accounts/update", string(body))
	item := mapValue(reply["item"])
	if item["has_two_factor_secret"] != true || stringValue(item["two_factor_secret"]) != "" || stringValue(item["access_token"]) != "" {
		t.Fatal("save confirmation or redaction incorrect")
	}
	// 重建服务以验证磁盘持久化，而非仅验证接口回显。
	reopened := New(cfg)
	saved, err := reopened.store.AccountList()
	if err != nil || len(saved) != 1 {
		t.Fatal("account not persisted")
	}
	if saved[0]["two_factor_secret"] != seed || saved[0]["email"] != "after@example.test" || saved[0]["access_token"] != "fixture-edit-token" || saved[0]["refresh_token"] != "fixture-oauth-refresh" {
		t.Fatal("edited fields or existing credentials not preserved")
	}
	list := request(reopened, "GET", "/api/accounts", "")
	listed := list["items"].([]any)[0].(map[string]any)
	if listed["has_two_factor_secret"] != true || stringValue(listed["two_factor_secret"]) != "" {
		t.Fatal("reloaded list flag or redaction incorrect")
	}
	request(reopened, "POST", "/api/accounts/twofa/code", `{"account_ref":"`+stringValue(listed["account_ref"])+`"}`)
}
