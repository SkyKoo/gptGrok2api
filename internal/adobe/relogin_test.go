package adobe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func verifiedBrowserAccount(a Account) Account {
	a.MediaSessionRefreshed = true
	a.MediaSessionStatus = "complete"
	return a
}

func TestReloginForcesBrowserAndPreservesUnknownReservation(t *testing.T) {
	s, a := readyStore(t)
	if _, err := s.Reserve("unknown-task"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveJob("unknown-task", Job{Kind: "video", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/existing", Status: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnknown("unknown-task"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get(a.ID)
	history, _ := os.ReadFile(s.jobPath("unknown-task"))
	calls := 0
	s.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
		calls++
		return verifiedBrowserAccount(current), nil
	})
	fresh, err := s.Relogin(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || fresh.Revision != before.Revision+1 || fresh.MediaSessionStatus != "refreshed" || fresh.MediaSessionUpdatedAt == 0 {
		t.Fatal("normal browser relogin was not verified")
	}
	if fresh.Status != before.Status || fresh.InFlight != before.InFlight || fresh.LastError != before.LastError || fresh.SettlementUntil != before.SettlementUntil || !reflect.DeepEqual(fresh.Quota, before.Quota) {
		t.Fatal("relogin changed pending submission state")
	}
	afterHistory, _ := os.ReadFile(s.jobPath("unknown-task"))
	state, _ := s.load()
	if state.Bindings["unknown-task"] != a.ID || !bytes.Equal(history, afterHistory) {
		t.Fatal("relogin changed task history or binding")
	}
	pub := Public(fresh)
	if pub["status"] != "submission_unknown" || pub["submission_unknown"] != true || pub["in_flight"] != true {
		t.Fatal("unknown task projected as normal generation")
	}
	if _, err := s.Reserve("unknown-task"); err == nil {
		t.Fatal("relogin allowed original task replay")
	}
	if _, err := s.Reserve("new-task"); !errors.Is(err, ErrNoAccount) {
		t.Fatal("relogin released account for another task")
	}
	fresh.Enabled = false
	if Public(fresh)["submission_unknown"] != true {
		t.Fatal("disabled projection concealed unknown task")
	}
}

func TestReloginRejectsActiveGenerationAndSettlement(t *testing.T) {
	for _, state := range []string{"generating", "settlement"} {
		t.Run(state, func(t *testing.T) {
			s, a := readyStore(t)
			s.SetBrowserRefresher(func(context.Context, Account) (Account, error) {
				t.Error("busy account reached browser")
				return Account{}, nil
			})
			if _, err := s.Reserve("active-task"); err != nil {
				t.Fatal(err)
			}
			if state == "settlement" {
				if err := s.Release("active-task"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Relogin(context.Background(), a.ID); !errors.Is(err, ErrBusy) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestReloginFailureNeverReplacesSavedCredentials(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Account)
		err    error
		want   string
	}{
		{name: "missing readiness evidence", change: func(a *Account) { a.MediaSessionRefreshed = false }, want: "adobe_relogin_media_session_incomplete"},
		{name: "retained cookie is not verified", change: func(a *Account) { a.MediaSessionStatus = "retained_previous" }, want: "adobe_relogin_media_session_incomplete"},
		{name: "missing media cookie", change: func(a *Account) { a.Cookie = "partial=value" }, want: "adobe_relogin_media_session_incomplete"},
		{name: "different account", change: func(a *Account) { a.AccountID = "other" }, want: ErrIdentity.Error()},
		{name: "different email", change: func(a *Account) { a.Email = "other@example.test" }, want: ErrIdentity.Error()},
		{name: "invalid token", change: func(a *Account) { a.Token = "invalid" }, want: "adobe_relogin_token_invalid"},
		{name: "expired token", change: func(a *Account) {
			claims, _ := json.Marshal(map[string]any{"user_id": a.AccountID, "exp": time.Now().Add(-time.Hour).Unix()})
			a.Token = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
		}, want: "adobe_relogin_token_expired"},
		{name: "verification required", err: &Error{Stage: "relogin", Code: "verification_required"}, want: "adobe_relogin_verification_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a := readyStore(t)
			before, _ := os.ReadFile(s.path)
			s.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
				current = verifiedBrowserAccount(current)
				if tc.change != nil {
					tc.change(&current)
				}
				return current, tc.err
			})
			_, err := s.Relogin(context.Background(), a.ID)
			if SafeError(err) != tc.want {
				t.Fatalf("got %v want %s", err, tc.want)
			}
			after, _ := os.ReadFile(s.path)
			if !bytes.Equal(before, after) {
				t.Fatal("failed relogin changed stored account")
			}
		})
	}
}

func TestReloginMaintenanceBlocksConcurrentWorkAndPreservesDisabled(t *testing.T) {
	s, a := readyStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	s.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
		close(entered)
		<-release
		return verifiedBrowserAccount(current), nil
	})
	done := make(chan error, 1)
	go func() { _, err := s.Relogin(context.Background(), a.ID); done <- err }()
	<-entered
	if _, err := s.Relogin(context.Background(), a.ID); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent relogin allowed")
	}
	if _, err := s.Refresh(context.Background(), NewClient(nil), a.ID, true); !errors.Is(err, ErrBusy) {
		t.Fatal("refresh crossed maintenance")
	}
	if _, err := s.Import([]Account{a}); !errors.Is(err, ErrBusy) {
		t.Fatal("import crossed maintenance")
	}
	if _, err := s.Delete([]string{a.ID}); !errors.Is(err, ErrBusy) {
		t.Fatal("delete crossed maintenance")
	}
	if _, err := s.Reserve("parallel"); !errors.Is(err, ErrNoAccount) {
		t.Fatal("generation crossed maintenance")
	}
	if err := s.SetEnabled([]string{a.ID}, false); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	fresh, _ := s.Get(a.ID)
	if fresh.Enabled {
		t.Fatal("relogin reenabled manually disabled account")
	}
	if _, err := s.Relogin(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestReloginCASAndCancellationPreventStaleCommit(t *testing.T) {
	for _, mode := range []string{"revision", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, a := readyStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
				if mode == "revision" {
					newer := current
					newer.Cookie += "; newSession=yes"
					if _, err := s.ReplaceCredentials(current, newer); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
				current.Cookie += "; staleSession=yes"
				return verifiedBrowserAccount(current), nil
			})
			_, err := s.Relogin(ctx, a.ID)
			if mode == "revision" && !errors.Is(err, ErrRevision) {
				t.Fatal("stale relogin overwritten newer credentials")
			}
			if mode == "cancel" && SafeError(err) != "adobe_relogin_cancelled" {
				t.Fatal("cancelled relogin committed")
			}
			got, _ := s.Get(a.ID)
			want := a.Cookie
			if mode == "revision" {
				want += "; newSession=yes"
			}
			if got.Cookie != want {
				t.Fatal("invalid final credential")
			}
			s.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
				return verifiedBrowserAccount(current), nil
			})
			if _, err := s.Relogin(context.Background(), a.ID); err != nil {
				t.Fatal("maintenance was not released", err)
			}
		})
	}
}
