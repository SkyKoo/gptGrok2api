package mailbox

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This test is opt-in because the normal unit-test suite must not require a
// PostgreSQL service. Run it with MAILBOX_TEST_DATABASE_URL pointing at an
// isolated test database.
func TestDisabledMailboxLifecycle(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("MAILBOX_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("MAILBOX_TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prefix := "it_disabled_"
	if _, err := store.db.ExecContext(ctx, `DELETE FROM mailboxes WHERE id LIKE $1`, prefix+"%"); err != nil {
		t.Fatal(err)
	}
	defer store.db.ExecContext(context.Background(), `DELETE FROM mailboxes WHERE id LIKE $1`, prefix+"%")

	first := Mailbox{ID: prefix + "first", SourceID: "hme-test", SourceType: "icloud_hme", SourceAccountID: "acc-test", SourceMailboxID: "alias-first", Email: prefix + "first@example.test", Status: "disabled"}
	second := Mailbox{ID: prefix + "second", SourceID: "hme-test", SourceType: "icloud_hme", SourceAccountID: "acc-test", SourceMailboxID: "alias-second", Email: prefix + "second@example.test", Status: "disabled"}
	if first, err = store.Upsert(ctx, first, "integration_seed"); err != nil {
		t.Fatal(err)
	}
	if second, err = store.Upsert(ctx, second, "integration_seed"); err != nil {
		t.Fatal(err)
	}
	// Reconciliation must never turn an explicitly disabled asset back on.
	first.Status = "available"
	updated, err := store.Upsert(ctx, first, "integration_reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "disabled" {
		t.Fatalf("reconcile re-enabled disabled mailbox: %q", updated.Status)
	}

	if _, err := store.SetStatus(ctx, first.ID, "available"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetStatus(ctx, first.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureClaim(ctx, "openai", prefix+"job", first); err != nil {
		t.Fatal(err)
	}
	views, total, err := store.List(ctx, 1, 20, MailboxListFilter{Status: "disabled", SourceID: "hme-test", Platform: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(views) != 1 || views[0].ID != first.ID || !views[0].OpenAIClaimed {
		t.Fatalf("disabled/platform filter mismatch: total=%d views=%+v", total, views)
	}
	if err := store.Release(ctx, "openai", prefix+"job", first, "integration_release"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM mailboxes WHERE id=$1`, first.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" {
		t.Fatalf("released disabled mailbox changed status: %q", status)
	}

	var acquireCalls atomic.Int32
	created, err := store.Reserve(ctx, "grok", prefix+"grok-job", "hme-test", func(context.Context) (Mailbox, error) {
		acquireCalls.Add(1)
		return Mailbox{ID: prefix + "created", SourceID: "hme-test", SourceType: "icloud_hme", SourceAccountID: "acc-test", SourceMailboxID: "alias-created", Email: prefix + "created@example.test"}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if acquireCalls.Load() != 1 || created.Email != prefix+"created@example.test" {
		t.Fatalf("disabled assets were reused instead of acquiring a new alias: calls=%d created=%+v", acquireCalls.Load(), created)
	}
}
