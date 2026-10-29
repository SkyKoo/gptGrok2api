package httpapi

import (
	"encoding/json"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountProxyBindingAPIAuditAndRuntime(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir, cfg.DataDir = root, root
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "keys.json")
	cfg.RegisterPath = filepath.Join(root, "register.json")
	cfg.GrokAccountsPath = filepath.Join(root, "grok_accounts.json")
	cfg.GrokRuntimeAccountsPath = filepath.Join(root, "grok_runtime_accounts.json")
	s := New(cfg)
	s.proxyManager.ConfigureTaskGroups([]proxyruntime.GroupConfig{{ID: "shared", Enabled: true, Nodes: []proxyruntime.NodeConfig{{ID: "one", URL: "http://fixture-user:fixture-secret@proxy.invalid:8080", Enabled: true}}}})
	_, _, rows, e := s.store.AddAccounts(nil, []map[string]any{{"access_token": "fixture-gpt-token", "email": "gpt@example.test"}})
	if e != nil {
		t.Fatal(e)
	}
	grok, _, e := s.registerStore.AddAccount(map[string]any{"sso": "fixture-grok-token", "email": "grok@example.test", "browser_profile": map[string]any{"locale": "en-US"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.syncRegisteredGrokRuntimeAccount(grok, "fixture-grok-token"); e != nil {
		t.Fatal(e)
	}
	_, _, e = s.grokRuntimeStore.UpdateAccount("fixture-grok-token", map[string]any{"enabled": false})
	if e != nil {
		t.Fatal(e)
	}
	call := func(method, path, body string, auth bool, status int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			req.Header.Set("Authorization", "Bearer admin-secret")
		}
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != status {
			t.Fatalf("%s: want %d got %d: %s", path, status, res.Code, res.Body.String())
		}
		var data map[string]any
		_ = json.Unmarshal(res.Body.Bytes(), &data)
		if strings.Contains(res.Body.String(), "fixture-secret") {
			t.Fatal("proxy credentials exposed")
		}
		return data
	}
	ref := accountPublicRef(rows[0])
	customBody, _ := json.Marshal(map[string]any{"platform": "openai", "account_id": ref, "reference": "http://fixture-user:fixture-secret@single.invalid:8080", "reason": "single proxy"})
	custom := call("POST", "/api/proxy/binding", string(customBody), true, 200)
	if !strings.HasPrefix(stringValue(mapValue(custom["binding"])["reference"]), "custom:") {
		t.Fatal("single proxy binding was not redacted")
	}

	gid := stringValue(grok["id"])
	call("GET", "/api/proxy/binding?platform=openai&account_id="+ref, "", false, 401)
	call("POST", "/api/proxy/binding", `{"platform":"openai","account_id":"`+ref+`","reference":"direct"}`, true, 400)
	call("POST", "/api/proxy/binding", `{"platform":"openai","account_id":"`+ref+`","reference":"group:shared","reason":"test"}`, true, 400)
	for _, v := range []struct{ platform, id string }{{"openai", ref}, {"grok", gid}} {
		for _, node := range []string{"node:shared/one", "direct"} {
			payload, _ := json.Marshal(map[string]any{"platform": v.platform, "account_id": v.id, "reference": node, "reason": "fixture switch"})
			call("POST", "/api/proxy/binding", string(payload), true, 200)
		}
		result := call("GET", "/api/proxy/binding?platform="+v.platform+"&account_id="+v.id, "", true, 200)
		expected := 2
		if v.platform == "openai" {
			expected = 3
		}
		if result["label"] != "direct" || len(result["history"].([]any)) != expected {
			t.Fatal("binding or audit history missing")
		}
	}
	call("POST", "/api/accounts/update", `{"access_token":"`+ref+`","proxy":"http://bypass.invalid:8080"}`, true, 409)
	runtime, _ := s.grokRuntimeStore.AccountList()
	if len(runtime) != 1 || runtime[0]["proxy"] != "direct" || runtime[0]["enabled"] != false {
		t.Fatal("binding sync changed account availability or lost route")
	}
	// A stale background observation must not restore an old proxy binding.
	if _, _, e = s.syncRegisteredGrokRuntimeAccount(grok, "fixture-grok-token"); e != nil {
		t.Fatal(e)
	}
	runtime, _ = s.grokRuntimeStore.AccountList()
	if runtime[0]["proxy"] != "direct" || runtime[0]["browser_profile"] == nil {
		t.Fatal("stale sync lost latest binding/profile")
	}
	reopened := New(cfg)
	saved, _ := reopened.store.AccountList()
	if len(saved) != 1 || saved[0]["proxy"] != "direct" || len(saved[0]["proxy_binding_history"].([]any)) != 3 {
		t.Fatal("GPT binding/history did not survive restart")
	}
	archive, _ := reopened.registerStore.ExportAccounts([]string{gid})
	if len(archive) != 1 || archive[0]["proxy"] != "direct" || len(archive[0]["proxy_binding_history"].([]any)) != 2 {
		t.Fatal("Grok binding/history did not survive restart")
	}
}
