package register

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdobeRecoveryPageDiagnosticUsesFixedBoundedLabels(t *testing.T) {
	d := adobeRegistrationDiagnostics{FailureAction: "inspect_form", RecoveryPage: json.RawMessage(`{"page_kind":"account_portal","detected_stage":"needs_manual","field_count":0,"email_fields":0,"password_fields":0,"verification_fields":0,"button_count":2,"link_count":3,"login_entries":1,"login_clicked":true,"elapsed_ms":60000,"url":"PRIVATE_SECRET","fields":["PRIVATE_SECRET"]}`)}
	got := adobeFailureDiagnosticText(d)
	for _, want := range []string{"恢复页面：Adobe 账户门户", "已识别表单：未识别为可继续表单", "页面控件：字段 0、邮箱 0、密码 0、验证码 0、按钮 2、链接 3、登录入口 1", "登录入口：已点击一次", "登录观察 60.0 秒", "出错步骤：检查表单"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing safe recovery observation %q: %q", want, got)
		}
	}
	if strings.Contains(got, "PRIVATE_SECRET") {
		t.Fatalf("raw page data entered recovery log: %q", got)
	}
	d.FailureAction = "submit_password"
	if got := adobeFailureDiagnosticText(d); got != "出错步骤：提交密码" {
		t.Fatalf("login-opening evidence was applied to another action: %q", got)
	}
}

func TestAdobeRecoveryPageDiagnosticDoesNotAffectCreationResult(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown_kind":   `{"page_kind":"PRIVATE_SECRET"}`,
		"string_count":   `{"page_kind":"authentication","field_count":"PRIVATE_SECRET"}`,
		"fraction_count": `{"page_kind":"authentication","field_count":0.5}`,
		"string_flag":    `{"page_kind":"authentication","login_clicked":"PRIVATE_SECRET"}`,
		"string_elapsed": `{"page_kind":"authentication","elapsed_ms":"PRIVATE_SECRET"}`,
		"array":          `["PRIVATE_SECRET"]`, "scalar": `"PRIVATE_SECRET"`,
		"oversized": `{"page_kind":"account_portal","raw":"` + strings.Repeat("x", 4096) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var result adobeBrowserResult
			if err := json.Unmarshal([]byte(`{"creation_outcome":"accepted","submission_started":true,"stage":"needs_manual","code":"recovery_login_form_unavailable","diagnostics":{"failure_action":"inspect_form","recovery_page":`+raw+`}}`), &result); err != nil {
				t.Fatalf("optional diagnostic discarded a valid creation result: %v", err)
			}
			if result.CreationOutcome != "accepted" || !result.SubmissionStarted || result.Code != "recovery_login_form_unavailable" {
				t.Fatalf("optional diagnostic changed business outcome: %+v", result)
			}
			if got := adobeFailureDiagnosticText(result.Diagnostics); got != "出错步骤：检查表单" {
				t.Fatalf("malformed observation reached log: %q", got)
			}
		})
	}
	var d adobeRegistrationDiagnostics
	if err := json.Unmarshal([]byte(`{"failure_action":"inspect_form","recovery_page":{"page_kind":"authentication","detected_stage":"PRIVATE_SECRET","field_count":-1,"email_fields":101,"password_fields":-1,"verification_fields":101,"button_count":101,"link_count":-1,"login_entries":101,"elapsed_ms":300001,"login_clicked":false}}`), &d); err != nil {
		t.Fatal(err)
	}
	if got := adobeFailureDiagnosticText(d); got != "恢复页面：Adobe 认证页；登录入口：尚未点击；出错步骤：检查表单" {
		t.Fatalf("out-of-range values entered diagnostic log: %q", got)
	}
}

func TestAdobeRecoveryPageDiagnosticPersistsWithoutChangingAcceptedAccount(t *testing.T) {
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
		if r.URL.Path == "/v1/adobe/run" && request["action"] == "prepare" {
			_, _ = w.Write([]byte(`{"stage":"prepared","registration_country":{"country":"US","source":"preserved"}}`))
			return
		}
		if r.URL.Path != "/v1/adobe/run" || request["action"] != "resume" {
			t.Error("recovery issued an unexpected command")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resumes.Add(1)
		_, _ = w.Write([]byte(`{"stage":"needs_manual","code":"recovery_login_form_unavailable","creation_outcome":"accepted","submission_started":true,"diagnostics":{"failure_action":"inspect_form","failure_phase":"before_submission","exception_type":"registration","recovery_page":{"page_kind":"account_portal","detected_stage":"needs_manual","field_count":0,"login_entries":0,"login_clicked":false,"elapsed_ms":60000,"raw":"PRIVATE_SECRET"}}}`))
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
	seedAdobeCompensation(t, engine, job)
	started, err := engine.RunCompensation(config, now)
	if err != nil || !started {
		t.Fatalf("recovery did not start: %v", err)
	}
	waitAdobe(t, engine)
	child := latestAdobe(t, engine, job.ID)
	if resumes.Load() != 1 || child.Error != "adobe: recovery_login_form_unavailable" || adobeCreationOutcome(child) != "accepted" {
		t.Fatalf("diagnostic changed recovery outcome: %+v", child)
	}
	reloaded := NewAdobeEngine(path, "test-key", nil, nil, nil)
	restored, err := reloaded.getJob(child.ID)
	if err != nil || !restored.AdobeRegistration.SubmissionStarted || adobeCreationOutcome(restored) != "accepted" {
		t.Fatalf("accepted account fence did not survive restart: %+v %v", restored, err)
	}
	found := false
	for _, log := range restored.Logs {
		if strings.Contains(log.Detail, "PRIVATE_SECRET") || log.Action == "adobe_creation_submitted" {
			t.Fatalf("raw data or speculative creation action reached log: %+v", log)
		}
		if log.Action == "adobe_registration_diagnostics" {
			found = true
			if !strings.Contains(log.Detail, "恢复页面：Adobe 账户门户") || !strings.Contains(log.Detail, "登录观察 60.0 秒") || !strings.Contains(log.Detail, "登录入口：尚未点击") {
				t.Fatalf("safe recovery page observation was not persisted: %+v", log)
			}
		}
	}
	if !found {
		t.Fatal("recovery page diagnostics did not reach the persisted job log")
	}
}
