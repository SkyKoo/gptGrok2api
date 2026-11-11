package adobe

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReloginJournalPersistsDeduplicatesAndRecoversWithoutReplay(t *testing.T) {
	store, a := readyStore(t)
	path := filepath.Join(t.TempDir(), "relogin.enc.json")
	jobs := NewReloginJobs(path, "fixture-journal-key")
	if err := jobs.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	initial, err := jobs.Start("request-a", []string{a.ID}, store)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := jobs.Start("request-a", []string{a.ID}, store)
	if err != nil || duplicate[0].ProgressID != initial[0].ProgressID {
		t.Fatal("request replay was not idempotent")
	}
	joined, err := jobs.Start("request-b", []string{a.ID}, store)
	if err != nil || joined[0].ProgressID != initial[0].ProgressID {
		t.Fatal("active account was not deduplicated")
	}
	if _, err = jobs.Start("request-a", []string{"other"}, store); SafeError(err) != "adobe_relogin_request_conflict" {
		t.Fatal("reused request identity accepted")
	}
	running, ok, err := jobs.Next()
	if err != nil || !ok || running.Stage != "running" {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, secret := range []string{a.Email, a.ID, a.Token, a.Cookie} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("plaintext account material in journal")
		}
	}
	// A standby GET must not alter the current writer's journal.
	standby := NewReloginJobs(path, "fixture-journal-key")
	visible, err := standby.List("request-b")
	if err != nil || len(visible) != 1 || visible[0].Done {
		t.Fatal("standby read altered task")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(raw, after) {
		t.Fatal("read-only listing wrote the journal")
	}
	// Only the new data-lease owner performs restart recovery, without callbacks.
	if err = standby.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	visible, _ = standby.List("request-a")
	if !visible[0].Done || visible[0].Stage != "interrupted" || !visible[0].ResultUnknown || visible[0].SessionUpdated {
		t.Fatal("interrupted login falsely claimed failure or success")
	}
	if _, ok, err = standby.Next(); err != nil || ok {
		t.Fatal("restart replayed pending login")
	}
	replay, err := standby.Start("request-a", []string{a.ID}, store)
	if err != nil || replay[0].ProgressID != running.ProgressID || !replay[0].Done {
		t.Fatal("old POST replay restarted interrupted login")
	}
}

func TestQueuedReloginDoesNotOverwriteReimportedCredentials(t *testing.T) {
	store, a := readyStore(t)
	jobs := NewReloginJobs(filepath.Join(t.TempDir(), "jobs.enc.json"), "key")
	if err := jobs.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Start("queued-request", []string{a.ID}, store); err != nil {
		t.Fatal(err)
	}
	newer := a
	newer.Cookie += "; newerSession=yes"
	if _, err := store.Import([]Account{newer}); err != nil {
		t.Fatal(err)
	}
	store.SetBrowserRefresher(func(context.Context, Account) (Account, error) {
		t.Fatal("stale queued revision reached browser")
		return Account{}, nil
	})
	task, ok, err := jobs.Next()
	if err != nil || !ok {
		t.Fatal(err)
	}
	account, failure := store.ReloginRevision(context.Background(), task.AccountRef, task.CredentialRevision)
	if failure != ErrRevision {
		t.Fatal("queued stale revision accepted")
	}
	if err = jobs.Finish(task.ProgressID, account, failure); err != nil {
		t.Fatal(err)
	}
	saved, _ := store.Get(a.ID)
	if saved.Cookie != newer.Cookie {
		t.Fatal("queued task overwrote new credentials")
	}
	final, _ := jobs.List("queued-request")
	if final[0].SessionUpdated || final[0].Error != ErrRevision.Error() {
		t.Fatal("stale task reported success")
	}
}
