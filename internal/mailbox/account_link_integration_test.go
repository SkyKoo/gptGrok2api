package mailbox

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestManualMailboxLinkConcurrentOwners(t *testing.T) {
	s := isolatedMailboxStore(t)
	box := seedMailbox(t, s, "concurrent-link")
	start := make(chan struct{})
	outcomes := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"adobe:one", "adobe:two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			_, _, err := s.LinkExistingAccount(context.Background(), "adobe", id, "", box.Email, nil)
			outcomes <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	linked, conflicts := 0, 0
	for err := range outcomes {
		if err == nil {
			linked++
		} else if errors.Is(err, ErrMailboxInUse) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if linked != 1 || conflicts != 1 {
		t.Fatalf("linked=%d conflicts=%d", linked, conflicts)
	}
	var claims int
	if err := s.db.QueryRow(`SELECT count(*) FROM mailbox_claims WHERE mailbox_id=$1 AND platform='adobe' AND status='active'`, box.ID).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("claims=%d err=%v", claims, err)
	}
}
