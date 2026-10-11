package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestRegistrationHistoryUsesFixedSizePages(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.DataDir, "openai_registration_tasks.json")
	jobs := make([]registerruntime.RegistrationJob, 123)
	for i := range jobs {
		jobs[i] = registerruntime.RegistrationJob{ID: fmt.Sprintf("job-%d", i), Status: "completed", Stage: "completed", Mailbox: registerruntime.Mailbox{Email: "saved@example.test"}}
		for k := 0; k < 3; k++ {
			jobs[i].Logs = append(jobs[i].Logs, registerruntime.JobLog{Text: fmt.Sprintf("job-%d-log-%d", i, k)})
		}
	}
	jobs[0].Status = "import_pending"
	jobs[0].Account = map[string]any{"access_token": "PRIVATE-TOKEN"}
	profile := registerruntime.NewBrowserProfile()
	jobs[0].BrowserProfile = &profile
	raw, _ := json.Marshal(jobs)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s := New(cfg)
	if _, err := s.registerStore.Update(map[string]any{"target": "openai"}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, tc := range []struct {
		path              string
		page, first, last int
	}{
		{"/api/register", 1, 73, 122},
		{"/api/register/openai/jobs?page=1&limit=100000", 1, 73, 122},
		{"/api/register/openai/jobs?page=2", 2, 23, 72},
		{"/api/register/openai/jobs?page=3", 3, 0, 22},
		{"/api/register/openai/jobs?page=9223372036854775807", 3, 0, 22},
		{"/api/register/openai/jobs?page=-1", 1, 73, 122},
	} {
		res := adminRequest(h, http.MethodGet, tc.path, nil)
		if res.Code != 200 {
			t.Fatalf("read failed: %d", res.Code)
		}
		if strings.Contains(res.Body.String(), "PRIVATE-TOKEN") || strings.Contains(res.Body.String(), "browser_profile") {
			t.Fatal("private recovery data leaked")
		}
		var value map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if tc.path == "/api/register" {
			value = mapValue(value["register"])
		}
		pageJobs := anyList(value["jobs"])
		if len(pageJobs) != tc.last-tc.first+1 || len(pageJobs) > 50 || intValue(value["jobs_total"]) != 123 || intValue(value["jobs_page_size"]) != 50 || intValue(value["jobs_page"]) != tc.page {
			t.Fatal("unbounded or incorrect page")
		}
		if mapValue(pageJobs[0])["id"] != fmt.Sprintf("job-%d", tc.first) || mapValue(pageJobs[len(pageJobs)-1])["id"] != fmt.Sprintf("job-%d", tc.last) {
			t.Fatal("wrong history window")
		}
		if tc.first == 0 && mapValue(pageJobs[0])["can_retry"] != true {
			t.Fatal("older pending result cannot be retried")
		}
		if intValue(mapValue(value["stats"])["success"]) != 122 {
			t.Fatal("stats counted only visible jobs")
		}
		logs := anyList(value["logs"])
		if len(logs) != 200 || mapValue(logs[0])["text"] != "job-56-log-1" || mapValue(logs[199])["text"] != "job-122-log-2" {
			t.Fatal("recent log window changed")
		}
	}
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/register/openai/jobs?page=2", nil))
	if unauth.Code != 401 {
		t.Fatal("history endpoint lacks authentication")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("reading pages modified history")
	}
	snapshot := s.registrationSnapshot()
	if len(anyList(snapshot["jobs"])) != 50 {
		t.Fatal("live snapshot sends full history")
	}
}
