package mailbox

import (
	"context"
	"errors"
	"testing"
)

func TestAdobeMailboxMigrationAndIndependentPolicy(t *testing.T) {
	ctx := context.Background()
	s := isolatedMailboxStore(t)
	box := seedMailbox(t, s, "adobe-shared")
	// Recreate the old constraints to exercise upgrading an existing database.
	for _, table := range []string{"mailbox_registration_blocks", "mailbox_registration_retirements"} {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT "+table+"_platform_check"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT "+table+"_platform_check CHECK (platform IN ('openai','grok'))"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPlatformStatus(ctx, box.ID, "openai", "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPlatformStatus(ctx, box.ID, "grok", "disabled"); err != nil {
		t.Fatal(err)
	}
	// Two blocked channels must not turn the third channel globally disabled.
	if err := s.EnsureRegistrationClaim(ctx, "adobe", "adobe-job", box); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, "adobe", "adobe-job", "adobe:identity", box); err != nil {
		t.Fatal(err)
	}
	views, _, err := s.List(ctx, 1, 20, MailboxListFilter{Platform: "adobe"})
	if err != nil || len(views) != 1 {
		t.Fatalf("Adobe claim missing: %v", err)
	}
	v := views[0]
	if !v.AdobeClaimed || v.AdobeDisabled || !v.OpenAIDisabled || !v.GrokDisabled {
		t.Fatalf("channel isolation lost: %+v", v)
	}
	if _, err := s.SetPlatformStatus(ctx, box.ID, "adobe", "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPlatformStatus(ctx, box.ID, "adobe", "available"); err != nil {
		t.Fatal(err)
	}
	views, _, err = s.List(ctx, 1, 20, MailboxListFilter{Status: "disabled_adobe"})
	if err != nil || len(views) != 0 {
		t.Fatalf("Adobe remained disabled: %v", err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "openai", "new-openai", box); !errors.Is(err, ErrMailboxRegistrationDisabled) {
		t.Fatalf("Adobe enable erased OpenAI policy: %v", err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	views, _, err = s.List(ctx, 1, 20, MailboxListFilter{Platform: "adobe"})
	if err != nil || len(views) != 1 || views[0].AdobeDisabled {
		t.Fatal("migration overwrote independent policy")
	}
}
