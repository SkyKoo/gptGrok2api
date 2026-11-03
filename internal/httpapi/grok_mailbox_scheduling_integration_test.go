package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

type schedulingProvider struct{}

func (*schedulingProvider) Acquire(context.Context, string) (mailboxstore.Mailbox, error) {
	return mailboxstore.Mailbox{}, &registerruntime.Failure{Stage: "test", Code: "unexpected_provider_creation"}
}
func (*schedulingProvider) Prepare(context.Context, mailboxstore.Mailbox) error { return nil }
func (*schedulingProvider) WaitCode(context.Context, mailboxstore.Mailbox, time.Time, func(string) bool) (string, error) {
	return "", nil
}
func (*schedulingProvider) Delete(context.Context, mailboxstore.Mailbox) error   { return nil }
func (*schedulingProvider) List(context.Context) ([]mailboxstore.Mailbox, error) { return nil, nil }
func (*schedulingProvider) Close()                                               {}

type schedulingFlow struct {
	profile registerruntime.BrowserProfile
	seen    chan string
}

func (f *schedulingFlow) BrowserProfile() registerruntime.BrowserProfile { return f.profile }
func (*schedulingFlow) Close()                                           {}
func (f *schedulingFlow) Register(_ context.Context, box registerruntime.Mailbox, _ registerruntime.GrokConfig, _ func(context.Context, time.Time) (string, error), progress registerruntime.Progress) (map[string]any, error) {
	f.seen <- box.Email
	if err := progress("send_code"); err != nil {
		return nil, err
	}
	return nil, &registerruntime.Failure{Stage: "send_code", Code: "network_error"}
}

// Reproduce the original bug through the engine, production resource adapter,
// resource manager and real PostgreSQL claims. No browser or upstream is used.
func TestGrokBatchMovesPastReleasedFailedMailbox(t *testing.T) {
	s, db := mailboxHTTPTestServer(t)
	ctx := context.Background()
	old := seedHTTPMailbox(t, s, "old-failed")
	seedHTTPMailbox(t, s, "next-eligible")
	seedHTTPMailbox(t, s, "third-eligible")
	if _, err := db.ExecContext(ctx, `UPDATE mailboxes SET created_at=now()-interval '1 day' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	claimHTTPMailbox(t, s, old, "openai", "existing-gpt")
	if err := s.mailboxStore.EnsureRegistrationClaim(ctx, "grok", "old", old); err != nil {
		t.Fatal(err)
	}
	if err := s.mailboxStore.Release(ctx, "grok", "old", old, "registration_failed_without_account"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.mailboxStore.UpsertSource(ctx, mailboxstore.Source{ID: "fixture", Type: "hme", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	manager := mailboxstore.NewResourceManager(s.mailboxStore, func(mailboxstore.Source, time.Duration) (mailboxstore.Provider, error) {
		return &schedulingProvider{}, nil
	})
	profile := registerruntime.NewBrowserProfile()
	path := filepath.Join(t.TempDir(), "jobs.json")
	raw, _ := json.Marshal([]registerruntime.RegistrationJob{{ID: "old", Status: "registration_pending", Stage: "send_code", Mailbox: registerMailbox(old), BrowserProfile: &profile, AutoRetry: &registerruntime.RegistrationCompensation{State: "exhausted", Attempts: 3}}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 2)
	e := registerruntime.NewGrokEngine(path, func(registerruntime.GrokConfig) (registerruntime.MailSource, registerruntime.GrokRegistrationFlow, error) {
		mail, err := newMailboxResourceSource(ctx, manager, "grok", "fixture", time.Millisecond)
		return mail, &schedulingFlow{profile: profile, seen: seen}, err
	}, nil, nil)
	e.SetMailboxReservationReleaser(func(ctx context.Context, job registerruntime.RegistrationJob) error {
		return s.mailboxStore.Release(ctx, "grok", firstNonEmpty(job.RootJobID, job.ID), mailboxFromRegister(job.Mailbox), "registration_failed_without_account")
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	config := map[string]any{"target": "grok", "mode": "total", "total": 2, "threads": 2, "mail": map[string]any{"providers": []any{map[string]any{"type": "icloud_hme", "enable": true, "api_base": "https://mail.example.test", "admin_password": "test-only", "account_id": "hme-account"}}}, "grok": map[string]any{"provider": "browser", "api_base": "http://grok-browser:8899"}}
	if _, err := e.Start(config); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.Snapshot()["running"] == true && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if e.Snapshot()["running"] == true {
		t.Fatal("batch did not finish")
	}
	if len(seen) != 2 {
		t.Fatal("not both eligible aliases reached registration", len(seen))
	}
	one, two := <-seen, <-seen
	if one == old.Email || two == old.Email || one == two {
		t.Fatal("released alias was reused", one, two)
	}
	view := mailboxView(t, s, old.ID)
	if view.GrokReserved || view.GrokClaimed || !view.OpenAIClaimed {
		t.Fatal("ownership or GPT changed", view)
	}
	var reserved int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_claims WHERE platform='grok' AND status='reserved'`).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatal("failed claims not released", reserved, err)
	}
}
