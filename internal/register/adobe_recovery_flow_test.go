package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAdobeRecoveryUsesOriginalProfileAndNormalOTPWithoutSignup(t *testing.T) {
	for _, scenario := range []string{"otp", "challenge", "invalid_timestamp", "saved_session"} {
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
					if scenario == "challenge" {
						_, _ = w.Write([]byte(`{"stage":"needs_manual","code":"human_verification_required","submission_started":true}`))
						return
					}
					timestamp := requestedAt.Format(time.RFC3339Nano)
					if scenario == "invalid_timestamp" {
						timestamp = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"stage": "awaiting_verification", "code": "verification_required", "submission_started": true, "verification_requested_at": timestamp})
					return
				}
				if scenario == "saved_session" && action != "resume" || scenario == "otp" && (action != "verify" || body["code"] != "246810") {
					t.Error("recovery replayed signup or changed verification")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"stage": "completed", "account": map[string]any{"email": "fixture@example.test", "account_id": "fixture", "token": "fixture-token"}})
			}))
			defer worker.Close()
			flow := AdobeBrowserFlow{Client: worker.Client()}
			account, err := flow.Register(context.Background(), "recovery-child", Mailbox{Email: "fixture@example.test"}, AdobeConfig{WorkerURL: worker.URL, BrowserProfileID: "original-profile", RecoveryMode: "resume_session", Proxy: "direct"}, func(_ context.Context, after time.Time) (string, error) {
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
