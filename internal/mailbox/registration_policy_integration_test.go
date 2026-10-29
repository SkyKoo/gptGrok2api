package mailbox

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestChannelDisabledMailboxAllocationAndCompensation(t *testing.T) {
	ctx := context.Background()
	s := isolatedMailboxStore(t)
	first := seedMailbox(t, s, "oldest-channel")
	second := seedMailbox(t, s, "newer-channel")
	if _, err := s.SetPlatformStatus(ctx, first.ID, "openai", "disabled"); err != nil {
		t.Fatal(err)
	}
	// GPT skips the oldest asset; Grok can still use it even when GPT claims it.
	if err := s.EnsureClaim(ctx, "openai", "historical-gpt", first); err != nil {
		t.Fatal(err)
	}
	calls := 0
	acquire := func(context.Context) (Mailbox, error) {
		calls++
		return Mailbox{SourceID: "source-test", SourceType: "hme", SourceMailboxID: "new-channel", Email: "new-channel@example.test"}, nil
	}
	prepareCalls := 0
	prepare := func(context.Context, Mailbox) error { prepareCalls++; return nil }
	gpt, err := s.Reserve(ctx, "openai", "gpt-new", "source-test", acquire, prepare)
	if err != nil || gpt.ID != second.ID {
		t.Fatalf("GPT allocation=%+v err=%v", gpt, err)
	}
	grok, err := s.Reserve(ctx, "grok", "grok-new", "source-test", acquire, prepare)
	if err != nil || grok.ID != first.ID || calls != 0 {
		t.Fatalf("Grok reuse=%+v err=%v created=%d", grok, err, calls)
	}
	// Sync, claim commit and release must not erase the channel restriction.
	if _, err := s.Upsert(ctx, first, "sync_test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, "grok", "grok-new", "grok-account", first); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "openai", "historical-gpt", first, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureRegistrationClaim(ctx, "openai", "gpt-compensation", first); !errors.Is(err, ErrMailboxRegistrationDisabled) {
		t.Fatalf("GPT compensation bypassed policy: %v", err)
	}
	// A newly disabled channel blocks retry of its existing reserved/active job.
	if _, err := s.SetPlatformStatus(ctx, first.ID, "grok", "disabled"); err != nil {
		t.Fatal(err)
	}
	before := prepareCalls
	if _, err := s.Reserve(ctx, "grok", "grok-new", "source-test", acquire, prepare); !errors.Is(err, ErrMailboxRegistrationDisabled) {
		t.Fatalf("saved job bypassed policy: %v", err)
	}
	if prepareCalls != before || calls != 0 {
		t.Fatal("blocked retry touched provider")
	}
	if _, err := s.SetPlatformStatus(ctx, second.ID, "grok", "disabled"); err != nil {
		t.Fatal(err)
	}
	created, err := s.Reserve(ctx, "grok", "grok-create", "source-test", acquire, prepare)
	if err != nil || calls != 1 || created.Email != "new-channel@example.test" {
		t.Fatalf("fallback=%+v calls=%d err=%v", created, calls, err)
	}
	// An upstream returning an already disabled alias cannot bypass the policy.
	if _, err := s.SetPlatformStatus(ctx, created.ID, "openai", "disabled"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Reserve(ctx, "openai", "gpt-provider-duplicate", "source-test", func(context.Context) (Mailbox, error) { return first, nil }, nil)
	if !errors.Is(err, ErrMailboxRegistrationDisabled) {
		t.Fatalf("duplicate provider alias bypassed policy: %v", err)
	}
}

func TestChannelDisabledLegacyCompatibilityAndLogs(t *testing.T) {
	ctx := context.Background()
	s := isolatedMailboxStore(t)
	b := seedMailbox(t, s, "legacy-channel")
	// Simulate an old database row with no new policy entries.
	if _, err := s.db.ExecContext(ctx, `UPDATE mailboxes SET status='disabled' WHERE id=$1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertPolicy := func(gpt, grok bool) {
		t.Helper()
		views, total, err := s.List(ctx, 1, 20, MailboxListFilter{})
		if err != nil || total != 1 || len(views) != 1 || views[0].OpenAIDisabled != gpt || views[0].GrokDisabled != grok {
			t.Fatalf("policy gpt=%v grok=%v views=%+v err=%v", gpt, grok, views, err)
		}
	}
	assertPolicy(true, true)
	if _, err := s.SetPlatformStatus(ctx, b.ID, "grok", "available"); err != nil {
		t.Fatal(err)
	}
	assertPolicy(true, false)
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, b, "sync_test"); err != nil {
		t.Fatal(err)
	}
	assertPolicy(true, false)
	for _, tc := range []struct {
		filter string
		total  int
	}{{"disabled", 1}, {"disabled_openai", 1}, {"disabled_grok", 0}} {
		_, total, err := s.List(ctx, 1, 20, MailboxListFilter{Status: tc.filter})
		if err != nil || total != tc.total {
			t.Fatalf("filter %s=%d err=%v", tc.filter, total, err)
		}
	}
	if _, err := s.SetPlatformStatus(ctx, b.ID, "unknown", "disabled"); !errors.Is(err, ErrInvalidMailboxPlatform) {
		t.Fatalf("invalid platform: %v", err)
	}
	assertPolicy(true, false)
	if _, err := s.SetPlatformStatus(ctx, b.ID, "openai", "available"); err != nil {
		t.Fatal(err)
	}
	assertPolicy(false, false)
	// Old API clients still apply their action to both channels.
	if _, err := s.SetStatus(ctx, b.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	assertPolicy(true, true)
	if _, err := s.SetStatus(ctx, b.ID, "available"); err != nil {
		t.Fatal(err)
	}
	assertPolicy(false, false)
	events, _, err := s.Events(ctx, 1, 50, b.ID, "grok")
	if err != nil || len(events) != 1 || events[0].EventType != "mailbox_enabled" || events[0].Actor != "admin" {
		t.Fatalf("channel audit=%+v err=%v", events, err)
	}
}

func TestChannelDisabledConcurrentUpdatesAndCleanup(t *testing.T) {
	ctx := context.Background()
	s := isolatedMailboxStore(t)
	b := seedMailbox(t, s, "parallel-channel")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, platform := range []string{"openai", "grok"} {
		wg.Add(1)
		go func(p string) { defer wg.Done(); _, err := s.SetPlatformStatus(ctx, b.ID, p, "disabled"); errs <- err }(platform)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	views, _, err := s.List(ctx, 1, 20, MailboxListFilter{})
	if err != nil || len(views) != 1 || !views[0].OpenAIDisabled || !views[0].GrokDisabled {
		t.Fatalf("concurrent policy=%+v err=%v", views, err)
	}
	if _, err := s.SetPlatformStatus(ctx, b.ID, "grok", "available"); err != nil {
		t.Fatal(err)
	}
	removed := 0
	remove := func(context.Context, Mailbox) error { removed++; return nil }
	if _, err := s.Delete(ctx, b.ID, "account_cleanup", remove); !errors.Is(err, ErrMailboxRegistrationDisabled) || removed != 0 {
		t.Fatalf("auto cleanup deleted blocked asset: %v calls=%d", err, removed)
	}
	if _, err := s.Delete(ctx, b.ID, "admin", remove); err != nil || removed != 1 {
		t.Fatalf("explicit cleanup=%v calls=%d", err, removed)
	}
	if _, err := s.SetPlatformStatus(ctx, b.ID, "openai", "available"); !errors.Is(err, ErrMailboxStateImmutable) {
		t.Fatalf("deleted policy modified: %v", err)
	}
}
