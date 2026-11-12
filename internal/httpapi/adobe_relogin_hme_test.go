package httpapi

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

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
)

// Only HME and Worker HTTP endpoints are fixtures: the source manager, sender
// filtering, same-lease browser continuation and credential store are real.
func TestAdobeBrowserReloginReceivesHMECodeOnceOnOriginalLease(t *testing.T) {
	for _, scenario := range []string{
		"success", "forwarded_sender", "legacy_source_id", "missing_timestamp", "repeat_otp", "challenge", "legacy_otp", "wrong_stage", "http_failure",
		"old_timestamp", "previous_attempt_timestamp", "future_timestamp", "missing_mailbox", "wrong_mailbox", "missing_source", "ambiguous_source",
		"hme_failure", "timeout", "cancel", "continuation_unknown", "identity_mismatch", "media_incomplete",
	} {
		t.Run(scenario, func(t *testing.T) {
			s := NewWithWorkers(adminTestConfig(t.TempDir()), false)
			t.Cleanup(func() { _ = s.ShutdownRegistration(context.Background()) })
			a := adobeFixtureAccount(t, s)
			a.ProfileID = "original-refresh-profile"
			a.Password = "PRIVATE_ACCOUNT_PASSWORD"
			a.RegistrationMailbox = map[string]any{"provider": "icloud_hme", "platform": "adobe", "source_id": "selected-hme", "source_type": "hme", "id": "asset", "source_mailbox_id": "alias", "email": a.Email, "account_id": "selected-account"}
			if scenario == "missing_mailbox" {
				a.RegistrationMailbox = nil
			}
			if scenario == "wrong_mailbox" {
				a.RegistrationMailbox["email"] = "wrong@example.test"
			}
			if scenario == "legacy_source_id" || scenario == "ambiguous_source" {
				delete(a.RegistrationMailbox, "source_id")
			}
			if scenario == "missing_source" {
				a.RegistrationMailbox["source_id"] = "missing"
			}
			rows, err := s.adobeStore.Import([]adobe.Account{a})
			if err != nil {
				t.Fatal(err)
			}
			a = rows[0]
			const code = "741852"
			updatedCookie := a.Cookie + "; fixture_refresh=updated"
			requested := time.Now().UTC()
			var acquires, runs, releases, logins, inboxes, details, unexpectedMail atomic.Int32
			mailEntered := make(chan struct{}, 1)
			sender := "Adobe <account-noreply@adobe.com>"
			if scenario == "forwarded_sender" {
				sender = "Adobe <account-noreply_at_adobe_com_opaque@icloud.com>"
			}
			hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respond := func(data any) { writeJSON(w, 200, map[string]any{"success": true, "data": data}) }
				if r.URL.Path == "/api/auth/login" {
					logins.Add(1)
					if scenario == "hme_failure" {
						writeJSON(w, 503, map[string]any{"success": false, "code": "PRIVATE_UPSTREAM_DETAIL"})
						return
					}
					var body map[string]string
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["password"] != "PRIVATE_HME_PASSWORD" {
						t.Error("wrong source credentials")
					}
					http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: "fixture", Path: "/"})
					respond(map[string]string{"csrf_token": "fixture"})
					return
				}
				if session, err := r.Cookie("hme_session"); err != nil || session.Value != "fixture" {
					t.Error("mailbox session was not retained")
				}
				date := requested.Add(-time.Second)
				if scenario == "missing_timestamp" {
					date = time.Now().Add(time.Second)
				}
				switch r.URL.Path {
				case "/api/inbox":
					inboxes.Add(1)
					if r.URL.Query().Get("alias") != a.Email || r.URL.Query().Get("account_id") != "selected-account" {
						t.Error("relogin read a different mailbox")
					}
					select {
					case mailEntered <- struct{}{}:
					default:
					}
					if scenario == "timeout" || scenario == "cancel" {
						respond(map[string]any{"method": "web_api", "messages": []any{}})
						return
					}
					respond(map[string]any{"method": "web_api", "messages": []map[string]string{
						{"id": "wrong-provider", "folder": "inbox", "from": "noreply@openai.com", "to": a.Email, "subject": "111111", "date": time.Now().Format(time.RFC3339Nano)},
						{"id": "wrong-recipient", "folder": "inbox", "from": sender, "to": "other@example.test", "subject": "222222", "date": time.Now().Format(time.RFC3339Nano)},
						{"id": "stale", "folder": "inbox", "from": sender, "to": a.Email, "subject": "333333", "date": requested.Add(-6 * time.Second).Format(time.RFC3339Nano)},
						{"id": "spoofed", "folder": "inbox", "from": "noreply@adobe.com.attacker.test", "to": a.Email, "subject": "444444", "date": time.Now().Format(time.RFC3339Nano)},
						{"id": "fresh", "folder": "junk", "from": sender, "to": a.Email, "subject": "Confirm login", "date": date.Format(time.RFC3339Nano)},
					}})
				case "/api/inbox/fresh":
					details.Add(1)
					respond(map[string]string{"from": sender, "to": a.Email, "date": date.Format(time.RFC3339Nano), "body": "Your verification code is " + code})
				default:
					unexpectedMail.Add(1)
					w.WriteHeader(404)
				}
			}))
			defer hme.Close()
			source := mailboxstore.Source{ID: "selected-hme", Type: "icloud_hme", Enabled: true, Config: map[string]any{"api_base": hme.URL, "admin_password": "PRIVATE_HME_PASSWORD", "account_id": "selected-account"}}
			sources := []mailboxstore.Source{{ID: "wrong-default", Type: "icloud_hme", Enabled: true, Config: map[string]any{"api_base": "http://wrong.invalid", "admin_password": "wrong", "account_id": "wrong"}}, source}
			if scenario == "ambiguous_source" {
				other := source
				other.ID = "duplicate-account-source"
				sources = append(sources, other)
			}
			s.mailboxManager = mailboxstore.NewResourceManager(nil, newMailboxProvider)
			s.mailboxManager.SetSourceLoader(func(context.Context) ([]mailboxstore.Source, error) { return sources, nil })
			var originalLease string
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					releases.Add(1)
					writeJSON(w, 200, map[string]string{"status": "closed"})
					return
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if r.URL.Path == "/v1/browser/reservations" {
					acquires.Add(1)
					originalLease = stringValue(body["id"])
					if body["adobe_mode"] != "refresh" || body["profile_id"] != a.ProfileID {
						t.Error("relogin changed browser identity")
					}
					writeJSON(w, 200, map[string]string{"status": "ready"})
					return
				}
				if r.URL.Path != "/v1/adobe/run" || body["action"] != "refresh" || body["browser_lease"] != originalLease {
					t.Error("unexpected continuation or changed browser lease")
				}
				n := runs.Add(1)
				if n == 1 {
					if _, ok := body["code"]; ok {
						t.Error("first refresh contained a verification code")
					}
					stage, failure, status := "awaiting_verification", "refresh_email_otp_required", 200
					at := requested.Format(time.RFC3339Nano)
					switch scenario {
					case "challenge":
						stage, failure = "needs_manual", "refresh_challenge_required"
					case "legacy_otp":
						stage, failure = "needs_manual", "refresh_otp_required"
					case "wrong_stage":
						stage = "needs_manual"
					case "http_failure":
						status = 403
					case "old_timestamp":
						at = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
					case "previous_attempt_timestamp":
						at = time.Now().Add(-5 * time.Minute).Format(time.RFC3339Nano)
					case "future_timestamp":
						at = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
					case "missing_timestamp":
						at = ""
					}
					writeJSON(w, status, map[string]string{"stage": stage, "code": failure, "verification_requested_at": at})
					return
				}
				if n != 2 || body["code"] != code {
					t.Error("wrong code, repeated code or third refresh")
				}
				if scenario == "repeat_otp" {
					writeJSON(w, 200, map[string]string{"stage": "awaiting_verification", "code": "refresh_email_otp_required"})
					return
				}
				if scenario == "continuation_unknown" {
					writeJSON(w, 503, map[string]string{"code": "PRIVATE_UPSTREAM_DETAIL"})
					return
				}
				replacement := a
				replacement.Cookie = updatedCookie
				replacement.MediaSessionRefreshed, replacement.MediaSessionStatus = scenario != "media_incomplete", "complete"
				if scenario == "identity_mismatch" {
					replacement.AccountID = "different"
				}
				writeJSON(w, 200, map[string]any{"stage": "completed", "account": replacement})
			}))
			defer worker.Close()
			if _, err := s.registerStore.UpdateForTarget("adobe", map[string]any{"adobe": map[string]any{"api_base": worker.URL}}); err != nil {
				t.Fatal(err)
			}
			s.adobeStore.SetBrowserRefresher(s.refreshAdobeBrowserAccount)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if scenario == "timeout" {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			if scenario == "cancel" {
				go func() {
					select {
					case <-mailEntered:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			_, err = s.adobeStore.Relogin(ctx, a.ID)
			want := map[string]string{
				"repeat_otp": "email_verification_required", "challenge": "verification_required", "legacy_otp": "email_verification_required", "wrong_stage": "email_verification_required", "http_failure": "email_verification_required",
				"old_timestamp": "email_verification_time_invalid", "previous_attempt_timestamp": "email_verification_time_invalid", "future_timestamp": "email_verification_time_invalid", "missing_mailbox": "email_source_unavailable", "wrong_mailbox": "email_source_unavailable", "missing_source": "email_source_unavailable", "ambiguous_source": "email_source_unavailable",
				"hme_failure": "email_verification_unavailable", "timeout": "email_verification_timeout", "cancel": "cancelled", "continuation_unknown": "browser_failed", "media_incomplete": "media_session_incomplete",
			}[scenario]
			if scenario == "identity_mismatch" {
				if err != adobe.ErrIdentity {
					t.Fatalf("identity failure was lost: %v", err)
				}
			} else if want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if adobe.SafeError(err) != "adobe_relogin_"+want {
				t.Fatalf("got %v want %s", err, want)
			}
			for _, secret := range []string{code, "PRIVATE_", a.Email} {
				if err != nil && strings.Contains(err.Error(), secret) {
					t.Fatal("private relogin data appeared in error")
				}
			}
			if acquires.Load() != 1 || releases.Load() != 1 || unexpectedMail.Load() != 0 {
				t.Fatal("lease leaked or mailbox alias lifecycle changed")
			}
			continued := scenario == "success" || scenario == "forwarded_sender" || scenario == "legacy_source_id" || scenario == "missing_timestamp" || scenario == "repeat_otp" || scenario == "continuation_unknown" || scenario == "identity_mismatch" || scenario == "media_incomplete"
			if continued {
				if runs.Load() != 2 || logins.Load() != 1 || inboxes.Load() != 1 || details.Load() != 1 {
					t.Fatalf("unexpected continuation counts: run=%d login=%d inbox=%d detail=%d", runs.Load(), logins.Load(), inboxes.Load(), details.Load())
				}
			} else if runs.Load() != 1 {
				t.Fatal("failure replayed refresh")
			}
			if !continued && scenario != "timeout" && scenario != "cancel" && scenario != "hme_failure" && logins.Load() != 0 {
				t.Fatal("unsupported OTP result or mailbox used HME")
			}
			saved, getErr := s.adobeStore.Get(a.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if err != nil && (saved.Revision != a.Revision || saved.Cookie != a.Cookie || saved.Token != a.Token || !reflect.DeepEqual(saved.RegistrationMailbox, a.RegistrationMailbox)) {
				t.Fatal("failed relogin replaced credentials or mailbox source")
			}
			if err == nil && (saved.Revision <= a.Revision || saved.Cookie != updatedCookie) {
				t.Fatal("verified session was not saved")
			}
		})
	}
}
