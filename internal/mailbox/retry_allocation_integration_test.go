package mailbox

import (
	"context"
	"testing"
)

func TestReleasedFailedMailboxExclusionDoesNotChangeAssetOrOtherChannel(t *testing.T) {
	s := isolatedMailboxStore(t)
	ctx := context.Background()
	first := seedMailbox(t, s, "retry-first")
	second := seedMailbox(t, s, "retry-second")
	if _, err := s.db.ExecContext(ctx, `UPDATE mailboxes SET created_at=now()-interval '1 day' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "grok", "old", first); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "grok", "old", first, "registration_failed_without_account"); err != nil {
		t.Fatal(err)
	}
	neverCreate := func(context.Context) (Mailbox, error) {
		t.Fatal("existing eligible mailbox skipped")
		return Mailbox{}, nil
	}
	got, err := s.ReserveExcluding(ctx, "grok", "fresh", first.SourceID, []string{first.Email}, neverCreate, nil)
	if err != nil || got.ID != second.ID {
		t.Fatal(got, err)
	}
	if original, err := s.Get(ctx, first.ID); err != nil || original.Status != "available" {
		t.Fatal("exclusion changed asset status", original, err)
	}
	got, err = s.Reserve(ctx, "openai", "gpt", first.SourceID, neverCreate, nil)
	if err != nil || got.ID != first.ID {
		t.Fatal("Grok scheduling affected GPT", got, err)
	}
	// Saved-task retry still reacquires its original mailbox; scheduling exclusion
	// only belongs to new-root allocation, not the existing ownership checks.
	if err := s.EnsureRegistrationClaim(ctx, "grok", "old", first); err != nil {
		t.Fatal(err)
	}
}

func TestExcludedProviderAliasCannotCreateNewRegistrationClaim(t *testing.T) {
	s := isolatedMailboxStore(t)
	ctx := context.Background()
	old := seedMailbox(t, s, "provider-duplicate")
	_, err := s.ReserveExcluding(ctx, "grok", "new", old.SourceID, []string{old.Email}, func(context.Context) (Mailbox, error) { return old, nil }, nil)
	if err == nil {
		t.Fatal("provider returned excluded alias")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_claims WHERE registration_job_id='new'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
