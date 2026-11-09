package httpapi

import (
	"context"
	"errors"
	"reflect"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// Only definitive credential rejection blocks scheduling. Transport errors,
// upstream overload and quota exhaustion do not establish an invalid identity.
func (s *Server) recordGrokCredentialFailure(token string, err error) {
	if !errors.Is(err, provider.ErrGrokInvalidCredentials) {
		return
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	_, _, _ = s.grokRuntimeStore.UpdateAccount(token, map[string]any{
		"status": "异常", "status_reason_code": "auth_invalid", "last_error_kind": "auth_invalid",
		"last_error_status": 401, "last_error_message": "Grok credentials are invalid", "last_error_at": stamp,
	})
	items, e := s.registerStore.ExportAccounts(nil)
	if e != nil {
		return
	}
	for _, item := range items {
		if strings.TrimSpace(stringValue(item["sso"])) != token {
			continue
		}
		_, _ = s.registerStore.UpdateRuntime(stringValue(item["id"]), nil, map[string]any{"status": "invalid", "checked_at": stamp}, "invalid", "Grok credentials are invalid")
	}
}

// Recovery requires an explicit successful credential verification, never a
// cached quota or an ordinary request success. Manual disable stays disabled.
func (s *Server) restoreVerifiedGrokCredential(token string) {
	fields := map[string]any{"status": "正常"}
	for _, key := range []string{"status_reason_code", "last_error_kind", "last_error_status", "last_error_message", "last_error_at", "cooldown_until", "relogin_blocked"} {
		fields[key] = nil
	}
	_, _, _ = s.grokRuntimeStore.UpdateAccountIf(token, fields, func(current map[string]any) bool {
		return boolValue(current["enabled"], true) && stringValue(current["status"]) != "disabled"
	})
	items, e := s.registerStore.ExportAccounts(nil)
	if e != nil {
		return
	}
	for _, item := range items {
		if strings.TrimSpace(stringValue(item["sso"])) != token {
			continue
		}
		if !boolValue(item["enabled"], true) || stringValue(item["status"]) == "disabled" {
			continue
		}
		_, _ = s.registerStore.UpdateRuntime(stringValue(item["id"]), nil, nil, "active", "")
	}
}

func grokAccountDisabled(row map[string]any) bool {
	return !boolValue(row["enabled"], true) || strings.EqualFold(stringValue(row["status"]), "disabled") || stringValue(row["status"]) == "禁用"
}

// Both the manual check and the periodic sweep use this read-only verification.
// Only a definitive identity rejection invalidates the whole account.
func (s *Server) verifyGrokCredential(ctx context.Context, token string, fields map[string]any) (provider.QuotaWindow, string, error) {
	var quota provider.QuotaWindow
	status := "unknown"
	archive, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		return quota, status, err
	}
	var observation map[string]any
	var upstreamErr error
	fetch := func(current accounts.Account) (map[string]any, error) {
		var checkErr error
		quota, checkErr = s.grokQuota.ProbeFast(ctx, current.Token, current.Fields)
		upstreamErr = checkErr
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		observation = map[string]any{"status": "unknown", "checked_at": stamp}
		updates := map[string]any{"grok_credential_check": observation}
		switch {
		case checkErr == nil:
			status = "valid"
			observation["quota"] = map[string]any{"remaining": quota.Remaining, "total": quota.Total}
			observation["last_success_at"] = stamp
			updates["status"] = "正常"
			for _, key := range []string{"status_reason_code", "last_error_kind", "last_error_status", "last_error_message", "last_error_at", "cooldown_until", "relogin_blocked"} {
				updates[key] = nil
			}
		case errors.Is(checkErr, provider.ErrGrokInvalidCredentials):
			status = "invalid"
			updates["status"], updates["status_reason_code"], updates["last_error_kind"] = "异常", "auth_invalid", "auth_invalid"
			updates["last_error_status"], updates["last_error_message"], updates["last_error_at"] = 401, "Grok credentials are invalid", stamp
		}
		observation["status"] = status
		if checkErr != nil {
			observation["error"] = safeRefreshError(checkErr)
			observation["last_success_at"] = mapValue(current.Fields["grok_credential_check"])["last_success_at"]
		}
		return updates, checkErr
	}
	rows, _, err := s.grokRuntimeStore.AccountSnapshot()
	if err != nil {
		return quota, status, err
	}
	inPool := false
	for _, row := range rows {
		if accountToken(row) == token {
			inPool = true
			err = s.grokAccountPool.CheckGrokCredential(accounts.Account{Token: token, Fields: row}, fetch)
			break
		}
	}
	if !inPool {
		if grokAccountDisabled(fields) {
			return quota, "unknown", accounts.ErrRefreshDeferred
		}
		_, err = fetch(accounts.Account{Token: token, Fields: fields})
	}
	if errors.Is(err, accounts.ErrRefreshDeferred) || observation == nil || err != upstreamErr {
		return quota, "unknown", err
	}
	savedStatus := map[string]string{"valid": "active", "invalid": "invalid"}[status]
	for _, item := range archive {
		if strings.TrimSpace(stringValue(item["sso"])) != token {
			continue
		}
		_, saveErr := s.registerStore.UpdateRuntimeIf(stringValue(item["id"]), nil, observation, savedStatus, stringValue(observation["error"]), func(current map[string]any) bool {
			return stringValue(current["sso"]) == stringValue(item["sso"]) && !grokAccountDisabled(current) &&
				stringValue(current["status"]) == stringValue(item["status"]) && reflect.DeepEqual(current["probe"], item["probe"])
		})
		if saveErr != nil {
			return quota, status, saveErr
		}
	}
	return quota, status, err
}
