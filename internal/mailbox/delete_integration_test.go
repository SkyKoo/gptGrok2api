package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func isolatedMailboxStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("MAILBOX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MAILBOX_TEST_DATABASE_URL is not configured")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("mailbox_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		if err != nil {
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
	store, err := Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedMailbox(t *testing.T, s *Store, name string) Mailbox {
	t.Helper()
	b, err := s.Upsert(context.Background(), Mailbox{SourceID: "source-test", SourceType: "hme", SourceAccountID: "upstream-test", SourceMailboxID: name, Email: name + "@example.test"}, "test_seed")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMailboxReleaseAndDeleteLifecycle(t *testing.T) {
	s := isolatedMailboxStore(t)
	ctx := context.Background()
	b := seedMailbox(t, s, "shared")
	for _, p := range []string{"openai", "grok"} {
		if err := s.EnsureClaim(ctx, p, p+"-job", b); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(ctx, p, p+"-job", p+"-account", b); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	remove := func(context.Context, Mailbox) error { calls++; return nil }
	if _, err := s.Delete(ctx, b.ID, "admin", remove); !errors.Is(err, ErrMailboxInUse) {
		t.Fatalf("occupied deletion = %v", err)
	}
	if calls != 0 {
		t.Fatal("occupied alias reached provider")
	}
	released, err := s.ReleaseForAccount(ctx, "openai", "openai-account", b.Email)
	if err != nil || len(released) != 1 || released[0].Status != "active" || released[0].SourceID != b.SourceID {
		t.Fatalf("release=%+v err=%v", released, err)
	}
	views, _, err := s.List(ctx, 1, 20, MailboxListFilter{})
	if err != nil || len(views) != 1 || views[0].OpenAIClaimed || !views[0].GrokClaimed {
		t.Fatalf("shared ownership=%+v %v", views, err)
	}
	if _, err := s.Delete(ctx, b.ID, "admin", remove); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal(err)
	}
	// The freed platform may reuse the same asset without creating a new alias.
	reused, err := s.Reserve(ctx, "openai", "new-job", b.SourceID, func(context.Context) (Mailbox, error) {
		t.Error("created an unnecessary alias")
		return Mailbox{}, errors.New("unexpected")
	}, nil)
	if err != nil || reused.ID != b.ID {
		t.Fatalf("reuse=%+v err=%v", reused, err)
	}
	if _, err := s.Delete(ctx, b.ID, "admin", remove); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "openai", "new-job", b, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetStatus(ctx, b.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	released, err = s.ReleaseForAccount(ctx, "grok", "grok-account", b.Email)
	if err != nil || len(released) != 1 || released[0].Status != "disabled" {
		t.Fatalf("disabled release=%+v %v", released, err)
	}
	// An explicit administrator action may delete a disabled, unclaimed asset.
	result, err := s.Delete(ctx, b.ID, "admin", remove)
	if err != nil || result.Status != "deleted" || calls != 1 {
		t.Fatalf("delete=%+v calls=%d err=%v", result, calls, err)
	}
	if _, err := s.Delete(ctx, b.ID, "admin", remove); err != nil || calls != 1 {
		t.Fatalf("idempotence calls=%d err=%v", calls, err)
	}
	b.Status = "available"
	result, err = s.Upsert(ctx, b, "stale_sync")
	if err != nil || result.Status != "deleted" {
		t.Fatalf("stale sync resurrected alias: %+v %v", result, err)
	}
	if err := s.EnsureClaim(ctx, "grok", "retry-old-job", b); !errors.Is(err, ErrMailboxStateImmutable) {
		t.Fatalf("deleted adoption=%v", err)
	}
	if err := s.Commit(ctx, "grok", "grok-job", "grok-account", b); !errors.Is(err, ErrMailboxStateImmutable) {
		t.Fatalf("deleted commit=%v", err)
	}
	events, _, err := s.Events(ctx, 1, 100, b.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range events {
		found[e.EventType] = true
	}
	for _, kind := range []string{"account_claim_released", "mailbox_delete_requested", "provider_cleanup_succeeded"} {
		if !found[kind] {
			t.Errorf("missing audit %s", kind)
		}
	}
}

func TestMailboxDeleteFailureRetryAndCancellation(t *testing.T) {
	for _, mode := range []string{"upstream_failure", "cancelled", "already_missing"} {
		t.Run(mode, func(t *testing.T) {
			s := isolatedMailboxStore(t)
			b := seedMailbox(t, s, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := s.Delete(ctx, b.ID, "admin", func(context.Context, Mailbox) error {
				if mode == "cancelled" {
					cancel()
					return context.Canceled
				}
				if mode == "already_missing" {
					return ErrProviderMailboxNotFound
				}
				return errors.New("test provider unavailable")
			})
			if mode == "already_missing" {
				if err != nil || result.Status != "deleted" {
					t.Fatalf("missing=%+v %v", result, err)
				}
				return
			}
			if err == nil || result.Status != "cleanup_pending" {
				t.Fatalf("failed delete=%+v %v", result, err)
			}
			saved, err := s.Get(context.Background(), b.ID)
			if err != nil || saved.Status != "cleanup_pending" || saved.LastError == "" {
				t.Fatalf("failure not durable=%+v %v", saved, err)
			}
			if err := s.EnsureClaim(context.Background(), "grok", "retry-job", b); !errors.Is(err, ErrMailboxStateImmutable) {
				t.Fatalf("pending adopted: %v", err)
			}
			result, err = s.Delete(context.Background(), b.ID, "admin", func(context.Context, Mailbox) error { return nil })
			if err != nil || result.Status != "deleted" {
				t.Fatalf("retry=%+v %v", result, err)
			}
		})
	}
}

func TestMailboxConcurrentDeleteAndReservation(t *testing.T) {
	s := isolatedMailboxStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b := seedMailbox(t, s, "concurrent")
	entered, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := s.Delete(ctx, b.ID, "admin", func(context.Context, Mailbox) error {
			calls.Add(1)
			close(entered)
			select {
			case <-finish:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("delete did not start")
	}
	defer func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	}()
	if _, err := s.Delete(ctx, b.ID, "admin", func(context.Context, Mailbox) error { calls.Add(1); return nil }); !errors.Is(err, ErrMailboxBusy) {
		t.Fatalf("concurrent delete=%v", err)
	}
	// SKIP LOCKED allocation must not use the mailbox being deleted.
	allocated, err := s.Reserve(ctx, "grok", "parallel-job", b.SourceID, func(context.Context) (Mailbox, error) {
		return Mailbox{SourceID: b.SourceID, SourceType: "hme", SourceAccountID: b.SourceAccountID, SourceMailboxID: "fresh", Email: "fresh@example.test"}, nil
	}, nil)
	if err != nil || allocated.ID == b.ID {
		t.Fatalf("reserved deleting alias: %+v %v", allocated, err)
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d", calls.Load())
	}
}
