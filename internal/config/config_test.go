package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUsesProjectVersionFile(t *testing.T) {
	t.Setenv("GO_VERSION", "")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("test-version\n"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Version != "test-version" {
		t.Fatalf("Load() version = %q, want project VERSION", cfg.Version)
	}
}

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
