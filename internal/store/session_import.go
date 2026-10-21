package store

import (
	"errors"
	"strings"
)

var ErrSessionIdentityConflict = errors.New("session identity conflicts with saved account")

// ImportSessionAccount adds a browser session or fills missing identity fields
// for the same token. Existing credentials and management settings are retained.
func (s *Store) ImportSessionAccount(payload map[string]any) (map[string]any, bool, error) {
	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return nil, false, err
	}
	next := cloneAccountList(s.accountsCache)
	token := accountToken(payload)
	if token == "" {
		return nil, false, errors.New("access token is required")
	}
	index := -1
	for i, item := range next {
		if accountToken(item) == token {
			index = i
			break
		}
	}
	added := index < 0
	if added {
		item := cloneMap(payload)
		normalizeAccount(item)
		next = append(next, item)
		index = len(next) - 1
	} else {
		item := next[index]
		for _, key := range []string{"email", "user_id"} {
			saved, incoming := stringValue(item[key]), stringValue(payload[key])
			equal := saved == incoming
			if key == "email" {
				equal = strings.EqualFold(saved, incoming)
			}
			if saved != "" && incoming != "" && !equal {
				return nil, false, ErrSessionIdentityConflict
			}
			if saved == "" && incoming != "" {
				item[key] = incoming
			}
		}
		if stringValue(item["expired"]) == "" && stringValue(payload["expired"]) != "" {
			item["expired"] = payload["expired"]
		}
	}
	if err := writeJSON(s.accountsPath, next); err != nil {
		return nil, false, err
	}
	next = normalizeAccountListJSON(next)
	s.replaceAccountsLocked(next)
	s.accountsDirty = false
	return cloneMap(next[index]), added, nil
}
