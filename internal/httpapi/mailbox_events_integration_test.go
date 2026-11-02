package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
)

func TestMailboxEventsIncludeEmailAfterDeletion(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	ctx := context.Background()
	deleted := seedHTTPMailbox(t, s, "deleted-history-with-full-address")
	other := seedHTTPMailbox(t, s, "other-history")
	for _, box := range []mailboxstore.Mailbox{deleted, other} {
		if _, err := s.mailboxStore.SetPlatformStatus(ctx, box.ID, "grok", "disabled"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.mailboxStore.Delete(ctx, deleted.ID, "admin", func(context.Context, mailboxstore.Mailbox) error { return nil }); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/icloud/events", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}

	for _, filtered := range []bool{false, true} {
		path := "/api/icloud/events?page=1&page_size=50"
		if filtered {
			path += "&mailbox_id=" + deleted.ID + "&platform=grok"
		}
		rec := adminRequest(handler, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("filtered=%v status=%d body=%s", filtered, rec.Code, rec.Body.String())
		}
		var payload struct {
			Events []struct {
				MailboxID string `json:"mailbox_id"`
				Email     string `json:"email"`
				EventType string `json:"event_type"`
				Platform  string `json:"platform"`
			} `json:"events"`
			Total int `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Events) == 0 || payload.Total != len(payload.Events) {
			t.Fatalf("unexpected event count=%d total=%d", len(payload.Events), payload.Total)
		}
		expected := map[string]string{deleted.ID: deleted.Email, other.ID: other.Email}
		seen := map[string]bool{}
		cleanupFound := false
		for _, event := range payload.Events {
			if email, ok := expected[event.MailboxID]; !ok || event.Email != email {
				t.Fatalf("event has incorrect email: %+v", event)
			}
			if filtered && (event.MailboxID != deleted.ID || event.Platform != "grok") {
				t.Fatalf("event outside filter: %+v", event)
			}
			seen[event.MailboxID] = true
			cleanupFound = cleanupFound || event.EventType == "provider_cleanup_succeeded"
		}
		if filtered && payload.Total != 1 {
			t.Fatalf("filtered total=%d, want 1", payload.Total)
		}
		if !filtered && (len(seen) != 2 || !cleanupFound) {
			t.Fatalf("history missing: mailboxes=%d cleanup=%v", len(seen), cleanupFound)
		}
	}
}
