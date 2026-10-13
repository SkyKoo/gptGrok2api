package httpapi

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

var validationRetryDelays = [...]time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

func validationDiagnostic(err error) *provider.AccountEndpointError {
	if err == nil {
		return nil
	}
	var diagnostic *provider.AccountEndpointError
	if errors.As(err, &diagnostic) {
		copy := *diagnostic
		return &copy
	}
	// Unknown errors may contain credentials. Never persist their raw text.
	return &provider.AccountEndpointError{Stage: "refresh_account", Kind: "validation_failed", Message: "account validation failed"}
}

// A complete identity + quota check is required before removing the login block.
func (s *Server) refreshValidatedAccount(ctx context.Context, account map[string]any) (provider.AccountRefreshResult, error) {
	result, err := s.openAIAccountClient().RefreshAccount(ctx, account)
	if err != nil {
		return result, err
	}
	email, userID := stringValue(result.Fields["email"]), stringValue(result.Fields["user_id"])
	if stringValue(account["email"]) != "" && !strings.EqualFold(email, stringValue(account["email"])) ||
		stringValue(account["user_id"]) != "" && userID != stringValue(account["user_id"]) {
		return result, &provider.AccountEndpointError{Stage: "user_info", Endpoint: "/backend-api/me", Status: 200, Kind: "account_identity_mismatch", Message: "account identity does not match saved account"}
	}
	if boolValue(account["relogin_blocked"], false) || account["relogin_validation"] != nil {
		if email == "" || userID == "" {
			return result, &provider.AccountEndpointError{Stage: "user_info", Endpoint: "/backend-api/me", Status: 200, Kind: "identity_missing", Message: "user identity missing from response"}
		}
		limits, present := result.Fields["limits_progress"].([]any)
		if !present || limits == nil {
			return result, &provider.AccountEndpointError{Stage: "quota", Endpoint: "/backend-api/conversation/init", Status: 200, Kind: "quota_snapshot_missing", Message: "quota snapshot missing from response", Retryable: true}
		}
		result.Fields["quota_pending"] = map[string]int{}
		result.Fields["capability_cooldowns"] = map[string]any{}
		result.Fields["survival_alive"] = true
		result.Fields["survival_status"] = result.Fields["type"]
		result.Fields["survival_last_probe_status"] = result.Fields["type"]
		result.Fields["survival_check_error"] = nil
		result.Fields["survival_last_checked_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	result.Fields["quota_refresh_needed"] = false
	result.Fields["quota_refresh_error"] = nil
	return result, nil
}

func validationFailureFields(err error, state map[string]any, now time.Time) map[string]any {
	diagnostic := validationDiagnostic(err)
	state = cloneMap(state)
	attempts := intValue(state["attempts"])
	state["pending"] = false
	state["next_attempt_at"] = ""
	state["updated_at"] = now.UTC().Format(time.RFC3339Nano)
	state["diagnostic"] = diagnostic
	if diagnostic.Retryable && attempts < len(validationRetryDelays) {
		delay := validationRetryDelays[attempts]
		if retry := time.Duration(diagnostic.RetryAfterSeconds) * time.Second; retry > delay {
			delay = retry
		}
		state["pending"] = true
		state["next_attempt_at"] = now.Add(delay).UTC().Format(time.RFC3339Nano)
	}
	fields := reloginFailureUpdates(err)
	fields["relogin_validation"] = state
	fields["last_account_validation_error"] = diagnostic
	fields["quota_refresh_needed"] = true
	fields["quota_refresh_error"] = diagnostic.Error()
	return fields
}

func sameValidation(state map[string]any) func(map[string]any) bool {
	return func(account map[string]any) bool {
		current := mapValue(account["relogin_validation"])
		return stringValue(current["id"]) == stringValue(state["id"]) &&
			intValue(current["attempts"]) == intValue(state["attempts"]) &&
			current["pending"] == state["pending"]
	}
}

// Called serially by the existing maintenance-aware quota scheduler. Pending
// work is in accounts.json, so restarts do not discard it or reset the budget.
// The retry only reads Web account endpoints; it never starts email login.
func (s *Server) retryAccountValidation(account map[string]any, now time.Time) bool {
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		return false
	}
	s.reloginMu.Lock()
	latest, err := s.accountByRef(accountPublicRef(account))
	if err != nil || accountToken(latest) != accountToken(account) || !boolValue(latest["enabled"], true) {
		s.reloginMu.Unlock()
		return false
	}
	state := cloneMap(mapValue(latest["relogin_validation"]))
	next, _ := time.Parse(time.RFC3339Nano, stringValue(state["next_attempt_at"]))
	if state["pending"] != true || now.Before(next) {
		s.reloginMu.Unlock()
		return false
	}
	activeAuto := 0
	for _, task := range s.reloginProgress {
		if !task.Done && task.Automatic {
			activeAuto++
		}
		if !task.Done && task.AccountRef == accountPublicRef(latest) {
			s.reloginMu.Unlock()
			return false
		}
	}
	if activeAuto >= autoReloginConcurrency {
		s.reloginMu.Unlock()
		return false
	}
	attempt := intValue(state["attempts"])
	if attempt >= len(validationRetryDelays) {
		// A restart during the final attempt must not create a fourth retry.
		stopped := cloneMap(state)
		stopped["pending"] = false
		stopped["next_attempt_at"] = ""
		_, _, _ = s.store.UpdateAccountIf(accountToken(latest), map[string]any{"relogin_validation": stopped}, sameValidation(state))
		s.reloginMu.Unlock()
		return false
	}
	id := newChatID()
	claimed := cloneMap(state)
	claimed["attempts"] = attempt + 1
	claimed["progress_id"] = id
	// Reserve the attempt durably before sending; after a crash the next attempt
	// waits until this request's deadline plus its backoff.
	claimed["next_attempt_at"] = now.Add(90*time.Second + validationRetryDelays[attempt]).UTC().Format(time.RFC3339Nano)
	if _, _, err = s.store.UpdateAccountIf(accountToken(latest), map[string]any{"relogin_validation": claimed}, sameValidation(state)); err != nil {
		s.reloginMu.Unlock()
		return false
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	s.reloginProgress[id] = &accountReloginProgress{ProgressID: id, AccountRef: accountPublicRef(latest), Email: stringValue(latest["email"]), accountToken: accountToken(latest), Stage: "refresh_account", StartedAt: stamp, UpdatedAt: stamp, Automatic: true, ValidationOnly: true, ValidationAttempt: attempt + 1, SessionUpdated: true, failureRecorded: true}
	s.reloginMu.Unlock()

	// Snapshot of the saved credential/environment, no OAuth refresh or email OTP.
	candidate := cloneMap(latest)
	candidate["refresh_token"] = ""
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	result, validationErr := s.refreshValidatedAccount(ctx, candidate)
	cancel()
	fields := result.Fields
	if validationErr != nil {
		fields = validationFailureFields(validationErr, claimed, time.Now())
		_, _, err = s.store.UpdateAccountIf(accountToken(latest), fields, sameValidation(claimed))
	} else {
		_, _, err = s.store.RotateAccountTokensIf(accountToken(latest), accountToken(latest), "", "", fields, sameValidation(claimed))
	}
	if err != nil {
		// Never mark another login's newer credential failed or unblock it.
		s.finishAccountRelogin(id, map[string]any{"session_updated": true}, errors.New("account changed or validation result could not be saved"))
		return true
	}
	if validationErr != nil {
		log.Printf("account validation progress=%s attempt=%d: %s", id, attempt+1, validationDiagnostic(validationErr).Error())
	}
	s.finishAccountRelogin(id, map[string]any{"session_updated": true, "quotas_refreshed": validationErr == nil}, validationErr)
	return true
}
