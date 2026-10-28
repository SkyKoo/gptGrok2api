package mailbox

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Provider is the source-neutral boundary for an upstream mailbox service.
// Registration and account flows only talk to ResourceSession; provider
// details such as HME's login, alias creation, inbox and delete APIs stay
// behind this interface.
type Provider interface {
	Acquire(context.Context, string) (Mailbox, error)
	Prepare(context.Context, Mailbox) error
	WaitCode(context.Context, Mailbox, time.Time, func(string) bool) (string, error)
	Delete(context.Context, Mailbox) error
	List(context.Context) ([]Mailbox, error)
	Close()
}

// ProviderFactory creates a provider session for one configured source. The
// polling interval belongs to the session so different registration flows can
// keep their own timing without exposing provider-specific configuration.
type ProviderFactory func(Source, time.Duration) (Provider, error)

// ResourceManager owns mailbox asset allocation and upstream provider access.
// It is intentionally small: the Store handles durable claims and events,
// while the factory handles source-specific protocol clients.
type ResourceManager struct {
	store        *Store
	factory      ProviderFactory
	sourceLoader func(context.Context) ([]Source, error)
}

func NewResourceManager(store *Store, factory ProviderFactory) *ResourceManager {
	return &ResourceManager{store: store, factory: factory}
}

func (m *ResourceManager) Enabled() bool {
	return m != nil && m.factory != nil && ((m.store != nil && m.store.Enabled()) || m.sourceLoader != nil)
}

// SetSourceLoader supplies a legacy/config-backed source catalog for
// installations that have not configured PostgreSQL yet. Calls still pass
// through ResourceSession and the provider boundary; the loader only supplies
// source configuration and never performs an upstream request.
func (m *ResourceManager) SetSourceLoader(loader func(context.Context) ([]Source, error)) {
	if m != nil {
		m.sourceLoader = loader
	}
}

func (m *ResourceManager) HasEnabledSourceType(ctx context.Context, sourceType string) bool {
	if m == nil || m.factory == nil {
		return false
	}
	sources, err := m.sources(ctx)
	if err != nil {
		return false
	}
	for _, source := range sources {
		if source.Enabled && strings.EqualFold(source.Type, sourceType) {
			return true
		}
	}
	return false
}

// Open creates a source session. A blank source ID is resolved to the first
// enabled source for legacy accounts that predate source IDs.
func (m *ResourceManager) Open(ctx context.Context, sourceID string, interval time.Duration) (*ResourceSession, error) {
	if m == nil || m.factory == nil {
		return nil, errors.New("mailbox resource manager is not configured")
	}
	source, err := m.resolveSource(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	provider, err := m.factory(source, interval)
	if err != nil {
		return nil, err
	}
	return &ResourceSession{manager: m, source: source, provider: provider}, nil
}

func (m *ResourceManager) resolveSource(ctx context.Context, sourceID string) (Source, error) {
	if m == nil || m.factory == nil {
		return Source{}, errors.New("mailbox resource manager is not configured")
	}
	if m.store == nil || !m.store.Enabled() {
		return m.resolveLegacySource(ctx, sourceID)
	}
	if id := strings.TrimSpace(sourceID); id != "" {
		source, err := m.store.GetSource(ctx, id)
		if err != nil {
			return Source{}, err
		}
		if !source.Enabled {
			return Source{}, errors.New("mailbox source is disabled")
		}
		return source, nil
	}
	sources, err := m.store.ListSources(ctx)
	if err != nil {
		return Source{}, err
	}
	for _, source := range sources {
		if source.Enabled {
			return source, nil
		}
	}
	return Source{}, errors.New("no enabled mailbox source")
}

func (m *ResourceManager) sources(ctx context.Context) ([]Source, error) {
	if m != nil && m.store != nil && m.store.Enabled() {
		return m.store.ListSources(ctx)
	}
	if m != nil && m.sourceLoader != nil {
		return m.sourceLoader(ctx)
	}
	return nil, nil
}

func (m *ResourceManager) resolveLegacySource(ctx context.Context, sourceID string) (Source, error) {
	sources, err := m.sources(ctx)
	if err != nil {
		return Source{}, err
	}
	if id := strings.TrimSpace(sourceID); id != "" {
		for _, source := range sources {
			if source.ID == id {
				if !source.Enabled {
					return Source{}, errors.New("mailbox source is disabled")
				}
				return source, nil
			}
		}
		return Source{}, errors.New("mailbox source not found")
	}
	for _, source := range sources {
		if source.Enabled {
			return source, nil
		}
	}
	return Source{}, errors.New("no enabled mailbox source")
}

// ResourceSession is the only mailbox API exposed to registration, OAuth,
// re-login and account cleanup flows. It combines durable asset claims with
// the selected upstream provider without leaking provider details.
type ResourceSession struct {
	manager  *ResourceManager
	source   Source
	provider Provider
}

func (s *ResourceSession) Source() Source {
	if s == nil {
		return Source{}
	}
	return s.source
}

func (s *ResourceSession) Acquire(ctx context.Context, platform, jobID string) (Mailbox, error) {
	if s == nil || s.manager == nil || s.provider == nil {
		return Mailbox{}, errors.New("mailbox resource session is not configured")
	}
	if s.manager.store == nil || !s.manager.store.Enabled() {
		return s.provider.Acquire(ctx, jobID)
	}
	return s.manager.store.Reserve(ctx, platform, jobID, s.source.ID,
		func(ctx context.Context) (Mailbox, error) {
			return s.provider.Acquire(ctx, jobID)
		}, func(ctx context.Context, box Mailbox) error {
			return s.provider.Prepare(ctx, box)
		})
}

func (s *ResourceSession) Prepare(ctx context.Context, box Mailbox) error {
	if s == nil || s.provider == nil {
		return errors.New("mailbox resource session is not configured")
	}
	return s.provider.Prepare(ctx, box)
}

func (s *ResourceSession) WaitCode(ctx context.Context, box Mailbox, after time.Time, matcher func(string) bool) (string, error) {
	if s == nil || s.provider == nil {
		return "", errors.New("mailbox resource session is not configured")
	}
	return s.provider.WaitCode(ctx, box, after, matcher)
}

func (s *ResourceSession) Delete(ctx context.Context, box Mailbox) error {
	if s == nil || s.provider == nil {
		return errors.New("mailbox resource session is not configured")
	}
	return s.provider.Delete(ctx, box)
}

func (s *ResourceSession) List(ctx context.Context) ([]Mailbox, error) {
	if s == nil || s.provider == nil {
		return nil, errors.New("mailbox resource session is not configured")
	}
	return s.provider.List(ctx)
}

func (s *ResourceSession) EnsureClaim(ctx context.Context, platform, jobID string, box Mailbox) error {
	if s == nil || s.manager == nil {
		return errors.New("mailbox resource session is not configured")
	}
	if s.manager.store == nil || !s.manager.store.Enabled() {
		return nil
	}
	return s.manager.store.EnsureClaim(ctx, platform, jobID, box)
}

func (s *ResourceSession) Commit(ctx context.Context, platform, jobID, accountID string, box Mailbox) error {
	if s == nil || s.manager == nil {
		return errors.New("mailbox resource session is not configured")
	}
	if s.manager.store == nil || !s.manager.store.Enabled() {
		return nil
	}
	return s.manager.store.Commit(ctx, platform, jobID, accountID, box)
}

func (s *ResourceSession) Release(ctx context.Context, platform, jobID string, box Mailbox, reason string) error {
	if s == nil || s.manager == nil {
		return errors.New("mailbox resource session is not configured")
	}
	if s.manager.store == nil || !s.manager.store.Enabled() {
		return nil
	}
	return s.manager.store.Release(ctx, platform, jobID, box, reason)
}

func (s *ResourceSession) ReleaseForAccount(ctx context.Context, platform, accountID, email string) ([]Mailbox, error) {
	if s == nil || s.manager == nil {
		return nil, errors.New("mailbox resource session is not configured")
	}
	if s.manager.store == nil || !s.manager.store.Enabled() {
		if strings.TrimSpace(email) == "" {
			return nil, nil
		}
		sourceAccountID := strings.TrimSpace(accountID)
		if configured, ok := s.source.Config["account_id"].(string); ok && strings.TrimSpace(configured) != "" {
			sourceAccountID = strings.TrimSpace(configured)
		}
		return []Mailbox{{
			Email:           strings.TrimSpace(email),
			SourceID:        s.source.ID,
			SourceType:      s.source.Type,
			SourceAccountID: sourceAccountID,
			SourceMailboxID: "",
		}}, nil
	}
	return s.manager.store.ReleaseForAccount(ctx, platform, accountID, email)
}

func (s *ResourceSession) MarkCleanup(ctx context.Context, box Mailbox, deleted bool, message string) error {
	if s == nil || s.manager == nil {
		return errors.New("mailbox resource session is not configured")
	}
	if s.manager.store == nil || !s.manager.store.Enabled() {
		return nil
	}
	return s.manager.store.MarkCleanup(ctx, box, deleted, message)
}

func (s *ResourceSession) Close() {
	if s != nil && s.provider != nil {
		s.provider.Close()
	}
}
