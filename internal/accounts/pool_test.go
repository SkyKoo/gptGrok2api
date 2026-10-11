package accounts

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func TestPoolRoundRobinAndFailureCooldown(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{
		{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"},
		{"access_token": "two", "pool": "basic", "enabled": true, "status": "正常"},
	}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	first, err := pool.Reserve(context.Background(), []string{"basic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(first)
	pool.Feedback(first.Account, 429, nil)
	second, err := pool.Reserve(context.Background(), []string{"basic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release(second)
	if second.Account.Token == first.Account.Token {
		t.Fatalf("cooling account selected again: %s", second.Account.Token)
	}
}

func TestPoolFeedbackHonorsUpstreamRetryWindow(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"}}); err != nil {
		t.Fatal(err)
	}
	p := New(repository)
	account := Account{Token: "one", Pool: "basic", Fields: map[string]any{"email": "one@example.test"}}
	before := time.Now().Add(19 * time.Hour)
	p.Feedback(account, 429, errors.New("{\"message\":\"Please try again in 20 hours.\"}"))
	if until := p.cooldowns[account.Token]; until.Before(before) {
		t.Fatalf("expected hour-scale cooldown, got %v", until)
	}
	items, err := repository.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["cooldown_until"] == nil || items[0]["cooldown_until"] == "" {
		t.Fatalf("expected persisted cooldown_until, got %#v", items)
	}
}

func TestPoolFeedbackInvokesInvalidCallbackOnlyForAuthFailures(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"}}); err != nil {
		t.Fatal(err)
	}
	p := New(repository)
	called := 0
	p.SetInvalidCallback(func(account Account) {
		called++
		if account.Token != "one" {
			t.Errorf("unexpected callback account: %#v", account)
		}
	})
	account := Account{Token: "one", Pool: "basic", Fields: map[string]any{"email": "one@example.test"}}
	p.Feedback(account, 502, errors.New("upstream error"))
	if called != 0 {
		t.Fatalf("temporary upstream error invoked invalid callback %d times", called)
	}
	p.Feedback(account, 403, errors.New("cloudflare forbidden"))
	if called != 0 {
		t.Fatalf("temporary forbidden response invoked invalid callback %d times", called)
	}
	items, err := repository.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	if got := items[0]["status"]; got != "正常" {
		t.Fatalf("403 marked account abnormal: %#v", items[0])
	}
	p.Feedback(account, 401, errors.New("unauthorized"))
	if called != 1 {
		t.Fatalf("expected one invalid callback, got %d", called)
	}
}

func TestPoolSuccessfulFeedbackDoesNotRestoreAbnormalAccount(t *testing.T) {
	for _, staleSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprint(staleSnapshot), func(t *testing.T) {
			p, repository := quotaPool(t, 50, 25, "正常")
			rows, _ := repository.AccountList()
			snapshot := rows[0]
			// An earlier request was dispatched before authentication failed.
			_, _, err := repository.UpdateAccount("token", map[string]any{
				"status": "异常", "status_reason_code": "auth_invalid", "last_error_kind": "auth_invalid",
				"last_error_status": 401, "last_error_message": "expired", "invalid_count": 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if staleSnapshot {
				// It carries a transient error to exercise atomic feedback filtering.
				snapshot["last_error_kind"] = "upstream_error"
			} else {
				rows, _ = repository.AccountList()
				snapshot = rows[0]
			}
			p.Feedback(Account{Token: "token", Pool: "basic", Fields: snapshot}, 200, nil)
			rows, _ = repository.AccountList()
			if rows[0]["status"] != "异常" || rows[0]["last_error_kind"] != "auth_invalid" {
				t.Fatal("delayed success restored abnormal account")
			}
			if l, e := reserveIntent(p, Intent{Kind: "chat"}); !errors.Is(e, ErrUnavailable) {
				p.Release(l)
				t.Fatalf("abnormal account selected: %v", e)
			}
		})
	}
}

func TestPoolSuccessfulFeedbackClearsTransientErrorsOnNormalAccount(t *testing.T) {
	p, repository := quotaPool(t, 50, 25, "正常")
	rows, _ := repository.AccountList()
	a := Account{Token: "token", Fields: rows[0]}
	p.Feedback(a, 503, errors.New("temporary service failure"))
	rows, _ = repository.AccountList()
	a.Fields = rows[0]
	p.Feedback(a, 200, nil)
	rows, _ = repository.AccountList()
	if rows[0]["status"] != "正常" || stringValue(rows[0]["last_error_kind"]) != "" {
		t.Fatal("normal account kept transient error")
	}
}

func TestPoolAbnormalNeverUsesCachedQuotaOrSurvival(t *testing.T) {
	markers := []map[string]any{}
	for _, status := range []string{"异常", "abnormal", "invalid", "error", "incomplete", "expired", "unauthorized", "disabled", "auto_disabled", "禁用"} {
		markers = append(markers, map[string]any{"status": status})
	}
	markers = append(markers, map[string]any{"last_error_kind": "auth_invalid"}, map[string]any{"last_error_kind": "parse_failure"}, map[string]any{"status_reason_code": "auth_invalid"}, map[string]any{"status_reason_code": "account_invalid"}, map[string]any{"relogin_blocked": true})
	for _, marker := range markers {
		t.Run(fmt.Sprint(marker), func(t *testing.T) {
			p, repository := quotaPool(t, 50, 25, "正常")
			marker["survival_alive"] = true
			if _, _, e := repository.UpdateAccount("token", marker); e != nil {
				t.Fatal(e)
			}
			for _, intent := range []Intent{{}, {Kind: "chat"}, {Kind: "image"}} {
				l, e := reserveIntent(p, intent)
				if !errors.Is(e, ErrUnavailable) {
					p.Release(l)
					t.Fatalf("selected abnormal/disabled account for %q: %v", intent.Kind, e)
				}
			}
		})
	}
}

func TestPoolReloginBlockPersistsUntilVerifiedRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s := store.New(path, "", "")
	if err := s.SaveAccounts([]map[string]any{{"access_token": "token", "status": "异常", "quota": 25, "survival_alive": true, "relogin_blocked": true, "custom": "preserve"}}); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	rows, _ := s.AccountList()
	a := Account{Token: "token", Fields: rows[0]}
	if err := p.RefreshCapabilities(a, func(Account) (map[string]any, error) {
		return map[string]any{"capability_quotas": Quotas([]any{map[string]any{"feature_name": "reason", "remaining": 50}, map[string]any{"feature_name": "image_gen", "remaining": 25}}, time.Now())}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushAccounts(); err != nil {
		t.Fatal(err)
	}
	reopened := store.New(path, "", "")
	p = New(reopened)
	if l, err := reserveIntent(p, Intent{Kind: "image"}); !errors.Is(err, ErrUnavailable) {
		p.Release(l)
		t.Fatalf("quota refresh or restart unblocked account: %v", err)
	}
	if _, _, err := reopened.RotateAccountTokens("token", "verified", "", "", map[string]any{"status": "正常"}); err != nil {
		t.Fatal(err)
	}
	l, err := reserveIntent(p, Intent{Kind: "image"})
	if err != nil {
		t.Fatalf("verified recovery still unavailable: %v", err)
	}
	defer p.Release(l)
	if l.Account.Token != "verified" || l.Account.Fields["custom"] != "preserve" || l.Account.Fields["relogin_blocked"] == true {
		t.Fatal("recovery corrupted account")
	}
}

func TestPoolConcurrencyLimitWaitsForAccountRelease(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	if err := repository.SaveAccounts([]map[string]any{{"access_token": "one", "pool": "basic", "enabled": true, "status": "正常"}}); err != nil {
		t.Fatal(err)
	}
	pool := New(repository)
	first, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan *Lease, 1)
	errorsOut := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		lease, reserveErr := pool.ReserveMatchingLimit(ctx, []string{"basic"}, nil, nil, 1)
		if reserveErr != nil {
			errorsOut <- reserveErr
			return
		}
		acquired <- lease
	}()
	select {
	case lease := <-acquired:
		pool.Release(lease)
		t.Fatal("second request bypassed the per-account concurrency limit")
	case err := <-errorsOut:
		t.Fatalf("second request failed instead of waiting: %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	pool.Release(first)
	select {
	case lease := <-acquired:
		pool.Release(lease)
	case err := <-errorsOut:
		t.Fatalf("waiting request failed after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("waiting request was not woken after account release")
	}
}

func BenchmarkPoolReserveWithTwoThousandCachedAccounts(b *testing.B) {
	root := b.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	items := make([]map[string]any, 2000)
	for index := range items {
		items[index] = map[string]any{"access_token": fmt.Sprintf("token-%04d", index), "pool": "basic", "enabled": true, "status": "正常"}
	}
	if err := repository.SaveAccounts(items); err != nil {
		b.Fatal(err)
	}
	pool := New(repository)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		lease, err := pool.ReserveMatchingLimit(context.Background(), []string{"basic"}, nil, nil, 1)
		if err != nil {
			b.Fatal(err)
		}
		pool.Release(lease)
	}
}

func TestPoolSkipsExpiredJWTWithoutChangingAccounts(t *testing.T) {
	root := t.TempDir()
	repository := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "keys.json"), filepath.Join(root, "config.json"))
	jwt := func(exp int64) string {
		return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp))) + ".signature"
	}
	expired, valid := jwt(time.Now().Add(-time.Hour).Unix()), jwt(time.Now().Add(time.Hour).Unix())
	items := []map[string]any{{"access_token": expired, "status": "正常", "quota": 25}, {"access_token": valid, "status": "正常"}}
	if err := repository.SaveAccounts(items); err != nil {
		t.Fatal(err)
	}
	p := New(repository)
	p.SetInvalidCallback(func(Account) { t.Error("offline expiry triggered re-login") })
	for i := 0; i < 4; i++ {
		lease, err := p.Reserve(context.Background(), []string{"basic"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Account.Token != valid {
			t.Fatal("expired credential consumed a lease")
		}
		p.Release(lease)
	}
	stored, err := repository.AccountList()
	if err != nil || len(stored) != 2 || stored[0]["status"] != "正常" {
		t.Fatalf("offline check modified stored accounts: %v", err)
	}
	if err := repository.SaveAccounts(items[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Reserve(context.Background(), []string{"basic"}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("all expired: %v", err)
	}
	// Replacing the credential invalidates the cached expiry without restarting.
	if err := repository.SaveAccounts(items[1:]); err != nil {
		t.Fatal(err)
	}
	lease, err := p.Reserve(context.Background(), []string{"basic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(lease)
}

func TestJWTExpiryEligibility(t *testing.T) {
	now := time.Unix(100, 500000000)
	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{`{"exp":99}`, false}, {`{"exp":100.5}`, false}, {`{"exp":100.6}`, true},
		{`{"exp":0}`, false}, {`{"exp":-1}`, false}, {`{"exp":101}`, true},
		{`{}`, true}, {`{"exp":null}`, true}, {`{"exp":"100"}`, true}, {`bad`, true},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(tc.payload)) + ".signature"
			account, _ := normalize(map[string]any{"access_token": token, "status": "正常"})
			if got := New(nil).available(account, []string{"basic"}, now); got != tc.want {
				t.Fatalf("available=%v want=%v", got, tc.want)
			}
		})
	}
	for _, token := range []string{"opaque-grok-token", "malformed.jwt.token"} {
		account, _ := normalize(map[string]any{"access_token": token})
		if !New(nil).available(account, []string{"basic"}, now) {
			t.Fatal("opaque token behavior changed")
		}
	}
}
