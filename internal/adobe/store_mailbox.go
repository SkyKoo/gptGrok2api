package adobe

import (
	"reflect"
	"time"
)

// LinkMailbox serializes metadata linking with account import, deletion and
// credential updates. The callback must durably protect mailbox ownership before
// returning metadata, and must not call this Store. If saving the encrypted file
// fails, the confirmed ownership guard remains; retrying repairs only metadata.
// Mailbox changes do not change the credential revision or scheduling state.
func (s *Store) LinkMailbox(id string, attach func(Account) (map[string]any, bool, error)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return false, err
	}
	for i, a := range d.Accounts {
		if a.ID != id {
			continue
		}
		metadata, claimChanged, err := attach(a)
		if err != nil {
			return false, err
		}
		if reflect.DeepEqual(metadata, a.RegistrationMailbox) {
			return claimChanged, nil
		}
		d.Accounts[i].RegistrationMailbox = metadata
		d.Accounts[i].UpdatedAt = time.Now().Unix()
		if err := s.save(d); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, ErrNotFound
}
