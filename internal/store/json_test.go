package store

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthKeyLifecycleKeepsPrivateHashOutOfPublicPayload(t *testing.T) {
	root := t.TempDir()
	repository := New(
		filepath.Join(root, "accounts.json"),
		filepath.Join(root, "auth_keys.json"),
		filepath.Join(root, "config.json"),
	)

	item, rawKey, err := repository.CreateKey("user", "", "admin-secret")
	if err != nil {
		t.Fatal(err)
	}
	if rawKey == "" || item["key_hash"] != nil {
		t.Fatalf("public key payload leaked private material: %#v", item)
	}

	identity, ok := repository.Authenticate(rawKey)
	if !ok || identity.Role != "user" {
		t.Fatalf("created key did not authenticate: %#v", identity)
	}
}

func TestAccountStoragePreservesJSONCompatibleFields(t *testing.T) {
	root := t.TempDir()
	accountPath := filepath.Join(root, "accounts.json")
	repository := New(accountPath, filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))

	added, skipped, items, err := repository.AddAccounts(nil, []map[string]any{{
		"access_token":  "token-1",
		"refresh_token": "refresh-secret",
		"email":         "user@example.test",
	}})
	if err != nil || added != 1 || skipped != 0 {
		t.Fatalf("unexpected add result: added=%d skipped=%d err=%v", added, skipped, err)
	}
	if len(items) != 1 || items[0]["access_token"] != "token-1" {
		t.Fatalf("unexpected stored accounts: %#v", items)
	}

	raw, err := os.ReadFile(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("stored account JSON is invalid: %v", err)
	}
	if decoded[0]["refresh_token"] != "refresh-secret" {
		t.Fatalf("account secret was not preserved in storage")
	}
}

func TestAccountHistoryTracksRegistrationAndDeletionWithoutSecrets(t *testing.T) {
	root := t.TempDir()
	repository := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	_, _, _, err := repository.AddAccountsWithAction("registered", nil, []map[string]any{{
		"id": "account-1", "access_token": "access-secret", "refresh_token": "refresh-secret",
		"email": "person@example.test", "user_id": "user-1", "source_type": "chatgpt_web",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.DeleteAccounts([]string{"access-secret"}); err != nil {
		t.Fatal(err)
	}
	items, total, err := repository.ListAccountHistory("", "person@example.test", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 || items[0]["action"] != "deleted" || items[1]["action"] != "registered" {
		t.Fatalf("unexpected history: total=%d items=%#v", total, items)
	}
	raw, err := os.ReadFile(filepath.Join(root, "account_history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "access-secret") || strings.Contains(string(raw), "refresh-secret") {
		t.Fatalf("account history leaked credentials: %s", raw)
	}
}

func TestValidatorUsesStoredUserKeys(t *testing.T) {
	root := t.TempDir()
	repository := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	_, rawKey, err := repository.CreateKey("user", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+rawKey)
	identity, ok := repository.Authenticate(rawKey)
	if !ok || identity.Role != "user" {
		t.Fatalf("stored user key was not accepted")
	}
}

func TestRotateAccountTokensClearsStaleErrorMarkers(t *testing.T) {
	root := t.TempDir()
	repository := New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	_, _, _, err := repository.AddAccounts(nil, []map[string]any{{
		"access_token":             "old-token",
		"status":                   "异常",
		"status_reason_code":       "auth_invalid",
		"last_refresh_error":       "old refresh failure",
		"last_refresh_error_at":    "2026-08-23T10:00:00Z",
		"last_error_kind":          "auth_invalid",
		"last_error_status":        401,
		"last_error_message":       "expired",
		"last_error_at":            "2026-08-23T10:00:00Z",
		"last_token_refresh_error": "old token failure",
		"relogin_blocked":          true,
		"last_relogin_error":       "old login failure",
		"last_relogin_error_at":    "2026-08-23T10:00:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := repository.RotateAccountTokens(
		"old-token",
		"new-token",
		"new-refresh",
		"",
		map[string]any{"quota": 23, "status": "正常"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated["status"] != "正常" || updated["quota"] != 23 {
		t.Fatalf("remote fields were not persisted: %#v", updated)
	}
	for _, key := range []string{
		"last_refresh_error",
		"last_refresh_error_at",
		"last_error_kind",
		"last_error_status",
		"last_error_message",
		"last_error_at",
		"last_token_refresh_error",
		"last_token_refresh_error_at",
		"status_reason_code",
		"relogin_blocked",
		"last_relogin_error",
		"last_relogin_error_at",
	} {
		if updated[key] != nil {
			t.Fatalf("stale marker %q was not cleared: %#v", key, updated[key])
		}
	}
}

func TestAccountSnapshotUsesCopyOnWriteAndFlushesRuntimeUpdates(t *testing.T) {
	root := t.TempDir()
	accountPath := filepath.Join(root, "accounts.json")
	repository := New(accountPath, filepath.Join(root, "auth_keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "status": "正常", "enabled": true}}); err != nil {
		t.Fatal(err)
	}

	before, beforeRevision, err := repository.AccountSnapshot()
	if err != nil || len(before) != 1 {
		t.Fatalf("unexpected initial snapshot: %#v %v", before, err)
	}
	if _, err := repository.UpdateAccountRuntime("one", map[string]any{"last_error_status": 502, "last_error_message": "temporary"}); err != nil {
		t.Fatal(err)
	}
	after, afterRevision, err := repository.AccountSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if afterRevision <= beforeRevision || after[0]["last_error_message"] != "temporary" {
		t.Fatalf("runtime update did not advance snapshot: before=%d after=%d item=%#v", beforeRevision, afterRevision, after[0])
	}
	if before[0]["last_error_message"] != nil {
		t.Fatalf("previous snapshot was mutated in place: %#v", before[0])
	}
	if err := repository.FlushAccounts(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted []map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0]["last_error_message"] != "temporary" {
		t.Fatalf("runtime update was not flushed: %#v", persisted)
	}
}

func TestRotateAccountTokensRetainsPartialReloginBlock(t *testing.T) {
	repository := New(filepath.Join(t.TempDir(), "accounts.json"), "", "")
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "old", "quota": 25, "enabled": false}}); err != nil {
		t.Fatal(err)
	}
	updated, _, err := repository.RotateAccountTokens("old", "new", "", "", map[string]any{"status": "异常", "relogin_blocked": true, "last_relogin_error": "account_refresh_failed"})
	if err != nil {
		t.Fatal(err)
	}
	if updated["relogin_blocked"] != true || updated["status"] != "异常" || updated["enabled"] != false || updated["access_token"] != "new" {
		t.Fatal("partial login was not atomically blocked")
	}
	updated, _, err = repository.RotateAccountTokens("new", "new", "", "", map[string]any{"status": "正常"})
	if err != nil {
		t.Fatal(err)
	}
	if updated["relogin_blocked"] == true || updated["last_relogin_error"] != nil || updated["enabled"] != false {
		t.Fatal("verified recovery did not clear block or changed enabled state")
	}
}
