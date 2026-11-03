package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestGrokStartupClearsFailedReservationWithOldSignupHistory(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	ctx := context.Background()
	box := seedHTTPMailbox(t, s, "failed-signup-history")
	claimHTTPMailbox(t, s, box, "openai", "used-gpt")
	if _, err := s.mailboxStore.ReleaseForAccount(ctx, "openai", "used-gpt", box.Email); err != nil {
		t.Fatal(err)
	}
	if err := s.mailboxStore.EnsureRegistrationClaim(ctx, "grok", "old-root", box); err != nil {
		t.Fatal(err)
	}
	old := registerruntime.RegistrationJob{ID: "old-root", Status: "registration_pending", Stage: "signup", Error: "signup: account_page_missing", Mailbox: registerMailbox(box), AutoRetry: &registerruntime.RegistrationCompensation{State: "superseded"}}
	child := old
	child.ID = "latest"
	child.RootJobID = old.ID
	child.Stage = "send_code"
	child.Error = "send_code: email_input_missing"
	child.AutoRetry = &registerruntime.RegistrationCompensation{State: "exhausted", Attempts: 3}
	dir := s.cfg.DataDir
	if s.cfg.RegisterPath != "" {
		dir = filepath.Dir(s.cfg.RegisterPath)
	}
	raw, err := json.Marshal([]registerruntime.RegistrationJob{old, child})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "grok_registration_tasks.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s.initGrokRegistration()
	v := mailboxView(t, s, box.ID)
	if v.GrokReserved || v.GrokClaimed || !v.OpenAIRetired || v.OpenAIReserved || v.GrokRetired || v.Status != "available" {
		t.Fatal(v)
	}
	response := adminRequest(s.Handler(), "GET", "/api/icloud/mailboxes?search="+box.Email, nil)
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(`"grok_reserved":false`)) || bytes.Contains(response.Body.Bytes(), []byte("review_required")) {
		t.Fatal(response.Body.String())
	}
	s.initGrokRegistration()
	events, _, err := s.mailboxStore.Events(ctx, 1, 50, box.ID, "grok")
	if err != nil {
		t.Fatal(err)
	}
	released := 0
	for _, event := range events {
		if event.EventType == "claim_released" {
			released++
		}
	}
	if released != 1 {
		t.Fatal("startup duplicated release", released)
	}
}
