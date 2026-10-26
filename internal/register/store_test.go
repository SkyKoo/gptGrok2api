package register

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRegistrationControlAndAccountLifecycle(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "register.json")
	accountsPath := filepath.Join(root, "grok_accounts.json")
	if err := os.WriteFile(accountsPath, []byte(`[{"id":"grok-one","email":"alice@example.com","password":"secret","sso":"sso-token","status":"active","source_type":"protocol","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(configPath, accountsPath)

	value, err := store.Start()
	if err != nil || value["enabled"] != true {
		t.Fatalf("start failed: %#v %v", value, err)
	}
	items, total, summary, err := store.ListAccounts("alice", "active")
	if err != nil || total != 1 || len(items) != 1 || summary["active"] != 1 {
		t.Fatalf("unexpected list: %#v %d %#v %v", items, total, summary, err)
	}
	if _, leaked := items[0]["sso"]; leaked || items[0]["has_sso"] != true || items[0]["email"] != "al***e@example.com" {
		t.Fatalf("public account leaked or was not masked: %#v", items[0])
	}

	count, err := store.SetDisabled([]string{"grok-one"}, true)
	if err != nil || count != 1 {
		t.Fatalf("disable failed: %d %v", count, err)
	}
	credentials, found, err := store.Credentials("grok-one")
	if err != nil || !found || credentials["password"] != "secret" {
		t.Fatalf("credentials failed: %#v %v %v", credentials, found, err)
	}

	exported, err := store.ExportAccounts(nil)
	if err != nil || len(exported) != 1 || exported[0]["sso"] != "sso-token" {
		t.Fatalf("export failed: %#v %v", exported, err)
	}
	if err := json.Valid(mustJSON(exported)); err != true {
		t.Fatal("export is not valid JSON")
	}
	removed, err := store.Delete([]string{"grok-one"})
	if err != nil || removed != 1 {
		t.Fatalf("delete failed: %d %v", removed, err)
	}
	if _, found, _ := store.Credentials("grok-one"); found {
		t.Fatal("account still exists after delete")
	}
	if !strings.Contains(string(mustRead(configPath)), "enabled") {
		t.Fatal("config was not persisted")
	}
}

func TestStorePublicAccountFlattensRuntimeAndProbeState(t *testing.T) {
	root := t.TempDir()
	accountsPath := filepath.Join(root, "grok_accounts.json")
	archive := `[{
		"id":"grok-runtime",
		"email":"runtime@example.com",
		"password":"secret",
		"sso":"private-sso",
		"status":"active",
		"runtime":{"present":true,"status":"active","synced_at":"2026-10-26T11:57:45Z","quota":{"fast":{"remaining":30,"total":30}}},
		"probe":{"status":"valid","checked_at":"2026-10-26T11:36:34Z","quota":{"remaining":30,"total":30}}
	}]`
	if err := os.WriteFile(accountsPath, []byte(archive), 0o600); err != nil {
		t.Fatal(err)
	}

	items, _, _, err := New(filepath.Join(root, "register.json"), accountsPath).ListAccounts("", "")
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected account list: %#v %v", items, err)
	}
	item := items[0]
	if item["runtime_status"] != "active" || item["sync_state"] != "synced" || item["refresh_status"] != "success" {
		t.Fatalf("runtime state was not flattened: %#v", item)
	}
	quota, ok := item["quota"].(map[string]any)
	if !ok || quota["fast"] == nil {
		t.Fatalf("runtime quota was not exposed: %#v", item["quota"])
	}
	if item["probe_status"] != "valid" || item["probe_at"] != "2026-10-26T11:36:34Z" {
		t.Fatalf("probe state was not flattened: %#v", item)
	}
	if _, leaked := item["sso"]; leaked {
		t.Fatalf("SSO leaked into public account: %#v", item)
	}
}

func TestRuntimeRequiresAllExternalProviders(t *testing.T) {
	runtime := NewRuntime()
	if runtime.Ready("grok") {
		t.Fatal("empty runtime should not be ready")
	}
	if err := runtime.Start("grok"); err != ErrExecutorNotConfigured {
		t.Fatalf("unexpected start error: %v", err)
	}
	status := runtime.Status("grok")
	if status["ready"] != false || status["last_error"] != ErrExecutorNotConfigured.Error() {
		t.Fatalf("unexpected runtime status: %#v", status)
	}
}

func TestOAuthAuthorizationStateIsPersisted(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "accounts.json")
	if err := os.WriteFile(path, []byte(`[{"id":"grok-one","email":"a@example.com","password":"p","sso":"s"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(filepath.Join(root, "register.json"), path)
	found, err := store.SetOAuthAuthorization("grok-one", "queued", "")
	if err != nil || !found {
		t.Fatalf("state update failed: %v %v", found, err)
	}
	items, err := store.GetAccounts([]string{"grok-one"})
	if err != nil || len(items) != 1 {
		t.Fatalf("account read failed: %#v %v", items, err)
	}
	state, _ := items[0]["oauth_authorization"].(map[string]any)
	if state["status"] != "queued" {
		t.Fatalf("unexpected OAuth state: %#v", state)
	}
}

func mustJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func mustRead(path string) []byte {
	raw, _ := os.ReadFile(path)
	return raw
}
