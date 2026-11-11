package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func TestAccountEditorFollowsDefaultAndPreservesExplicitChoice(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir, cfg.DataDir = root, root
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "keys.json")
	cfg.RegisterPath = filepath.Join(root, "register.json")
	cfg.ProxyURL = "http://daily-proxy.invalid:8080"
	s := New(cfg)
	old := map[string]any{
		"access_token": "fixture-policy-token", "email": "policy@example.test",
		"registration_job_id": "registration-fixture", "refresh_token": "fixture-refresh",
		"proxy": "direct", "proxy_binding": map[string]any{"reference": "direct", "platform": "openai", "purpose": "registration"},
	}
	_, _, rows, err := s.store.AddAccounts(nil, []map[string]any{old})
	if err != nil {
		t.Fatal(err)
	}
	ref := accountPublicRef(rows[0])
	public := accountForAPI(rows[0])
	if public["proxy"] != "" || public["proxy_policy"] != "inherit" || public["proxy_binding"] != nil {
		t.Fatal("account editor exposed an obsolete registration route")
	}
	if rows[0]["proxy"] != "direct" || rows[0]["proxy_binding"] == nil {
		t.Fatal("public routing view mutated stored registration evidence")
	}
	update := func(proxy string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"access_token": ref, "proxy": proxy})
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/update", strings.NewReader(string(payload)))
		req.Header.Set("Authorization", "Bearer admin-secret")
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("account proxy save returned %d: %s", res.Code, res.Body.String())
		}
	}
	// Saving the unchanged form does not rewrite or erase the old task evidence.
	update("")
	stored, _ := s.store.AccountList()
	if stored[0]["proxy"] != "direct" || stored[0]["proxy_binding"] == nil {
		t.Fatal("unchanged editor save rewrote the legacy route")
	}
	update("direct")
	s = New(cfg)
	stored, _ = s.store.AccountList()
	if stored[0]["proxy_policy"] != "explicit" || s.proxyManager.Resolve(stored[0], false) != "" {
		t.Fatal("explicit direct selection was lost after restart")
	}
	update("")
	s = New(cfg)
	stored, _ = s.store.AccountList()
	if stored[0]["proxy_policy"] != "inherit" || s.proxyManager.Resolve(stored[0], false) != cfg.ProxyURL {
		t.Fatal("clearing the account override did not restore the default proxy")
	}
	s.proxyManager.SetDefault("http://new-daily-proxy.invalid:8080", nil)
	if s.proxyManager.Resolve(stored[0], false) != "http://new-daily-proxy.invalid:8080" {
		t.Fatal("inherited account did not follow a subsequent default change")
	}
	if stored[0]["access_token"] != old["access_token"] || stored[0]["refresh_token"] != old["refresh_token"] || stored[0]["registration_job_id"] != old["registration_job_id"] {
		t.Fatal("proxy edit changed account credentials or registration identity")
	}
	if len(mapList(stored[0]["proxy_binding_history"])) != 2 || !proxyruntime.AccountInheritsDefault(stored[0]) {
		t.Fatal("account edits did not retain audit history and inheritance")
	}
}

func TestGrokImageKeepsSelectedDefaultThroughDownload(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "grok-image-fixture.invalid" {
			t.Error("image download did not use the HTTP proxy")
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG)
	}))
	defer proxy.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.ProxyURL = proxy.URL
	s := New(cfg)
	grokTestAccounts(t, s)
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	result, err := s.generateReviewedGrokImages(r, "fixture", "grok-imagine-image-pro", "1024x1024", "b64_json", 1, nil, func(ctx context.Context, lease *accounts.Lease, count int) ([]provider.ImageResult, error) {
		if got := proxyruntime.URLFromContext(s.proxyManager.RequestContext(ctx, lease.Account.Fields, false)); got != proxy.URL {
			t.Fatal("image generation did not select the configured proxy")
		}
		s.proxyManager.SetDefault("http://127.0.0.1:1", nil)
		return []provider.ImageResult{{URL: "http://grok-image-fixture.invalid/output.png", MIME: "image/png"}}, nil
	})
	if err != nil || len(result) != 1 || result[0]["b64_json"] == "" {
		t.Fatalf("image download did not retain its generation proxy: %v", err)
	}
}
