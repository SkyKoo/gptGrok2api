package config

import "testing"

func TestMailboxDatabaseURLDoesNotReuseSQLiteDatabaseURL(t *testing.T) {
	t.Setenv("GO_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "sqlite:////app/data/accounts.db")
	if got := mailboxDatabaseURL(); got != "" {
		t.Fatalf("SQLite account database was reused for mailbox assets: %q", got)
	}
	t.Setenv("DATABASE_URL", "postgresql://user:pass@db/cfm")
	if got := mailboxDatabaseURL(); got != "postgresql://user:pass@db/cfm" {
		t.Fatalf("PostgreSQL legacy URL was not accepted: %q", got)
	}
	t.Setenv("GO_DATABASE_URL", "postgresql://user:pass@db/mailbox")
	if got := mailboxDatabaseURL(); got != "postgresql://user:pass@db/mailbox" {
		t.Fatalf("GO_DATABASE_URL did not take precedence: %q", got)
	}
}
