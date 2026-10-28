package mailbox

import (
	"context"
	"testing"
	"time"
)

type managerProviderStub struct {
	box      Mailbox
	acquires int
	prepares int
	waits    int
	deletes  int
	closed   bool
}

func (p *managerProviderStub) Acquire(context.Context, string) (Mailbox, error) {
	p.acquires++
	return p.box, nil
}

func (p *managerProviderStub) Prepare(context.Context, Mailbox) error {
	p.prepares++
	return nil
}

func (p *managerProviderStub) WaitCode(context.Context, Mailbox, time.Time, func(string) bool) (string, error) {
	p.waits++
	return "230250", nil
}

func (p *managerProviderStub) Delete(context.Context, Mailbox) error {
	p.deletes++
	return nil
}

func (p *managerProviderStub) List(context.Context) ([]Mailbox, error) {
	return []Mailbox{p.box}, nil
}

func (p *managerProviderStub) Close() { p.closed = true }

func TestResourceManagerRoutesLegacySourceThroughProviderBoundary(t *testing.T) {
	provider := &managerProviderStub{box: Mailbox{ID: "alias-1", Email: "alias@example.test", SourceType: "icloud_hme", SourceAccountID: "hme-account", SourceMailboxID: "alias-1"}}
	manager := NewResourceManager(nil, func(source Source, _ time.Duration) (Provider, error) {
		if source.ID != "hme-primary" {
			t.Fatalf("unexpected source: %+v", source)
		}
		return provider, nil
	})
	manager.SetSourceLoader(func(context.Context) ([]Source, error) {
		return []Source{{ID: "hme-primary", Type: "icloud_hme", Enabled: true, Config: map[string]any{"account_id": "hme-account"}}}, nil
	})
	if !manager.Enabled() || !manager.HasEnabledSourceType(context.Background(), "icloud_hme") {
		t.Fatal("legacy source loader did not enable the resource manager")
	}

	session, err := manager.Open(context.Background(), "hme-primary", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	box, err := session.Acquire(context.Background(), "grok", "job-1")
	if err != nil || box.Email != provider.box.Email {
		t.Fatalf("acquire did not route through provider: box=%+v err=%v", box, err)
	}
	if err := session.Prepare(context.Background(), box); err != nil {
		t.Fatal(err)
	}
	if code, err := session.WaitCode(context.Background(), box, time.Now(), nil); err != nil || code != "230250" {
		t.Fatalf("wait code = %q, err=%v", code, err)
	}
	if err := session.Delete(context.Background(), box); err != nil {
		t.Fatal(err)
	}
	if provider.acquires != 1 || provider.prepares != 1 || provider.waits != 1 || provider.deletes != 1 {
		t.Fatalf("unexpected provider calls: %+v", provider)
	}
	session.Close()
	if !provider.closed {
		t.Fatal("provider session was not closed")
	}
}
