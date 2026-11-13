package adobe

import (
	"context"
	"errors"
	"strings"
)

// WithoutEmailAccount serializes a short local mailbox action with all account
// imports/deletes/links. The callback must not call Store again or do upstream
// network work; the established lock order is account store -> mailbox DB.
func (s *Store) WithoutEmailAccount(ctx context.Context, email string, action func() error) (bool, error) {
	email = strings.TrimSpace(email)
	if email == "" || action == nil {
		return false, errors.New("adobe_mailbox_guard_invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	state, err := s.load()
	if err != nil {
		return false, err
	}
	for _, account := range state.Accounts {
		if strings.EqualFold(strings.TrimSpace(account.Email), email) {
			return false, nil
		}
	}
	if err := action(); err != nil {
		return true, err
	}
	return true, nil
}

// Registration callbacks share the same lock with manual imports. An existing
// account is allowed only for an idempotent retry of this exact registration.
func (s *Store) registrationMailboxOwnerLocked(email, root, accountID string) (*Account, error) {
	state, err := s.load()
	if err != nil {
		return nil, err
	}
	var existing *Account
	for _, account := range state.Accounts {
		sameEmail := strings.EqualFold(strings.TrimSpace(account.Email), strings.TrimSpace(email))
		if account.ID != accountID && !sameEmail {
			continue
		}
		if !sameEmail || account.ID != accountID || account.RegistrationJobID != root {
			return nil, ErrIdentity
		}
		copy := account
		existing = &copy
	}
	return existing, nil
}

func (s *Store) WithRegistrationMailboxOwner(ctx context.Context, email, root, accountID string, action func() error) error {
	email, root, accountID = strings.TrimSpace(email), strings.TrimSpace(root), strings.TrimSpace(accountID)
	if strings.TrimSpace(email) == "" || root == "" || accountID == "" || action == nil {
		return ErrIdentity
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.registrationMailboxOwnerLocked(email, root, accountID); err != nil {
		return err
	}
	return action()
}

// ImportRegistration closes the store-check/claim-commit/import race. Commit
// must be a short local DB transaction, never an upstream/browser action.
func (s *Store) ImportRegistration(ctx context.Context, account Account, commit func() error) error {
	account.Email, account.RegistrationJobID, account.AccountID = strings.TrimSpace(account.Email), strings.TrimSpace(account.RegistrationJobID), strings.TrimSpace(account.AccountID)
	if account.Email == "" || account.RegistrationJobID == "" || account.AccountID == "" {
		return ErrIdentity
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	identity, _, err := TokenIdentity(strings.TrimSpace(account.Token))
	if err != nil {
		return err
	}
	if identity != account.AccountID {
		return ErrIdentity
	}
	if _, _, err = MediaSessionARP(account); err != nil {
		return err
	}
	if s.quotaQueries["adobe:"+account.AccountID] {
		return ErrBusy
	}
	existing, err := s.registrationMailboxOwnerLocked(account.Email, account.RegistrationJobID, "adobe:"+account.AccountID)
	if err != nil {
		return err
	}
	if commit != nil {
		if err = commit(); err != nil {
			return err
		}
	}
	if existing != nil {
		return nil
	}
	_, err = s.importLocked([]Account{account})
	return err
}
