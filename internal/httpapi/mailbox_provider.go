package httpapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

// hmeMailboxProvider is the only HME protocol adapter used by the server.
// Callers use mailbox.ResourceSession and never construct an upstream client
// themselves. Adding another source means adding another Provider here, not
// changing registration, OAuth, or account-management flows.
type hmeMailboxProvider struct {
	client *registerruntime.HME
}

func newMailboxProvider(source mailboxstore.Source, interval time.Duration) (mailboxstore.Provider, error) {
	if !strings.EqualFold(source.Type, "icloud_hme") {
		return nil, errors.New("mailbox source type is not supported")
	}
	cfg, ok := hmeConfigFromSource(source)
	if !ok {
		return nil, errors.New("icloud mailbox source is not configured")
	}
	client, err := registerruntime.NewHME(cfg, interval)
	if err != nil {
		return nil, err
	}
	return &hmeMailboxProvider{client: client}, nil
}

func (p *hmeMailboxProvider) Acquire(ctx context.Context, jobID string) (mailboxstore.Mailbox, error) {
	box, err := p.client.Acquire(ctx, jobID)
	if err != nil {
		return mailboxstore.Mailbox{}, err
	}
	return mailboxstore.Mailbox{ID: box.ID, Email: box.Email, SourceID: box.SourceID, SourceType: box.SourceType, SourceAccountID: box.AccountID, SourceMailboxID: box.SourceMailboxID}, nil
}

func (p *hmeMailboxProvider) Prepare(ctx context.Context, box mailboxstore.Mailbox) error {
	return p.client.Prepare(ctx, registerMailbox(box))
}

func (p *hmeMailboxProvider) WaitCode(ctx context.Context, box mailboxstore.Mailbox, after time.Time, matcher func(string) bool) (string, error) {
	return p.client.WaitCodeFor(ctx, registerMailbox(box), after, matcher)
}

func (p *hmeMailboxProvider) Delete(ctx context.Context, box mailboxstore.Mailbox) error {
	err := p.client.DeleteMailbox(ctx, registerMailbox(box))
	if isHMEErrorCode(err, "mailbox_not_found") {
		return mailboxstore.ErrProviderMailboxNotFound
	}
	if err != nil {
		return errors.New(hmeCleanupMessage(err))
	}
	return nil
}

func (p *hmeMailboxProvider) List(ctx context.Context) ([]mailboxstore.Mailbox, error) {
	items, err := p.client.ListMailboxes(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]mailboxstore.Mailbox, 0, len(items))
	for _, box := range items {
		result = append(result, mailboxstore.Mailbox{ID: box.ID, Email: box.Email, SourceID: box.SourceID, SourceType: box.SourceType, SourceAccountID: box.AccountID, SourceMailboxID: box.SourceMailboxID})
	}
	return result, nil
}

func (p *hmeMailboxProvider) Close() {
	if p != nil && p.client != nil {
		p.client.Close()
	}
}

func registerMailbox(box mailboxstore.Mailbox) registerruntime.Mailbox {
	return registerruntime.Mailbox{ID: box.ID, Email: box.Email, SourceID: box.SourceID, SourceType: box.SourceType, AccountID: box.SourceAccountID, SourceMailboxID: box.SourceMailboxID}
}

// mailboxResourceSource adapts the provider-neutral resource session to the
// registration engine's existing MailSource contract. The engine therefore
// has no access to HME or any future upstream implementation.
type mailboxResourceSource struct {
	session  *mailboxstore.ResourceSession
	platform string
	mu       sync.RWMutex
	jobID    string
}

func newMailboxResourceSource(ctx context.Context, manager *mailboxstore.ResourceManager, platform, sourceID string, interval time.Duration) (*mailboxResourceSource, error) {
	session, err := manager.Open(ctx, sourceID, interval)
	if err != nil {
		return nil, err
	}
	return &mailboxResourceSource{session: session, platform: strings.ToLower(strings.TrimSpace(platform))}, nil
}

func (s *mailboxResourceSource) Acquire(ctx context.Context, jobID string) (registerruntime.Mailbox, error) {
	s.mu.Lock()
	s.jobID = strings.TrimSpace(jobID)
	s.mu.Unlock()
	box, err := s.session.Acquire(ctx, s.platform, jobID)
	if err != nil {
		return registerruntime.Mailbox{}, err
	}
	return registerMailbox(box), nil
}

func (s *mailboxResourceSource) Prepare(ctx context.Context, box registerruntime.Mailbox) error {
	return s.session.Prepare(ctx, mailboxFromRegister(box))
}

func (s *mailboxResourceSource) PrepareFor(ctx context.Context, jobID string, box registerruntime.Mailbox) error {
	s.mu.Lock()
	s.jobID = strings.TrimSpace(jobID)
	s.mu.Unlock()
	converted := mailboxFromRegister(box)
	if err := s.session.EnsureClaim(ctx, s.platform, jobID, converted); err != nil {
		return err
	}
	return s.session.Prepare(ctx, converted)
}

func (s *mailboxResourceSource) WaitCode(ctx context.Context, box registerruntime.Mailbox, after time.Time) (string, error) {
	return s.session.WaitCode(ctx, mailboxFromRegister(box), after, nil)
}

func (s *mailboxResourceSource) WaitCodeFor(ctx context.Context, box registerruntime.Mailbox, after time.Time, matcher func(string) bool) (string, error) {
	return s.session.WaitCode(ctx, mailboxFromRegister(box), after, matcher)
}

func (s *mailboxResourceSource) Commit(ctx context.Context, accountID string, box registerruntime.Mailbox) error {
	s.mu.RLock()
	jobID := s.jobID
	s.mu.RUnlock()
	return s.session.Commit(ctx, s.platform, jobID, accountID, mailboxFromRegister(box))
}

func (s *mailboxResourceSource) Release(ctx context.Context, box registerruntime.Mailbox, reason string) error {
	s.mu.RLock()
	jobID := s.jobID
	s.mu.RUnlock()
	return s.session.Release(ctx, s.platform, jobID, mailboxFromRegister(box), reason)
}

func (s *mailboxResourceSource) Close() {
	if s != nil && s.session != nil {
		s.session.Close()
	}
}

func mailboxFromRegister(box registerruntime.Mailbox) mailboxstore.Mailbox {
	return mailboxstore.Mailbox{ID: box.ID, Email: box.Email, SourceID: box.SourceID, SourceType: box.SourceType, SourceAccountID: box.AccountID, SourceMailboxID: box.SourceMailboxID}
}

func mailboxSourceIDFromAccount(account map[string]any, fallback string) string {
	mailbox := mapValue(account["registration_mailbox"])
	return firstNonEmpty(stringValue(mailbox["source_id"]), stringValue(mailbox["source"]), fallback)
}

func mailboxFromAccount(account map[string]any, fallbackAccountID, fallbackSourceID string) registerruntime.Mailbox {
	mailbox := mapValue(account["registration_mailbox"])
	return registerruntime.Mailbox{
		ID:              strings.TrimSpace(stringValue(mailbox["id"])),
		Email:           strings.TrimSpace(firstNonEmpty(stringValue(mailbox["email"]), stringValue(account["email"]))),
		AccountID:       strings.TrimSpace(firstNonEmpty(stringValue(mailbox["account_id"]), fallbackAccountID)),
		SourceID:        strings.TrimSpace(firstNonEmpty(stringValue(mailbox["source_id"]), fallbackSourceID)),
		SourceType:      firstNonEmpty(stringValue(mailbox["source_type"]), "hme"),
		SourceMailboxID: strings.TrimSpace(firstNonEmpty(stringValue(mailbox["source_mailbox_id"]), stringValue(mailbox["id"]))),
	}
}
