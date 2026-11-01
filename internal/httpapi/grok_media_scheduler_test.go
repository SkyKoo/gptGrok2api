package httpapi

import (
	"context"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testGrokMediaSnapshot() map[string]any {
	return map[string]any{"status": "success", "checked_at": time.Now().UTC().Format(time.RFC3339Nano), "quotas": map[string]any{"imagePro": map[string]any{"available": true, "remaining": 10}, "video720p": map[string]any{"available": true, "remaining": 10}}}
}
func TestGrokMediaRequestPreflightAndMaintenance(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/rest/media/imagine/quota_info" {
			t.Error("unexpected endpoint")
		}
		w.Write([]byte(`{"imagePro":{"available":true,"remainingQueries":2},"video720p":{"available":true,"remainingQueries":1}}`))
	}))
	defer upstream.Close()
	cfg := adminTestConfig(t.TempDir())
	cfg.GrokRateLimitsURL = upstream.URL + "/rest/rate-limits"
	s := New(cfg)
	_, _, _, _ = s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "media-test", "enabled": true, "status": "active"}, {"access_token": "invalid-test", "enabled": true, "status": "invalid"}})
	s.ensureGrokMediaQuotas(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	s.ensureGrokMediaQuotas(context.Background())
	if calls.Load() != 1 {
		t.Fatal("fresh snapshot queried again")
	}
	l, err := s.grokAccountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: "image", Model: "grok-imagine-image-edit"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Account.Token != "media-test" {
		t.Fatal("refresh sorting changed account identity")
	}
	s.grokAccountPool.MarkSent(l, "image_gen")
	s.grokAccountPool.Release(l)
	s.ensureGrokMediaQuotas(context.Background())
	if calls.Load() != 1 {
		t.Fatal("settlement queried too soon")
	}
	_, _, _ = s.grokRuntimeStore.UpdateAccount("media-test", map[string]any{"media_quota_not_before": time.Now().Add(-time.Minute).Format(time.RFC3339), "media_quota_last_attempt_at": time.Now().Add(-time.Minute).Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "cfm-maintenance"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	s.ensureGrokMediaQuotas(context.Background())
	if calls.Load() != 1 {
		t.Fatal("maintenance ignored")
	}
	if _, err = s.grokAccountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: "image", Model: "grok-imagine-image-pro"}); !errors.Is(err, accounts.ErrUnavailable) {
		rows, _, _ := s.grokRuntimeStore.AccountSnapshot()
		t.Fatalf("dirty snapshot leased err=%v account_count=%d", err, len(rows))
	}
}
