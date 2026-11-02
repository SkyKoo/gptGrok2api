package store

import (
	"fmt"
	"os"
	"time"
)

// RecordAccountUsage counts completed upstream attempts atomically. Historical
// accounts keep missing statistics until their first observed outcome.
func (s *Store) RecordAccountUsage(token string, success bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return err
	}
	index, found := s.accountsIndex[token]
	if !found {
		return os.ErrNotExist
	}
	current := s.accountsCache[index]
	count := func(key string) int64 {
		var value int64
		_, _ = fmt.Sscan(fmt.Sprint(current[key]), &value)
		return max(0, value)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	updates := map[string]any{"use_count": count("use_count"), "fail_count": count("fail_count"), "last_used_at": now}
	key := "fail_count"
	if success {
		key = "use_count"
	}
	updates[key] = count(key) + 1
	if stringValue(current["usage_started_at"]) == "" {
		updates["usage_started_at"] = now
	}
	items, _, ok := s.updatedAccountsLocked(token, updates)
	if !ok {
		return os.ErrNotExist
	}
	s.replaceAccountsLocked(items)
	s.accountsDirty = true
	s.scheduleAccountFlushLocked(accountRuntimeFlushDelay)
	return nil
}
