package httpapi

import (
	"context"
	"encoding/json"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type unusedIsolationGrokFlow struct{}

func (*unusedIsolationGrokFlow) Register(context.Context, registerruntime.Mailbox, registerruntime.GrokConfig, func(context.Context, time.Time) (string, error), registerruntime.Progress) (map[string]any, error) {
	return nil, nil
}
func (*unusedIsolationGrokFlow) Close() {}

func isolationState(t *testing.T, s *Server, target string) map[string]any {
	t.Helper()
	res := adminRequest(s.Handler(), http.MethodGet, "/api/register?target="+target, nil)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return mapValue(payload["register"])
}
func isolationWait(t *testing.T, s *Server, target string, running bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state := s.freeRegister.Snapshot()
		if target == "grok" {
			state = s.grokRegister.Snapshot()
		}
		if state["running"] == running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s did not become running=%v", target, running)
}
func TestRegistrationChannelsRunAndStopIndependently(t *testing.T) {
	for _, first := range []string{"openai", "grok"} {
		t.Run(first, func(t *testing.T) {
			s := scheduleServer(t)
			second := "grok"
			if first == "grok" {
				second = "openai"
			}
			s.freeRegister = registerruntime.NewFreeEngine(filepath.Join(t.TempDir(), "openai.json"), func(registerruntime.FreeConfig) (registerruntime.MailSource, registerruntime.RegistrationFlow, error) {
				return &blockingScheduleMail{}, &unusedScheduleFlow{}, nil
			}, nil, nil)
			s.grokRegister = registerruntime.NewGrokEngine(filepath.Join(t.TempDir(), "grok.json"), func(registerruntime.GrokConfig) (registerruntime.MailSource, registerruntime.GrokRegistrationFlow, error) {
				return &blockingScheduleMail{}, &unusedIsolationGrokFlow{}, nil
			}, nil, nil)
			post := func(path, body string, want int) {
				t.Helper()
				res := adminRequest(s.Handler(), http.MethodPost, path, strings.NewReader(body))
				if res.Code != want {
					t.Fatalf("%s: status=%d body=%s", path, res.Code, res.Body.String())
				}
			}
			post("/api/register?target="+first, `{"total":1,"threads":1}`, 200)
			post("/api/register/start?target="+first, ``, 200)
			isolationWait(t, s, first, true)
			if isolationState(t, s, second)["enabled"] != false {
				t.Fatal("idle channel shows running")
			}
			post("/api/register?target="+first, `{"total":2}`, 409)
			post("/api/register?target="+second, `{"total":1,"threads":1,"proxy":"direct"}`, 200)
			post("/api/register/start?target="+second, ``, 200)
			isolationWait(t, s, second, true)
			state := isolationState(t, s, second)
			for _, target := range []string{first, second} {
				if mapValue(mapValue(state["target_runtime"])[target])["enabled"] != true {
					t.Fatal("missing independent running state", target)
				}
			}
			if s.registerStore.Get()["target"] != "openai" {
				t.Fatal("scoped action changed legacy target")
			}
			for _, query := range []string{"target=", "target=invalid", "target=openai&target=grok"} {
				post("/api/register/stop?"+query, ``, 400)
				if s.freeRegister.Snapshot()["running"] != true || s.grokRegister.Snapshot()["running"] != true {
					t.Fatal("invalid target stopped task")
				}
			}
			post("/api/register/stop?target="+first, ``, 200)
			isolationWait(t, s, first, false)
			if isolationState(t, s, second)["enabled"] != true {
				t.Fatal("stopping other channel cancelled this one")
			}
			before := isolationState(t, s, second)
			post("/api/register/reset?target="+first, ``, 200)
			after := isolationState(t, s, second)
			if after["enabled"] != true || after["jobs_total"] != before["jobs_total"] {
				t.Fatal("reset crossed channels")
			}
			post("/api/register/stop?target="+second, ``, 200)
			isolationWait(t, s, second, false)
		})
	}
}

func TestRegistrationScopedSaveCannotOverwriteOtherChannelOrMailboxSecrets(t *testing.T) {
	s := scheduleServer(t)
	res := adminRequest(s.Handler(), http.MethodPost, "/api/register?target=grok", strings.NewReader(`{"target":"openai","total":7,"target_settings":{"openai":{"total":99},"grok":{"proxy":"direct"}},"mail":{"source_id":"grok-source","providers":[{"admin_password":"REPLACED"}]},"openai_free":{"timeout_seconds":1},"grok":{"request_timeout":55}}`))
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	o, g := s.registerStore.ConfigForTarget("openai"), s.registerStore.ConfigForTarget("grok")
	if intValue(o["total"]) != 3 || intValue(g["total"]) != 7 || g["proxy"] != "direct" || s.registerStore.Get()["target"] != "openai" {
		t.Fatal("scoped profile overwritten")
	}
	if mapValue(o["mail"])["source_id"] == "grok-source" || mapValue(g["mail"])["source_id"] != "grok-source" {
		t.Fatal("mailbox choice not isolated")
	}
	if intValue(mapValue(o["openai_free"])["timeout_seconds"]) == 1 {
		t.Fatal("Grok overwrote OpenAI config")
	}
	providers := mapValue(g["mail"])["providers"].([]any)
	if mapValue(providers[0])["admin_password"] != "PRIVATE-PASSWORD" {
		t.Fatal("scoped form changed shared credentials")
	}
	if strings.Contains(res.Body.String(), "PRIVATE-PASSWORD") {
		t.Fatal("password leaked")
	}
}

func TestRegistrationIndependentTimersKeepTheirOwners(t *testing.T) {
	s := scheduleServer(t)
	var openai, grok atomic.Int32
	s.freeRegister = registerruntime.NewFreeEngine(filepath.Join(t.TempDir(), "openai.json"), func(c registerruntime.FreeConfig) (registerruntime.MailSource, registerruntime.RegistrationFlow, error) {
		openai.Add(1)
		return nil, nil, &registerruntime.Failure{Stage: "test", Code: "fixture"}
	}, nil, nil)
	s.grokRegister = registerruntime.NewGrokEngine(filepath.Join(t.TempDir(), "grok.json"), func(c registerruntime.GrokConfig) (registerruntime.MailSource, registerruntime.GrokRegistrationFlow, error) {
		grok.Add(1)
		return nil, nil, &registerruntime.Failure{Stage: "test", Code: "fixture"}
	}, nil, nil)
	var due time.Time
	for _, target := range []string{"openai", "grok"} {
		for _, req := range []struct{ path, body string }{{"/api/register?target=" + target, `{"total":1,"threads":1}`}, {"/api/register/schedule?target=" + target, `{"enabled":true,"interval_minutes":1}`}} {
			res := adminRequest(s.Handler(), http.MethodPost, req.path, strings.NewReader(req.body))
			if res.Code != 200 {
				t.Fatal(res.Body.String())
			}
		}
		next, err := time.Parse(time.RFC3339Nano, s.registrationScheduleSnapshot(target).NextRunAt)
		if err != nil {
			t.Fatal(err)
		}
		if next.After(due) {
			due = next
		}
	}
	// Changing the old default must not redirect either timer.
	if _, err := s.registerStore.Update(map[string]any{"target": "grok"}); err != nil {
		t.Fatal(err)
	}
	s.runRegistrationSchedule(due)
	for _, target := range []string{"openai", "grok"} {
		isolationWait(t, s, target, false)
		if s.registrationScheduleSnapshot(target).LastResult != "started" {
			t.Fatal("timer failed", target)
		}
	}
	if openai.Load() != 1 || grok.Load() != 1 {
		t.Fatalf("timer dispatch mixed: openai=%d grok=%d", openai.Load(), grok.Load())
	}
	before := s.registrationScheduleSnapshot("grok")
	res := adminRequest(s.Handler(), http.MethodPost, "/api/register/schedule?target=openai", strings.NewReader(`{"enabled":false,"interval_minutes":1}`))
	if res.Code != 200 || s.registrationScheduleSnapshot("grok") != before {
		t.Fatal("disabling OpenAI changed Grok timer")
	}
}

func TestRegistrationLegacyGrokTimerMigrationSurvivesDefaultChange(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	store := registerruntime.New(cfg.RegisterPath, cfg.GrokAccountsPath)
	if _, err := store.Update(map[string]any{"target": "grok"}); err != nil {
		t.Fatal(err)
	}
	old := registerruntime.NewSchedule(filepath.Join(filepath.Dir(cfg.RegisterPath), "openai_registration_schedule.json"))
	if err := old.Configure(true, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	before := old.Snapshot()
	s := New(cfg)
	migrated := s.registrationScheduleSnapshot("grok")
	if migrated.Target != "grok" || migrated.NextRunAt != before.NextRunAt || !migrated.Enabled || s.registrationScheduleSnapshot("openai").Enabled {
		t.Fatal("legacy timer changed channel/time")
	}
	if err := s.ShutdownRegistration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(map[string]any{"target": "openai"}); err != nil {
		t.Fatal(err)
	}
	s = New(cfg)
	t.Cleanup(func() { _ = s.ShutdownRegistration(context.Background()) })
	if s.registrationScheduleSnapshot("grok") != migrated || s.registrationScheduleSnapshot("openai").Target != "openai" {
		t.Fatal("restart reassigned timer ownership")
	}
}

func TestRegistrationCompensationIgnoresViewedDefaultTarget(t *testing.T) {
	s := scheduleServer(t)
	if _, err := s.registerStore.Update(map[string]any{"target": "grok"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	profile := registerruntime.NewBrowserProfile()
	job := registerruntime.RegistrationJob{ID: "pending-openai", Status: "registration_pending", Stage: "wait_code", Error: "task_timeout", Mailbox: registerruntime.Mailbox{Email: "fixture@example.test", AccountID: "test"}, MailboxAPIBase: "https://mail.example.test", BrowserProfile: &profile, AutoRetry: &registerruntime.RegistrationCompensation{State: "waiting", NextAttemptAt: now.UTC().Format(time.RFC3339Nano)}}
	raw, err := json.Marshal([]registerruntime.RegistrationJob{job})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pending.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.freeRegister = registerruntime.NewFreeEngine(path, func(registerruntime.FreeConfig) (registerruntime.MailSource, registerruntime.RegistrationFlow, error) {
		calls.Add(1)
		return nil, nil, &registerruntime.Failure{Stage: "mail", Code: "network_error"}
	}, nil, nil)
	s.runRegistrationSchedule(now)
	isolationWait(t, s, "openai", false)
	if calls.Load() != 1 {
		t.Fatal("Grok selection prevented OpenAI compensation")
	}
}
