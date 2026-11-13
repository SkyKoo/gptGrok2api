package adobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMailboxGuardProtectsEveryStoredAccountAndUnreadableStore(t *testing.T) {
	s, a := readyStore(t)
	if err := s.SetEnabled([]string{a.ID}, false); err != nil {
		t.Fatal(err)
	}
	called := false
	ran, err := s.WithoutEmailAccount(context.Background(), " "+strings.ToUpper(a.Email)+" ", func() error { called = true; return nil })
	if err != nil || ran || called {
		t.Fatal(ran, err, called)
	}
	if err := os.WriteFile(s.path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	ran, err = s.WithoutEmailAccount(context.Background(), "other@example.test", func() error { called = true; return nil })
	if err == nil || ran || called {
		t.Fatal("unreadable store allowed cleanup")
	}
}

func TestMailboxGuardSerializesManualImport(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "accounts.enc"), "test")
	account := fixtureAccount(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := s.WithoutEmailAccount(context.Background(), account.Email, func() error { close(entered); <-release; return nil })
		done <- err
	}()
	<-entered
	imported := make(chan error, 1)
	go func() { _, err := s.Import([]Account{account}); imported <- err }()
	select {
	case err := <-imported:
		t.Fatal("import crossed guard", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-imported; err != nil {
		t.Fatal(err)
	}
	ran, err := s.WithoutEmailAccount(context.Background(), account.Email, func() error { t.Fatal("late cleanup ignored imported account"); return nil })
	if err != nil || ran {
		t.Fatal(ran, err)
	}
}

func TestRegistrationImportProtectsExternalOwnerAndKeepsSavedCredentials(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "accounts.enc"), "test")
	account := fixtureAccount(t)
	account.RegistrationJobID = "root"
	commits := 0
	if err := s.ImportRegistration(context.Background(), account, func() error { commits++; return nil }); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Get("adobe:" + account.AccountID)
	if err := s.ImportRegistration(context.Background(), account, func() error { commits++; return nil }); err != nil {
		t.Fatal(err)
	}
	second, _ := s.Get(first.ID)
	if first.Revision != second.Revision || commits != 2 {
		t.Fatal("recovery replaced saved credentials")
	}
	account.RegistrationJobID = "other-root"
	if err := s.ImportRegistration(context.Background(), account, func() error { t.Fatal("committed other owner"); return nil }); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
}

func TestRegistrationImportRejectsExistingIDUnderDifferentEmail(t *testing.T) {
	s, a := readyStore(t)
	a.Email = "different@example.test"
	a.RegistrationJobID = "new-root"
	if err := s.ImportRegistration(context.Background(), a, func() error { t.Fatal("committed mismatched stored ID"); return nil }); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
}

func TestRegistrationImportValidatesBeforeClaimAndPreservesCommitOnDiskFailure(t *testing.T) {
	for _, kind := range []string{"token", "identity", "media"} {
		t.Run(kind, func(t *testing.T) {
			s := NewStore(filepath.Join(t.TempDir(), "accounts.enc"), "test")
			a := fixtureAccount(t)
			a.RegistrationJobID = "root"
			switch kind {
			case "token":
				a.Token = "invalid"
			case "identity":
				a.AccountID = "other"
			case "media":
				a.MediaSessionARPMode = "present"
				a.MediaSessionARPSource = "spl_runtime"
				a.Cookie = "sherlockToken="
			}
			if err := s.ImportRegistration(context.Background(), a, func() error { t.Fatal("invalid payload committed claim"); return nil }); err == nil {
				t.Fatal("invalid payload accepted")
			}
		})
	}
	s := NewStore(filepath.Join(t.TempDir(), "accounts.enc"), "test")
	a := fixtureAccount(t)
	a.RegistrationJobID = "root"
	committed := false
	err := s.ImportRegistration(context.Background(), a, func() error { committed = true; return os.Mkdir(s.path, 0700) })
	if err == nil || !committed {
		t.Fatal("expected durable account write failure after local commit")
	}
	if err = os.Remove(s.path); err != nil {
		t.Fatal(err)
	}
	if err = s.ImportRegistration(context.Background(), a, func() error {
		if !committed {
			t.Fatal("lost committed owner")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
