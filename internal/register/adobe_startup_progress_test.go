package register

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
)

type adobeStartupTestFlow struct{}

func (adobeStartupTestFlow) Close() {}
func (adobeStartupTestFlow) Register(_ context.Context, _ string, _ Mailbox, cfg AdobeConfig, _ func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	for _, stage := range []string{"waiting_browser", "starting_browser"} {
		for i := 0; i < 80; i++ {
			if err := progress(stage); err != nil {
				return nil, err
			}
		}
	}
	for _, event := range []browserqueue.StartupEvent{
		{Sequence: 1, Stage: "starting_browser", Status: "started"},
		{Sequence: 2, Stage: "starting_browser", Status: "completed", ElapsedMS: 7000, DurationMS: 7000},
		{Sequence: 3, Stage: "adobe_waiting_registration_form", Status: "started", ElapsedMS: 7000},
		{Sequence: 4, Stage: "adobe_waiting_registration_form", Status: "failed", ElapsedMS: 27000, DurationMS: 20000, ExceptionType: "timeout"},
	} {
		if err := cfg.startupProgress(event); err != nil {
			return nil, err
		}
		for i := 0; i < 80; i++ {
			if err := progress(event.Stage); err != nil {
				return nil, err
			}
		}
	}
	return nil, fail("adobe", "login_ui_not_ready")
}

func TestAdobeStartupHeartbeatsDoNotFloodDurableLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adobe.json")
	engine := NewAdobeEngine(path, "test-key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		return adobeTestMail{}, adobeStartupTestFlow{}, nil
	}, nil, nil)
	id, err := engine.Start(adobeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	waitAdobe(t, engine)
	engine = NewAdobeEngine(path, "test-key", nil, nil, nil)
	job, err := engine.getJob(id)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, log := range job.Logs {
		counts[log.Action]++
		if log.Action == "adobe_waiting_registration_form_failed" && (!strings.Contains(log.Text, "本步 20.0 秒") || !strings.Contains(log.Text, "累计 27.0 秒") || !strings.Contains(log.Text, "等待超时")) {
			t.Fatalf("missing failure timing: %+v", log)
		}
	}
	for _, action := range []string{"waiting_browser", "starting_browser", "starting_browser_completed", "adobe_waiting_registration_form_started", "adobe_waiting_registration_form_failed"} {
		if counts[action] != 1 {
			t.Fatalf("%s: %d logs; %+v", action, counts[action], counts)
		}
	}
	if len(job.Logs) > 15 {
		t.Fatalf("heartbeat flooded log: %d", len(job.Logs))
	}
	if job.AdobeRegistration.FlowStarted || job.AdobeRegistration.SubmissionStarted {
		t.Fatal("startup observation became submission evidence")
	}
}

func TestAdobeStartupFailureTimingProjection(t *testing.T) {
	elapsed, duration := int64(58000), int64(20000)
	d := adobeRegistrationDiagnostics{FailureAction: "adobe_waiting_create_entry", FailurePhase: "before_submission", ExceptionType: "timeout", ElapsedMS: &elapsed, DurationMS: &duration}
	text := adobeFailureDiagnosticText(d)
	for _, part := range []string{"等待创建账户入口", "本步 20.0 秒", "累计 58.0 秒", "等待超时"} {
		if !strings.Contains(text, part) {
			t.Fatalf("missing %s: %s", part, text)
		}
	}
	elapsed = -1
	if strings.Contains(adobeFailureDiagnosticText(d), "秒") {
		t.Fatal("negative timings escaped")
	}
}
