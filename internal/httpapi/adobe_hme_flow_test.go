package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

// Exercise the real engine -> resource manager -> HME adapter -> browser-code
// callback, with only the external HME and browser endpoints replaced locally.
func TestAdobeRegistrationReceivesCodeThroughSharedHME(t *testing.T) {
	for _, sender := range []string{"Adobe <account-noreply@adobe.com>", "Adobe <account-noreply_at_adobe_com_opaque123@icloud.com>"} {
		t.Run(sender, func(t *testing.T) {
			var aliases, inboxes, details, submissions, verifications, releases, imports atomic.Int32
			const email = "adobe-fixture@icloud.com"
			hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respond := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data}) }
				if r.URL.Path == "/api/auth/login" {
					http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: "fixture", Path: "/"})
					respond(map[string]string{"csrf_token": "fixture"})
					return
				}
				if cookie, err := r.Cookie("hme_session"); err != nil || cookie.Value != "fixture" {
					t.Error("HME session was not preserved")
				}
				switch r.URL.Path {
				case "/api/create":
					aliases.Add(1)
					respond(map[string]string{"id": "alias-fixture", "email": email, "account_id": "hme-fixture"})
				case "/api/inbox":
					inboxes.Add(1)
					if r.URL.Query().Get("alias") != email || r.URL.Query().Get("folder") != "all" || r.URL.Query().Get("account_id") != "hme-fixture" {
						t.Error("HME query lost the selected mailbox")
					}
					now := time.Now().Add(time.Second).UTC().Format(time.RFC3339)
					respond(map[string]any{"method": "web_api", "messages": []map[string]string{
						{"id": "gpt", "folder": "inbox", "from": "noreply@openai.com", "to": email, "subject": "111111", "date": now},
						{"id": "grok", "folder": "inbox", "from": "noreply@x.ai", "to": email, "subject": "222222", "date": now},
						{"id": "other", "folder": "inbox", "from": sender, "to": "other@icloud.com", "subject": "333333", "date": now},
						{"id": "old", "folder": "inbox", "from": sender, "to": email, "subject": "444444", "date": time.Now().Add(-time.Hour).Format(time.RFC3339)},
						{"id": "fake", "folder": "inbox", "from": "Adobe <noreply@adobe.com.attacker.test>", "to": email, "subject": "555555", "date": now},
						{"id": "fake-forward", "folder": "inbox", "from": "Adobe <noreply_at_adobe_com_attacker_com_opaque123@icloud.com>", "to": email, "subject": "666666", "date": now},
						{"id": "adobe", "folder": "junk", "from": sender, "to": email, "subject": "Verify your email", "date": now},
					}})
				case "/api/inbox/adobe":
					details.Add(1)
					respond(map[string]string{"from": sender, "to": email, "date": time.Now().Add(time.Second).UTC().Format(time.RFC3339), "body": "Your Adobe verification code is 246810."})
				default:
					t.Errorf("unexpected HME path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer hme.Close()
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/browser/reservations/") {
					releases.Add(1)
					_, _ = w.Write([]byte(`{"status":"released"}`))
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				switch r.URL.Path {
				case "/v1/browser/reservations":
					if body["provider"] != "adobe" || body["email"] != email || mapValue(body["adobe_profile"])["country"] != "US" {
						t.Error("browser received incorrect Adobe identity/profile")
					}
					_, _ = w.Write([]byte(`{"status":"ready"}`))
				case "/v1/adobe/run":
					switch body["action"] {
					case "run":
						submissions.Add(1)
						_, _ = w.Write([]byte(`{"stage":"awaiting_verification","code":"verification_required","creation_outcome":"accepted"}`))
					case "verify":
						verifications.Add(1)
						if body["code"] != "246810" {
							t.Error("incorrect provider/recipient/stale code reached Adobe")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"stage": "completed", "account": map[string]any{"email": email, "account_id": "adobe-fixture", "token": "fixture-token", "cookie": "fixture-cookie"}})
					default:
						t.Error("unexpected Adobe action")
					}
				default:
					t.Error("unexpected browser endpoint")
				}
			}))
			defer worker.Close()
			manager := mailboxstore.NewResourceManager(nil, newMailboxProvider)
			manager.SetSourceLoader(func(context.Context) ([]mailboxstore.Source, error) {
				return []mailboxstore.Source{{ID: "hme-selected", Type: "icloud_hme", Enabled: true, Config: map[string]any{"api_base": hme.URL, "admin_password": "fixture", "account_id": "hme-fixture"}}}, nil
			})
			engine := registerruntime.NewAdobeEngine(filepath.Join(t.TempDir(), "adobe.enc.json"), "fixture-key", func(cfg registerruntime.AdobeConfig) (registerruntime.MailSource, registerruntime.AdobeRegistrationFlow, error) {
				mail, err := newMailboxResourceSource(context.Background(), manager, "adobe", cfg.MailboxSourceID, cfg.PollInterval)
				return mail, &registerruntime.AdobeBrowserFlow{Client: worker.Client()}, err
			}, func(_ context.Context, _ string, account map[string]any) error {
				imports.Add(1)
				if mapValue(account["registration_mailbox"])["source_id"] != "hme-selected" || account["platform"] != "adobe" {
					t.Error("account lost HME ownership")
				}
				return nil
			}, func(context.Context, string, map[string]any) error { return nil })
			cfg := map[string]any{"target": "adobe", "mode": "total", "total": 1, "threads": 1, "proxy": "direct", "mail": map[string]any{"source_id": "hme-selected", "wait_timeout": -1, "wait_interval": -1, "providers": []any{map[string]any{"id": "hme-selected", "type": "icloud_hme", "enable": true, "api_base": hme.URL, "admin_password": "fixture", "account_id": "hme-fixture"}}}, "adobe": map[string]any{"api_base": worker.URL}}
			if _, err := engine.Start(cfg); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for engine.Snapshot()["running"].(bool) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if engine.Snapshot()["running"].(bool) {
				t.Fatal("Adobe HME flow did not complete")
			}
			job := engine.Snapshot()["jobs"].([]any)[0].(map[string]any)
			if job["status"] != "completed" {
				t.Fatalf("unexpected terminal status: %v", job["status"])
			}
			if aliases.Load() != 1 || inboxes.Load() != 1 || details.Load() != 1 || submissions.Load() != 1 || verifications.Load() != 1 || imports.Load() != 1 || releases.Load() != 1 {
				t.Fatalf("unexpected lifecycle counts: aliases=%d inbox=%d details=%d submit=%d verify=%d import=%d release=%d", aliases.Load(), inboxes.Load(), details.Load(), submissions.Load(), verifications.Load(), imports.Load(), releases.Load())
			}
		})
	}
}
