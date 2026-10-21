package httpapi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

const autoReloginCooldown = 30 * time.Minute
const autoReloginConcurrency = 2

// Only an explicit upstream authentication rejection starts browser recovery.
// Timeouts, 403 challenges, 429 quota errors and 5xx failures are not login failures.
func (s *Server) recoverInvalidAccount(account map[string]any, err error) {
	if !errors.Is(err, provider.ErrInvalidAccessToken) {
		return
	}
	latest, lookupErr := s.accountByRef(accountPublicRef(account))
	if lookupErr != nil || accountToken(latest) != accountToken(account) {
		return
	}
	if _, _, e := s.store.UpdateAccount(accountToken(latest), accountRefreshFailureUpdates(err)); e != nil {
		return
	}
	_, _ = s.startAccountRelogin(latest, true)
}

func (s *Server) autoReloginEligible(account map[string]any) bool {
	if !boolValue(account["enabled"], true) || strings.TrimSpace(stringValue(account["refresh_token"])) != "" ||
		strings.TrimSpace(stringValue(mapValue(account["fp"])["oai-device-id"])) == "" ||
		strings.TrimSpace(stringValue(account["email"])) == "" || !isOpenAIAccount(accounts.Account{Token: accountToken(account), Fields: account}) {
		return false
	}
	// Automatic recovery must reuse a stored environment, never silently choose a
	// fresh browser profile. Manual login retains its legacy import behavior.
	profile, err := s.accountReloginProfile(account)
	return err == nil && profile != nil
}

// Manual and automatic starts share the same per-account task, including the
// follow-up account refresh when automatic recovery joins an existing login.
func (s *Server) startAccountRelogin(account map[string]any, automatic bool) (accountReloginProgress, error) {
	s.authStartMu.Lock()
	defer s.authStartMu.Unlock()
	if s.twoFAActive(accountPublicRef(account)) {
		return accountReloginProgress{}, errors.New("该账号正在开启 2FA")
	}
	s.reloginMu.Lock()
	defer s.reloginMu.Unlock()
	empty := accountReloginProgress{}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		return empty, errors.New("maintenance in progress")
	}
	latest, err := s.accountByRef(accountPublicRef(account))
	if err != nil || accountToken(latest) != accountToken(account) {
		return empty, errors.New("account changed")
	}
	if automatic && !s.autoReloginEligible(latest) {
		return empty, errors.New("account is not eligible for automatic relogin")
	}
	publicRef := accountPublicRef(latest)
	activeAuto := 0
	for _, existing := range s.reloginProgress {
		if !existing.Done && existing.AccountRef == publicRef {
			if automatic && !existing.Automatic && !existing.autoJoined {
				if err := s.persistAutoReloginAttempt(latest); err != nil {
					return empty, err
				}
				existing.refreshAfterLogin = true
				existing.autoJoined = true
			}
			return reloginSummary(existing), nil
		}
		if !existing.Done && existing.Automatic {
			activeAuto++
		}
	}
	now := time.Now().UTC()
	if automatic {
		if activeAuto >= autoReloginConcurrency {
			return empty, errors.New("automatic relogin busy")
		}
		next, _ := time.Parse(time.RFC3339, stringValue(latest["auto_relogin_next_attempt_at"]))
		if now.Before(next) {
			return empty, errors.New("automatic relogin cooling down")
		}
		// Persist before starting; restarting the server must not bypass the cooldown.
		err = s.persistAutoReloginAttempt(latest)
		if err != nil {
			return empty, err
		}
	}
	// An explicit new login supersedes the previous saved-session validation.
	// Its later failure must not accidentally revive that old retry job.
	if latest["relogin_validation"] != nil {
		if _, _, err := s.store.UpdateAccount(accountToken(latest), map[string]any{"relogin_validation": nil}); err != nil {
			return empty, err
		}
	}
	id := newChatID()
	progress := &accountReloginProgress{accountToken: accountToken(latest), ProgressID: id, AccountRef: publicRef, Email: stringValue(latest["email"]), Stage: "queued", StartedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano), Automatic: automatic, refreshAfterLogin: automatic || s.autoReloginEligible(latest)}
	s.reloginProgress[id] = progress
	go s.runAccountRelogin(id, publicRef)
	return reloginSummary(progress), nil
}

func (s *Server) skipAccountRecoveryProbe(account map[string]any) bool {
	s.reloginMu.RLock()
	defer s.reloginMu.RUnlock()
	ref := accountPublicRef(account)
	for _, task := range s.reloginProgress {
		if !task.Done && task.AccountRef == ref {
			return true
		}
	}
	next, _ := time.Parse(time.RFC3339, stringValue(account["auto_relogin_next_attempt_at"]))
	return (stringValue(account["last_error_kind"]) == "auth_invalid" || boolValue(account["relogin_blocked"], false)) && time.Now().Before(next)
}

func (s *Server) persistAutoReloginAttempt(account map[string]any) error {
	now := time.Now().UTC()
	_, _, err := s.store.UpdateAccount(accountToken(account), map[string]any{
		"auto_relogin_last_started_at": now.Format(time.RFC3339),
		"auto_relogin_next_attempt_at": now.Add(autoReloginCooldown).Format(time.RFC3339),
	})
	return err
}
