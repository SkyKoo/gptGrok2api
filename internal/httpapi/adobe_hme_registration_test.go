package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestMailboxSourcePublicUsesCurrentAdapterCapabilities(t *testing.T) {
	for _, mode := range []string{"legacy", "disabled", "missing_password", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			source := mailboxstore.Source{ID: "legacy", Type: "icloud_hme", Label: "Saved HME", Enabled: true, Status: "available", Config: map[string]any{"api_base": "http://hme.test", "admin_password": "PRIVATE-HME", "account_id": "fixture"}, Capabilities: map[string]any{"openai": true, "grok": true, "create": true, "status": "available"}}
			switch mode {
			case "disabled":
				source.Enabled = false
			case "missing_password":
				delete(source.Config, "admin_password")
			case "unsupported":
				source.Type = "cloudmail_gen"
			}
			before, _ := json.Marshal(source)
			result := mailboxSourcePublic(source)
			capable := mode == "legacy" || mode == "disabled"
			capabilities := mapValue(result["capabilities"])
			for _, target := range []string{"openai", "grok", "adobe", "create"} {
				if capabilities[target] != capable {
					t.Fatalf("%s capability=%v", target, capabilities[target])
				}
			}
			if result["enable"] != source.Enabled {
				t.Fatal("enabled preference changed")
			}
			if capable && result["status"] != "available" || !capable && result["status"] != "config_only" {
				t.Fatalf("status=%v", result["status"])
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "PRIVATE-HME") {
				t.Fatal("source password leaked")
			}
			after, _ := json.Marshal(source)
			if string(before) != string(after) {
				t.Fatal("public projection mutated stored source")
			}
		})
	}
}

func TestLegacyHMESourceAPIAddsAdobeWithoutDatabaseRewrite(t *testing.T) {
	s, db := mailboxHTTPTestServer(t)
	ctx := context.Background()
	source := mailboxstore.Source{ID: "legacy", Type: "icloud_hme", Label: "Saved HME", Enabled: true, Status: "available", Config: map[string]any{"api_base": "http://hme.test", "admin_password": "PRIVATE-HME", "account_id": "fixture", "delete_hme_mailbox_on_account_delete": true}, Capabilities: map[string]any{"openai": true, "grok": true, "create": true, "status": "available"}}
	if _, err := s.mailboxStore.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var raw string
		if err := db.QueryRow(`SELECT to_jsonb(s)::text FROM mailbox_sources s WHERE id='legacy'`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	for attempt := 0; attempt < 2; attempt++ {
		response := adminRequest(s.Handler(), http.MethodGet, "/api/icloud/sources", nil)
		if response.Code != http.StatusOK {
			t.Fatal(response.Body.String())
		}
		var body struct {
			Sources []map[string]any `json:"sources"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Sources) != 1 || body.Sources[0]["id"] != "legacy" || mapValue(body.Sources[0]["capabilities"])["adobe"] != true {
			t.Fatalf("missing current Adobe capability: %s", response.Body.String())
		}
		if strings.Contains(response.Body.String(), "PRIVATE-HME") {
			t.Fatal("source secret leaked")
		}
	}
	if before != snapshot() {
		t.Fatal("GET rewrote source configuration or capability snapshot")
	}
}

func TestAdobeRegistrationUsesHMESourceAndIndependentClaims(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		name := "create"
		if reuse {
			name = "reuse"
		}
		t.Run(name, func(t *testing.T) {
			s, db := mailboxHTTPTestServer(t)
			ctx := context.Background()
			var creates, inboxes, verifications, releases atomic.Int32
			var alias = registerruntime.Mailbox{ID: "hme-alias", Email: "adobe-fixture@example.test", AccountID: "hme-account", SourceMailboxID: "hme-alias", SourceType: "hme"}
			hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respond := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": v}) }
				switch r.URL.Path {
				case "/api/auth/login":
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["password"] != "PRIVATE-HME" {
						t.Error("wrong HME password")
					}
					http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: "fixture", Path: "/", HttpOnly: true})
					respond(map[string]string{"csrf_token": "fixture-csrf"})
				case "/api/create":
					creates.Add(1)
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["account_id"] != alias.AccountID || r.Header.Get("X-CSRF-Token") != "fixture-csrf" {
						t.Error("HME create scope differs")
					}
					respond(alias)
				case "/api/inbox":
					inboxes.Add(1)
					if r.URL.Query().Get("account_id") != alias.AccountID || r.URL.Query().Get("alias") != alias.Email {
						t.Error("HME inbox scope differs")
					}
					cookie, err := r.Cookie("hme_session")
					if err != nil || cookie.Value != "fixture" {
						t.Error("HME session missing")
					}
					now := time.Now().UTC()
					message := func(id, from, to, code string, at time.Time) map[string]string {
						return map[string]string{"ID": id, "Folder": "inbox", "From": from, "To": to, "Subject": "Verification " + code, "Date": at.Format(time.RFC3339Nano)}
					}
					respond(map[string]any{"method": "web_api", "messages": []any{
						message("gpt", "noreply@openai.com", alias.Email, "111111", now.Add(time.Second)),
						message("grok", "noreply@x.ai", alias.Email, "222222", now.Add(time.Second)),
						message("other", "noreply@adobe.com", "other@example.test", "333333", now.Add(time.Second)),
						message("old", "noreply@adobe.com", alias.Email, "444444", now.Add(-time.Hour)),
						message("adobe", "account_noreply_at_adobe_com_forward@icloud.com", alias.Email, "654321", now),
					}})
				default:
					t.Errorf("unexpected HME endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer hme.Close()
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer PRIVATE-WORKER" {
					t.Error("worker authentication differs")
				}
				if strings.HasPrefix(r.URL.Path, "/v1/browser/reservations/") && r.Method == http.MethodDelete {
					releases.Add(1)
					writeJSON(w, 200, map[string]string{"status": "released"})
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				switch r.URL.Path {
				case "/v1/browser/reservations":
					if body["provider"] != "adobe" || body["email"] != alias.Email {
						t.Error("worker received wrong identity")
					}
					writeJSON(w, 200, map[string]string{"status": "ready"})
				case "/v1/adobe/run":
					if body["action"] == "run" {
						writeJSON(w, 200, map[string]string{"stage": "awaiting_verification", "code": "verification_required", "creation_outcome": "accepted"})
						return
					}
					if body["action"] != "verify" || body["code"] != "654321" {
						t.Error("wrong platform/mailbox/age code selected")
					}
					verifications.Add(1)
					writeJSON(w, 200, map[string]any{"stage": "completed", "account": map[string]any{"email": alias.Email, "account_id": "adobe-identity", "token": "fixture-token", "cookie": "fixture-cookie"}})
				default:
					t.Errorf("unexpected Worker endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer worker.Close()
			provider := map[string]any{"id": "fixture", "type": "icloud_hme", "enable": true, "api_base": hme.URL, "admin_password": "PRIVATE-HME", "account_id": alias.AccountID}
			source := mailboxSourceFromProvider(provider, 0)
			// This is deliberately the old stored snapshot that omitted Adobe support.
			source.Capabilities = map[string]any{"openai": true, "grok": true, "create": true}
			if _, err := s.mailboxStore.UpsertSource(ctx, source); err != nil {
				t.Fatal(err)
			}
			var box mailboxstore.Mailbox
			if reuse {
				var err error
				box, err = s.mailboxStore.Upsert(ctx, mailboxstore.Mailbox{SourceID: "fixture", SourceType: "hme", SourceAccountID: alias.AccountID, SourceMailboxID: alias.ID, Email: alias.Email}, "test")
				if err != nil {
					t.Fatal(err)
				}
				for _, platform := range []string{"openai", "grok"} {
					claimHTTPMailbox(t, s, box, platform, platform+"-account")
				}
			}
			captured := make(chan map[string]any, 1)
			engine := registerruntime.NewAdobeEngine(filepath.Join(t.TempDir(), "adobe.enc.json"), "fixture-secret", func(cfg registerruntime.AdobeConfig) (registerruntime.MailSource, registerruntime.AdobeRegistrationFlow, error) {
				mail, err := newMailboxResourceSource(ctx, s.mailboxManager, "adobe", cfg.MailboxSourceID, cfg.PollInterval)
				return mail, &registerruntime.AdobeBrowserFlow{Client: worker.Client()}, err
			}, func(_ context.Context, _ string, account map[string]any) error { captured <- account; return nil }, func(context.Context, string, map[string]any) error { return nil })
			raw := map[string]any{"target": "adobe", "total": 1, "threads": 1, "mode": "total", "proxy": "direct", "mail": map[string]any{"source_id": "fixture", "providers": []any{provider}}, "adobe": map[string]any{"api_base": worker.URL, "api_key": "PRIVATE-WORKER"}}
			job, err := engine.Start(raw)
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Stop()
			deadline := time.Now().Add(5 * time.Second)
			for engine.Snapshot()["running"] == true && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond * 5)
			}
			snapshot := engine.Snapshot()
			if snapshot["running"] == true {
				t.Fatal("registration fixture did not finish")
			}
			jobs := snapshot["jobs"].([]any)
			if len(jobs) != 1 || mapValue(jobs[0])["status"] != "completed" {
				t.Fatalf("registration not complete: %+v", snapshot)
			}
			var account map[string]any
			select {
			case account = <-captured:
			default:
				t.Fatal("registered account not imported")
			}
			metadata := mapValue(account["registration_mailbox"])
			expected := map[string]any{"provider": "icloud_hme", "platform": "adobe", "source_id": "fixture", "source_type": "hme", "source_mailbox_id": alias.ID, "account_id": alias.AccountID, "email": alias.Email}
			for key, value := range expected {
				if metadata[key] != value {
					t.Fatalf("metadata %s differs", key)
				}
			}
			if account["registration_job_id"] != job || metadata["id"] == "" {
				t.Fatal("registration link metadata missing")
			}
			rows, total, err := s.mailboxStore.List(ctx, 1, 10, mailboxstore.MailboxListFilter{})
			if err != nil || total != 1 || len(rows) != 1 {
				t.Fatalf("mailbox list count=%d err=%v", total, err)
			}
			view := rows[0]
			if !view.AdobeClaimed || view.AdobeReserved || view.OpenAIClaimed != reuse || view.GrokClaimed != reuse {
				t.Fatalf("platform claims crossed: %+v", view)
			}
			if reuse && view.ID != box.ID {
				t.Fatal("created a replacement rather than reuse")
			}
			var owner, registrationJob string
			if err := db.QueryRow(`SELECT account_id,registration_job_id FROM mailbox_claims WHERE platform='adobe' AND status='active'`).Scan(&owner, &registrationJob); err != nil || owner != "adobe:adobe-identity" || registrationJob != job {
				t.Fatalf("adobe claim owner=%s job=%s err=%v", owner, registrationJob, err)
			}
			expectedCreates := int32(1)
			if reuse {
				expectedCreates = 0
			}
			if creates.Load() != expectedCreates || inboxes.Load() != 1 || verifications.Load() != 1 || releases.Load() != 1 {
				t.Fatalf("unexpected request counts create=%d inbox=%d verify=%d release=%d", creates.Load(), inboxes.Load(), verifications.Load(), releases.Load())
			}
			afterSource, err := s.mailboxStore.GetSource(ctx, "fixture")
			if err != nil || !reflect.DeepEqual(afterSource.Capabilities, source.Capabilities) {
				t.Fatal("registration mutated source capabilities")
			}
		})
	}
}
