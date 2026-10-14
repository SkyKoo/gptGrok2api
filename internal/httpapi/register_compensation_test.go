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

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestCompensationSchedulerIndependentTimerAndMaintenance(t *testing.T) {
	s := scheduleServer(t)
	now := time.Now()
	profile := registerruntime.NewBrowserProfile()
	job := registerruntime.RegistrationJob{ID: "recover-existing", Status: "registration_pending", Stage: "wait_code", Error: "task_timeout", Mailbox: registerruntime.Mailbox{Email: "alias@example.test", AccountID: "test"}, MailboxAPIBase: "https://mail.example.test", BrowserProfile: &profile, AutoRetry: &registerruntime.RegistrationCompensation{State: "waiting", NextAttemptAt: now.UTC().Format(time.RFC3339Nano)}}
	path := filepath.Join(t.TempDir(), "jobs.json")
	raw, _ := json.Marshal([]registerruntime.RegistrationJob{job})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.freeRegister = registerruntime.NewFreeEngine(path, func(cfg registerruntime.FreeConfig) (registerruntime.MailSource, registerruntime.RegistrationFlow, error) {
		calls.Add(1)
		if cfg.BrowserProfile == nil || !reflect.DeepEqual(*cfg.BrowserProfile, profile) || cfg.HME.AccountID != "test" {
			t.Error("not using saved environment/current mailbox config")
		}
		return nil, nil, &registerruntime.Failure{Stage: "mail", Code: "network_error"}
	}, nil, nil)
	if s.registrationSchedule.Snapshot().Enabled {
		t.Fatal("test requires batch timer disabled")
	}
	h := s.Handler()
	for _, endpoint := range []string{"/api/register/openai/retry-registration", "/api/register/openai/retry-result"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"id":"recover-existing"}`)))
		if w.Code != 401 {
			t.Fatal("retry lacks admin auth")
		}
	}
	marker := filepath.Join(s.cfg.DataDir, "cfm-maintenance")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.runRegistrationSchedule(now)
	if calls.Load() != 0 {
		t.Fatal("compensation started during maintenance")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("maintenance changed journal")
	}
	for _, endpoint := range []string{"/api/register/openai/retry-registration", "/api/register/openai/retry-result"} {
		w := adminRequest(h, http.MethodPost, endpoint, strings.NewReader(`{"id":"recover-existing"}`))
		if w.Code != 503 {
			t.Fatal("manual retry ignored maintenance", w.Code)
		}
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	s.runRegistrationSchedule(now)
	scheduleWait(t, s)
	if calls.Load() != 1 {
		t.Fatal("disabled batch timer blocked compensation")
	}
	res := adminRequest(h, http.MethodGet, "/api/register", nil)
	body := res.Body.String()
	if res.Code != 200 || !strings.Contains(body, `"auto_retry"`) || !strings.Contains(body, `"attempts":1`) || !strings.Contains(body, "next_attempt_at") {
		t.Fatal("missing live compensation state")
	}
	for _, secret := range []string{"PRIVATE-PASSWORD", "browser_profile", "mailbox_api_base"} {
		if strings.Contains(body, secret) {
			t.Fatal("private task details leaked", secret)
		}
	}
	if err := s.ShutdownRegistration(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.runRegistrationSchedule(now.Add(24 * time.Hour))
	if calls.Load() != 1 {
		t.Fatal("launched during shutdown")
	}
}
