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
	sourceID                               string
}

func (c *mailboxCoordinatorStub) Reserve(ctx context.Context, _ string, _ string, sourceID string, acquire func(context.Context) (Mailbox, error), prepare func(context.Context, Mailbox) error) (Mailbox, error) {
	c.reserved++
	c.sourceID = sourceID
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

type existingMailboxCoordinatorStub struct {
	box         Mailbox
	reserved    int
	acquireCall int
	prepareCall int
}

func (c *existingMailboxCoordinatorStub) Reserve(ctx context.Context, _ string, _ string, _ string, acquire func(context.Context) (Mailbox, error), prepare func(context.Context, Mailbox) error) (Mailbox, error) {
	c.reserved++
	if c.box.ID != "" {
		if prepare != nil {
			c.prepareCall++
			if err := prepare(ctx, c.box); err != nil {
				return Mailbox{}, err
			}
		}
		return c.box, nil
	}
	c.acquireCall++
	return acquire(ctx)
}

func (c *existingMailboxCoordinatorStub) EnsureClaim(context.Context, string, string, Mailbox) error {
	return nil
}
func (c *existingMailboxCoordinatorStub) Commit(context.Context, string, string, string, Mailbox) error {
	return nil
}
func (c *existingMailboxCoordinatorStub) Release(context.Context, string, string, Mailbox, string) error {
	return nil
}

func TestCoordinatedMailSourceKeepsProviderAndClaimLifecycleTogether(t *testing.T) {
	source := &coordinatedMailSourceStub{}
	coordinator := &mailboxCoordinatorStub{}
	mail := NewCoordinatedMailSource("openai", "hme-main", source, coordinator)
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
	if coordinator.sourceID != "hme-main" {
		t.Fatalf("mailbox source was not passed to coordinator: %q", coordinator.sourceID)
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

func TestCoordinatedMailSourceReusesExistingAssetWithoutCreatingAlias(t *testing.T) {
	source := &coordinatedMailSourceStub{}
	coordinator := &existingMailboxCoordinatorStub{box: Mailbox{
		ID:              "mbx_shared",
		SourceMailboxID: "alias-shared",
		Email:           "shared@example.test",
		AccountID:       "icloud",
		SourceType:      "hme",
	}}
	mail := NewCoordinatedMailSource("grok", "hme-main", source, coordinator)

	box, err := mail.Acquire(context.Background(), "grok-job-1")
	if err != nil {
		t.Fatal(err)
	}
	if box.Email != "shared@example.test" || box.SourceMailboxID != "alias-shared" {
		t.Fatalf("unexpected reused mailbox: %#v", box)
	}
	if coordinator.reserved != 1 || coordinator.prepareCall != 1 || coordinator.acquireCall != 0 {
		t.Fatalf("existing asset must be prepared without provider acquire: coordinator=%+v", coordinator)
	}
	if source.acquired != 0 || source.prepared != 1 {
		t.Fatalf("provider alias creation must not run for a reused asset: source=%+v", source)
	}
}
