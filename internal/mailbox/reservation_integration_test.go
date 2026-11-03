package mailbox

import (
	"context"
	"errors"
	"testing"
)

func TestRegistrationReservationIsNotAccountOccupancy(t *testing.T) {
	s := isolatedMailboxStore(t)
	ctx := context.Background()
	box := seedMailbox(t, s, "reservation")
	if err := s.EnsureRegistrationClaim(ctx, "grok", "signup", box); err != nil {
		t.Fatal(err)
	}
	assertView := func(claimed, reserved bool) {
		t.Helper()
		v, _, err := s.List(ctx, 1, 20, MailboxListFilter{})
		if err != nil || len(v) != 1 || v[0].GrokClaimed != claimed || v[0].GrokReserved != reserved {
			t.Fatalf("view=%+v err=%v", v, err)
		}
	}
	assertView(false, true)
	counts, err := s.Counts(ctx)
	if err != nil || counts["active"] != 0 || counts["reserved"] != 1 || counts["available"] != 0 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
	for status, want := range map[string]int{"active": 0, "reserved": 1, "available": 0} {
		v, n, err := s.List(ctx, 1, 20, MailboxListFilter{Status: status})
		if err != nil || n != want || n > 0 && v[0].Status != "reserved" {
			t.Fatalf("status=%s total=%d view=%+v err=%v", status, n, v, err)
		}
	}
	for filter, want := range map[string]int{"grok": 0, "reserved": 1, "unclaimed": 1} {
		_, n, err := s.List(ctx, 1, 20, MailboxListFilter{Platform: filter})
		if err != nil || n != want {
			t.Fatalf("filter=%s total=%d err=%v", filter, n, err)
		}
	}
	if _, err := s.Delete(ctx, box.ID, "admin", func(context.Context, Mailbox) error { t.Fatal("deleted reserved mailbox"); return nil }); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal(err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "grok", "other", box); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal("stole reservation", err)
	}
	if err := s.Release(ctx, "grok", "other", box, "old failure"); err != nil {
		t.Fatal(err)
	}
	assertView(false, true)
	if err := s.Commit(ctx, "grok", "signup", "account", box); err != nil {
		t.Fatal(err)
	}
	assertView(true, false)
	if err := s.Release(ctx, "grok", "signup", box, "late failure"); err != nil {
		t.Fatal(err)
	}
	assertView(true, false)
	if err := s.EnsureRegistrationClaim(ctx, "grok", "signup", box); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal("replayed registered mailbox", err)
	}
}

func TestFailedReservationReleasePreservesOtherChannelAndRetirement(t *testing.T) {
	s := isolatedMailboxStore(t)
	ctx := context.Background()
	box := seedMailbox(t, s, "released")
	if err := s.EnsureClaim(ctx, "openai", "gpt-job", box); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, "openai", "gpt-job", "gpt-account", box); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "grok", "failed-job", box); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "grok", "failed-job", box, "registration_failed_without_account"); err != nil {
		t.Fatal(err)
	}
	v, _, err := s.List(ctx, 1, 20, MailboxListFilter{})
	if err != nil || len(v) != 1 || !v[0].OpenAIClaimed || v[0].GrokClaimed || v[0].GrokReserved || v[0].GrokRetired {
		t.Fatalf("view=%+v err=%v", v, err)
	}
	if _, err := s.ReleaseForAccount(ctx, "openai", "gpt-account", box.Email); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "grok", "new-job", box); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "grok", "failed-job", box, "stale cleanup"); err != nil {
		t.Fatal(err)
	}
	v, _, err = s.List(ctx, 1, 20, MailboxListFilter{})
	if err != nil || len(v) != 1 || !v[0].OpenAIRetired || !v[0].GrokReserved {
		t.Fatalf("view=%+v err=%v", v, err)
	}
	if err := s.Release(ctx, "grok", "new-job", box, "failed"); err != nil {
		t.Fatal(err)
	}
	v, _, err = s.List(ctx, 1, 20, MailboxListFilter{})
	if err != nil || len(v) != 1 || v[0].GrokReserved || v[0].Status != "available" || !v[0].OpenAIRetired {
		t.Fatalf("view=%+v err=%v", v, err)
	}
}

func TestRetryReclaimsReleasedMailboxWithoutStealingAnotherTask(t *testing.T) {
	s := isolatedMailboxStore(t)
	ctx := context.Background()
	box := seedMailbox(t, s, "retry-reclaim")
	if err := s.EnsureRegistrationClaim(ctx, "grok", "old-root", box); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "grok", "old-root", box, "registration_failed_without_account"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "grok", "old-root", box); err != nil {
		t.Fatal("retry could not reacquire free mailbox", err)
	}
	if err := s.Release(ctx, "grok", "old-root", box, "registration_failed_without_account"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "grok", "new-root", box); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "grok", "old-root", box); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal("retry stole new task reservation", err)
	}
	if err := s.Release(ctx, "grok", "old-root", box, "stale cleanup"); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.List(ctx, 1, 20, MailboxListFilter{})
	if err != nil || len(rows) != 1 || !rows[0].GrokReserved {
		t.Fatal("stale cleanup released another task", rows, err)
	}
	if err := s.Commit(ctx, "grok", "new-root", "new-account", box); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "grok", "old-root", box); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal("retry reused successful account", err)
	}
}
