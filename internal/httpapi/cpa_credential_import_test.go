package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type cpaBindingFixture struct {
	s         *Server
	mu        sync.Mutex
	files     map[string]map[string]any
	downloads []string
	uploaded  []string
}

func cpaFixtureJWT(email, id string) string {
	raw, _ := json.Marshal(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": id}, "exp": 2100000000})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".fixture-signature"
}

func newCPABindingFixture(t *testing.T) *cpaBindingFixture {
	t.Helper()
	f := &cpaBindingFixture{files: map[string]map[string]any{}}
	f.files["turb-original.json"] = map[string]any{"type": "codex", "email": "person@example.test", "account_id": "workspace-one", "access_token": cpaFixtureJWT("person@example.test", "workspace-one"), "refresh_token": "fixture-cpa-refresh", "id_token": cpaFixtureJWT("person@example.test", "workspace-one"), "expired": "2036-07-18T13:20:00Z", "last_refresh": "2026-10-20T00:00:00Z"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fixture-cpa-admin" {
			t.Error("missing CPA authorization")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v0/management/auth-files/download":
			name := r.URL.Query().Get("name")
			f.downloads = append(f.downloads, name)
			if file, ok := f.files[name]; ok {
				json.NewEncoder(w).Encode(file)
			} else {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"error":"fixture-cpa-refresh"}`)
			}
		case r.URL.Path == "/v0/management/auth-files" && r.Method == "GET":
			rows := []map[string]any{}
			for name, file := range f.files {
				rows = append(rows, map[string]any{"name": name, "email": file["email"]})
			}
			json.NewEncoder(w).Encode(map[string]any{"files": rows})
		case r.URL.Path == "/v0/management/auth-files" && r.Method == "POST":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer file.Close()
			var payload map[string]any
			if err := json.NewDecoder(file).Decode(&payload); err != nil {
				t.Error(err)
			}
			f.uploaded = append(f.uploaded, header.Filename)
			f.files[header.Filename] = payload
			fmt.Fprint(w, `{"ok":true}`)
		default:
			t.Error("unexpected upstream call")
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	f.s = sessionImportTestServer(t, upstream.URL)
	if err := f.s.external.saveCPAList([]externalCPAPool{{ID: "fixture-pool", Name: "Fixture CPA", BaseURL: upstream.URL, SecretKey: "fixture-cpa-admin"}}); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := f.s.store.AddAccounts(nil, []map[string]any{{"id": "cfm-one", "access_token": "fixture.chatgpt.session", "email": "person@example.test", "login_password": "fixture-original-password", "two_factor_secret": "fixture-original-twofa", "group_id": "fixture-group", "proxy": "direct", "enabled": false, "quota": 7, "source_type": "session_json"}, {"id": "cfm-other", "access_token": "fixture.other.session", "email": "other@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *cpaBindingFixture) call(t *testing.T, method, path string, body any, auth string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	res := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(res, req)
	var reply map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-cpa-refresh", "fixture-cpa-admin", "fixture-original-password", "fixture-original-twofa", "fixture.chatgpt.session", "fixture-signature"} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatal("response leaked credential")
		}
	}
	if strings.HasSuffix(path, "/preview") || strings.HasSuffix(path, "/bind") {
		if auth == "fixture-admin" && res.Code != http.StatusServiceUnavailable && res.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("preview/bind must not be cached")
		}
	}
	return res.Code, reply
}
func (f *cpaBindingFixture) preview(t *testing.T, names ...string) []any {
	t.Helper()
	code, result := f.call(t, "POST", "/api/cpa/pools/fixture-pool/preview", map[string]any{"names": names}, "fixture-admin")
	if code != 200 {
		t.Fatalf("preview HTTP %d", code)
	}
	return result["items"].([]any)
}
func bindingFromPreview(t *testing.T, item map[string]any, ref string) map[string]any {
	t.Helper()
	for _, v := range item["candidates"].([]any) {
		c := v.(map[string]any)
		if c["account_ref"] == ref {
			return map[string]any{"name": item["name"], "account_ref": ref, "source_version": item["source_version"], "target_version": c["target_version"]}
		}
	}
	t.Fatal("candidate not found")
	return nil
}
func (f *cpaBindingFixture) bind(t *testing.T, body any) (int, map[string]any) {
	t.Helper()
	return f.call(t, "POST", "/api/cpa/pools/fixture-pool/bind", body, "fixture-admin")
}

func TestCPABindSelectedCredentialPersistsAndSyncsOriginalFilename(t *testing.T) {
	f := newCPABindingFixture(t)
	before, _ := f.s.store.AccountList()
	preview := f.preview(t, "turb-original.json")[0].(map[string]any)
	if preview["suggested_ref"] != "cfm-one" || preview["has_refresh_token"] != true {
		t.Fatal("unique identity was not suggested")
	}
	if len(f.downloads) != 1 || len(f.uploaded) != 0 {
		t.Fatal("preview should only download selected files")
	}
	binding := bindingFromPreview(t, preview, "cfm-one")
	code, result := f.bind(t, binding)
	if code != 200 || result["status"] != "bound" {
		t.Fatalf("bind failed: %d %v", code, result)
	}
	saved, _ := f.s.store.AccountList()
	credential := mapValue(saved[0]["codex_credential"])
	if len(saved) != len(before) || credential["refresh_token"] != "fixture-cpa-refresh" || credential["cpa_source_file_name"] != "turb-original.json" || credential["cpa_sync_status"] != "imported" {
		t.Fatal("complete credential/source not saved")
	}
	copy := cloneMap(saved[0])
	delete(copy, "codex_credential")
	if !reflect.DeepEqual(copy, before[0]) || !reflect.DeepEqual(saved[1], before[1]) {
		t.Fatal("original account fields or other accounts changed")
	}
	if len(f.uploaded) != 0 {
		t.Fatal("import must never upload to CPA")
	}
	code, result = f.bind(t, binding)
	if code != 200 || result["status"] != "skipped" {
		t.Fatal("duplicate binding is not idempotent")
	}
	again, _ := f.s.store.AccountList()
	if !reflect.DeepEqual(saved, again) {
		t.Fatal("idempotent import rewrote metadata")
	}
	// 从磁盘重新创建服务；管理页可见状态但不返回密钥。
	f.s = New(f.s.cfg)
	code, list := f.call(t, "GET", "/api/accounts", nil, "fixture-admin")
	if code != 200 || mapValue(list["items"].([]any)[0].(map[string]any)["codex_credential"])["has_refresh_token"] != true {
		t.Fatal("credential not durable or public flags missing")
	}
	code, result = f.call(t, "POST", "/api/accounts/codex/sync", map[string]any{"account_ref": "cfm-one", "cpa_pool_id": "fixture-pool"}, "fixture-admin")
	if code != 200 || result["ok"] != true || len(f.uploaded) != 1 || f.uploaded[0] != "turb-original.json" {
		t.Fatalf("sync did not reuse source filename: %d", code)
	}
}

func TestCPABindPreservesExistingUnlessExplicitlyReplaced(t *testing.T) {
	f := newCPABindingFixture(t)
	old := map[string]any{"access_token": "old-at", "refresh_token": "old-rt", "email": "person@example.test", "account_id": "workspace-one"}
	f.s.store.UpdateAccount("fixture.chatgpt.session", map[string]any{"codex_credential": old})
	binding := bindingFromPreview(t, f.preview(t, "turb-original.json")[0].(map[string]any), "cfm-one")
	code, result := f.bind(t, binding)
	if code != 200 || result["status"] != "skipped" {
		t.Fatal("default import overwrites old credential")
	}
	rows, _ := f.s.store.AccountList()
	if !reflect.DeepEqual(mapValue(rows[0]["codex_credential"]), old) {
		t.Fatal("old credential changed")
	}
	binding["replace"] = true
	code, result = f.bind(t, binding)
	if code != 200 || result["status"] != "bound" {
		t.Fatal("explicit replacement failed")
	}
}

func TestCPABindRejectsStalePreviewOrIdentityMismatch(t *testing.T) {
	for _, scenario := range []string{"source-changed", "target-changed", "email-conflict", "workspace-conflict", "deleted", "ambiguous-ref", "maintenance", "auth-active"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCPABindingFixture(t)
			binding := bindingFromPreview(t, f.preview(t, "turb-original.json")[0].(map[string]any), "cfm-one")
			switch scenario {
			case "source-changed":
				f.mu.Lock()
				f.files["turb-original.json"]["refresh_token"] = "rotated-fixture"
				f.mu.Unlock()
			case "target-changed":
				f.s.store.UpdateAccount("fixture.chatgpt.session", map[string]any{"codex_credential": map[string]any{"refresh_token": "new-credential"}})
			case "email-conflict":
				binding["account_ref"] = "cfm-other"
			case "workspace-conflict":
				f.s.store.UpdateAccount("fixture.chatgpt.session", map[string]any{"codex_credential": map[string]any{"account_id": "different-workspace"}})
			case "deleted":
				f.s.store.DeleteAccounts([]string{"fixture.chatgpt.session"})
			case "ambiguous-ref":
				f.s.store.AddAccounts(nil, []map[string]any{{"id": "cfm-one", "access_token": "extra.fixture.token", "email": "person@example.test"}})
			case "maintenance":
				os.WriteFile(filepath.Join(f.s.cfg.DataDir, "cfm-maintenance"), nil, 0600)
			case "auth-active":
				f.s.codexMu.Lock()
				f.s.codexProgress["fixture"] = &codexOAuthProgress{AccountRef: "cfm-one", Email: "person@example.test", Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339)}
				f.s.codexMu.Unlock()
			}
			before, _ := f.s.store.AccountList()
			code, _ := f.bind(t, binding)
			expected := http.StatusConflict
			if scenario == "maintenance" {
				expected = http.StatusServiceUnavailable
			} // 全局维护门禁在路由前拦截。
			if code != expected {
				t.Fatalf("expected %d, got %d", expected, code)
			}
			after, _ := f.s.store.AccountList()
			if !reflect.DeepEqual(before, after) || len(f.uploaded) > 0 {
				t.Fatal("conflict mutated accounts or CPA")
			}
		})
	}
}

func TestCPAPreviewPerFileErrorsManualSelectionAndAuth(t *testing.T) {
	f := newCPABindingFixture(t)
	f.files["bad.json"] = map[string]any{"type": "codex", "access_token": "at-only"}
	f.files["xai.json"] = map[string]any{"type": "xai", "access_token": "at", "refresh_token": "rt"}
	f.files["mismatch.json"] = map[string]any{"type": "codex", "access_token": cpaFixtureJWT("other@example.test", "workspace-one"), "refresh_token": "rt", "email": "person@example.test"}
	f.files["unmatched.json"] = map[string]any{"type": "codex", "access_token": "opaque-at", "refresh_token": "rt", "email": "absent@example.test"}
	result := f.preview(t, "turb-original.json", "bad.json", "xai.json", "mismatch.json", "absent.json", "unmatched.json")
	if len(result) != 6 {
		t.Fatal("partial preview lost entries")
	}
	for _, i := range []int{1, 2, 3, 4} {
		if result[i].(map[string]any)["error"] == "" {
			t.Fatal("invalid credential accepted")
		}
	}
	if result[5].(map[string]any)["suggested_ref"] != "" {
		t.Fatal("unmatched credential was bound automatically")
	}
	// 相同邮箱的两个不同ID不能自动选；由用户手动明确选择。
	f.s.store.AddAccounts(nil, []map[string]any{{"id": "cfm-same-email", "access_token": "another.fixture.token", "email": "person@example.test"}})
	preview := f.preview(t, "turb-original.json")[0].(map[string]any)
	if preview["suggested_ref"] != "" {
		t.Fatal("ambiguous email automatically selected")
	}
	code, reply := f.bind(t, bindingFromPreview(t, preview, "cfm-same-email"))
	if code != 200 || reply["status"] != "bound" {
		t.Fatal("manual selection failed")
	}
	for _, path := range []string{"preview", "bind"} {
		for _, auth := range []string{"", "fixture-api"} {
			code, _ := f.call(t, "POST", "/api/cpa/pools/fixture-pool/"+path, map[string]any{}, auth)
			if code != 401 {
				t.Fatal("admin authentication missing")
			}
		}
		code, _ := f.call(t, "GET", "/api/cpa/pools/fixture-pool/"+path, nil, "fixture-admin")
		if code != 405 {
			t.Fatal("method not enforced")
		}
	}
	// 旧入口不能继续把Codex Token当成ChatGPT Session新增账号。
	before, _ := f.s.store.AccountList()
	code, _ = f.call(t, "POST", "/api/cpa/pools/fixture-pool/import", map[string]any{"names": []string{"turb-original.json"}}, "fixture-admin")
	after, _ := f.s.store.AccountList()
	if code != 400 || !reflect.DeepEqual(before, after) {
		t.Fatal("legacy token-only import still writes")
	}
}

func TestCPAPreviewValidatesNamesLimitsAndRedirects(t *testing.T) {
	f := newCPABindingFixture(t)
	before := len(f.downloads)
	for _, name := range []string{"../private.json", "file\\private.json", "x.txt", "bad\n.json"} {
		result := f.preview(t, name)[0].(map[string]any)
		if result["error"] == "" {
			t.Fatal("invalid path accepted")
		}
	}
	if len(f.downloads) != before {
		t.Fatal("invalid file name reached CPA")
	}
	names := []string{}
	for i := 0; i < 21; i++ {
		names = append(names, fmt.Sprintf("%d.json", i))
	}
	code, _ := f.call(t, "POST", "/api/cpa/pools/fixture-pool/preview", map[string]any{"names": names}, "fixture-admin")
	if code != 400 {
		t.Fatal("preview size limit missing")
	}
	var redirected bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer source.Close()
	_, err := downloadCPACredential(context.Background(), externalCPAPool{BaseURL: source.URL, SecretKey: "do-not-forward"}, "file.json")
	if err == nil || redirected {
		t.Fatal("credential download followed redirect")
	}
}

func TestCPASyncReservesOnlyTargetAndPreservesConcurrentChanges(t *testing.T) {
	f := newCPABindingFixture(t)
	binding := bindingFromPreview(t, f.preview(t, "turb-original.json")[0].(map[string]any), "cfm-one")
	if code, _ := f.bind(t, binding); code != 200 {
		t.Fatal("bind failed")
	}
	started, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			close(started)
			<-release
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		json.NewEncoder(w).Encode(f.files["turb-original.json"])
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.s.external.saveCPAList([]externalCPAPool{{ID: "fixture-pool", BaseURL: upstream.URL, SecretKey: "fixture-cpa-admin"}})
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest("POST", "/api/accounts/codex/sync", strings.NewReader(`{"account_ref":"cfm-one","cpa_pool_id":"fixture-pool"}`))
		req.Header.Set("Authorization", "Bearer fixture-admin")
		res := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(res, req)
		done <- res.Code
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("sync did not start")
	}
	// 慢 CPA 不应占用全局准入锁，其他账号操作仍可继续。
	if !f.s.authStartMu.TryLock() {
		t.Fatal("sync blocks all account operations")
	}
	f.s.authStartMu.Unlock()
	if !f.s.cpaSyncActive("cfm-one", "person@example.test") || f.s.cpaSyncActive("cfm-other", "other@example.test") {
		t.Fatal("sync reservation is not scoped to target")
	}
	if code, _ := f.bind(t, binding); code != 409 {
		t.Fatal("concurrent binding not rejected")
	}
	if code, _ := f.call(t, "POST", "/api/accounts/codex/sync", map[string]any{"account_ref": "cfm-one"}, "fixture-admin"); code != 409 {
		t.Fatal("concurrent sync not rejected")
	}
	// 即使写入从其他入口发生，也不能用旧同步快照覆盖较新的凭据。
	replacement := map[string]any{"access_token": "new-codex-at", "refresh_token": "new-codex-rt"}
	f.s.store.UpdateAccount("fixture.chatgpt.session", map[string]any{"codex_credential": replacement})
	close(release)
	select {
	case code := <-done:
		if code != 502 {
			t.Fatalf("expected conflict report after upload, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sync did not complete")
	}
	account, _ := f.s.accountByRef("cfm-one")
	if !reflect.DeepEqual(mapValue(account["codex_credential"]), replacement) || f.s.cpaSyncActive("cfm-one", "person@example.test") {
		t.Fatal("new credential overwritten or sync reservation leaked")
	}
}

func TestCPASyncDifferentPoolDoesNotReuseSourceFilename(t *testing.T) {
	f := newCPABindingFixture(t)
	binding := bindingFromPreview(t, f.preview(t, "turb-original.json")[0].(map[string]any), "cfm-one")
	f.bind(t, binding)
	pool, _ := f.s.external.getCPA("fixture-pool")
	pool.ID = "another-pool"
	f.s.external.saveCPAList([]externalCPAPool{pool})
	code, _ := f.call(t, "POST", "/api/accounts/codex/sync", map[string]any{"account_ref": "cfm-one", "cpa_pool_id": "another-pool"}, "fixture-admin")
	if code != 200 || len(f.uploaded) != 1 || f.uploaded[0] == "turb-original.json" || !strings.HasPrefix(f.uploaded[0], "codex-") {
		t.Fatal("different CPA pool reused original filename")
	}
}

func TestCPASyncOAuthFailureDoesNotRestoreOldCredential(t *testing.T) {
	f := newCPABindingFixture(t)
	replacement := map[string]any{"access_token": "newer-codex-access", "refresh_token": "newer-codex-refresh"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := f.s.store.UpdateAccount("fixture.chatgpt.session", map[string]any{"codex_credential": replacement}); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	f.s.external.saveCPAList([]externalCPAPool{{ID: "fixture-pool", BaseURL: upstream.URL, SecretKey: "fixture-cpa-admin"}})
	result, err := f.s.finishCodexTokens(context.Background(), "fixture-session", "cfm-one", "fixture-pool", "", map[string]string{"access_token": cpaFixtureJWT("person@example.test", "workspace-one"), "refresh_token": "oauth-refresh"})
	if err != nil || result["ok"] != false {
		t.Fatal("expected stale sync report")
	}
	account, _ := f.s.accountByRef("cfm-one")
	if !reflect.DeepEqual(mapValue(account["codex_credential"]), replacement) {
		t.Fatal("OAuth sync error restored older credential")
	}
}
