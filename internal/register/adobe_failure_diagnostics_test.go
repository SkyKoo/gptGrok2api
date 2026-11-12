package register

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdobeFailureDiagnosticProjectionRejectsUntrustedValues(t *testing.T) {
	diagnostics := adobeRegistrationDiagnostics{
		FormError: "PRIVATE_SECRET", InvalidFields: []string{"email", "email", "PRIVATE_SECRET", "password", "verification_code", "profile"},
		FailureAction: "PRIVATE_SECRET", FailurePhase: "PRIVATE_SECRET", ExceptionType: "PRIVATE_SECRET",
	}
	if got := adobeFailureDiagnosticText(diagnostics); got != "校验未通过字段：邮箱、密码、验证码" {
		t.Fatalf("unsafe or unbounded projection: %q", got)
	}
	diagnostics.InvalidFields = []string{"PRIVATE_SECRET"}
	if got := adobeFailureDiagnosticText(diagnostics); got != "" {
		t.Fatalf("unknown values entered diagnostic log: %q", got)
	}
	diagnostics = adobeRegistrationDiagnostics{FormError: "invalid_email", InvalidFields: []string{"email"}, FailureAction: "wait_login_result", FailurePhase: "submission_started", ExceptionType: "registration"}
	got := adobeFailureDiagnosticText(diagnostics)
	for _, want := range []string{"表单反馈：邮箱被拒绝", "出错步骤：等待登录结果", "步骤进度：动作已开始提交", "异常类别：流程校验"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing fixed diagnostic %q: %q", want, got)
		}
	}
	if strings.Contains(got, "旧表单") || strings.Contains(got, "创建账户") {
		t.Fatalf("diagnostic inferred an unsupported cause: %q", got)
	}
}

func TestAdobeRecoveryFailureLogsKeepOriginalSubmissionFence(t *testing.T) {
	var resumes atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			_, _ = w.Write([]byte(`{"status":"released"}`))
			return
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		if r.URL.Path == "/v1/browser/reservations" {
			if request["adobe_mode"] != "resume" || request["profile_id"] != "original-adobe-profile" {
				t.Error("recovery changed the original flow identity")
			}
			_, _ = w.Write([]byte(`{"status":"ready"}`))
			return
		}
		if r.URL.Path != "/v1/adobe/run" || request["action"] != "resume" {
			t.Error("recovery issued an unexpected command")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resumes.Add(1)
		_, _ = w.Write([]byte(`{"stage":"needs_manual","code":"form_rejected","submission_started":true,"diagnostics":{"submission_started":true,"form_error":"invalid_email","invalid_fields":["email","PRIVATE_SECRET"],"failure_action":"wait_login_result","failure_phase":"submission_started","exception_type":"registration","raw_error":"PRIVATE_SECRET"}}`))
	}))
	defer worker.Close()
	config := adobeTestConfig()
	config["adobe"] = map[string]any{"api_base": worker.URL}
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	engine := NewAdobeEngine(path, "test-key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		return &adobeCompensationMail{}, &AdobeBrowserFlow{Client: worker.Client()}, nil
	}, nil, nil)
	now := time.Now().UTC()
	job := adobeCompensationFixture(t, now)
	// Go knew the flow had started but had not received the Worker's Create
	// marker. Observing the original marker during recovery must persist it.
	job.Stage = "adobe_recovery_started"
	job.AdobeRegistration.SubmissionStarted = false
	seedAdobeCompensation(t, engine, job)
	started, err := engine.RunCompensation(config, now)
	if err != nil || !started {
		t.Fatalf("recovery did not start: %v", err)
	}
	waitAdobe(t, engine)
	child := latestAdobe(t, engine, job.ID)
	if resumes.Load() != 1 || child.Error != "adobe: form_rejected" || child.AutoRetry.State != "manual" || child.AutoRetry.Reason != "failure_requires_review" {
		t.Fatalf("recovery failure policy changed: %+v", child)
	}
	original, diagnostic := 0, 0
	for _, log := range child.Logs {
		if log.Action == "adobe_creation_submitted" || strings.Contains(log.Detail, "PRIVATE_SECRET") {
			t.Fatalf("misleading or private log: %+v", log)
		}
		if log.Action == "adobe_original_submission_recorded" && log.Level == "info" {
			original++
		}
		if log.Action == "adobe_registration_diagnostics" {
			diagnostic++
			if !strings.Contains(log.Detail, "邮箱被拒绝") || !strings.Contains(log.Detail, "等待登录结果") {
				t.Fatalf("missing safe diagnostic evidence: %+v", log)
			}
		}
	}
	if original != 1 || diagnostic != 1 {
		t.Fatalf("unexpected original=%d diagnostic=%d", original, diagnostic)
	}
	reloaded := NewAdobeEngine(path, "test-key", nil, nil, nil)
	restored, err := reloaded.getJob(child.ID)
	if err != nil || !restored.AdobeRegistration.SubmissionStarted || !restored.AdobeRegistration.FlowStarted || adobeRegistrationRecoveryMode(restored) != "resume_session" {
		t.Fatalf("original submission fence did not survive restart: %+v %v", restored.AdobeRegistration, err)
	}
}

func TestAdobeSnapshotOrdersEventsStablyByActualTime(t *testing.T) {
	engine := &AdobeEngine{jobs: []RegistrationJob{{ID: "old"}, {ID: "new"}}}
	engine.jobs[0].Logs = []JobLog{
		{Time: "bad-old-time", Action: "invalid-old"},
		{Time: "2026-11-12T06:55:00.1Z", Action: "old-task-new-event"},
		{Time: "2026-11-12T06:56:00Z", Action: "equal-first"},
	}
	engine.jobs[1].Logs = []JobLog{
		{Time: "", Action: "invalid-new"},
		{Time: "2026-11-12T06:55:00Z", Action: "new-task-earlier-event"},
		{Time: "2026-11-12T14:56:00+08:00", Action: "equal-second"},
	}
	logs := engine.SnapshotPage(1)["logs"].([]JobLog)
	want := []string{"invalid-old", "invalid-new", "new-task-earlier-event", "old-task-new-event", "equal-first", "equal-second"}
	for i, action := range want {
		if logs[i].Action != action {
			t.Fatalf("events not stably chronological: %+v", logs)
		}
	}
	if engine.jobs[0].Logs[0].Action != "invalid-old" || engine.jobs[1].Logs[0].Action != "invalid-new" {
		t.Fatal("snapshot mutated the durable task logs")
	}
}

func TestAdobeSnapshotKeepsNewestOldTaskEventBeforeTailLimit(t *testing.T) {
	engine := &AdobeEngine{jobs: make([]RegistrationJob, 6)}
	base := time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)
	for jobIndex := range engine.jobs {
		for offset := 0; offset < 40; offset++ {
			i := jobIndex*40 + offset
			engine.jobs[jobIndex].Logs = append(engine.jobs[jobIndex].Logs, JobLog{Time: base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano), Action: fmt.Sprint(i)})
		}
	}
	engine.jobs[0].Logs = append(engine.jobs[0].Logs, JobLog{Time: base.Add(time.Hour).Format(time.RFC3339Nano), Action: "latest-recovery"})
	logs := engine.SnapshotPage(1)["logs"].([]JobLog)
	if len(logs) != 200 || logs[0].Action != "41" || logs[199].Action != "latest-recovery" {
		t.Fatalf("tail limit lost chronological events: count=%d first=%s last=%s", len(logs), logs[0].Action, logs[len(logs)-1].Action)
	}
}

func TestAdobeHumanDiagnosticMetadataUsesFixedLabels(t *testing.T) {
	var diagnostic adobeRegistrationDiagnostics
	if err := json.Unmarshal([]byte(`{"verification_instruction_kind":"identity_instruction","verification_frame_kind":"adobe_page","puzzle_entry_visible":false}`), &diagnostic); err != nil {
		t.Fatal(err)
	}
	text := adobeFailureDiagnosticText(diagnostic)
	for _, expected := range []string{"身份验证提示，尚未确认具体拼图", "Adobe 页面", "已检查，当前未见"} {
		if !strings.Contains(text, expected) {
			t.Fatal("missing fixed verification diagnostic", expected)
		}
	}
	if err := json.Unmarshal([]byte(`{"verification_instruction_kind":"PRIVATE_TEXT","verification_frame_kind":"https://PRIVATE_URL"}`), &diagnostic); err != nil {
		t.Fatal(err)
	}
	diagnostic.PuzzleEntryVisible = nil
	if text := adobeFailureDiagnosticText(diagnostic); text != "" {
		t.Fatal("untrusted verification metadata escaped whitelist")
	}
	visible := true
	diagnostic.PuzzleEntryVisible = &visible
	if text := adobeFailureDiagnosticText(diagnostic); text != "开始拼图按钮：可见" {
		t.Fatal("checked visible puzzle entry lost")
	}
}
