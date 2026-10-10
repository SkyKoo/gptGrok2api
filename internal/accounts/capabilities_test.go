package accounts

import (
	"context"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/store"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func quotaPool(t *testing.T, reason, image int, status string) (*Pool, *store.Store) {
	t.Helper()
	root := t.TempDir()
	s := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	err := s.SaveAccounts([]map[string]any{{"access_token": "token", "user_id": "stable", "enabled": true, "status": status, "quota": image, "limits_progress": []any{map[string]any{"feature_name": "reason", "remaining": reason}, map[string]any{"feature_name": "image_gen", "remaining": image}}}})
	if err != nil {
		t.Fatal(err)
	}
	return New(s), s
}
func reserveIntent(p *Pool, i Intent) (*Lease, error) {
	return p.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, i)
}
func TestQuotaImageExhaustionDoesNotDisableText(t *testing.T) {
	p, _ := quotaPool(t, 2, 0, "限流")
	if _, e := reserveIntent(p, Intent{Kind: "image"}); !errors.Is(e, ErrUnavailable) {
		t.Fatalf("image: %v", e)
	}
	l, e := reserveIntent(p, Intent{Kind: "chat"})
	if e != nil {
		t.Fatal(e)
	}
	p.Release(l)
	for _, status := range []string{"禁用", "异常", "invalid"} {
		p, _ := quotaPool(t, 2, 0, status)
		if _, e := reserveIntent(p, Intent{Kind: "chat"}); !errors.Is(e, ErrUnavailable) {
			t.Fatalf("auth/disabled %s: %v", status, e)
		}
	}
}
func TestQuotaAtomicReservationAndRestart(t *testing.T) {
	p, s := quotaPool(t, 1, 1, "正常")
	var mu sync.Mutex
	var got []*Lease
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, e := reserveIntent(p, Intent{Kind: "image"})
			if e == nil {
				mu.Lock()
				got = append(got, l)
				mu.Unlock()
			} else if !errors.Is(e, ErrUnavailable) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if len(got) != 1 {
		t.Fatalf("oversubscription: %d", len(got))
	}
	// A crash before reconciliation never refunds the durable reservation.

	p.MarkSent(got[0], "reason")
	p.MarkSent(got[0], "image_gen")
	p.Release(got[0])
	if err := s.FlushAccounts(); err != nil {
		t.Fatal(err)
	}
	restarted := New(s)
	if _, e := reserveIntent(restarted, Intent{Kind: "image"}); !errors.Is(e, ErrUnavailable) {
		t.Fatal("ambiguous attempt refunded")
	}
	err := restarted.RefreshCapabilities(got[0].Account, func(Account) (map[string]any, error) {
		return map[string]any{"capability_quotas": Quotas([]any{map[string]any{"feature_name": "reason", "remaining": 0}, map[string]any{"feature_name": "image_gen", "remaining": 0}}, time.Now())}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, e := reserveIntent(restarted, Intent{Kind: "image"}); !errors.Is(e, ErrUnavailable) {
		t.Fatal("invented quota")
	}
}
func TestQuotaPreSendReleaseAndRefreshIsolation(t *testing.T) {
	p, _ := quotaPool(t, 1, 1, "正常")
	l, e := reserveIntent(p, Intent{Kind: "chat"})
	if e != nil {
		t.Fatal(e)
	}
	called := false
	_ = p.RefreshCapabilities(l.Account, func(Account) (map[string]any, error) { called = true; return nil, nil })
	if called {
		t.Fatal("snapshot read while work active")
	}
	p.Release(l)
	l, e = reserveIntent(p, Intent{Kind: "chat"})
	if e != nil {
		t.Fatal("pre-send failure not released", e)
	}
	p.Release(l)
	started, finish := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.RefreshCapabilities(l.Account, func(Account) (map[string]any, error) { close(started); <-finish; return nil, errors.New("offline") })
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, e := p.ReserveIntent(ctx, []string{"basic"}, nil, nil, 0, Intent{Kind: "chat"}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("lease raced snapshot", e)
	}
	close(finish)
	<-done
	l, e = reserveIntent(p, Intent{Kind: "chat"})
	if e != nil {
		t.Fatal("refresh failure erased last good state", e)
	}
	p.Release(l)
}
func TestQuotaCooldownScopedAndRotation(t *testing.T) {
	p, s := quotaPool(t, 10, 10, "正常")
	l, e := reserveIntent(p, Intent{Kind: "image", Model: "gpt-5-3"})
	if e != nil {
		t.Fatal(e)
	}
	p.Release(l)
	p.FeedbackIntent(l, 429, errors.New("image_gen quota exceeded"))
	if _, e := reserveIntent(p, Intent{Kind: "image", Model: "gpt-5-3"}); !errors.Is(e, ErrUnavailable) {
		t.Fatal("image block lost")
	}
	chat, e := reserveIntent(p, Intent{Kind: "chat", Model: "auto"})
	if e != nil {
		t.Fatal("image block affected chat", e)
	}
	p.Release(chat)
	_, _, e = s.RotateAccountTokens("token", "rotated", "", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	rows, _ := s.AccountList()
	a, _ := normalize(rows[0])
	if Identity(a) != Identity(l.Account) {
		t.Fatal("identity changed after rotation")
	}
	if e = p.RefreshCapabilities(l.Account, func(a Account) (map[string]any, error) {
		if a.Token != "rotated" {
			t.Fatal("old credential used")
		}
		return map[string]any{"capability_quotas": Quotas(nil, time.Now())}, nil
	}); e != nil {
		t.Fatal(e)
	}
}
func TestUnknownQuotaAndResetAreNotInvented(t *testing.T) {
	q := Quotas([]any{map[string]any{"feature_name": "reason", "remaining": 0, "reset_after": 60}}, time.Now())
	if q["image_gen"].Remaining != nil || q["reason"].Remaining == nil || *q["reason"].Remaining != 0 {
		t.Fatal(q)
	}
	p, _ := quotaPool(t, 0, 2, "正常")
	if _, e := reserveIntent(p, Intent{Kind: "chat"}); !errors.Is(e, ErrUnavailable) {
		t.Fatal("zero treated as unknown")
	}
}

func TestDurableQuotaSurvivesDiskReloadAndLegacyFields(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "accounts.json")
	old := map[string]any{"access_token": "original", "user_id": "one", "quota": 3, "status": "正常", "limits_progress": []any{map[string]any{"feature_name": "reason", "remaining": 1}}, "custom_field": "preserve"}
	s := store.New(path, "", "")
	if e := s.SaveAccounts([]map[string]any{old}); e != nil {
		t.Fatal(e)
	}
	p := New(s)
	l, e := reserveIntent(p, Intent{Kind: "chat"})
	if e != nil {
		t.Fatal(e)
	}
	reopened := store.New(path, "", "")
	rows, e := reopened.AccountList()
	if e != nil {
		t.Fatal(e)
	}
	if rows[0]["custom_field"] != "preserve" || rows[0]["access_token"] != "original" || intValue(rows[0]["quota"]) != 3 {
		t.Fatal("legacy data changed")
	}
	if _, e := reserveIntent(New(reopened), Intent{Kind: "chat"}); !errors.Is(e, ErrUnavailable) {
		t.Fatal("restart refunded pending quota", e)
	}
	p.Release(l)
}
