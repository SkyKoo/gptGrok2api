package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

const scheduleBatchConfig = `{"target":"openai","mode":"total","total":3,"threads":2,"mail":{"providers":[{"enable":true,"type":"icloud_hme","api_base":"https://mail.example.test","admin_password":"PRIVATE-PASSWORD","account_id":"test"}]}}`

func scheduleServer(t *testing.T) *Server {
	t.Helper()
	s := New(adminTestConfig(t.TempDir()))
	res := adminRequest(s.Handler(), http.MethodPost, "/api/register", strings.NewReader(scheduleBatchConfig))
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	t.Cleanup(func() { _ = s.ShutdownRegistration(context.Background()) })
	return s
}
func scheduleDue(t *testing.T, s *Server) time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339Nano, s.registrationSchedule.Snapshot().NextRunAt)
	if err != nil {
		t.Fatal(err)
	}
	return now
}
func scheduleWait(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.freeRegister.Snapshot()["running"] == false {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("batch did not finish")
}
func TestRegistrationScheduleAPIAndCurrentBatchOptions(t *testing.T) {
	s := scheduleServer(t)
	h := s.Handler()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		res := httptest.NewRecorder()
		h.ServeHTTP(res, httptest.NewRequest(method, "/api/register/schedule", strings.NewReader(`{"enabled":true,"interval_minutes":1}`)))
		if res.Code != 401 {
			t.Fatal("schedule lacks admin auth")
		}
	}
	for _, body := range []string{`{}`, `{"enabled":true,"interval_minutes":0}`, `{"enabled":true,"interval_minutes":1.5}`, `{"enabled":true,"interval_minutes":10081}`} {
		res := adminRequest(h, http.MethodPost, "/api/register/schedule", strings.NewReader(body))
		if res.Code != 400 {
			t.Fatalf("invalid accepted: %d", res.Code)
		}
	}
	res := adminRequest(h, http.MethodPost, "/api/register/schedule", strings.NewReader(`{"enabled":true,"interval_minutes":1,"next_run_at":"2000-01-01T00:00:00Z"}`))
	if res.Code != 200 || strings.Contains(res.Body.String(), "PRIVATE") || strings.Contains(res.Body.String(), "2000-01-01") {
		t.Fatal("unsafe schedule response")
	}
	before := s.registrationSchedule.Snapshot()
	// Ordinary configuration autosaves cannot disable or forge scheduler state.
	res = adminRequest(h, http.MethodPost, "/api/register", strings.NewReader(`{"registration_schedule":{"enabled":false},"total":2,"threads":1}`))
	if res.Code != 200 || s.registrationSchedule.Snapshot() != before {
		t.Fatal("config autosave changed timing state")
	}
	observed := make(chan registerruntime.FreeConfig, 10)
	s.freeRegister = registerruntime.NewFreeEngine(filepath.Join(t.TempDir(), "jobs.json"), func(c registerruntime.FreeConfig) (registerruntime.MailSource, registerruntime.RegistrationFlow, error) {
		observed <- c
		return nil, nil, &registerruntime.Failure{Stage: "test", Code: "fixture"}
	}, nil, nil)
	s.runRegistrationSchedule(scheduleDue(t, s))
	scheduleWait(t, s)
	if len(observed) != 2 {
		t.Fatalf("did not inherit total: %d", len(observed))
	}
	for len(observed) > 0 {
		c := <-observed
		if c.Total != 2 || c.Threads != 1 || c.HME.Password != "PRIVATE-PASSWORD" {
			t.Fatal("batch configuration not reused")
		}
	}
	if s.registrationSchedule.Snapshot().LastResult != "started" {
		t.Fatal("missing started state")
	}
	// A scheduled start does not wipe other batch, mailbox or retry settings.
	raw := s.registerStore.Get()
	if intValue(raw["total"]) != 2 || intValue(raw["threads"]) != 1 {
		t.Fatal("batch mutated")
	}
	if err := s.ShutdownRegistration(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.runRegistrationSchedule(scheduleDue(t, s))
	if len(observed) != 0 {
		t.Fatal("started during shutdown")
	}
}

type blockingScheduleMail struct{}

func (*blockingScheduleMail) Acquire(ctx context.Context, _ string) (registerruntime.Mailbox, error) {
	<-ctx.Done()
	return registerruntime.Mailbox{}, ctx.Err()
}
func (*blockingScheduleMail) WaitCode(context.Context, registerruntime.Mailbox, time.Time) (string, error) {
	return "", nil
}
func (*blockingScheduleMail) Close() {}

type unusedScheduleFlow struct{}

func (*unusedScheduleFlow) Register(context.Context, registerruntime.Mailbox, registerruntime.FreeConfig, func(context.Context, time.Time) (string, error), registerruntime.Progress) (map[string]any, error) {
	return nil, nil
}
func (*unusedScheduleFlow) Close() {}

func TestRegistrationScheduleManualOverlapMaintenanceAndDisable(t *testing.T) {
	s := scheduleServer(t)
	h := s.Handler()
	s.freeRegister = registerruntime.NewFreeEngine(filepath.Join(t.TempDir(), "jobs.json"), func(registerruntime.FreeConfig) (registerruntime.MailSource, registerruntime.RegistrationFlow, error) {
		return &blockingScheduleMail{}, &unusedScheduleFlow{}, nil
	}, nil, nil)
	res := adminRequest(h, http.MethodPost, "/api/register/schedule", strings.NewReader(`{"enabled":true,"interval_minutes":1}`))
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	res = adminRequest(h, http.MethodPost, "/api/register/start", nil)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	s.runRegistrationSchedule(scheduleDue(t, s))
	if !strings.Contains(s.registrationSchedule.Snapshot().LastError, "already_running") {
		t.Fatal("overlapping registration not skipped")
	}
	res = adminRequest(h, http.MethodPost, "/api/register/schedule", strings.NewReader(`{"enabled":false,"interval_minutes":1}`))
	if res.Code != 200 || s.freeRegister.Snapshot()["running"] != true {
		t.Fatal("disabling interrupted current batch")
	}
	res = adminRequest(h, http.MethodPost, "/api/register/stop", nil)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	scheduleWait(t, s)
	_ = s.registrationSchedule.Configure(true, 1, time.Now())
	marker := filepath.Join(s.cfg.DataDir, "cfm-maintenance")
	if err := os.WriteFile(marker, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	s.runRegistrationSchedule(scheduleDue(t, s))
	if !strings.Contains(s.registrationSchedule.Snapshot().LastError, "maintenance") {
		t.Fatal("maintenance not respected")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	// Manual stop controls only the current batch, not the recurring switch.
	res = adminRequest(h, http.MethodPost, "/api/register/stop", nil)
	if res.Code != 200 || !s.registrationSchedule.Snapshot().Enabled {
		t.Fatal("manual stop disabled schedule")
	}
	res = adminRequest(h, http.MethodGet, "/api/register", nil)
	var response map[string]any
	_ = json.Unmarshal(res.Body.Bytes(), &response)
	if mapValue(response["register"])["registration_schedule"] == nil {
		t.Fatal("missing live schedule snapshot")
	}
}

func TestRegistrationScheduleFullHistoryPreservesRecovery(t *testing.T) {
	s := scheduleServer(t)
	path := filepath.Join(t.TempDir(), "jobs.json")
	jobs := make([]map[string]any, 100)
	for i := range jobs {
		jobs[i] = map[string]any{"id": "saved-job-" + strconv.Itoa(i), "status": "registration_pending", "mailbox": map[string]any{"email": "saved@example.test"}}
	}
	raw, _ := json.Marshal(jobs)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s.freeRegister = registerruntime.NewFreeEngine(path, func(registerruntime.FreeConfig) (registerruntime.MailSource, registerruntime.RegistrationFlow, error) {
		t.Error("must not create mailbox")
		return nil, nil, nil
	}, nil, nil)
	_ = s.registrationSchedule.Configure(true, 1, time.Now())
	s.runRegistrationSchedule(scheduleDue(t, s))
	state := s.registrationSchedule.Snapshot()
	if state.LastResult != "failed" || !strings.Contains(state.LastError, "history_full") {
		t.Fatal("history failure missing")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("recovery journal changed")
	}
}
