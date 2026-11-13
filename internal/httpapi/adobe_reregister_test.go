package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

const adobeReregisterFixtureKey = "reregister-fixture-key"
const adobeReregisterFixtureID = "unknown-adobe-registration"

func adobeReregisterServer(t *testing.T, changes ...func(*registerruntime.RegistrationJob)) (*Server, string, *atomic.Int32, <-chan registerruntime.AdobeConfig) {
	t.Helper()
	s := scheduleServer(t)
	_, err := s.registerStore.Update(map[string]any{"mail": map[string]any{"providers": []any{
		map[string]any{"id": "adobe-source", "enable": true, "type": "icloud_hme", "api_base": "https://adobe-mail.example.test", "admin_password": "PRIVATE-HME-RETRY", "account_id": "adobe-source"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	response := adminRequest(s.Handler(), http.MethodPost, "/api/register?target=adobe", strings.NewReader(`{"total":1,"threads":1,"mail_source_id":"adobe-source"}`))
	if response.Code != http.StatusOK {
		t.Fatal("Adobe configuration rejected", response.Code)
	}
	var job registerruntime.RegistrationJob
	if err := json.Unmarshal([]byte(`{"id":"unknown-adobe-registration","status":"registration_pending","stage":"adobe_creation_submitted","error":"adobe: human_verification_required","mailbox":{"email":"fixture@example.test","account_id":"adobe-source","source_id":"adobe-source"},"mailbox_api_base":"https://adobe-mail.example.test","adobe_profile":{"country":"US","birthYear":1988,"birthMonth":10,"birthDay":2},"adobe_registration":{"browser_profile_id":"old-browser-profile","proxy":"direct","proxy_recorded":true,"flow_started":true,"submission_started":true,"creation_outcome":"unknown"},"auto_retry":{"state":"manual","reason":"registration_result_uncertain","attempts":2}}`), &job); err != nil {
		t.Fatal(err)
	}
	job.CreatedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	job.UpdatedAt = job.CreatedAt
	for _, change := range changes {
		change(&job)
	}
	plain, err := json.Marshal([]registerruntime.RegistrationJob{job})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := adobe.Seal(adobeReregisterFixtureKey, plain)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "data": sealed})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	observed := make(chan registerruntime.AdobeConfig, 4)
	s.adobeRegister = registerruntime.NewAdobeEngine(path, adobeReregisterFixtureKey, func(cfg registerruntime.AdobeConfig) (registerruntime.MailSource, registerruntime.AdobeRegistrationFlow, error) {
		calls.Add(1)
		observed <- cfg
		// Stop before constructing any mail client or browser lease.
		return nil, nil, &registerruntime.Failure{Stage: "test", Code: "offline_fixture"}
	}, nil, nil)
	s.adobeRegister.SetReregistrationAccountCheck(s.adobeReregistrationAccountExists)
	return s, path, calls, observed
}

func TestAdobeReregisterAPIAcceptsCreatedButUnsavedAccountWithoutOldEnvironment(t *testing.T) {
	s, path, calls, observed := adobeReregisterServer(t, func(job *registerruntime.RegistrationJob) {
		job.AdobeProfile = nil
		job.AdobeRegistration.CreationOutcome = "accepted"
		job.AdobeRegistration.BrowserProfileID = ""
		job.AdobeRegistration.ProxyRecorded = false
		job.Error = "adobe: media_session_incomplete"
	})
	before := readAdobeReregisterJobs(t, path)[0]
	response := adminRequest(s.Handler(), http.MethodGet, "/api/register/adobe/jobs", nil)
	var snapshot struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || len(snapshot.Jobs) != 1 || snapshot.Jobs[0]["can_reregister"] != true {
		t.Fatal("unsaved accepted attempt did not expose explicit fresh registration")
	}
	response = adminRequest(s.Handler(), http.MethodPost, "/api/register/adobe/reregister", strings.NewReader(`{"id":"`+adobeReregisterFixtureID+`"}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("fresh registration rejected due to missing prior environment: HTTP %d", response.Code)
	}
	waitAdobeReregisterIdle(t, s)
	jobs := readAdobeReregisterJobs(t, path)
	if len(jobs) != 2 || calls.Load() != 1 || !reflect.DeepEqual(jobs[0].AdobeRegistration, before.AdobeRegistration) || jobs[0].AdobeProfile != nil || jobs[0].Error != before.Error {
		t.Fatal("explicit action lost old evidence or created multiple attempts")
	}
	child := jobs[1]
	if child.AdobeProfile == nil || child.AdobeRegistration.BrowserProfileID != child.ID || !child.AdobeRegistration.ManualReregistration || child.AutoRetry.Attempts != before.AutoRetry.Attempts || child.Mailbox != before.Mailbox || child.RootJobID != before.ID {
		t.Fatal("fresh child did not preserve root ownership and budget with a new profile")
	}
	select {
	case cfg := <-observed:
		if cfg.RecoveryMode != "retry_registration" || cfg.BrowserProfileID != child.ID {
			t.Fatal("explicit API request resumed the previous environment")
		}
	default:
		t.Fatal("fresh attempt did not reach the offline factory")
	}
}

func readAdobeReregisterJobs(t *testing.T, path string) []registerruntime.RegistrationJob {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct{ Data []byte }
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	plain, err := adobe.Open(adobeReregisterFixtureKey, envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var jobs []registerruntime.RegistrationJob
	if err := json.Unmarshal(plain, &jobs); err != nil {
		t.Fatal(err)
	}
	return jobs
}

func waitAdobeReregisterIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.adobeRegister.Snapshot()["running"] == false {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("offline Adobe registration did not finish")
}

func TestAdobeReregisterAPIStartsFreshChildAndPreservesUnknownParent(t *testing.T) {
	s, path, calls, observed := adobeReregisterServer(t)
	before := readAdobeReregisterJobs(t, path)[0]
	response := adminRequest(s.Handler(), http.MethodPost, "/api/register/adobe/reregister", strings.NewReader(`{"id":"`+adobeReregisterFixtureID+`"}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("explicit re-registration rejected: %d %s", response.Code, response.Body.String())
	}
	waitAdobeReregisterIdle(t, s)
	jobs := readAdobeReregisterJobs(t, path)
	if len(jobs) != 2 || calls.Load() != 1 {
		t.Fatal("explicit action did not create exactly one offline attempt")
	}
	parent, child := jobs[0], jobs[1]
	if parent.RetryJobID != child.ID || child.ParentJobID != parent.ID || child.RootJobID != parent.ID || child.ID == parent.ID {
		t.Fatal("registration child lost its parent or original mailbox claim")
	}
	if !reflect.DeepEqual(parent.AdobeRegistration, before.AdobeRegistration) || parent.Error != before.Error || parent.Status != before.Status {
		t.Fatal("explicit action rewrote the original unknown outcome")
	}
	if child.AdobeRegistration == nil || child.AdobeRegistration.BrowserProfileID != child.ID || child.AdobeRegistration.BrowserProfileID == before.AdobeRegistration.BrowserProfileID || child.AdobeRegistration.SubmissionStarted {
		t.Fatal("fresh registration reused the previous browser or submission state")
	}
	if !reflect.DeepEqual(child.Mailbox, before.Mailbox) || child.AutoRetry == nil || child.AutoRetry.Attempts != before.AutoRetry.Attempts {
		t.Fatal("fresh registration changed mailbox identity or reset automatic budget")
	}
	select {
	case cfg := <-observed:
		if cfg.RecoveryMode != "retry_registration" || cfg.BrowserProfileID != child.ID || cfg.HME.AccountID != "adobe-source" {
			t.Fatal("explicit re-registration selected resume or another mailbox source")
		}
	default:
		t.Fatal("offline factory was not called")
	}
	for _, secret := range []string{"PRIVATE-HME-RETRY", "browser_profile_id", "old-browser-profile", "proxy_recorded"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("private registration environment leaked in API response")
		}
	}
	response = adminRequest(s.Handler(), http.MethodPost, "/api/register/adobe/reregister", strings.NewReader(`{"id":"`+adobeReregisterFixtureID+`"}`))
	if response.Code < 400 || calls.Load() != 1 || len(readAdobeReregisterJobs(t, path)) != 2 {
		t.Fatal("repeated parent request created another registration")
	}
}

func TestAdobeReregisterKeepsLegacyRetryUnknownProtection(t *testing.T) {
	s, path, calls, _ := adobeReregisterServer(t)
	before, _ := os.ReadFile(path)
	response := adminRequest(s.Handler(), http.MethodPost, "/api/register/adobe/retry-registration", strings.NewReader(`{"id":"`+adobeReregisterFixtureID+`"}`))
	after, _ := os.ReadFile(path)
	if response.Code < 400 || !strings.Contains(response.Body.String(), "registration_result_uncertain") || calls.Load() != 0 || string(before) != string(after) {
		t.Fatal("legacy recovery endpoint allowed a fresh unknown registration")
	}
}

func TestAdobeReregisterAPIAuthorizationMaintenanceAndShutdown(t *testing.T) {
	for _, mode := range []string{"unauthenticated", "api_key", "maintenance", "shutdown", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			s, path, calls, _ := adobeReregisterServer(t)
			before, _ := os.ReadFile(path)
			body := `{"id":"` + adobeReregisterFixtureID + `"}`
			want := http.StatusServiceUnavailable
			switch mode {
			case "maintenance":
				if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "cfm-maintenance"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				if err := s.ShutdownRegistration(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				body, want = "{", http.StatusBadRequest
			default:
				want = http.StatusUnauthorized
			}
			request := httptest.NewRequest(http.MethodPost, "/api/register/adobe/reregister", strings.NewReader(body))
			if mode == "api_key" {
				request.Header.Set("Authorization", "Bearer api-secret")
			} else if mode != "unauthenticated" {
				request.Header.Set("Authorization", "Bearer admin-secret")
			}
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, request)
			after, _ := os.ReadFile(path)
			if response.Code != want || calls.Load() != 0 || string(before) != string(after) {
				t.Fatalf("%s guard failed: HTTP %d", mode, response.Code)
			}
		})
	}
}

func TestAdobeReregisterRejectsUnlinkedImportedAccountAndLookupFailure(t *testing.T) {
	for _, mode := range []string{"imported", "corrupt_store", "missing_store"} {
		t.Run(mode, func(t *testing.T) {
			s, path, calls, _ := adobeReregisterServer(t)
			want := "mailbox_already_has_saved_account"
			switch mode {
			case "imported":
				account := adobeFixtureAccount(t, s)
				if account.RegistrationJobID != "" || len(account.RegistrationMailbox) != 0 {
					t.Fatal("fixture must be manually imported without mailbox linkage")
				}
			case "corrupt_store":
				badPath := filepath.Join(t.TempDir(), "invalid-account-store")
				if err := os.WriteFile(badPath, []byte("not an encrypted store"), 0600); err != nil {
					t.Fatal(err)
				}
				s.adobeStore = adobe.NewStore(badPath, "fixture-key")
				want = "account_lookup_failed"
			case "missing_store":
				s.adobeStore = nil
				want = "account_lookup_failed"
			}
			before, _ := os.ReadFile(path)
			response := adminRequest(s.Handler(), http.MethodPost, "/api/register/adobe/reregister", strings.NewReader(`{"id":"`+adobeReregisterFixtureID+`"}`))
			after, _ := os.ReadFile(path)
			if response.Code < 400 || !strings.Contains(response.Body.String(), want) || calls.Load() != 0 || string(before) != string(after) {
				t.Fatalf("%s failed to protect existing accounts: %d %s", mode, response.Code, response.Body.String())
			}
		})
	}
}

func TestAdobeReregistrationAccountCheckUsesNormalizedExactEmail(t *testing.T) {
	s := scheduleServer(t)
	adobeFixtureAccount(t, s)
	for _, tc := range []struct {
		email string
		want  bool
	}{
		{"  FIXTURE@EXAMPLE.TEST  ", true},
		{"fixture+new@example.test", false},
		{"other-fixture@example.test", false},
	} {
		got, err := s.adobeReregistrationAccountExists(tc.email)
		if err != nil || got != tc.want {
			t.Fatalf("normalized exact match failed: got %v, error %v", got, err)
		}
	}
}

func TestAdobeReregisterInitialEngineChecksImportedAccount(t *testing.T) {
	s, fixturePath, _, _ := adobeReregisterServer(t)
	adobeFixtureAccount(t, s)
	plain, err := json.Marshal(readAdobeReregisterJobs(t, fixturePath))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := adobe.Seal(s.cfg.AdminKey, plain)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "data": sealed})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(s.cfg.RegisterPath), "adobe_registration_tasks.enc.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Exercise production initialization, not the mock engine's injected guard.
	s.initAdobeRegistration()
	response := adminRequest(s.Handler(), http.MethodPost, "/api/register/adobe/reregister", strings.NewReader(`{"id":"`+adobeReregisterFixtureID+`"}`))
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code < 400 || !strings.Contains(response.Body.String(), "mailbox_already_has_saved_account") || string(raw) != string(after) {
		t.Fatal("production engine omitted imported-account guard")
	}
}
