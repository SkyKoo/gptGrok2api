package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"github.com/auucoder/gptgrok2api-go/internal/tasks"
)

func TestGrokReloginSharedQueueRotatesSSOAndRefreshesQuota(t *testing.T) {
	for _, scenario := range []string{"success", "quota_failure", "wrong_identity", "verification_required"} {
		t.Run(scenario, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			const email = "grok-relogin@example.test"
			const password = "fixture-password"
			token := func(subject, marker string) string {
				raw, _ := json.Marshal(map[string]string{"sub": subject, "email": email})
				return "e30." + base64.RawURLEncoding.EncodeToString(raw) + "." + marker
			}
			oldToken, newToken := token("fixture-user", "old"), token("fixture-user", "new")
			if scenario == "wrong_identity" {
				newToken = token("another-user", "new")
			}
			answer := func(req *http.Request, status int, raw any) (*http.Response, error) {
				data, _ := json.Marshal(raw)
				return &http.Response{Request: req, StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			}
			account, _, err := s.registerStore.AddAccount(map[string]any{"sso": oldToken, "email": email, "password": password, "status": "active"})
			if err != nil {
				t.Fatal(err)
			}
			id := stringValue(account["id"])
			if _, _, err = s.syncRegisteredGrokRuntimeAccount(account, oldToken); err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.grokRuntimeStore.UpdateAccount(oldToken, map[string]any{"pool": "paid", "use_count": 8, "status": "invalid"}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.registerStore.UpdateAccountFields(id, map[string]any{"status": "invalid"}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.registerStore.Update(map[string]any{"grok": map[string]any{"api_base": "http://worker.test", "api_key": "fixture-worker-key"}}); err != nil {
				t.Fatal(err)
			}
			var reserved, released atomic.Bool
			var loginCalls, quotaCalls atomic.Int32
			var lease string
			s.requestClient = &http.Client{Transport: updateRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Authorization") != "Bearer fixture-worker-key" {
					t.Error("missing worker credentials")
				}
				switch {
				case req.Method == http.MethodPost && req.URL.Path == "/v1/browser/reservations":
					var payload map[string]any
					_ = json.NewDecoder(req.Body).Decode(&payload)
					lease = stringValue(payload["id"])
					if lease == "" || payload["provider"] != "grok" || payload["password"] != nil {
						t.Error("invalid reservation payload")
					}
					reserved.Store(true)
					return answer(req, 200, map[string]any{"status": "ready"})
				case req.Method == http.MethodPost && req.URL.Path == "/v1/grok/relogin":
					loginCalls.Add(1)
					var payload map[string]string
					_ = json.NewDecoder(req.Body).Decode(&payload)
					if !reserved.Load() || payload["browser_lease"] != lease || payload["email"] != email || payload["password"] != password {
						t.Error("login did not use the reserved browser and saved account")
					}
					if scenario == "verification_required" {
						return answer(req, 200, map[string]any{"code": "verification_required"})
					}
					return answer(req, 200, map[string]any{"sso": newToken, "sso_rw": "new-rw-fixture"})
				case req.Method == http.MethodDelete && req.URL.Path == "/v1/browser/reservations/"+lease:
					released.Store(true)
					return answer(req, 200, map[string]any{"status": "closed"})
				default:
					t.Errorf("unexpected worker request to %s", req.URL.Path)
					return answer(req, 404, nil)
				}
			})}
			s.grokQuota = provider.NewGrokQuota("https://upstream.test/rest/rate-limits", &http.Client{Transport: updateRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				quotaCalls.Add(1)
				if !released.Load() || req.Header.Get("Cookie") != "sso="+newToken+"; sso-rw=new-rw-fixture" {
					t.Error("quota query did not use the new cookies after releasing the browser")
				}
				pool, _, _ := s.grokRuntimeStore.AccountSnapshot()
				if len(pool) != 1 || accountToken(pool[0]) != newToken {
					t.Error("quota query started before pool sync")
				}
				if scenario == "quota_failure" {
					return answer(req, 429, nil)
				}
				if req.URL.Path == "/rest/media/imagine/quota_info" {
					return answer(req, 200, map[string]any{
						"imagePro":  map[string]any{"available": true, "remainingQueries": 2},
						"video720p": map[string]any{"available": true, "remainingQueries": 1},
					})
				}
				return answer(req, 200, map[string]any{"remainingQueries": 4, "totalQueries": 10})
			})}, nil)

			const path = "/api/register/grok/accounts/relogin"
			body, _ := json.Marshal(map[string]string{"account_id": id, "request_id": "fixture-relogin-request"})
			response := imageTaskCall(s, "POST", path, "admin-secret", string(body))
			var result struct{ Job tasks.Task }
			if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Job.ID == "" {
				t.Fatalf("relogin was not queued: %d", response.Code)
			}
			again := imageTaskCall(s, "POST", path, "admin-secret", string(body))
			if again.Code != http.StatusAccepted || !strings.Contains(again.Body.String(), result.Job.ID) {
				t.Fatal("duplicate did not reuse the task")
			}
			s.taskQueue.Start(1)
			t.Cleanup(func() { s.taskQueue.(*tasks.Queue).StopDeployment() })
			var job tasks.Task
			for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				job, _ = s.taskQueue.Get(result.Job.ID)
				if job.Status == "completed" || job.Status == "failed" {
					break
				}
			}
			current, _, _ := s.grokRuntimeStore.AccountSnapshot()
			archive, _ := s.registerStore.GetAccounts([]string{id})
			if len(current) != 1 || len(archive) != 1 || loginCalls.Load() != 1 || !released.Load() {
				t.Fatal("unexpected account or browser task count")
			}
			progress := imageTaskCall(s, "GET", path, "admin-secret", "")
			for _, secret := range []string{password, oldToken, newToken, "new-rw-fixture", lease} {
				if strings.Contains(progress.Body.String(), secret) {
					t.Fatal("progress contains a private value")
				}
			}
			if scenario == "wrong_identity" || scenario == "verification_required" {
				want := map[string]string{"wrong_identity": "grok_relogin_identity_mismatch", "verification_required": "grok_relogin_verification_required"}[scenario]
				if job.Status != "failed" || job.Error != want || quotaCalls.Load() != 0 || archive[0]["sso"] != oldToken || accountToken(current[0]) != oldToken {
					t.Fatalf("failed login changed existing credentials: %s %s", job.Status, job.Error)
				}
				return
			}
			if job.Status != "completed" || job.Result["session_updated"] != true || archive[0]["sso"] != newToken ||
				accountToken(current[0]) != newToken || current[0]["pool"] != "paid" || intValue(current[0]["use_count"]) != 8 || quotaCalls.Load() < 2 {
				t.Fatalf("session rotation or refresh did not complete: %s %s", job.Status, job.Error)
			}
			for _, field := range []string{"text_quota", "image_quota", "video_quota"} {
				if job.Result[field] != (scenario == "success") {
					t.Fatalf("wrong result for %s: %v", field, job.Result[field])
				}
			}
			s.persistRuntimeToken(oldToken, nil, "active", "")
			rows, _, _ := s.grokRuntimeStore.AccountSnapshot()
			if len(rows) != 1 || accountToken(rows[0]) != newToken {
				t.Fatal("late quota result restored the old pool token")
			}
		})
	}
}
