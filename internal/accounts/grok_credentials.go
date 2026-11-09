package accounts

import (
	"errors"
	"reflect"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

// CheckGrokCredential shares the pool's per-account exclusion with media refresh
// and generation. Late checks cannot undo a disable or a newer auth failure.
func (p *Pool) CheckGrokCredential(a Account, fetch func(Account) (map[string]any, error)) error {
	p.mu.Lock()
	current, err := p.currentAccountLocked(a)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	id := Identity(current)
	if p.refreshing[id] || p.inflight[current.Token] > 0 || !boolValue(current.Fields["enabled"], true) || strings.EqualFold(stringValue(current.Fields["status"]), "disabled") {
		p.mu.Unlock()
		return ErrRefreshDeferred
	}
	p.refreshing[id] = true
	p.mu.Unlock()
	updates, checkErr := fetch(current)
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.signalLocked()
	defer delete(p.refreshing, id)
	if errors.Is(checkErr, ErrRefreshDeferred) {
		return checkErr
	}
	_, _, err = p.repository.UpdateAccountIf(current.Token, updates, func(row map[string]any) bool {
		return reflect.DeepEqual(row["enabled"], current.Fields["enabled"]) &&
			reflect.DeepEqual(row["grok_credential_check"], current.Fields["grok_credential_check"]) &&
			stringValue(row["status"]) == stringValue(current.Fields["status"]) &&
			stringValue(row["last_error_at"]) == stringValue(current.Fields["last_error_at"])
	})
	if errors.Is(err, store.ErrAccountChanged) {
		return ErrRefreshDeferred
	}
	if err != nil {
		return err
	}
	if checkErr == nil {
		delete(p.cooldowns, current.Token)
		delete(p.failures, current.Token)
	}
	return checkErr
}
