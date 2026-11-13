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

func TestAdobeMediaRegistrationDiagnosisExplainsObservedReasons(t *testing.T) {
	for reason, want := range map[string]string{
		"performance_unavailable": "无法读取初始化阶段记录", "instance_missing": "尚未观察到初始化实例记录",
		"instance_ambiguous": "初始化实例记录不唯一", "vendor_unrecognized": "出现未识别的初始化组件",
		"vendors_pending": "初始化组件尚未全部完成", "token_pending": "初始化组件已完成，尚未收到媒体凭据", "token_settling": "媒体凭据尚未稳定",
		"editor_not_ready": "媒体编辑页尚未就绪", "observer_unavailable": "媒体观察接口不可用",
		"context_changed": "媒体页面上下文已变化", "identity_pending": "最终身份同步尚未完成",
		"identity_changed": "最终身份与采集记录不一致", "runtime_changed": "媒体运行状态在收尾校验时变化",
		"verification_timeout": "最终媒体会话校验超时", "authentication_missing": "媒体认证组件尚未出现",
		"arp_observer_missing": "媒体凭据观察接口尚未出现", "editor_route_changed": "页面已离开媒体编辑路径",
		"page_hidden": "媒体观察期间页面已离开", "observation_cancelled": "媒体观察已取消",
		"evaluation_failed": "无法读取媒体页面运行状态", "navigation_budget_exhausted": "媒体编辑页导航已耗尽等待时间",
	} {
		t.Run(reason, func(t *testing.T) {
			d := adobeRegistrationDiagnostics{MediaRuntime: json.RawMessage(fmt.Sprintf(`{"state":"pending","readiness_reason":%q}`, reason))}
			if got := adobeFailureDiagnosticText(d); got != "媒体会话："+want {
				t.Fatalf("reason lost or changed: %q", got)
			}
		})
	}
	for state, want := range map[string]string{"pending": "仍在等待就绪", "observation_timeout": "媒体运行状态观察超时", "invalid_contract": "媒体观察接口不符合预期"} {
		if got := adobeFailureDiagnosticText(adobeRegistrationDiagnostics{MediaRuntime: json.RawMessage(fmt.Sprintf(`{"state":%q}`, state))}); got != "媒体会话："+want {
			t.Fatalf("state fallback lost: %q", got)
		}
	}
}

func TestAdobeMediaRegistrationDiagnosisRejectsPrivateAndUnboundedValues(t *testing.T) {
	var d adobeRegistrationDiagnostics
	if err := json.Unmarshal([]byte(`{
		"media_runtime":{"state":"pending","readiness_reason":"vendors_pending","value_kind":"null","vendors_started":3,"vendors_complete":2,"token":"PRIVATE","url":"https://PRIVATE"},
		"media_page":{"route":"editor","matching_host_count":2,"authentication_count":1,"arp_observer_count":1,"scan_complete":false,"host":"PRIVATE"},
		"media_timing":{"candidate_changes":0,"source_changes":1,"authentication_changes":1,"emissions":65535,"value_changes":2,"proof_changes":1,"first_source_ms":0,"first_nonempty_ms":1250,"vendors_ready_ms":60000,"last_candidate_ms":2000,"token":"PRIVATE"}
	}`), &d); err != nil {
		t.Fatal(err)
	}
	text := adobeFailureDiagnosticText(d)
	for _, want := range []string{
		"初始化组件尚未全部完成", "当前观测为空", "初始化组件已启动 3 个", "初始化组件已完成 2 个",
		"媒体页面：图片编辑页", "已观察到媒体组件 2 个", "已观察到认证实例 1 个", "已观察到凭据观察器 1 个", "媒体页面扫描未完成",
		"候选会话变化 0 次", "凭据观察回调 65535 次", "首次观察接口在观察开始后 0.0 秒", "首次非空凭据在观察开始后 1.2 秒",
		"组件首次就绪在观察开始后 60.0 秒", "最近候选会话在观察开始后 2.0 秒",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing bounded observation %q: %q", want, text)
		}
	}
	if strings.Contains(text, "PRIVATE") || strings.Contains(text, "https:") {
		t.Fatal("private runtime detail reached registration log")
	}
	for _, raw := range []string{`null`, `[]`, `"PRIVATE"`, `true`, `12`, `{"state":"PRIVATE","readiness_reason":"https://PRIVATE","value_kind":"PRIVATE","vendors_started":4,"vendors_complete":-1}`, `{"state":"pending","extra":"` + strings.Repeat("x", 8192) + `"}`} {
		// Optional malformed data must not turn accepted creation into unknown.
		var result adobeBrowserResult
		if err := json.Unmarshal([]byte(`{"creation_outcome":"accepted","code":"media_session_incomplete","diagnostics":{"media_runtime":`+raw+`,"media_page":{"route":"editor"}}}`), &result); err != nil {
			t.Fatalf("optional diagnostic rejected creation response: %v", err)
		}
		if result.CreationOutcome != "accepted" || result.Code != "media_session_incomplete" || adobeFailureDiagnosticText(result.Diagnostics) != "" {
			t.Fatal("malformed observation changed outcome or escaped the allowlist")
		}
	}
	for _, invalid := range []string{`-1`, `33`, `1.5`, `"1"`, `true`, `null`} {
		d.MediaRuntime = json.RawMessage(`{"state":"pending"}`)
		d.MediaPage = json.RawMessage(fmt.Sprintf(`{"route":"https://PRIVATE","matching_host_count":%s,"authentication_count":%s,"arp_observer_count":%s,"scan_complete":"PRIVATE"}`, invalid, invalid, invalid))
		d.MediaTiming = json.RawMessage(`{"emissions":65536,"value_changes":-1,"proof_changes":1.5,"first_source_ms":60001,"last_candidate_ms":"PRIVATE"}`)
		if got := adobeFailureDiagnosticText(d); got != "媒体会话：仍在等待就绪" {
			t.Fatalf("invalid observation escaped projection: %q", got)
		}
	}
	d.MediaPage = json.RawMessage(`{"matching_host_count":1,"authentication_count":2,"arp_observer_count":2}`)
	if got := adobeFailureDiagnosticText(d); got != "媒体会话：仍在等待就绪" {
		t.Fatalf("inconsistent page counts escaped projection: %q", got)
	}
}

func TestAdobeMediaFailureLogsPersistAndKeepAcceptedRecoveryPolicy(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery_%t", recovery), func(t *testing.T) {
			var commands, preparations atomic.Int32
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					_, _ = w.Write([]byte(`{"status":"released"}`))
					return
				}
				var request map[string]any
				_ = json.NewDecoder(r.Body).Decode(&request)
				if r.URL.Path == "/v1/browser/reservations" {
					if recovery && (request["adobe_mode"] != "resume" || request["profile_id"] != "original-adobe-profile") {
						t.Error("accepted recovery changed original browser profile")
					}
					_, _ = w.Write([]byte(`{"status":"ready"}`))
					return
				}
				if r.URL.Path == "/v1/adobe/run" && request["action"] == "prepare" {
					preparations.Add(1)
					_, _ = w.Write([]byte(`{"stage":"prepared","registration_country":{"country":"JP","source":"preserved"}}`))
					return
				}
				action := "run"
				if recovery {
					action = "resume"
				}
				if r.URL.Path != "/v1/adobe/run" || request["action"] != action {
					t.Error("unexpected registration command")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				commands.Add(1)
				_, _ = w.Write([]byte(`{"stage":"needs_manual","code":"media_session_incomplete","creation_outcome":"accepted","submission_started":true,"diagnostics":{"failure_action":"export_session","exception_type":"registration","media_runtime":{"state":"pending","readiness_reason":"token_pending","value_kind":"null","vendors_started":3,"vendors_complete":3,"token":"PRIVATE_SECRET"},"media_page":{"route":"editor","authentication_count":1},"media_timing":{"value_changes":2,"last_candidate_ms":29750,"url":"PRIVATE_SECRET"}}}`))
			}))
			defer worker.Close()
			cfg := adobeTestConfig()
			cfg["adobe"] = map[string]any{"api_base": worker.URL}
			path := filepath.Join(t.TempDir(), "adobe.enc.json")
			engine := NewAdobeEngine(path, "test-key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				var mail MailSource = adobeTestMail{}
				if recovery {
					mail = &adobeCompensationMail{}
				}
				return mail, &AdobeBrowserFlow{Client: worker.Client()}, nil
			}, nil, nil)
			now := time.Now().UTC()
			var root string
			if recovery {
				job := adobeCompensationFixture(t, now)
				job.AdobeProfile.Country = "JP"
				root = job.ID
				seedAdobeCompensation(t, engine, job)
				if started, err := engine.RunCompensation(cfg, now); err != nil || !started {
					t.Fatalf("recovery did not start: %v", err)
				}
			} else {
				if _, err := engine.Start(cfg); err != nil {
					t.Fatal(err)
				}
			}
			waitAdobe(t, engine)
			job := cloneJob(engine.jobs[len(engine.jobs)-1])
			if root != "" {
				job = latestAdobe(t, engine, root)
			}
			attempts := 0
			if recovery {
				attempts = 1
			}
			if commands.Load() != 1 || preparations.Load() != 1 || job.Error != "adobe: media_session_incomplete" || adobeCreationOutcome(job) != "accepted" || adobeRegistrationRecoveryMode(job) != "resume_session" || job.AutoRetry.State != "waiting" || job.AutoRetry.Attempts != attempts || len(job.Account) != 0 {
				t.Fatalf("media diagnostic changed submission or recovery policy: %+v", job)
			}
			reloaded := NewAdobeEngine(path, "test-key", nil, nil, nil)
			saved, err := reloaded.getJob(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			diagnostics := 0
			for _, log := range saved.Logs {
				if strings.Contains(log.Detail, "PRIVATE_SECRET") || recovery && log.Action == "adobe_creation_submitted" {
					t.Fatalf("private or misleading log: %+v", log)
				}
				if log.Action == "adobe_registration_diagnostics" {
					diagnostics++
					for _, want := range []string{"初始化组件已完成，尚未收到媒体凭据", "当前观测为空", "初始化组件已完成 3 个", "媒体页面：图片编辑页", "观察值变化 2 次", "29.8 秒", "出错步骤：导出登录会话"} {
						if !strings.Contains(log.Detail, want) {
							t.Fatalf("durable log lost bounded media evidence %q: %q", want, log.Detail)
						}
					}
				}
			}
			if diagnostics != 1 || adobeCreationOutcome(saved) != "accepted" || adobeRegistrationRecoveryMode(saved) != "resume_session" {
				t.Fatal("media diagnostic or accepted creation did not survive restart")
			}
		})
	}
}
