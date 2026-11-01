package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestGrokVerificationBlocksEveryRequestKindAndRequiresExplicitRecovery(t *testing.T) {
	var status atomic.Int32
	status.Store(401)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		if status.Load() == 200 {
			_, _ = w.Write([]byte(`{"remainingQueries":30,"totalQueries":30}`))
		}
	}))
	defer upstream.Close()
	s := New(adminTestConfig(t.TempDir()))
	s.grokQuota = provider.NewGrokQuota(upstream.URL, upstream.Client(), nil)
	item, _, err := s.registerStore.AddAccount(map[string]any{"sso": "test-grok-state", "email": "grok@example.test", "enabled": true, "status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.syncRegisteredGrokRuntimeAccount(item, "test-grok-state"); err != nil {
		t.Fatal(err)
	}
	id := stringValue(item["id"])
	verify := func() {
		t.Helper()
		w := adminRequest(s.Handler(), "POST", "/api/register/grok/accounts/runtime/verify", strings.NewReader(`{"ids":["`+id+`"]}`))
		if w.Code != 200 {
			t.Fatalf("verify: %d %s", w.Code, w.Body.String())
		}
	}
	assertPool := func(blocked bool) {
		t.Helper()
		for _, kind := range []string{"chat", "image", "video"} {
			l, e := s.grokAccountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: kind})
			if l != nil {
				s.grokAccountPool.Release(l)
			}
			if blocked && !errors.Is(e, accounts.ErrUnavailable) || !blocked && e != nil {
				t.Fatalf("%s blocked=%v err=%v", kind, blocked, e)
			}
		}
	}
	verify()
	assertPool(true)
	rows, _ := s.registerStore.ExportAccounts([]string{id})
	if stringValue(rows[0]["status"]) != "invalid" {
		t.Fatal("archive not invalid")
	}
	// Refresh and explicit sync cannot erase a known invalid state.
	s.persistRuntimeToken("test-grok-state", nil, "active", "")
	assertPool(true)
	if _, _, e := s.syncRegisteredGrokRuntimeAccount(item, "test-grok-state"); e == nil {
		t.Fatal("sync bypassed invalid state")
	}
	status.Store(503)
	verify()
	assertPool(true)
	status.Store(200)
	verify()
	assertPool(false)
	// An unknown/network error on a healthy account does not invalidate it.
	status.Store(503)
	verify()
	assertPool(false)
	// Runtime admin quota refresh must propagate definite rejection, too.
	status.Store(401)
	s.refreshGrokRuntimeTokens(context.Background(), []string{"test-grok-state"})
	assertPool(true)
	// Explicit revalidation must preserve an administrator's disabled setting.
	_, _ = s.grokRuntimeStore.UpdateAccountRuntime("test-grok-state", map[string]any{"enabled": false, "status": "disabled"})
	status.Store(200)
	verify()
	assertPool(true)
}
