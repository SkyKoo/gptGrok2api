package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
)

func mailboxHTTPTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("MAILBOX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MAILBOX_TEST_DATABASE_URL is not configured")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("mailbox_http_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	cfg := adminTestConfig(t.TempDir())
	cfg.MailboxDatabaseURL = u.String()
	s := New(cfg)
	if s.mailboxStore == nil {
		t.Fatalf("mailbox init: %v", s.mailboxInitErr)
	}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.mailboxStore.Close(); db.Close() })
	return s, db
}

func seedHTTPMailbox(t *testing.T, s *Server, name string) mailboxstore.Mailbox {
	t.Helper()
	b, err := s.mailboxStore.Upsert(context.Background(), mailboxstore.Mailbox{SourceID: "fixture", SourceType: "hme", SourceAccountID: "hme-account", SourceMailboxID: "alias-" + name, Email: name + "@example.test"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func claimHTTPMailbox(t *testing.T, s *Server, box mailboxstore.Mailbox, platform, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.mailboxStore.EnsureClaim(ctx, platform, id+"-job", box); err != nil {
		t.Fatal(err)
	}
	if err := s.mailboxStore.Commit(ctx, platform, id+"-job", id, box); err != nil {
		t.Fatal(err)
	}
}

func mailboxView(t *testing.T, s *Server, id string) mailboxstore.MailboxView {
	t.Helper()
	rows, _, err := s.mailboxStore.List(context.Background(), 1, 100, mailboxstore.MailboxListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("missing mailbox %s", id)
	return mailboxstore.MailboxView{}
}

func TestAccountDeletionReleasesMailboxWithoutUpstream(t *testing.T) {
	for _, mode := range []string{"policy_off", "source_disabled", "invalid_source", "legacy_email", "non_hme"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := mailboxHTTPTestServer(t)
			var calls atomic.Int32
			hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer hme.Close()
			source := mailboxstore.Source{ID: "fixture", Type: "icloud_hme", Enabled: mode != "source_disabled", Config: map[string]any{"api_base": hme.URL, "admin_password": "fixture-secret", "account_id": "hme-account", "delete_hme_mailbox_on_account_delete": false}}
			if mode == "invalid_source" {
				source.Config = map[string]any{}
			}
			if mode == "non_hme" {
				source.Type = "custom"
			}
			if _, err := s.mailboxStore.UpsertSource(context.Background(), source); err != nil {
				t.Fatal(err)
			}
			tokens := []string{}
			for _, name := range []string{"shared", "solo"} {
				b := seedHTTPMailbox(t, s, name)
				claimHTTPMailbox(t, s, b, "openai", name)
				if name == "shared" {
					claimHTTPMailbox(t, s, b, "grok", "grok-account")
				}
				a := map[string]any{"id": name, "access_token": "token-" + name, "email": b.Email}
				if mode != "legacy_email" {
					a["registration_mailbox"] = map[string]any{"email": b.Email, "provider": "icloud_hme", "source_id": "fixture"}
				}
				if _, _, _, err := s.store.AddAccounts(nil, []map[string]any{a}); err != nil {
					t.Fatal(err)
				}
				tokens = append(tokens, "token-"+name)
			}
			raw, _ := json.Marshal(map[string]any{"tokens": tokens})
			rec := adminRequest(s.Handler(), http.MethodDelete, "/api/accounts", strings.NewReader(string(raw)))
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"released":2`) {
				t.Fatalf("delete %d %s", rec.Code, rec.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatalf("unnecessary upstream calls %d", calls.Load())
			}
			views, _, _ := s.mailboxStore.List(context.Background(), 1, 10, mailboxstore.MailboxListFilter{})
			for _, v := range views {
				if v.OpenAIClaimed || (v.Email == "shared@example.test" && !v.GrokClaimed) {
					t.Fatalf("claims %+v", v)
				}
			}
			accounts, _ := s.store.AccountList()
			if len(accounts) != 0 {
				t.Fatal("accounts not deleted")
			}
		})
	}
}

func TestMailboxDeletionAPIAndLastPlatformCleanup(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	var deletes, lists atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	hme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data}) }
		switch r.URL.Path {
		case "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: "test", Path: "/"})
			respond(map[string]string{"csrf_token": "csrf"})
		case "/api/aliases":
			lists.Add(1)
			if r.URL.Query().Get("account_id") != "hme-account" {
				t.Error("wrong source account")
			}
			respond(map[string]any{"aliases": []map[string]string{{"anonymousId": "upstream-opaque-id", "email": "shared@example.test"}}})
		case "/api/aliases/upstream-opaque-id":
			deletes.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Method != "DELETE" || body["account_id"] != "hme-account" || r.Header.Get("X-CSRF-Token") != "csrf" {
				t.Errorf("wrong delete protocol %s %#v", r.Method, body)
			}
			if fail.Load() {
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":"fixture upstream failure"}`))
				return
			}
			respond(map[string]any{"success": true})
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer hme.Close()
	_, err := s.mailboxStore.UpsertSource(context.Background(), mailboxstore.Source{ID: "fixture", Type: "icloud_hme", Enabled: false, Config: map[string]any{"api_base": hme.URL, "admin_password": "fixture-secret", "account_id": "hme-account", "delete_hme_mailbox_on_account_delete": true}})
	if err != nil {
		t.Fatal(err)
	}
	b := seedHTTPMailbox(t, s, "shared")
	claimHTTPMailbox(t, s, b, "openai", "gpt-account")
	claimHTTPMailbox(t, s, b, "grok", "grok-account")
	path := "/api/icloud/mailboxes/" + b.ID
	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest("DELETE", path, nil))
	if unauth.Code == 200 {
		t.Fatal("missing auth")
	}
	rec := adminRequest(s.Handler(), "DELETE", path, nil)
	if rec.Code != 409 || lists.Load() != 0 {
		t.Fatalf("occupied delete %d %s", rec.Code, rec.Body.String())
	}
	_, _, _, err = s.store.AddAccounts(nil, []map[string]any{{"id": "gpt-account", "access_token": "token", "email": b.Email}})
	if err != nil {
		t.Fatal(err)
	}
	rec = adminRequest(s.Handler(), "DELETE", "/api/accounts", strings.NewReader(`{"tokens":["token"]}`))
	if rec.Code != 200 || deletes.Load() != 0 || mailboxView(t, s, b.ID).OpenAIClaimed {
		t.Fatalf("shared release: %d %s", rec.Code, rec.Body.String())
	}
	_, _, err = s.registerStore.AddAccount(map[string]any{"id": "grok-account", "sso": "fixture-sso", "email": b.Email})
	if err != nil {
		t.Fatal(err)
	}
	rec = adminRequest(s.Handler(), "DELETE", "/api/register/grok/accounts", strings.NewReader(`{"ids":["grok-account"]}`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"failed":1`) {
		t.Fatalf("last claim %d %s", rec.Code, rec.Body.String())
	}
	v := mailboxView(t, s, b.ID)
	if v.GrokClaimed || v.Status != "cleanup_pending" || deletes.Load() != 1 {
		t.Fatalf("pending %+v deletes=%d", v, deletes.Load())
	}
	rec = adminRequest(s.Handler(), "DELETE", path, nil)
	if rec.Code != 502 {
		t.Fatalf("failure %d %s", rec.Code, rec.Body.String())
	}
	fail.Store(false)
	rec = adminRequest(s.Handler(), "DELETE", path, nil)
	if rec.Code != 200 || mailboxView(t, s, b.ID).Status != "deleted" {
		t.Fatalf("retry %d %s", rec.Code, rec.Body.String())
	}
	count := deletes.Load()
	rec = adminRequest(s.Handler(), "DELETE", path, nil)
	if rec.Code != 200 || deletes.Load() != count {
		t.Fatal("duplicate deletion")
	}
	missing := seedHTTPMailbox(t, s, "already-missing")
	rec = adminRequest(s.Handler(), "DELETE", "/api/icloud/mailboxes/"+missing.ID, nil)
	if rec.Code != 200 || mailboxView(t, s, missing.ID).Status != "deleted" {
		t.Fatalf("missing alias %d %s", rec.Code, rec.Body.String())
	}
	events, _, err := s.mailboxStore.Events(context.Background(), 1, 100, b.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	foundFailure, foundSuccess := false, false
	for _, e := range events {
		foundFailure = foundFailure || e.EventType == "provider_cleanup_failed"
		foundSuccess = foundSuccess || e.EventType == "provider_cleanup_succeeded"
	}
	if !foundFailure || !foundSuccess {
		t.Fatal("missing audit events")
	}
}

func TestMailboxReconcileRepairsOnlyProvenDeletedClaims(t *testing.T) {
	s, db := mailboxHTTPTestServer(t)
	ctx := context.Background()
	for _, name := range []string{"deleted", "recreated", "new-reservation", "new-activation", "external"} {
		b := seedHTTPMailbox(t, s, name)
		claimHTTPMailbox(t, s, b, "openai", name)
		claimHTTPMailbox(t, s, b, "grok", "grok-"+name)
		if _, err := db.Exec(`UPDATE mailbox_claims SET updated_at=now()-interval '1 minute' WHERE mailbox_id=$1`, b.ID); err != nil {
			t.Fatal(err)
		}
		if name == "external" {
			continue
		}
		_, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"id": name, "email": b.Email, "access_token": name}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.store.DeleteAccounts([]string{name}); err != nil {
			t.Fatal(err)
		}
		if name == "recreated" {
			_, _, _, err = s.store.AddAccounts(nil, []map[string]any{{"id": "new-account", "email": b.Email, "access_token": "new"}})
			if err != nil {
				t.Fatal(err)
			}
		}
		if name == "new-reservation" || name == "new-activation" {
			if _, err := s.mailboxStore.ReleaseForAccount(ctx, "openai", name, b.Email); err != nil {
				t.Fatal(err)
			}
			if err := s.mailboxStore.EnsureClaim(ctx, "openai", "new-"+name, b); err != nil {
				t.Fatal(err)
			}
			if name == "new-activation" {
				if err := s.mailboxStore.Commit(ctx, "openai", "new-"+name, name, b); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	n, err := s.reconcileDeletedMailboxClaims(ctx)
	if err != nil || n != 1 {
		t.Fatalf("repair count=%d err=%v", n, err)
	}
	views, _, _ := s.mailboxStore.List(ctx, 1, 100, mailboxstore.MailboxListFilter{})
	for _, v := range views {
		if !v.GrokClaimed || (v.Email == "deleted@example.test" && v.OpenAIClaimed) || (v.Email != "deleted@example.test" && !v.OpenAIClaimed) {
			t.Fatalf("unsafe repair %+v", v)
		}
	}
	n, err = s.reconcileDeletedMailboxClaims(ctx)
	if err != nil || n != 0 {
		t.Fatalf("repair idempotence %d %v", n, err)
	}
}
