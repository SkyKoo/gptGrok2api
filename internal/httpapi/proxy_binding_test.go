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

func TestAccountProxyBindingInheritanceAcrossPlatforms(t *testing.T) {
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
	s.proxyManager.SetDefault("http://default-one.invalid:8080", nil)
	_, _, gptRows, err := s.store.AddAccounts(nil, []map[string]any{{
		"access_token": "inherit-gpt", "email": "inherit@example.test", "proxy": "direct", "proxy_url": "http://obsolete.invalid:8080",
		"proxy_binding": proxyruntime.Binding{Reference: "direct", Platform: "openai", Purpose: "registration"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	grok, _, err := s.registerStore.AddAccount(map[string]any{"sso": "inherit-grok", "email": "grok-inherit@example.test", "proxy": "direct", "registration_job_id": "saved-registration", "proxy_binding": proxyruntime.Binding{Reference: "direct", Platform: "grok", Purpose: "account"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.syncRegisteredGrokRuntimeAccount(grok, "inherit-grok"); err != nil {
		t.Fatal(err)
	}
	adobeAccount := adobeFixtureAccount(t, s)
	getFields := func(platform string) map[string]any {
		t.Helper()
		switch platform {
		case "openai":
			rows, err := s.store.AccountList()
			if err != nil {
				t.Fatal(err)
			}
			return rows[0]
		case "grok":
			rows, err := s.grokRuntimeStore.AccountList()
			if err != nil {
				t.Fatal(err)
			}
			return rows[0]
		default:
			a, err := s.adobeStore.Get(adobeAccount.ID)
			if err != nil {
				t.Fatal(err)
			}
			return a.RoutingFields()
		}
	}
	call := func(method, platform, id, reference string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"platform": platform, "account_id": id, "reference": reference, "reason": "fixture account route"})
		path := "/api/proxy/binding"
		if method == "GET" {
			path += "?platform=" + platform + "&account_id=" + id
		}
		req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer admin-secret")
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, platform, res.Code, res.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, target := range []struct{ platform, id string }{{"openai", accountPublicRef(gptRows[0])}, {"grok", stringValue(grok["id"])}, {"adobe", adobeAccount.ID}} {
		t.Run(target.platform, func(t *testing.T) {
			initial := call("GET", target.platform, target.id, "")
			if initial["policy"] != "inherit" || initial["label"] != "跟随默认出口" || initial["binding"] != nil {
				t.Fatalf("automatic/empty account route was displayed as an override: %#v", initial)
			}
			call("POST", target.platform, target.id, "direct")
			if got := s.proxyManager.Resolve(getFields(target.platform), false); got != "" {
				t.Fatalf("explicit direct was ignored: %q", got)
			}
			result := call("POST", target.platform, target.id, "inherit")
			if result["policy"] != "inherit" || result["binding"] != nil {
				t.Fatalf("inherit was saved as fixed binding: %#v", result)
			}
			if got := s.proxyManager.Resolve(getFields(target.platform), false); got != "http://default-one.invalid:8080" {
				t.Fatalf("did not inherit current default: %q", got)
			}
			s.proxyManager.SetDefault("http://default-two.invalid:8080", nil)
			if got := s.proxyManager.Resolve(getFields(target.platform), false); got != "http://default-two.invalid:8080" {
				t.Fatalf("default change did not reach inherited account: %q", got)
			}
			s.proxyManager.SetDefault("http://default-one.invalid:8080", nil)
			loaded := call("GET", target.platform, target.id, "")
			if loaded["policy"] != "inherit" || len(loaded["history"].([]any)) != 2 {
				t.Fatal("inherit or audit history missing after reload")
			}
		})
	}
	// Background observations must preserve the latest inherited account policy.
	if _, _, err = s.syncRegisteredGrokRuntimeAccount(grok, "inherit-grok"); err != nil {
		t.Fatal(err)
	}
	if fields := getFields("grok"); fields["proxy_policy"] != "inherit" || s.proxyManager.Resolve(fields, false) != "http://default-one.invalid:8080" {
		t.Fatal("stale Grok sync resurrected the old automatic direct route")
	}
	updatedAdobe, err := s.adobeStore.Get(adobeAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedAdobe.Token != adobeAccount.Token || updatedAdobe.Cookie != adobeAccount.Cookie || updatedAdobe.Revision != adobeAccount.Revision {
		t.Fatal("Adobe binding update changed credentials")
	}
	reopened := New(cfg)
	rows, _ := reopened.store.AccountList()
	if rows[0]["proxy_policy"] != "inherit" || rows[0]["proxy_binding"] != nil || stringValue(rows[0]["proxy_url"]) != "" {
		t.Fatal("GPT inherited policy did not survive restart or clear old alias")
	}
	runtime, _ := reopened.grokRuntimeStore.AccountList()
	archive, _ := reopened.registerStore.ExportAccounts([]string{stringValue(grok["id"])})
	if runtime[0]["proxy_policy"] != "inherit" || archive[0]["proxy_policy"] != "inherit" {
		t.Fatal("Grok inherited policy did not persist in both stores")
	}
	a, err := reopened.adobeStore.Get(adobeAccount.ID)
	if err != nil || a.ProxyPolicy != "inherit" || a.ProxyBinding != nil || len(a.ProxyBindingHistory) != 2 {
		t.Fatal("Adobe inherited policy or audit did not survive restart")
	}
}
