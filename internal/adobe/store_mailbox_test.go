package adobe

import (
	"testing"
	"time"
)

func TestMailboxLinkSerializesDeleteCutoff(t *testing.T) {
	store, account := readyStore(t)
	attachStarted := make(chan struct{})
	allowAttach := make(chan struct{})
	linkDone := make(chan error, 1)
	var guardTime time.Time
	go func() {
		_, err := store.LinkMailbox(account.ID, func(current Account) (map[string]any, bool, error) {
			close(attachStarted)
			<-allowAttach
			guardTime = time.Now()
			return map[string]any{"email": current.Email, "id": "asset"}, true, nil
		})
		linkDone <- err
	}()
	<-attachStarted
	type deletion struct {
		accounts []Account
		cutoff   time.Time
		err      error
	}
	deleteStarted := make(chan struct{})
	deleteDone := make(chan deletion, 1)
	go func() {
		close(deleteStarted)
		items, cutoff, err := store.DeleteWithCutoff([]string{account.ID})
		deleteDone <- deletion{items, cutoff, err}
	}()
	<-deleteStarted
	select {
	case <-deleteDone:
		t.Fatal("deletion passed an in-progress association")
	case <-time.After(10 * time.Millisecond):
	}
	close(allowAttach)
	if err := <-linkDone; err != nil {
		t.Fatal(err)
	}
	result := <-deleteDone
	if result.err != nil || len(result.accounts) != 1 {
		t.Fatalf("delete err=%v count=%d", result.err, len(result.accounts))
	}
	if result.cutoff.Before(guardTime) {
		t.Fatal("cleanup cutoff predates confirmed association")
	}
	if result.accounts[0].RegistrationMailbox["id"] != "asset" {
		t.Fatal("delete lost linked metadata")
	}
}
