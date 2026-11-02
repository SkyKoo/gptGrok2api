package oauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreEncryptsAndManagesAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.enc")
	store := NewStore(path, "test-secret")
	item, err := store.Import(Account{Email: "person@example.com", AccessToken: "access-secret", RefreshToken: "refresh-secret"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "access-secret") || strings.Contains(string(raw), "refresh-secret") {
		t.Fatal("OAuth token appeared in encrypted store")
	}
	items, err := store.List()
	if err != nil || len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("unexpected stored accounts: %#v %v", items, err)
	}
	if count, err := store.SetDisabled([]string{item.ID}, true); err != nil || count != 1 {
		t.Fatalf("disable failed: count=%d err=%v", count, err)
	}
	if count, err := store.Delete([]string{item.ID}); err != nil || count != 1 {
		t.Fatalf("delete failed: count=%d err=%v", count, err)
	}
	if _, err := NewStore(path, "wrong-secret").List(); err == nil {
		t.Fatal("wrong encryption secret unexpectedly decrypted store")
	}
}

func TestReauthorizationPreservesIDAndInvalidatesCPASync(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oauth.enc"), "key")
	before, _ := store.Import(Account{Email: "person@example.test", AccessToken: "old", RefreshToken: "refresh", CPASync: &CPASync{Status: "synced", PoolID: "pool", FileName: "xai-person@example.test.json"}})
	after, _ := store.Import(Account{Email: "PERSON@example.test", AccessToken: "new", RefreshToken: "new-refresh"})
	if before.ID != after.ID || after.CPASync == nil || after.CPASync.Status != "pending" || after.CPASync.PoolID != "pool" {
		t.Fatal("reauthorization lost identity/delivery state")
	}
	current, applied, err := store.UpdateIfCredential(before, func(a *Account) { a.AccessToken = "stale" })
	if err != nil || applied || current.AccessToken != "new" {
		t.Fatal("stale request overwrote new tokens")
	}
}
func TestOAuthPublicQuotaCompatibilityAndSecretRemoval(t *testing.T) {
	item := Account{AccessToken: "SECRET_ACCESS", RefreshToken: "SECRET_REFRESH", IDToken: "SECRET_ID", ExpiresAt: 1793630549, Probe: map[string]any{"quota": map[string]any{"remaining_percent": 0}}}
	value := Public(item)
	if value["quota"].(map[string]any)["remaining_percent"] != 0 || value["expires_at"] != int64(1793630549) {
		t.Fatal("legacy nested quota/expiry missing")
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "token_endpoint"} {
		if _, exists := value[key]; exists {
			t.Fatalf("private field exposed: %s", key)
		}
	}
}

func TestProbeRotationInvalidatesCPASyncWithoutOverwritingNewAuthorization(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oauth.enc"), "key")
	before, _ := store.Import(Account{Email: "one@example.test", AccessToken: "old", RefreshToken: "old-refresh", CPASync: &CPASync{Status: "synced", PoolID: "pool"}})
	after, applied, err := store.UpdateProbe(before, "new", "new-refresh", "", 1900000000, map[string]any{"quota": map[string]any{"remaining_percent": 75}}, "")
	if err != nil || !applied || after.CPASync.Status != "pending" || after.ExpiresAt != 1900000000 || after.Quota["remaining_percent"] != 75 {
		t.Fatal("probe rotation lost expiry/quota or left CPA synced")
	}
	_, applied, err = store.UpdateProbe(before, "stale", "stale-refresh", "", 1, nil, "")
	if err != nil || applied {
		t.Fatal("delayed probe overwrote a newer credential")
	}
}
