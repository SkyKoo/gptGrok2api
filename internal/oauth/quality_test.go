package oauth

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/xaiquality"
)

func TestNewGrantPreservesRiskButNotOldPass(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		store := NewStore(filepath.Join(t.TempDir(), "oauth.bin"), "fixture")
		q := &xaiquality.Report{Model: xaiquality.Model, Version: xaiquality.Version, Status: "normal", At: time.Now().Unix(), Blocked: blocked}
		if blocked {
			q.Status = "high_risk"
			q.ConfirmedAt = q.At
		}
		old, err := store.Import(Account{Email: "account@example.test", AccessToken: "old", RefreshToken: "old-refresh", Quality: q})
		if err != nil {
			t.Fatal(err)
		}
		current, err := store.Import(Account{Email: old.Email, AccessToken: "new", RefreshToken: "new-refresh"})
		if err != nil {
			t.Fatal(err)
		}
		if blocked && (current.Quality == nil || !current.Quality.Blocked || current.Quality.ConfirmedAt != q.At) {
			t.Fatal("new grant erased account risk")
		}
		if !blocked && current.Quality != nil {
			t.Fatal("new grant reused stale pass")
		}
		_, applied, err := store.UpdateIfDelivery(old, func(a *Account) { a.Quality = nil })
		if err != nil || applied {
			t.Fatal("old completion cleared new grant risk")
		}
	}
}

func TestManualCPARemovalPersistsUntilExplicitNewGrant(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oauth.bin"), "fixture")
	old, err := store.Import(Account{Email: "removed@example.test", AccessToken: "old", RefreshToken: "refresh", CPASync: &CPASync{Status: "removed", PoolID: "pool", Managed: true, ManualRemoval: &xaiquality.Removal{Status: "removed", FileName: "xai-removed.json"}}})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, applied, err := store.UpdateIfDelivery(old, func(a *Account) { a.LastError = "test" })
	if err != nil || !applied || !CPARemovalRequested(unchanged) {
		t.Fatal("removal receipt did not survive a store roundtrip")
	}
	same, err := store.Import(Account{Email: old.Email, AccessToken: old.AccessToken, RefreshToken: old.RefreshToken, CPASync: &CPASync{Status: "pending", PoolID: "pool", ReplaceExisting: true}})
	if err != nil || !CPARemovalRequested(same) {
		t.Fatal("stale import erased the manual removal fence")
	}
	newGrant, err := store.Import(Account{Email: old.Email, AccessToken: "new", RefreshToken: "new-refresh", CPASync: &CPASync{Status: "pending", PoolID: "pool", ReplaceExisting: true}})
	if err != nil || CPARemovalRequested(newGrant) {
		t.Fatal("explicit new grant did not restore eligibility")
	}
	_, applied, err = store.UpdateIfDelivery(old, func(a *Account) { a.CPASync = old.CPASync })
	if err != nil || applied {
		t.Fatal("stale removal overwrote a new grant")
	}
}
