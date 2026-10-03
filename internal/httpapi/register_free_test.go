package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestHMEConfigSecretRetentionAndEndpointChange(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	handler := s.Handler()
	payload := `{"target":"openai","mail":{"providers":[{"id":"hme","type":"icloud_hme","enable":true,"api_base":"https://hme.example.test","account_id":"acc-test","admin_password":"PRIVATE-PASSWORD"}]}}`
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		body := strings.NewReader("")
		if method == http.MethodPost {
			body = strings.NewReader(payload)
		}
		res := adminRequest(handler, method, "/api/register", body)
		if res.Code != 200 || strings.Contains(res.Body.String(), "PRIVATE-PASSWORD") || !strings.Contains(res.Body.String(), `"admin_password_set":true`) {
			t.Fatalf("unsafe or missing secret status: %d %s", res.Code, res.Body.String())
		}
	}
	res := adminRequest(handler, http.MethodPost, "/api/register", strings.NewReader(strings.ReplaceAll(payload, `"admin_password":"PRIVATE-PASSWORD"`, `"admin_password":""`)))
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	check := func() string {
		providers := mapValue(s.registerStore.Get()["mail"])["providers"].([]any)
		return stringValue(mapValue(providers[0])["admin_password"])
	}
	if check() != "PRIVATE-PASSWORD" {
		t.Fatal("blank password did not retain saved credential")
	}
	for _, path := range []string{"/api/register/checkout-retries/stop", "/api/register/checkout-history/clear", "/api/register/outlook-pool/reset", "/api/register/openai/retry-registration"} {
		result := adminRequest(handler, http.MethodPost, path, strings.NewReader(`{}`))
		if strings.Contains(result.Body.String(), "PRIVATE-PASSWORD") {
			t.Fatalf("legacy action leaked HME credential: %s", path)
		}
	}
	changed := strings.ReplaceAll(strings.ReplaceAll(payload, `"admin_password":"PRIVATE-PASSWORD"`, `"admin_password":""`), "https://hme.example.test", "https://different.example.test")
	res = adminRequest(handler, http.MethodPost, "/api/register", strings.NewReader(changed))
	if res.Code != 200 || check() != "" {
		t.Fatal("credential copied to changed endpoint")
	}
	unauth := httptest.NewRecorder()
	handler.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/api/register/openai/retry-result", strings.NewReader(`{"id":"x"}`)))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatal("retry endpoint lacks authentication")
	}
}

func TestRegisteredAccountRemainsDisabledUntilVerification(t *testing.T) {
	var allow atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allow.Load() {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "alias@example.test", "id": "user-test"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "free"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.OpenAIBaseURL = upstream.URL
	s := New(cfg)
	account := map[string]any{"access_token": "test-token", "email": "alias@example.test", "source_type": "chatgpt_web", "enabled": false, "status": "待验证", "registration_job_id": "job-test"}
	for i := 0; i < 2; i++ {
		if err := s.importRegisteredAccount(context.Background(), "job-test", account); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := s.store.AccountList()
	if len(items) != 1 || items[0]["enabled"] != false {
		t.Fatal("duplicate import or premature enabling")
	}
	if err := s.verifyRegisteredAccount(context.Background(), "job-test", account); err == nil {
		t.Fatal("invalid account verified")
	}
	items, _ = s.store.AccountList()
	if items[0]["enabled"] != false {
		t.Fatal("failed account enabled")
	}
	allow.Store(true)
	if err := s.verifyRegisteredAccount(context.Background(), "job-test", account); err != nil {
		t.Fatal(err)
	}
	items, _ = s.store.AccountList()
	if items[0]["enabled"] != true || items[0]["registration_verified"] != true {
		t.Fatal("verified account not enabled")
	}
}

func TestRegistrationConfigDoesNotAcceptRuntimeState(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	res := adminRequest(s.Handler(), http.MethodPost, "/api/register", strings.NewReader(`{"enabled":true,"stats":{"success":99},"jobs":[{"account":{"access_token":"injected"}}]}`))
	if res.Code != http.StatusOK {
		t.Fatal(res.Body.String())
	}
	value := s.registrationSnapshot()
	if value["enabled"] != false || strings.Contains(res.Body.String(), "injected") {
		t.Fatal("accepted untrusted runtime state")
	}
	_, err := registerruntime.ParseFreeConfig(s.registerStore.Get())
	if err == nil {
		t.Fatal("empty config is ready")
	}
}

func TestRegistrationCleanupReturnsCountAndKeepsRecovery(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	cfg.RegisterPath = filepath.Join(cfg.DataDir, "register.json")
	journal := filepath.Join(cfg.DataDir, "openai_registration_tasks.json")
	jobs := `[{"id":"done","status":"completed","imported":true,"verified":true},{"id":"failed","status":"failed"},{"id":"pending","status":"registration_pending","mailbox":{"email":"retry@example.test"}}]`
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte(jobs), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(cfg)
	if _, err := s.registerStore.Update(map[string]any{"target": "openai"}); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	unauth := httptest.NewRecorder()
	handler.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/api/register/reset", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatal("cleanup endpoint lacks authentication")
	}
	for _, want := range []int{2, 0} {
		res := adminRequest(handler, http.MethodPost, "/api/register/reset", nil)
		var body struct {
			Removed  int `json:"removed"`
			Register struct {
				Jobs []struct {
					ID    string `json:"id"`
					Email string `json:"email"`
				} `json:"jobs"`
			} `json:"register"`
		}
		if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &body) != nil {
			t.Fatalf("cleanup failed: %d %s", res.Code, res.Body.String())
		}
		if body.Removed != want || len(body.Register.Jobs) != 1 || body.Register.Jobs[0].ID != "pending" || body.Register.Jobs[0].Email != "retry@example.test" {
			t.Fatalf("incorrect cleanup result: %s", res.Body.String())
		}
	}
}
