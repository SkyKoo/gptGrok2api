package register

import (
	"context"
	"testing"
	"time"
)

type coordinatedMailSourceStub struct {
	acquired int
	prepared int
}

func (s *coordinatedMailSourceStub) Acquire(context.Context, string) (Mailbox, error) {
	s.acquired++
	return Mailbox{ID: "mbx_canonical", SourceMailboxID: "alias-1", Email: "alias@example.test", AccountID: "icloud"}, nil
}
func (s *coordinatedMailSourceStub) Prepare(context.Context, Mailbox) error { s.prepared++; return nil }
func (s *coordinatedMailSourceStub) WaitCode(context.Context, Mailbox, time.Time) (string, error) {
	return "123456", nil
}
func (s *coordinatedMailSourceStub) Close() {}

type mailboxCoordinatorStub struct {
	reserved, ensured, committed, released int
}

func (c *mailboxCoordinatorStub) Reserve(ctx context.Context, _ string, _ string, acquire func(context.Context) (Mailbox, error), prepare func(context.Context, Mailbox) error) (Mailbox, error) {
	c.reserved++
	box, err := acquire(ctx)
	if err != nil {
		return Mailbox{}, err
	}
	if prepare != nil {
		if err := prepare(ctx, box); err != nil {
			return Mailbox{}, err
		}
	}
	return box, nil
}
func (c *mailboxCoordinatorStub) EnsureClaim(context.Context, string, string, Mailbox) error {
	c.ensured++
	return nil
}
func (c *mailboxCoordinatorStub) Commit(context.Context, string, string, string, Mailbox) error {
	c.committed++
	return nil
}
func (c *mailboxCoordinatorStub) Release(context.Context, string, string, Mailbox, string) error {
	c.released++
	return nil
}

func TestCoordinatedMailSourceKeepsProviderAndClaimLifecycleTogether(t *testing.T) {
	source := &coordinatedMailSourceStub{}
	coordinator := &mailboxCoordinatorStub{}
	mail := NewCoordinatedMailSource("openai", source, coordinator)
	box, err := mail.Acquire(context.Background(), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if box.ID != "mbx_canonical" || box.SourceMailboxID != "alias-1" {
		t.Fatalf("provider identity was not preserved: %#v", box)
	}
	if source.acquired != 1 || source.prepared != 1 || coordinator.reserved != 1 {
		t.Fatalf("unexpected reserve calls: source=%+v coordinator=%+v", source, coordinator)
	}
	if err := mail.PrepareFor(context.Background(), "job-1", box); err != nil {
		t.Fatal(err)
	}
	if err := mail.Commit(context.Background(), "account-1", box); err != nil {
		t.Fatal(err)
	}
	if err := mail.Release(context.Background(), box, "registration_failed"); err != nil {
		t.Fatal(err)
	}
	if coordinator.ensured != 1 || coordinator.committed != 1 || coordinator.released != 1 || source.prepared != 2 {
		t.Fatalf("unexpected lifecycle calls: source=%+v coordinator=%+v", source, coordinator)
	}
}
