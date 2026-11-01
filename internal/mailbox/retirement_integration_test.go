package mailbox

import (
	"context"
	"errors"
	"testing"
)

func TestDeletedAccountRetiresOnlyItsPlatform(t *testing.T) {
	for _, platform := range []string{"openai", "grok"} {
		t.Run(platform, func(t *testing.T) {
			s := isolatedMailboxStore(t)
			ctx := context.Background()
			b := seedMailbox(t, s, "retired")
			other := "grok"
			if platform == "grok" {
				other = "openai"
			}
			if err := s.EnsureClaim(ctx, platform, "old-job", b); err != nil {
				t.Fatal(err)
			}
			if err := s.Commit(ctx, platform, "old-job", "old-account", b); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReleaseForAccount(ctx, platform, "old-account", b.Email); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReleaseForAccount(ctx, platform, "old-account", b.Email); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Upsert(ctx, b, "reconcile"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetPlatformStatus(ctx, b.ID, platform, "available"); !errors.Is(err, ErrMailboxRegistrationRetired) {
				t.Fatalf("enable bypass: %v", err)
			}
			if _, err := s.SetStatus(ctx, b.ID, "available"); !errors.Is(err, ErrMailboxRegistrationRetired) {
				t.Fatalf("legacy enable bypass: %v", err)
			}
			if _, err := s.SetPlatformStatus(ctx, b.ID, other, "available"); err != nil {
				t.Fatal(err)
			}
			if err := s.EnsureRegistrationClaim(ctx, platform, "retry", b); !errors.Is(err, ErrMailboxRegistrationRetired) {
				t.Fatalf("retry bypass: %v", err)
			}
			second := seedMailbox(t, s, "fresh")
			acquire := func(context.Context) (Mailbox, error) { t.Fatal("unexpected provider creation"); return Mailbox{}, nil }
			got, err := s.Reserve(ctx, platform, "new-job", "source-test", acquire, nil)
			if err != nil || got.ID != second.ID {
				t.Fatalf("allocation=%+v err=%v", got, err)
			}
			got, err = s.Reserve(ctx, other, "other-job", "source-test", acquire, nil)
			if err != nil || got.ID != b.ID {
				t.Fatalf("other platform=%+v err=%v", got, err)
			}
			for _, filter := range []string{"retired", "retired_" + platform} {
				views, total, err := s.List(ctx, 1, 20, MailboxListFilter{Status: filter})
				if err != nil || total != 1 || len(views) != 1 {
					t.Fatalf("filter=%s total=%d err=%v", filter, total, err)
				}
				if platform == "openai" && (!views[0].OpenAIRetired || views[0].GrokRetired) || platform == "grok" && (!views[0].GrokRetired || views[0].OpenAIRetired) {
					t.Fatalf("view=%+v", views[0])
				}
			}
			// Old provider responses must not recreate a consumed address as a new asset.
			if _, err := s.Reserve(ctx, platform, "duplicate", "missing-source", func(context.Context) (Mailbox, error) { return b, nil }, nil); !errors.Is(err, ErrMailboxRegistrationRetired) {
				t.Fatalf("duplicate bypass: %v", err)
			}
			// Migration repairs only confirmed account deletion, never ordinary failure.
			failed := seedMailbox(t, s, "failed")
			if err := s.EnsureRegistrationClaim(ctx, platform, "failed-job", failed); err != nil {
				t.Fatal(err)
			}
			if err := s.Release(ctx, platform, "failed-job", failed, "registration_failed"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`DELETE FROM mailbox_registration_retirements`); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := s.migrate(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.EnsureRegistrationClaim(ctx, platform, "failed-retry", failed); err != nil {
				t.Fatal(err)
			}
			if err := s.EnsureRegistrationClaim(ctx, platform, "retired-retry", b); !errors.Is(err, ErrMailboxRegistrationRetired) {
				t.Fatalf("backfill lost: %v", err)
			}
		})
	}
}
