package httpapi

import (
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

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestReloginReusesEnvironmentAndPersistsItAcrossRestart(t *testing.T) {
	for _, scenario := range []string{"account", "journal", "legacy_fp", "fresh", "login_failure", "identity_mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			profile := registerruntime.NewBrowserProfile()
			const email = "alias@example.test"
			expectedDevice := "saved-device-123"
			if scenario == "fresh" {
				expectedDevice = ""
			}
			var mu sync.Mutex
			var loggingIDs []string
			var devices []string
			csrfCount, anonymousCount, otpCount := 0, 0, 0
			var base string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
				switch r.URL.Path {
				case "/api/auth/login":
					write(map[string]any{"success": true, "data": map[string]string{"csrf_token": "hme-csrf"}})
				case "/api/inbox":
					if r.URL.Query().Get("alias") != email {
						t.Error("requested wrong mailbox")
					}
					write(map[string]any{"success": true, "data": map[string]any{"messages": []any{map[string]string{
						"From": "noreply@openai.com", "To": email, "Date": time.Now().UTC().Format(time.RFC3339), "Subject": "Code 987654",
					}}}})
				case "/api/auth/providers":
					if r.Header.Get("User-Agent") != profile.UserAgent || r.Header.Get("Sec-Ch-Ua") != profile.SecCHUA || r.Header.Get("Accept-Language") != profile.AcceptLanguage {
						t.Error("HTTP headers do not match saved profile")
					}
					if expectedDevice != "" || len(devices) > 0 {
						want := expectedDevice
						if want == "" {
							want = devices[0]
						}
						if r.Header.Get("Oai-Device-Id") != want {
							t.Error("initial request lost saved device header")
						}
						if c, err := r.Cookie("oai-did"); err != nil || c.Value != want {
							t.Error("initial request lost saved device cookie")
						}
					}
					write(map[string]any{})
				case "/api/auth/session":
					if _, err := r.Cookie("web-session"); err != nil {
						anonymousCount++
						write(map[string]any{})
					} else {
						userID := "user-1"
						if scenario == "identity_mismatch" {
							userID = "user-other"
						}
						write(map[string]any{"accessToken": fmt.Sprintf("new-token-%d", csrfCount), "user": map[string]string{"email": email, "id": userID}})
					}
				case "/api/auth/csrf":
					csrfCount++
					write(map[string]string{"csrfToken": fmt.Sprintf("csrf-%d", csrfCount)})
				case "/api/auth/signin/openai":
					device := r.URL.Query().Get("ext-oai-did")
					if device == "" || expectedDevice != "" && device != expectedDevice {
						t.Error("signin changed saved device")
					}
					if len(devices) > 0 && device != devices[0] {
						t.Error("second login changed saved device")
					}
					devices = append(devices, device)
					loggingIDs = append(loggingIDs, r.URL.Query().Get("auth_session_logging_id"))
					_ = r.ParseForm()
					if r.Form.Get("csrfToken") != fmt.Sprintf("csrf-%d", csrfCount) {
						t.Error("reused stale CSRF")
					}
					write(map[string]string{"url": base + "/api/accounts/authorize?device_id=" + device + fmt.Sprintf("&state=state-%d", csrfCount)})
				case "/api/accounts/authorize":
					http.Redirect(w, r, "/email-verification", http.StatusFound)
				case "/email-verification", "/":
					_, _ = w.Write([]byte("page"))
				case "/api/accounts/email-otp/validate":
					otpCount++
					if scenario == "login_failure" {
						w.WriteHeader(http.StatusForbidden)
						write(map[string]any{})
						return
					}
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["code"] != "987654" {
						t.Error("missing fresh OTP")
					}
					write(map[string]string{"continue_url": base + fmt.Sprintf("/api/auth/callback/openai?state=state-%d&code=new-code", csrfCount)})
				case "/api/auth/callback/openai":
					if r.URL.Query().Get("state") != fmt.Sprintf("state-%d", csrfCount) {
						t.Error("stale authorization state")
					}
					http.SetCookie(w, &http.Cookie{Name: "web-session", Value: "fresh-session", Path: "/"})
					http.Redirect(w, r, "/", http.StatusFound)
				default:
					t.Errorf("unexpected request (must not create mailbox/account): %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			base = upstream.URL
			root := t.TempDir()
			cfg := testConfig()
			cfg.RootDir, cfg.DataDir = root, root
			cfg.AccountsPath = filepath.Join(root, "accounts.json")
			cfg.AuthKeysPath = filepath.Join(root, "keys.json")
			cfg.ConfigPath = filepath.Join(root, "config.json")
			cfg.RegisterPath = filepath.Join(root, "register.json")
			cfg.OpenAIBaseURL, cfg.OpenAIAuthBaseURL = base+"/backend-api", base
			original := map[string]any{"access_token": "old-token", "email": email, "user_id": "user-1", "enabled": false, "created_at": "2026-01-01T00:00:00Z", "source_type": "chatgpt_web", "registration_job_id": "job-1"}
			if scenario != "fresh" {
				original["fp"] = map[string]any{"profile": profile.Name, "user-agent": profile.UserAgent, "impersonate": profile.TLSProfile, "oai-device-id": expectedDevice, "legacy-extra": "keep"}
			}
			if scenario == "account" {
				// Account state wins over an older journal/template with the same name.
				profile.ScreenWidth += 7
				original["browser_profile"] = profile
			}
			journalPath := filepath.Join(root, "openai_registration_tasks.json")
			if scenario == "journal" {
				profile.ScreenHeight += 7
				raw, _ := json.Marshal([]registerruntime.RegistrationJob{{ID: "job-1", Status: "completed", Mailbox: registerruntime.Mailbox{Email: email}, BrowserProfile: &profile}})
				if err := os.WriteFile(journalPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			server := New(cfg)
			_, _, _, err := server.store.AddAccounts(nil, []map[string]any{original})
			if err != nil {
				t.Fatal(err)
			}
			_, err = server.registerStore.Update(map[string]any{"target": "openai", "mode": "total", "total": 1, "threads": 1, "proxy": "direct", "mail": map[string]any{"providers": []any{map[string]any{"id": "test", "type": "icloud_hme", "api_base": base, "admin_password": "test-password", "account_id": "acc-test", "enable": true}}}})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(cfg.AccountsPath)
			attempts := 2
			failing := scenario == "login_failure" || scenario == "identity_mismatch"
			if failing {
				attempts = 1
			}
			for attempt := 1; attempt <= attempts; attempt++ {
				items, _ := server.store.AccountList()
				ref := accountPublicRef(items[0])
				body, _ := json.Marshal(map[string]string{"account_ref": ref})
				req := httptest.NewRequest(http.MethodPost, "/api/accounts/relogin", strings.NewReader(string(body)))
				req.Header.Set("Authorization", "Bearer admin-secret")
				rec := httptest.NewRecorder()
				server.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
				}
				var start map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &start)
				var progress map[string]any
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
					req = httptest.NewRequest(http.MethodGet, "/api/accounts/relogin/progress/"+stringValue(start["progress_id"]), nil)
					req.Header.Set("Authorization", "Bearer admin-secret")
					rec = httptest.NewRecorder()
					server.Handler().ServeHTTP(rec, req)
					_ = json.Unmarshal(rec.Body.Bytes(), &progress)
					if progress["done"] == true {
						break
					}
				}
				if progress["done"] != true {
					t.Fatal("login did not finish")
				}
				if failing {
					if progress["stage"] != "failed" {
						t.Fatal("expected login failure")
					}
					after, _ := os.ReadFile(cfg.AccountsPath)
					assertReloginFailurePreservesAccount(t, before, after)
					continue
				}
				if progress["stage"] != "completed" {
					t.Fatalf("login failed: %v", progress["error"])
				}
				if strings.Contains(rec.Body.String(), "browser_profile") || strings.Contains(rec.Body.String(), "new-token-") {
					t.Fatal("private state leaked into progress")
				}
				items, _ = server.store.AccountList()
				saved := items[0]
				got, ok := registerruntime.ParseBrowserProfile(saved["browser_profile"])
				if !ok {
					t.Fatal("successful login did not persist full profile")
				}
				if scenario == "fresh" && attempt == 1 {
					profile = got
				}
				if !reflect.DeepEqual(got, profile) {
					t.Fatal("saved profile does not match login environment")
				}
				if saved["access_token"] != fmt.Sprintf("new-token-%d", attempt) || saved["enabled"] != false || saved["created_at"] != original["created_at"] {
					t.Fatal("token rotation lost account state")
				}
				if scenario != "fresh" && mapValue(saved["fp"])["legacy-extra"] != "keep" {
					t.Fatal("lost unrelated fingerprint metadata")
				}
				mu.Lock()
				savedDevice := devices[0]
				mu.Unlock()
				if mapValue(saved["fp"])["oai-device-id"] != savedDevice {
					t.Fatal("did not persist device identity")
				}
				if _, err := server.freeRegister.ClearFinished(); err != nil {
					t.Fatal(err)
				}
				server = New(cfg) // Reload only persisted state, with no registration history.
			}
			mu.Lock()
			defer mu.Unlock()
			if csrfCount != attempts || anonymousCount != attempts || otpCount != attempts {
				t.Fatal("login reused an authenticated session or skipped fresh authorization")
			}
			if attempts == 2 && (loggingIDs[0] == "" || loggingIDs[0] == loggingIDs[1]) {
				t.Fatal("reused per-login authorization logging ID")
			}
		})
	}
}

func TestReloginRejectsUnsupportedStoredProfile(t *testing.T) {
	server := &Server{}
	for _, account := range []map[string]any{
		{"browser_profile": map[string]any{"tls_profile": "unknown"}},
		{"fp": map[string]any{"profile": "unknown"}},
	} {
		if _, err := server.accountReloginProfile(account); err == nil {
			t.Fatal("silently replaced saved profile")
		}
	}
	if profile, err := server.accountReloginProfile(map[string]any{}); err != nil || profile != nil {
		t.Fatal("blocked legacy imported account without profile")
	}
}

func TestReloginConcurrentAccountsAndDuplicateStart(t *testing.T) {
	arrived, release := make(chan struct{}, 32), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"error":"unauthorized"}`))
	}))
	defer upstream.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir, cfg.DataDir = root, root
	cfg.AccountsPath, cfg.AuthKeysPath = filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json")
	cfg.ConfigPath, cfg.RegisterPath = filepath.Join(root, "config.json"), filepath.Join(root, "register.json")
	cfg.OpenAIBaseURL, cfg.OpenAIAuthBaseURL = upstream.URL+"/backend-api", upstream.URL
	server := New(cfg)
	_, _, _, err := server.store.AddAccounts(nil, []map[string]any{
		{"access_token": "private-token-a", "email": "a@example.test", "user_id": "account-a"},
		{"access_token": "private-token-b", "email": "b@example.test", "user_id": "account-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.registerStore.Update(map[string]any{"target": "openai", "mode": "total", "total": 1, "threads": 1, "proxy": "direct", "mail": map[string]any{"providers": []any{map[string]any{"id": "test", "type": "icloud_hme", "api_base": upstream.URL, "admin_password": "private-hme-password", "account_id": "acc-test", "enable": true}}}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(cfg.AccountsPath)
	handler := server.Handler()
	request := func(method, path, body, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for _, auth := range []string{"", "api-secret"} {
		if rec := request(http.MethodGet, "/api/accounts/relogin", "", auth); rec.Code == 200 {
			t.Fatal("progress list is not admin-only")
		}
	}
	var wg sync.WaitGroup
	ids := make(chan map[string]string, 24)
	for i := 0; i < 24; i++ {
		ref := []string{"account-a", "account-b"}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := request(http.MethodPost, "/api/accounts/relogin", `{"account_ref":"`+ref+`"}`, "admin-secret")
			if rec.Code != 200 {
				t.Errorf("start: %d", rec.Code)
				return
			}
			var response struct {
				ProgressID string                 `json:"progress_id"`
				Progress   accountReloginProgress `json:"progress"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Error(err)
				return
			}
			if response.Progress.StartedAt == "" || response.Progress.Email == "" || response.Progress.ProgressID != response.ProgressID {
				t.Error("missing progress metadata")
			}
			ids <- map[string]string{"ref": ref, "id": response.ProgressID}
		}()
	}
	wg.Wait()
	close(ids)
	byAccount := map[string]string{}
	for item := range ids {
		if previous := byAccount[item["ref"]]; previous != "" && previous != item["id"] {
			t.Fatal("duplicate login started for same account")
		}
		byAccount[item["ref"]] = item["id"]
	}
	if len(byAccount) != 2 || byAccount["account-a"] == byAccount["account-b"] {
		t.Fatal("accounts did not get independent tasks")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("different accounts did not run concurrently")
		}
	}
	list := func() []accountReloginProgress {
		rec := request(http.MethodGet, "/api/accounts/relogin", "", "admin-secret")
		if rec.Code != 200 {
			t.Fatalf("list: %d", rec.Code)
		}
		for _, secret := range []string{"private-token", "private-hme", "browser_profile", "result"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("list leaked %s", secret)
			}
		}
		var response struct {
			Items []accountReloginProgress `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Items
	}
	running := list()
	if len(running) != 2 || running[0].Done || running[1].Done {
		t.Fatal("list did not restore both running tasks")
	}
	unblock()
	finished := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		tasks := list()
		if len(tasks) == 2 && tasks[0].Done && tasks[1].Done {
			for _, task := range tasks {
				if task.Stage != "failed" || task.FailedStage != "mail" || task.FinishedAt == "" || task.Error == "" {
					t.Fatalf("missing failure stage/result: %+v", task)
				}
			}
			finished = true
			break
		}
	}
	if !finished {
		t.Fatal("tasks did not finish")
	}
	after, _ := os.ReadFile(cfg.AccountsPath)
	assertReloginFailurePreservesAccount(t, before, after)
	select {
	case <-arrived:
		t.Fatal("extra upstream login was launched")
	default:
	}
}

func TestReloginIndependentCompletionAndRetention(t *testing.T) {
	s := &Server{reloginProgress: map[string]*accountReloginProgress{}}
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("finished-%03d", i)
		s.reloginProgress[id] = &accountReloginProgress{ProgressID: id, Done: true, FinishedAt: time.Unix(int64(i), 0).UTC().Format(time.RFC3339Nano)}
	}
	s.reloginProgress["a"] = &accountReloginProgress{ProgressID: "a", AccountRef: "a", Stage: "wait_code"}
	s.reloginProgress["b"] = &accountReloginProgress{ProgressID: "b", AccountRef: "b", Stage: "session"}
	expiry := "2026-10-21T00:00:00Z"
	s.finishAccountRelogin("b", map[string]any{"token_expires_at": expiry, "item": map[string]any{"email": "b@example.test"}}, nil)
	if a := s.reloginProgress["a"]; a.Done || a.Stage != "wait_code" {
		t.Fatal("finishing B changed A")
	}
	if b := s.reloginProgress["b"]; !b.Done || b.TokenExpiresAt != expiry || b.FinishedAt == "" {
		t.Fatal("missing completion metadata")
	}
	if len(s.reloginProgress) != 101 || s.reloginProgress["finished-000"] != nil {
		t.Fatal("retention did not preserve all active plus latest 100 ended tasks")
	}
	if summary := reloginSummary(s.reloginProgress["b"]); summary.Result != nil || summary.TokenExpiresAt != expiry {
		t.Fatal("summary lost expiry or included account result")
	}
}

// Failure may change status diagnostics, but never credentials, saved environment,
// enabled state, quota, or unrelated account fields.
func assertReloginFailurePreservesAccount(t *testing.T, before, after []byte) {
	t.Helper()
	var oldRows, newRows []map[string]any
	if json.Unmarshal(before, &oldRows) != nil || json.Unmarshal(after, &newRows) != nil || len(oldRows) != len(newRows) {
		t.Fatal("account history changed on login failure")
	}
	for i, row := range newRows {
		if row["status"] != "异常" || row["relogin_blocked"] != true || stringValue(row["last_relogin_error"]) == "" || stringValue(row["last_relogin_error_at"]) == "" {
			t.Fatal("failed login was not persistently blocked")
		}
		for _, key := range []string{"status", "relogin_blocked", "last_relogin_error", "last_relogin_error_at"} {
			delete(row, key)
			delete(oldRows[i], key)
		}
		if !reflect.DeepEqual(oldRows[i], row) {
			t.Fatal("failed login changed credentials/environment/other account data")
		}
	}
}
