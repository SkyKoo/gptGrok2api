package httpapi

import (
	"errors"
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
