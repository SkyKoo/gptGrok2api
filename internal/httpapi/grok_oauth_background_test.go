package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
)

func TestGrokBackgroundOAuthLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, workerStatus, workerCode, email, want string
		cancel                                      bool
	}{
		{name: "success", workerStatus: "approved", email: "selected@example.test", want: "authorized"},
		{name: "lost_confirmation_response", workerStatus: "uncertain", email: "selected@example.test", want: "authorized"},
		{name: "wrong_identity", workerStatus: "approved", email: "other@example.test", want: "failed"},
		{name: "missing_identity", workerStatus: "approved", want: "failed"},
		{name: "login_required", workerStatus: "manual_action_required", workerCode: "oauth_login_required", want: "manual_action_required"},
		{name: "worker_secret_error", workerStatus: "failed", workerCode: "SSO-SECRET MUST-NOT-LEAK", want: "failed"},
		{name: "cancelled", workerStatus: "approved", email: "selected@example.test", want: "failed", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var starts, confirmations, polls atomic.Int32
			var s *Server
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/device":
					starts.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "DEVICE-SECRET", "user_code": "ABCD-EFGH", "verification_uri_complete": "https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH", "interval": 1})
				case "/token":
					polls.Add(1)
					claims, _ := json.Marshal(map[string]string{"email": tc.email})
					token := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "refresh_token": "REFRESH-SECRET", "expires_in": 3600})
				case "/v1/oauth/authorize-device":
					confirmations.Add(1)
					var body oauth.BrowserDeviceRequest
					if json.NewDecoder(r.Body).Decode(&body) != nil || body.SSO != "SSO-SECRET" || body.Email != "selected@example.test" || body.Proxy != "" || body.UserCode != "ABCD-EFGH" || r.Header.Get("Authorization") != "Bearer WORKER-SECRET" {
						t.Error("worker received wrong account, route or authentication")
					}
					if tc.cancel {
						for _, task := range s.taskQueue.List() {
							if task.Kind == "grok_oauth_authorize" {
								s.taskQueue.Cancel(task.ID)
							}
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"status": tc.workerStatus, "code": tc.workerCode})
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			cfg := adminTestConfig(t.TempDir())
			cfg.OAuthDeviceURL = upstream.URL + "/device"
			cfg.OAuthTokenURL = upstream.URL + "/token"
			s = New(cfg)
			_, _, err := s.registerStore.AddAccount(map[string]any{"id": "selected", "email": "selected@example.test", "sso": "SSO-SECRET", "status": "active", "proxy_binding": map[string]any{"reference": "direct"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.registerStore.Update(map[string]any{"grok": map[string]any{"api_base": upstream.URL, "api_key": "WORKER-SECRET"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.oauthStore.Import(oauth.Account{Email: "existing@example.test", AccessToken: "EXISTING-SECRET"})
			if err != nil {
				t.Fatal(err)
			}
			handler := s.Handler()
			denied := httptest.NewRecorder()
			handler.ServeHTTP(denied, httptest.NewRequest("POST", "/api/grok/oauth/protocol/start", strings.NewReader(`{"account_id":"selected"}`)))
			if denied.Code != 401 {
				t.Fatal("missing auth")
			}
			start := adminRequest(handler, "POST", "/api/grok/oauth/protocol/start", strings.NewReader(`{"account_id":"selected"}`))
			if start.Code != 202 {
				t.Fatalf("start: %d %s", start.Code, start.Body.String())
			}
			var first, second struct {
				Job    map[string]any `json:"job"`
				Reused bool           `json:"reused"`
			}
			_ = json.Unmarshal(start.Body.Bytes(), &first)
			duplicate := adminRequest(handler, "POST", "/api/grok/oauth/protocol/start", strings.NewReader(`{"account_id":"selected"}`))
			_ = json.Unmarshal(duplicate.Body.Bytes(), &second)
			if !second.Reused || first.Job["id"] != second.Job["id"] || len(s.taskQueue.List()) != 1 {
				t.Fatal("duplicate authorization was not reused")
			}
			s.taskQueue.Start(1)
			deadline := time.Now().Add(3 * time.Second)
			var job map[string]any
			for time.Now().Before(deadline) {
				response := adminRequest(handler, "GET", "/api/grok/oauth/protocol/jobs/"+stringValue(first.Job["id"]), nil)
				var body map[string]any
				_ = json.Unmarshal(response.Body.Bytes(), &body)
				job = mapValue(body["job"])
				for _, secret := range []string{"SSO-SECRET", "WORKER-SECRET", "DEVICE-SECRET", "REFRESH-SECRET", "EXISTING-SECRET", "MUST-NOT-LEAK", "ABCD-EFGH"} {
					if strings.Contains(response.Body.String(), secret) {
						t.Fatal("job leaked a credential or upstream text")
					}
				}
				if !slicesContains([]string{"pending", "running"}, stringValue(job["status"])) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if job["status"] != tc.want {
				t.Fatalf("status %v expected %s", job, tc.want)
			}
			// A cancelled job becomes terminal before the running handler cleans up.
			for time.Now().Before(deadline) {
				if _, live := s.grokOAuthRuns.Load("selected"); !live {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			stored, err := s.oauthStore.List()
			if err != nil {
				t.Fatal(err)
			}
			wanted := 1
			if tc.want == "authorized" {
				wanted = 2
			}
			if len(stored) != wanted {
				t.Fatalf("stored %d credentials; expected %d", len(stored), wanted)
			}
			if starts.Load() != 1 || confirmations.Load() != 1 {
				t.Fatal("flow repeated a device request or confirmation")
			}
			if (tc.workerStatus == "manual_action_required" || tc.cancel || tc.workerStatus == "failed") && polls.Load() != 0 {
				t.Fatal("queried token after explicit stop")
			}
		})
	}
}

func slicesContains(items []string, value string) bool {
	for _, item := range items {
		if value == item {
			return true
		}
	}
	return false
}

func TestGrokBackgroundOAuthRejectsMissingAndAbnormalAccounts(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	for _, row := range []map[string]any{
		{"id": "missing-email", "sso": "fake"}, {"id": "missing-sso", "email": "a@example.test"},
		{"id": "disabled", "email": "a@example.test", "sso": "disabled", "status": "disabled"},
		{"id": "runtime-invalid", "email": "a@example.test", "sso": "runtime-invalid", "status": "active"},
	} {
		if _, _, err := s.registerStore.AddAccount(row); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, _ = s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "runtime-invalid", "status": "invalid"}})
	for _, id := range []string{"", "unknown", "missing-email", "missing-sso", "disabled", "runtime-invalid"} {
		if _, _, err := s.startGrokOAuth(id); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
	if len(s.taskQueue.List()) != 0 {
		t.Fatal("invalid account created a job")
	}
}

func TestGrokBackgroundOAuthInterruptedGrantIsNotReplayed(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	_, _, _ = s.registerStore.AddAccount(map[string]any{"id": "selected", "email": "selected@example.test", "sso": "SSO-SECRET"})
	_, _ = s.registerStore.SetOAuthAuthorization("selected", "running", "")
	task := s.taskQueue.Submit("grok_oauth_authorize", map[string]any{"account_id": "selected"})
	value, err := s.runGrokOAuthTask(task)
	if err != nil || value["status"] != "manual_action_required" || value["code"] != "oauth_interrupted" {
		t.Fatalf("unexpected restart result %v %v", value, err)
	}
}

func TestGrokBackgroundConcurrentStartsAndReload(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	_, _, _ = s.registerStore.AddAccount(map[string]any{"id": "selected", "email": "selected@example.test", "sso": "SSO-SECRET"})
	ids := make(chan string, 10)
	for i := 0; i < 10; i++ {
		go func() {
			job, _, err := s.startGrokOAuth("selected")
			if err != nil {
				t.Error(err)
				ids <- ""
				return
			}
			ids <- job.ID
		}()
	}
	var id string
	for i := 0; i < 10; i++ {
		next := <-ids
		if id != "" && next != id {
			t.Error("concurrent starts created multiple grants")
		}
		id = next
	}
	if len(s.taskQueue.List()) != 1 {
		t.Fatal("expected single queued job")
	}
	response := adminRequest(s.Handler(), "GET", "/api/grok/oauth/candidates", nil)
	var inventory struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &inventory)
	if len(inventory.Items) != 1 || inventory.Items[0]["oauth_status"] != "queued" || inventory.Items[0]["oauth_job_id"] != id {
		t.Fatal("reload cannot recover queued authorization")
	}
	if strings.Contains(response.Body.String(), "SSO-SECRET") {
		t.Fatal("inventory exposed SSO")
	}
	other := s.taskQueue.Submit("unrelated", map[string]any{"secret": "never-expose"})
	response = adminRequest(s.Handler(), "GET", "/api/grok/oauth/protocol/jobs/"+other.ID, nil)
	if response.Code != 404 {
		t.Fatal("OAuth endpoint exposed an unrelated job")
	}
}
