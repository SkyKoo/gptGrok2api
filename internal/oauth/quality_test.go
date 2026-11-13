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
