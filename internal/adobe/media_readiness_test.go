package adobe

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func idleMediaStore(t *testing.T) (*Store, Account) {
	t.Helper()
	s, a := readyStore(t)
	if err := s.update(a.ID, 0, func(a *Account) { a.ProfileID = "fixture-profile" }); err != nil {
		t.Fatal(err)
	}
	a, _ = s.Get(a.ID)
	return s, a
}

func mediaDirectResolver(ctx context.Context, _ Account) (context.Context, error) {
	return proxyruntime.WithURL(ctx, ""), nil
}

func TestMediaReadyRequiresTrustedCollectionAndFixedEgress(t *testing.T) {
	s, a := idleMediaStore(t)
	if s.MediaSessionReady(mediaTestContext(), a, 0) {
		t.Fatal("imported session trusted")
	}
	if _, err := s.WarmMediaSession(context.Background(), a.ID, a.Revision); SafeError(err) != "adobe_media_prepare_egress_required" {
		t.Fatal(err)
	}
	calls := 0
	s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) { calls++; return freshMediaAccount(a), nil })
	fresh, err := s.WarmMediaSession(mediaTestContext(), a.ID, a.Revision)
	if err != nil || calls != 1 || !s.MediaSessionReady(mediaTestContext(), fresh, time.Minute) {
		t.Fatal("warm session not ready", err)
	}
	ready := s.mediaReady[a.ID]
	if _, err := s.WarmMediaSession(mediaTestContext(), fresh.ID, fresh.Revision); err != nil || calls != 1 {
		t.Fatal("fresh session rewarmed", err)
	}
	if !reflect.DeepEqual(ready, s.mediaReady[a.ID]) {
		t.Fatal("reuse extended window")
	}
	if s.MediaSessionReady(proxyruntime.WithURL(context.Background(), "http://changed.invalid:8080"), fresh, 0) {
		t.Fatal("different actual default exit reused cache")
	}
	if NewStore(s.path, s.secret).MediaSessionReady(mediaTestContext(), fresh, 0) {
		t.Fatal("restart trusted persisted collection flags")
	}
	if s.MediaSessionReady(mediaTestContext(), a, 0) {
		t.Fatal("old credential revision trusted")
	}
	if s.MediaPreparationDue(mediaTestContext(), fresh) {
		t.Fatal("fresh session reported due")
	}
	s.mu.Lock()
	r := s.mediaReady[a.ID]
	r.ReusableTo = time.Now().Add(30 * time.Second)
	s.mediaReady[a.ID] = r
	s.mu.Unlock()
	if !s.MediaSessionReady(mediaTestContext(), fresh, 0) || !s.MediaPreparationDue(mediaTestContext(), fresh) {
		t.Fatal("warm lead did not distinguish usable from refresh due")
	}
	s.mu.Lock()
	r.ReadyAt = time.Now().Add(time.Minute)
	s.mediaReady[a.ID] = r
	s.mu.Unlock()
	if s.MediaSessionReady(mediaTestContext(), fresh, 0) {
		t.Fatal("clock rollback reused session")
	}
}

func TestMediaReadySurvivesSuccessfulTaskAndQuotaOnlyMaintenance(t *testing.T) {
	s, a := idleMediaStore(t)
	calls := 0
	s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) { calls++; return freshMediaAccount(a), nil })
	a, err := s.WarmMediaSession(mediaTestContext(), a.ID, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	ready := s.mediaReady[a.ID]
	first, firstCtx, err := s.ReserveMediaForModel(context.Background(), "first", "", mediaDirectResolver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareMediaForTask(firstCtx, "first", first.ID, first.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("first"); err != nil {
		t.Fatal(err)
	}
	if err := s.update(a.ID, 0, func(a *Account) { a.SettlementUntil = 0 }); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateQuota(a.ID, a.Revision, Quota{Status: "known", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateGenerationPermissions(a.ID, a.Revision, GenerationEntitlements{Status: "known"}); err != nil {
		t.Fatal(err)
	}
	second, secondCtx, err := s.ReserveMediaForModel(context.Background(), "second", "", mediaDirectResolver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareMediaForTask(secondCtx, "second", second.ID, second.Revision); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !reflect.DeepEqual(ready, s.mediaReady[a.ID]) {
		t.Fatal("success/quota read lost cache or extended TTL")
	}
	d, _ := s.load()
	if d.PreparedMediaTasks["first"] != a.Revision || d.PreparedMediaTasks["second"] != a.Revision {
		t.Fatal("cache hit did not durably mark each task")
	}
	if _, err := s.PrepareMediaForTask(secondCtx, "second", second.ID, second.Revision); SafeError(err) != "adobe_media_prepare_already_prepared" {
		t.Fatal("task replay permitted", err)
	}
}

func TestMediaReadyInvalidationIsRevisionScopedAndUnknownInvalidates(t *testing.T) {
	s, a := reservedMediaStore(t)
	calls := 0
	s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) { calls++; return freshMediaAccount(a), nil })
	a, err := s.PrepareMediaForTask(mediaTestContext(), a.InFlight, a.ID, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	s.InvalidateMediaSession(a.ID, a.Revision-1)
	if !s.MediaSessionReady(mediaTestContext(), a, 0) {
		t.Fatal("old failure invalidated new revision")
	}
	s.InvalidateMediaSession(a.ID, a.Revision)
	if s.MediaSessionReady(mediaTestContext(), a, 0) {
		t.Fatal("same revision not invalidated")
	}
	if _, err := s.PrepareMediaForTask(mediaTestContext(), a.InFlight, a.ID, a.Revision); SafeError(err) != "adobe_media_prepare_already_prepared" {
		t.Fatal("invalidated task replayed")
	}
	if err := s.Release(a.InFlight); err != nil {
		t.Fatal(err)
	}
	_ = s.update(a.ID, 0, func(a *Account) { a.SettlementUntil = 0 })
	a, ctx, err := s.ReserveMediaForModel(context.Background(), "new-after-failure", "", mediaDirectResolver)
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.PrepareMediaForTask(ctx, a.InFlight, a.ID, a.Revision)
	if err != nil || calls != 2 {
		t.Fatal("new task did not recollect", err)
	}
	if err := s.MarkUnknown(a.InFlight); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.mediaReady[a.ID]; ok {
		t.Fatal("unknown submission retained reusable session")
	}
}

func TestMediaWarmFailureBackoffPersistsWithoutChangingAccount(t *testing.T) {
	for _, code := range []string{"browser_failed", "verification_required"} {
		t.Run(code, func(t *testing.T) {
			s, a := idleMediaStore(t)
			if err := s.update(a.ID, 0, func(a *Account) { a.RefreshCookie = "ims=fixture" }); err != nil {
				t.Fatal(err)
			}
			a, _ = s.Get(a.ID)
			calls := 0
			s.SetBrowserRefresher(func(context.Context, Account) (Account, error) {
				calls++
				return Account{}, &Error{Stage: "relogin", Code: code}
			})
			if _, err := s.WarmMediaSession(mediaTestContext(), a.ID, a.Revision); err == nil {
				t.Fatal("browser failure accepted")
			}
			got, _ := s.Get(a.ID)
			if !reflect.DeepEqual(a, got) {
				t.Fatal("warm failure changed credentials or quota")
			}
			d, _ := s.load()
			retry := d.MediaFailures[a.ID]
			if retry.RetryAfter < time.Now().Add(4*time.Minute).Unix() {
				t.Fatal("failure backoff missing")
			}
			if code == "verification_required" && retry.RetryAfter < time.Now().Add(14*time.Minute).Unix() {
				t.Fatal("verification backoff too short")
			}
			s = NewStore(s.path, s.secret)
			s.SetBrowserRefresher(func(context.Context, Account) (Account, error) { calls++; return Account{}, nil })
			if s.MediaPreparationDue(mediaTestContext(), a) {
				t.Fatal("backoff ignored after restart")
			}
			if _, err := s.WarmMediaSession(mediaTestContext(), a.ID, a.Revision); SafeError(err) != "adobe_media_prepare_cooldown" || calls != 1 {
				t.Fatal("restarted warm bypassed backoff", err)
			}
			if _, _, err := s.ReserveMediaForModel(context.Background(), "backoff-request", "", mediaDirectResolver); !errors.Is(err, ErrNoAccount) {
				t.Fatal("request bypassed backoff", err)
			}
		})
	}
}

func TestMediaWarmSingleflightAndReservationWait(t *testing.T) {
	s, a := idleMediaStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) {
		calls.Add(1)
		close(entered)
		<-release
		return freshMediaAccount(a), nil
	})
	warm := make(chan error, 1)
	go func() { _, err := s.WarmMediaSession(mediaTestContext(), a.ID, a.Revision); warm <- err }()
	<-entered
	if _, err := s.WarmMediaSession(proxyruntime.WithURL(context.Background(), "http://another.invalid:8080"), a.ID, a.Revision); !errors.Is(err, ErrBusy) {
		t.Fatal("different exit joined flight")
	}
	joined := make(chan error, 1)
	go func() { _, err := s.WarmMediaSession(mediaTestContext(), a.ID, a.Revision); joined <- err }()
	reserved := make(chan error, 1)
	go func() {
		account, ctx, err := s.ReserveMediaForModel(context.Background(), "wait-for-warm", "", mediaDirectResolver)
		if err == nil {
			_, err = s.PrepareMediaForTask(ctx, account.InFlight, account.ID, account.Revision)
		}
		reserved <- err
	}()
	select {
	case err := <-reserved:
		t.Fatalf("reservation did not wait for warm: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-warm; err != nil {
		t.Fatal(err)
	}
	if err := <-joined; err != nil && !errors.Is(err, ErrRevision) && !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err := <-reserved; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("warm/request opened duplicate browsers")
	}
}

func TestMediaReservationWaitsForAccountMaintenanceAndCancellation(t *testing.T) {
	for _, mode := range []string{"maintenance", "quota", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, a := idleMediaStore(t)
			s.mu.Lock()
			if mode == "quota" {
				s.quotaQueries[a.ID] = true
			} else {
				s.maintenance[a.ID] = true
			}
			s.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := s.ReserveMediaForModel(ctx, "waiting-reservation", "", mediaDirectResolver)
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("busy account rejected without waiting: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			s.endAccountMaintenance("unrelated-account")
			select {
			case err := <-done:
				t.Fatalf("unrelated account completion woke reservation: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			if mode == "cancel" {
				cancel()
			} else if mode == "quota" {
				s.endAccountQuotaQuery(a.ID)
			} else {
				s.endAccountMaintenance(a.ID)
			}
			err := <-done
			if mode == "cancel" {
				if SafeError(err) != "adobe_media_prepare_cancelled" {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMediaReservationReloadsChangedSnapshotAndPreservesRoutingErrors(t *testing.T) {
	s, a := idleMediaStore(t)
	calls := 0
	resolver := func(ctx context.Context, account Account) (context.Context, error) {
		calls++
		if calls == 1 {
			if err := s.UpdateQuota(a.ID, a.Revision, a.Quota); err != nil {
				t.Fatal(err)
			}
		}
		return mediaDirectResolver(ctx, account)
	}
	if _, _, err := s.ReserveMediaForModel(context.Background(), "snapshot-change", "", resolver); err != nil || calls != 2 {
		t.Fatal("quota drift caused false no-account", err)
	}
	s, _ = idleMediaStore(t)
	_, _, err := s.ReserveMediaForModel(context.Background(), "bad-routing", "", func(ctx context.Context, a Account) (context.Context, error) {
		return ctx, &Error{Stage: "routing", Code: "proxy_unavailable"}
	})
	if SafeError(err) != "adobe_routing_proxy_unavailable" {
		t.Fatal("routing error lost", err)
	}
	_, _, err = s.ReserveMediaForModel(context.Background(), "unfixed-routing", "", func(ctx context.Context, a Account) (context.Context, error) { return ctx, nil })
	if SafeError(err) != "adobe_media_prepare_egress_required" {
		t.Fatal("unfixed route treated as direct", err)
	}
}

func TestMediaReservationPrefersReadyAndFiltersProfileless(t *testing.T) {
	s, a := idleMediaStore(t)
	other := fixtureAccount(t)
	other.AccountID = "second@AdobeID"
	other.Token = "e30.eyJ1c2VyX2lkIjoic2Vjb25kQEFkb2JlSUQiLCJleHAiOjQxMDI0NDQ4MDB9.fixture"
	other.Email, other.ProfileID = "second@example.test", "second-profile"
	items, err := s.Import([]Account{other})
	if err != nil {
		t.Fatal(err)
	}
	b := items[0]
	_ = s.update(b.ID, 0, func(a *Account) { a.Status = "active"; a.LastUsedAt = time.Now().Unix() })
	s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) { return freshMediaAccount(a), nil })
	b, _ = s.Get(b.ID)
	if _, err := s.WarmMediaSession(mediaTestContext(), b.ID, b.Revision); err != nil {
		t.Fatal(err)
	}
	chosen, _, err := s.ReserveMediaForModel(context.Background(), "prefer-ready", "", mediaDirectResolver)
	if err != nil || chosen.ID != b.ID {
		t.Fatal("cold oldest beat ready account", err)
	}
	_ = s.SetEnabled([]string{b.ID}, false)
	_ = s.update(a.ID, 0, func(a *Account) { a.ProfileID = "" })
	if _, _, err := s.ReserveMediaForModel(context.Background(), "no-profile", "", mediaDirectResolver); !errors.Is(err, ErrNoAccount) {
		t.Fatal("profileless cold account selected", err)
	}
}

func TestBrowserReloginAndFallbackPopulateReadyButIMSDoesNot(t *testing.T) {
	for _, mode := range []string{"relogin", "fallback", "ims"} {
		t.Run(mode, func(t *testing.T) {
			s, a := idleMediaStore(t)
			if err := s.update(a.ID, 0, func(a *Account) { a.RefreshCookie = "ims=fixture" }); err != nil {
				t.Fatal(err)
			}
			a, _ = s.Get(a.ID)
			calls := 0
			s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) { calls++; return freshMediaAccount(a), nil })
			var fresh Account
			var err error
			if mode == "relogin" {
				fresh, err = s.Relogin(mediaTestContext(), a.ID)
			} else {
				client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Host == "adobeid-na1.services.adobe.com" {
						if mode == "fallback" {
							return jsonResponse(401, `{}`), nil
						}
						return jsonResponse(200, `{"access_token":"`+a.Token+`"}`), nil
					}
					return jsonResponse(200, `{"total":{"quota":{"total":10,"available":10,"used":0}}}`), nil
				})})
				fresh, err = s.Refresh(mediaTestContext(), client, a.ID, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ims" {
				if s.MediaSessionReady(mediaTestContext(), fresh, 0) || calls != 0 {
					t.Fatal("IMS fabricated ready")
				}
				return
			}
			if !s.MediaSessionReady(mediaTestContext(), fresh, 0) {
				t.Fatal("verified browser result not reusable")
			}
			if _, err := s.WarmMediaSession(mediaTestContext(), fresh.ID, fresh.Revision); err != nil || calls != 1 {
				t.Fatal("browser login followed by duplicate warm", err)
			}
		})
	}
}

func TestRefreshDuePermissionsRechecksAndKeepsGateThroughTail(t *testing.T) {
	s, a := idleMediaStore(t)
	if _, attempted, err := s.RefreshDueWithPermissions(mediaTestContext(), NewClient(nil), a.ID, time.Now()); err != nil || attempted {
		t.Fatal("fresh snapshot refreshed", err)
	}
	_ = s.update(a.ID, 0, func(a *Account) { a.Quota.Status = "stale" })
	requests := 0
	client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host == "bks.adobe.io" {
			if _, err := s.WarmMediaSession(mediaTestContext(), a.ID, a.Revision); !errors.Is(err, ErrBusy) {
				t.Fatal("permissions tail escaped maintenance")
			}
			return jsonResponse(200, `{"featuresAuthorization":[],"effectiveQuotas":[]}`), nil
		}
		return jsonResponse(200, `{"total":{"quota":{"total":10,"available":10,"used":0}}}`), nil
	})})
	if _, attempted, err := s.RefreshDueWithPermissions(mediaTestContext(), client, a.ID, time.Now()); err != nil || !attempted {
		t.Fatal("due refresh failed", err)
	}
	if requests != 2 {
		t.Fatalf("got %d requests", requests)
	}
	if _, attempted, err := s.RefreshDueWithPermissions(mediaTestContext(), client, a.ID, time.Now()); err != nil || attempted || requests != 2 {
		t.Fatal("stale scheduler snapshot duplicated requests", err)
	}
	s.mu.Lock()
	s.quotaQueries[a.ID] = true
	s.mu.Unlock()
	if _, err := s.RefreshWithPermissions(mediaTestContext(), client, a.ID, false); !errors.Is(err, ErrBusy) {
		t.Fatal("refresh overlapped quota query")
	}
}
