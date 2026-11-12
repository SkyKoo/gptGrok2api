package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdobeRecoveryUsesOriginalProfileAndNormalOTPWithoutSignup(t *testing.T) {
	for _, scenario := range []string{"otp", "challenge", "form_rejected", "diagnostic_write_failure", "invalid_timestamp", "saved_session"} {
		t.Run(scenario, func(t *testing.T) {
			requestedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
			var actions, stages []string
			mailCalls, closes := 0, 0
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					closes++
					_, _ = w.Write([]byte(`{"status":"released"}`))
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if r.URL.Path == "/v1/browser/reservations" {
					if body["profile_id"] != "original-profile" || body["adobe_recovery_id"] != "recovery-child" || body["adobe_mode"] != "resume" || body["email"] != "fixture@example.test" || body["proxy"] != "direct" {
						t.Error("recovery changed original browser identity or action")
					}
					_, _ = w.Write([]byte(`{"status":"ready"}`))
					return
				}
				if r.URL.Path != "/v1/adobe/run" {
					t.Error("unexpected endpoint")
					w.WriteHeader(404)
					return
				}
				action := stringValue(body["action"])
				actions = append(actions, action)
				if action == "resume" && scenario != "saved_session" {
					if scenario == "form_rejected" || scenario == "diagnostic_write_failure" {
						_, _ = w.Write([]byte(`{"stage":"needs_manual","code":"form_rejected","submission_started":true}`))
						return
					}
					if scenario == "challenge" {
						_, _ = w.Write([]byte(`{"stage":"needs_manual","code":"human_verification_required","submission_started":true}`))
						return
					}
					timestamp := requestedAt.Format(time.RFC3339Nano)
					if scenario == "invalid_timestamp" {
						timestamp = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"stage": "awaiting_verification", "code": "verification_required", "creation_outcome": "accepted", "submission_started": true, "verification_requested_at": timestamp})
					return
				}
				if scenario == "saved_session" && action != "resume" || scenario == "otp" && (action != "verify" || body["code"] != "246810") {
					t.Error("recovery replayed signup or changed verification")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"stage": "completed", "account": map[string]any{"email": "fixture@example.test", "account_id": "fixture", "token": "fixture-token"}})
			}))
			defer worker.Close()
			flow := AdobeBrowserFlow{Client: worker.Client()}
			cfg := AdobeConfig{WorkerURL: worker.URL, BrowserProfileID: "original-profile", RecoveryMode: "resume_session", Proxy: "direct"}
			if scenario == "diagnostic_write_failure" {
				cfg.diagnosticProgress = func(adobeRegistrationDiagnostics) error { return fail("storage", "journal_write_failed") }
			}
			account, err := flow.Register(context.Background(), "recovery-child", Mailbox{Email: "fixture@example.test"}, cfg, func(_ context.Context, after time.Time) (string, error) {
				mailCalls++
				if !after.Equal(requestedAt.Add(-5 * time.Second)) {
					t.Error("HME window did not follow the recovery email request")
				}
				return "246810", nil
			}, func(stage string) error { stages = append(stages, stage); return nil })
			flow.Close()
			wantActions := []string{"resume"}
			if scenario == "otp" {
				wantActions = append(wantActions, "verify")
			}
			if !reflect.DeepEqual(actions, wantActions) || closes != 1 {
				t.Fatalf("unexpected commands=%v closes=%d", actions, closes)
			}
			if scenario == "otp" || scenario == "saved_session" {
				if err != nil || account["email"] != "fixture@example.test" {
					t.Fatalf("recovery did not complete: %v", err)
				}
			} else if err == nil {
				t.Fatal("challenge or invalid verification timestamp was accepted")
			}
			if scenario == "otp" && mailCalls != 1 || scenario != "otp" && mailCalls != 0 {
				t.Fatal("unexpected email polling")
			}
			if !strings.Contains(strings.Join(stages, ","), "adobe_recovery_started") {
				t.Fatal("recovery was presented as a new signup")
			}
			for _, stage := range stages {
				if stage == "adobe_creation_submitted" {
					t.Fatal("cumulative submission marker was presented as a new Create")
				}
			}
			if scenario != "saved_session" && scenario != "diagnostic_write_failure" && !strings.Contains(strings.Join(stages, ","), "adobe_original_submission_recorded") {
				t.Fatal("recovery lost the original submission record")
			}
			if scenario == "form_rejected" && safeFailure(err) != "adobe: form_rejected" {
				t.Fatal("form rejection was relabeled as a different failure")
			}
			if scenario == "diagnostic_write_failure" && safeFailure(err) != "storage: journal_write_failed" {
				t.Fatal("diagnostic journal failure was swallowed")
			}
		})
	}
}

func TestAdobeRecoveryCannotFallBackToNewProfile(t *testing.T) {
	for _, cfg := range []AdobeConfig{{RecoveryMode: "resume_session"}, {RecoveryMode: "unknown"}} {
		flow := AdobeBrowserFlow{}
		if _, err := flow.Register(context.Background(), "child", Mailbox{}, cfg, nil, nil); err == nil {
			t.Fatal("invalid recovery fell through to registration")
		}
	}
}

func TestAdobeAcquireRequiresNewStorageOnlyForNewRegistration(t *testing.T) {
	for _, scenario := range []struct {
		name, recoveryMode, profileID, expectedProfile, expectedMode, expectedAction string
		requireNew                                                                   bool
	}{
		{"new_root", "", "", "task", "registration", "run", true},
		{"new_root_saved_profile_id", "", "new-profile", "new-profile", "registration", "run", true},
		{"pre_submit_compensation", "retry_registration", "new-child-profile", "new-child-profile", "registration", "run", true},
		{"resume_original_session", "resume_session", "original-profile", "original-profile", "resume", "resume", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			acquired, commands := make(chan map[string]any, 2), make(chan map[string]any, 2)
			var releases atomic.Int32
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					releases.Add(1)
					json.NewEncoder(w).Encode(map[string]any{"status": "closed"})
					return
				}
				var body map[string]any
				if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch r.URL.Path {
				case "/v1/browser/reservations":
					acquired <- body
					json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
				case "/v1/adobe/run":
					commands <- body
					json.NewEncoder(w).Encode(map[string]any{"stage": "completed", "account": map[string]any{"email": "fixture@example.test", "account_id": "fixture", "token": "fixture-token"}})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer worker.Close()
			flow := AdobeBrowserFlow{Client: worker.Client()}
			cfg := AdobeConfig{WorkerURL: worker.URL, RecoveryMode: scenario.recoveryMode, BrowserProfileID: scenario.profileID, Proxy: "direct"}
			if _, err := flow.Register(context.Background(), "task", Mailbox{Email: "fixture@example.test"}, cfg, nil, func(string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			flow.Close()
			if len(acquired) != 1 || len(commands) != 1 || releases.Load() != 1 {
				t.Fatal("flow did not acquire, run and release exactly once")
			}
			payload, command := <-acquired, <-commands
			actual, present := payload["adobe_require_new"].(bool)
			if !present || actual != scenario.requireNew {
				t.Fatalf("wrong new-storage requirement: %v", payload["adobe_require_new"])
			}
			if payload["profile_id"] != scenario.expectedProfile || payload["adobe_mode"] != scenario.expectedMode || payload["adobe_recovery_id"] != "task" {
				t.Fatal("isolation option changed the existing profile or mode")
			}
			if command["action"] != scenario.expectedAction || command["browser_lease"] != payload["id"] {
				t.Fatal("isolation option changed the registration action or lease")
			}
		})
	}
}

func TestAdobeProfileIsolationFailureStopsBeforeBrowserRun(t *testing.T) {
	for _, code := range []string{"run_already_exists", "browser_profile_missing", "browser_profile_unavailable"} {
		t.Run(code, func(t *testing.T) {
			var businessRequests, releases atomic.Int32
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					releases.Add(1)
				} else if r.URL.Path != "/v1/browser/reservations" {
					businessRequests.Add(1)
				}
				json.NewEncoder(w).Encode(map[string]string{"status": "failed", "code": "browser_start_failed", "diagnostic_code": code})
			}))
			defer worker.Close()
			flow := AdobeBrowserFlow{Client: worker.Client()}
			_, err := flow.Register(context.Background(), "fixture", Mailbox{Email: "fixture@example.test"}, AdobeConfig{WorkerURL: worker.URL}, nil, nil)
			if safeFailure(err) != "adobe: "+code || businessRequests.Load() != 0 || releases.Load() != 1 {
				t.Fatalf("profile failure lost or registration continued: err=%v commands=%d releases=%d", err, businessRequests.Load(), releases.Load())
			}
		})
	}
}
