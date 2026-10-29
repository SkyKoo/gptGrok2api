package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestMailboxChannelPolicyAPI(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	b := seedHTTPMailbox(t, s, "policy-api")
	claimHTTPMailbox(t, s, b, "openai", "existing-account")
	handler := s.Handler()
	path := "/api/icloud/mailboxes/" + b.ID + "/status"
	call := func(body map[string]any, want int) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := adminRequest(handler, "POST", path, bytes.NewReader(raw))
		if rec.Code != want {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	call(map[string]any{"status": "disabled", "platform": "openai"}, http.StatusOK)
	view := mailboxView(t, s, b.ID)
	if !view.OpenAIDisabled || view.GrokDisabled || !view.OpenAIClaimed {
		t.Fatalf("policy/claims=%+v", view)
	}
	rec := adminRequest(handler, "GET", "/api/icloud/mailboxes?status=disabled_openai", nil)
	var payload struct {
		Mailboxes []struct {
			Email          string `json:"email"`
			OpenAIDisabled bool   `json:"openai_disabled"`
			GrokDisabled   bool   `json:"grok_disabled"`
		} `json:"mailboxes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(payload.Mailboxes) != 1 || !payload.Mailboxes[0].OpenAIDisabled || payload.Mailboxes[0].GrokDisabled {
		t.Fatalf("list=%s", rec.Body.String())
	}
	call(map[string]any{"status": "disabled", "platform": "invalid"}, http.StatusBadRequest)
	call(map[string]any{"status": "deleted", "platform": "grok"}, http.StatusBadRequest)
	if mailboxView(t, s, b.ID).GrokDisabled {
		t.Fatal("invalid request changed other channel")
	}
	call(map[string]any{"status": "disabled", "platform": "grok"}, http.StatusOK)
	call(map[string]any{"status": "available", "platform": "openai"}, http.StatusOK)
	view = mailboxView(t, s, b.ID)
	if view.OpenAIDisabled || !view.GrokDisabled || !view.OpenAIClaimed {
		t.Fatalf("independent enable=%+v", view)
	}
	// Legacy clients and historical claim reconciliation stay compatible.
	call(map[string]any{"status": "available"}, http.StatusOK)
	events, _, err := s.mailboxStore.Events(context.Background(), 1, 50, b.ID, "grok")
	if err != nil || len(events) != 1 || events[0].EventType != "mailbox_disabled" {
		t.Fatalf("audit=%+v err=%v", events, err)
	}
	counts, err := s.mailboxStore.Counts(context.Background())
	if err != nil || counts["disabled"] != 0 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
}
