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
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestAdobeCompensationSchedulerAndAPIGuards(t *testing.T) {
	s := scheduleServer(t)
	h := s.Handler()
	// Source connections are shared assets; only their selection is scoped to a tab.
	var sources map[string]any
	_ = json.Unmarshal([]byte(`{"mail":{"source_id":"openai-source","providers":[{"id":"openai-source","enable":true,"type":"icloud_hme","api_base":"https://mail.example.test","admin_password":"PRIVATE-OPENAI-MAIL","account_id":"test"},{"id":"adobe-source","enable":true,"type":"icloud_hme","api_base":"https://adobe-mail.example.test","admin_password":"PRIVATE-ADOBE-MAIL","account_id":"adobe-source"}]}}`), &sources)
	if _, err := s.registerStore.Update(sources); err != nil {
		t.Fatal(err)
	}
	res := adminRequest(h, http.MethodPost, "/api/register?target=adobe", strings.NewReader(`{"total":1,"mail_source_id":"adobe-source"}`))
	if res.Code != 200 {
		t.Fatal("Adobe configuration rejected", res.Code)
	}
	if s.registerStore.Get()["target"] != "openai" {
		t.Fatal("test must keep another target selected")
	}
	now := time.Now().UTC()
	var job registerruntime.RegistrationJob
	if err := json.Unmarshal([]byte(`{"id":"adobe-original","status":"registration_pending","stage":"adobe_creation_submitted","error":"adobe: submission_result_unknown","mailbox":{"email":"fixture@example.test","account_id":"adobe-source"},"mailbox_api_base":"https://adobe-mail.example.test","adobe_profile":{"country":"US","birthYear":1988,"birthMonth":10,"birthDay":2},"adobe_registration":{"browser_profile_id":"adobe-original","proxy":"direct","proxy_recorded":true,"flow_started":true,"submission_started":true}}`), &job); err != nil {
		t.Fatal(err)
	}
	job.AdobeRegistration.CreationOutcome = "accepted"
	plain, _ := json.Marshal([]registerruntime.RegistrationJob{job})
	sealed, err := adobe.Seal("fixture-key", plain)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"version": 1, "data": sealed})
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.adobeRegister = registerruntime.NewAdobeEngine(path, "fixture-key", func(cfg registerruntime.AdobeConfig) (registerruntime.MailSource, registerruntime.AdobeRegistrationFlow, error) {
		calls.Add(1)
		if cfg.HME.AccountID != "adobe-source" || cfg.HME.BaseURL != "https://adobe-mail.example.test" {
			t.Error("Adobe compensation used another target's source")
		}
		return nil, nil, &registerruntime.Failure{Stage: "browser", Code: "adobe_browser_unavailable"}
	}, nil, nil)
	if s.adobeRegistrationSchedule.Snapshot().Enabled {
		t.Fatal("batch timer must be disabled")
	}
	endpoints := []string{"/api/register/adobe/retry-registration", "/api/register/adobe/retry-result"}
	for _, endpoint := range endpoints {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"id":"adobe-original"}`)))
		if w.Code != 401 {
			t.Fatal("retry lacks administrator authentication")
		}
	}
	marker := filepath.Join(s.cfg.DataDir, "cfm-maintenance")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.runRegistrationSchedule(now)
	after, _ := os.ReadFile(path)
	if calls.Load() != 0 || string(after) != string(raw) {
		t.Fatal("maintenance allowed compensation or journal mutation")
	}
	for _, endpoint := range endpoints {
		if w := adminRequest(h, http.MethodPost, endpoint, strings.NewReader(`{"id":"adobe-original"}`)); w.Code != 503 {
			t.Fatal("retry ignored maintenance")
		}
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	s.PauseDeployment(true)
	s.runRegistrationSchedule(now)
	after, _ = os.ReadFile(path)
	if calls.Load() != 0 || string(after) != string(raw) {
		t.Fatal("paused deployment migrated legacy journal or started compensation")
	}
	s.PauseDeployment(false)
	if w := adminRequest(h, http.MethodPost, endpoints[1], strings.NewReader(`{"id":"adobe-original"}`)); w.Code < 400 {
		t.Fatal("saved-result endpoint started signup recovery")
	}
	if calls.Load() != 0 {
		t.Fatal("saved-result endpoint launched browser")
	}
	s.runRegistrationSchedule(now)
	if calls.Load() != 0 {
		t.Fatal("legacy task skipped initial compensation cooldown")
	}
	s.runRegistrationSchedule(now.Add(6 * time.Minute))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && s.adobeRegister.Snapshot()["running"] == true {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 || s.adobeRegister.Snapshot()["running"] == true {
		t.Fatalf("disabled timer or another selected tab blocked Adobe compensation: calls=%d snapshot=%+v", calls.Load(), s.adobeRegister.Snapshot())
	}
	res = adminRequest(h, http.MethodGet, "/api/register/adobe/jobs", nil)
	body := res.Body.String()
	if res.Code != 200 || !strings.Contains(body, `"attempts":1`) || !strings.Contains(body, `"parent_job_id":"adobe-original"`) {
		t.Fatal("missing compensation child or durable attempt count")
	}
	for _, secret := range []string{"PRIVATE-ADOBE-MAIL", "browser_profile_id", "proxy_recorded"} {
		if strings.Contains(body, secret) {
			t.Fatal("private recovery state leaked")
		}
	}
	if err := s.ShutdownRegistration(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.runRegistrationSchedule(now.Add(24 * time.Hour))
	for _, endpoint := range endpoints {
		if w := adminRequest(h, http.MethodPost, endpoint, strings.NewReader(`{"id":"adobe-original"}`)); w.Code != 503 {
			t.Fatal("retry ignored shutdown")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("compensation launched during shutdown")
	}
}

func TestAdobeUnknownRegistrationAPIRefusesSignupAndLogin(t *testing.T) {
	s := scheduleServer(t)
	job := registerruntime.RegistrationJob{ID: "unknown-registration", Status: "registration_pending", Stage: "adobe_flow_started", Error: "adobe: login_ui_not_ready", Mailbox: registerruntime.Mailbox{Email: "fixture@example.test"}, AdobeRegistration: &registerruntime.AdobeRegistrationState{BrowserProfileID: "original", FlowStarted: true}}
	plain, _ := json.Marshal([]registerruntime.RegistrationJob{job})
	sealed, err := adobe.Seal("fixture-key", plain)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"version": 1, "data": sealed})
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.adobeRegister = registerruntime.NewAdobeEngine(path, "fixture-key", func(registerruntime.AdobeConfig) (registerruntime.MailSource, registerruntime.AdobeRegistrationFlow, error) {
		calls.Add(1)
		return nil, nil, &registerruntime.Failure{Stage: "test", Code: "unexpected"}
	}, nil, nil)
	response := adminRequest(s.Handler(), http.MethodPost, "/api/register/adobe/retry-registration", strings.NewReader(`{"id":"unknown-registration"}`))
	if response.Code < 400 || !strings.Contains(response.Body.String(), "registration_result_uncertain") || calls.Load() != 0 {
		t.Fatalf("unknown request admitted: %d", response.Code)
	}
	response = adminRequest(s.Handler(), http.MethodGet, "/api/register/adobe/jobs", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"recovery_mode":"result_uncertain"`) || !strings.Contains(response.Body.String(), `"can_retry_registration":false`) || !strings.Contains(response.Body.String(), `"can_resume_registration":false`) {
		t.Fatal("unknown state has wrong public capabilities")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("rejected API request changed original journal")
	}
}
