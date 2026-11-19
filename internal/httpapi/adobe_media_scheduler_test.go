package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func adobeSchedulingAccount(t *testing.T, s *Server, id string, used time.Time, withProfile ...bool) adobe.Account {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{"user_id": id, "exp": time.Now().Add(time.Hour).Unix()})
	profile := "profile-" + id
	if len(withProfile) > 0 && !withProfile[0] {
		profile = ""
	}
	rows, err := s.adobeStore.Import([]adobe.Account{{AccountID: id, Email: id + "@example.test", Enabled: true, ProfileID: profile, Token: "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature", Cookie: "sherlockToken=fixture-imported", LastUsedAt: used.Unix()}})
	if err != nil {
		t.Fatal(err)
	}
	a := rows[0]
	zero, one, available := 0.0, 1.0, 3.0
	if err = s.adobeStore.UpdateQuota(a.ID, a.Revision, adobe.Quota{Status: "known", Available: &zero, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	generation := adobe.GenerationEntitlements{Status: "known", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Pools: []adobe.GenerationPool{{ID: "free-media", ResourceType: "firefly_complimentary_generations", Available: &available}}}
	for _, m := range adobe.FreeModels() {
		generation.Models = append(generation.Models, adobe.ModelEntitlement{Model: m.ID, ResourceType: "firefly_complimentary_generations", PoolID: "free-media", Authorized: true, Cost: &one})
	}
	if err = s.adobeStore.UpdateGenerationPermissions(a.ID, a.Revision, generation); err != nil {
		t.Fatal(err)
	}
	a, err = s.adobeStore.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAdobeImageSelectionUsesReadyAccountAndPreservesSelectedEgress(t *testing.T) {
	s := imageTaskTestServer(t)
	adobeSchedulingAccount(t, s, "cold-first", time.Now().Add(-20*time.Minute))
	ready := adobeSchedulingAccount(t, s, "ready-second", time.Now().Add(-time.Minute))
	wantProxy := "http://selected.invalid:8080"
	s.proxyManager.SetDefault(wantProxy, nil)
	var prepares atomic.Int32
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		prepares.Add(1)
		if current.ID != ready.ID || proxyruntime.URLFromContext(ctx) != wantProxy {
			t.Error("unexpected media preparation account or egress")
		}
		return adobeFixtureMediaRefresh(ctx, current)
	})
	fixed, err := s.adobeAccountContext(context.Background(), ready)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.adobeStore.WarmMediaSession(fixed, ready.ID, ready.Revision); err != nil {
		t.Fatal(err)
	}
	selected, err := s.selectImageImplementation(context.Background(), "gemini-3.1-flash-image", "", "1024x1024", "auto", 1, 0, provider.ImageOutputOptions{}, false, nil)
	if err != nil || selected.Adobe.ID != ready.ID {
		t.Fatalf("ready account not preferred: id=%s err=%v", selected.Adobe.ID, err)
	}
	s.proxyManager.SetDefault("http://changed.invalid:8080", nil)
	fixture := &adobeMediaCapture{expectedProxy: wantProxy, output: adobeMediaPNG(t, 1024, 1024)}
	s.adobeClient = fixture.client(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	if _, err = s.generateAdobeImageData(r, context.Background(), selected, "a paper boat", "gemini-3.1-flash-image", "1024x1024", "url", "", 1, nil, provider.ImageOutputOptions{}); err != nil {
		t.Fatal(err)
	}
	if prepares.Load() != 1 {
		t.Fatalf("ready request recollected browser session: %d", prepares.Load())
	}
}

func TestAdobeImageSelectionRejectsColdAccountWithoutBrowser(t *testing.T) {
	s := imageTaskTestServer(t)
	adobeSchedulingAccount(t, s, "cold", time.Now())
	var prepares atomic.Int32
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		prepares.Add(1)
		return adobeFixtureMediaRefresh(ctx, current)
	})
	_, err := s.selectImageImplementation(context.Background(), "gemini-3.1-flash-image", "", "1024x1024", "auto", 1, 0, provider.ImageOutputOptions{}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "Adobe media session is warming") {
		t.Fatalf("cold account was accepted: %v", err)
	}
	if prepares.Load() != 0 {
		t.Fatalf("cold selection launched browser-worker: %d", prepares.Load())
	}
}

func TestAdobeVideoSelectionUsesWarmAccountWithoutRecollection(t *testing.T) {
	s := imageTaskTestServer(t)
	adobeSchedulingAccount(t, s, "cold-first", time.Now().Add(-20*time.Minute))
	ready := adobeSchedulingAccount(t, s, "ready-second", time.Now().Add(-time.Minute))
	var prepares atomic.Int32
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		prepares.Add(1)
		return adobeFixtureMediaRefresh(ctx, current)
	})
	fixed, err := s.adobeAccountContext(context.Background(), ready)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.adobeStore.WarmMediaSession(fixed, ready.ID, ready.Revision); err != nil {
		t.Fatal(err)
	}
	s.adobeClient = (&adobeMediaCapture{video: true}).client(t)
	_, route, detail := adobePreparationRequest(t, s, "video")
	if route == nil || route.AccountID != ready.ID || !strings.Contains(detail, "upstream_poll_failed") || prepares.Load() != 1 {
		t.Fatalf("video did not reuse warm selected account: route=%+v details=%s prepares=%d", route, detail, prepares.Load())
	}
}

func TestAdobeWarmSchedulerFollowsRecentDemandAndStopsAtTwoReady(t *testing.T) {
	s := imageTaskTestServer(t)
	adobeSchedulingAccount(t, s, "cold", time.Now().Add(-20*time.Minute))
	older := adobeSchedulingAccount(t, s, "recent-older", time.Now().Add(-5*time.Minute))
	newest := adobeSchedulingAccount(t, s, "recent-newest", time.Now().Add(-time.Minute))
	var collected []string
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		collected = append(collected, current.ID)
		return adobeFixtureMediaRefresh(ctx, current)
	})
	s.warmRecentAdobeMediaSessions(context.Background())
	if len(collected) != 1 || collected[0] != newest.ID {
		t.Fatalf("first tick did not warm only most recent demand: %v", collected)
	}
	s.warmRecentAdobeMediaSessions(context.Background())
	if len(collected) != 2 || collected[1] != older.ID {
		t.Fatalf("second tick did not reuse ready session and warm next candidate: %v", collected)
	}
	adobeSchedulingAccount(t, s, "third-recent", time.Now())
	s.warmRecentAdobeMediaSessions(context.Background())
	if len(collected) != 2 {
		t.Fatalf("two ready accounts still triggered browser warming: %v", collected)
	}
}

func TestAdobeWarmSchedulerSkipsUnavailableOrUnrequestedAccounts(t *testing.T) {
	for _, condition := range []string{"cold", "no_profile", "unknown_model_quota", "zero_model_quota", "disabled", "in_flight", "maintenance_disabled", "cancelled"} {
		t.Run(condition, func(t *testing.T) {
			s := imageTaskTestServer(t)
			used := time.Now().Add(-time.Minute)
			if condition == "cold" {
				used = time.Now().Add(-11 * time.Minute)
			}
			a := adobeSchedulingAccount(t, s, "candidate", used, condition != "no_profile")
			ctx := context.Background()
			switch condition {
			case "unknown_model_quota":
				if err := s.adobeStore.UpdateGenerationPermissions(a.ID, a.Revision, adobe.GenerationEntitlements{Status: "unknown"}); err != nil {
					t.Fatal(err)
				}
			case "zero_model_quota":
				zero := 0.0
				generation := *a.Quota.Generation
				generation.Pools[0].Available = &zero
				if err := s.adobeStore.UpdateGenerationPermissions(a.ID, a.Revision, generation); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := s.adobeStore.SetEnabled([]string{a.ID}, false); err != nil {
					t.Fatal(err)
				}
			case "in_flight":
				if _, err := s.adobeStore.Reserve("active-task"); err != nil {
					t.Fatal(err)
				}
			case "maintenance_disabled":
				if _, err := s.store.UpdateConfig("adobe_maintenance", map[string]any{"enabled": false}); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var calls atomic.Int32
			s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
				calls.Add(1)
				return adobeFixtureMediaRefresh(ctx, current)
			})
			s.warmRecentAdobeMediaSessions(ctx)
			if calls.Load() != 0 {
				t.Fatal("unavailable or unrequested account was warmed")
			}
		})
	}
}

func TestAdobeMediaSchedulerRefreshesAllEligibleSessions(t *testing.T) {
	s := imageTaskTestServer(t)
	older := adobeSchedulingAccount(t, s, "scan-older", time.Now().Add(-2*time.Minute))
	newer := adobeSchedulingAccount(t, s, "scan-newer", time.Now().Add(-time.Minute))
	adobeSchedulingAccount(t, s, "scan-no-profile", time.Now(), false)
	disabled := adobeSchedulingAccount(t, s, "scan-disabled", time.Now())
	if err := s.adobeStore.SetEnabled([]string{disabled.ID}, false); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	called := []string{}
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		mu.Lock()
		called = append(called, current.ID)
		mu.Unlock()
		return adobeFixtureMediaRefresh(ctx, current)
	})
	s.refreshAllAdobeMediaSessions(context.Background())

	mu.Lock()
	got := append([]string(nil), called...)
	mu.Unlock()
	if len(got) != 1 || got[0] != newer.ID {
		t.Fatalf("media session scan did not start the newest eligible session only: %v", got)
	}

	// One browser session is allowed per scheduler tick. The next tick advances
	// to the next eligible account, while a later tick sees both as reusable.
	s.refreshAllAdobeMediaSessions(context.Background())
	mu.Lock()
	second := append([]string(nil), called...)
	mu.Unlock()
	if len(second) != 2 || second[1] != older.ID {
		t.Fatalf("second scheduler tick did not advance to the next account: %v", second)
	}
	s.refreshAllAdobeMediaSessions(context.Background())
	mu.Lock()
	deferred := append([]string(nil), called...)
	mu.Unlock()
	if len(deferred) != len(second) {
		t.Fatalf("ready sessions were refreshed again: %v", deferred)
	}
}

func TestAdobeMediaSchedulerHonorsSessionFailureBackoff(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeSchedulingAccount(t, s, "scan-failed", time.Now())
	var calls atomic.Int32
	s.adobeStore.SetBrowserRefresher(func(context.Context, adobe.Account) (adobe.Account, error) {
		calls.Add(1)
		return adobe.Account{}, &adobe.Error{Stage: "relogin", Code: "media_session_incomplete"}
	})
	s.refreshAllAdobeMediaSessions(context.Background())
	s.refreshAllAdobeMediaSessions(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("failed session ignored scheduler backoff: calls=%d account=%s", calls.Load(), a.ID)
	}
}

func TestAdobeMaintenanceRefreshesPermissionsBeforeWarming(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeSchedulingAccount(t, s, "recent", time.Now().Add(-time.Minute))
	if err := s.adobeStore.UpdateQuota(a.ID, a.Revision, adobe.Quota{Status: "stale"}); err != nil {
		t.Fatal(err)
	}
	var reads, prepares atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClient(t, &reads, false)
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		prepares.Add(1)
		if reads.Load() != 2 || !adobeHasKnownMediaCapacity(current, time.Now()) {
			t.Error("browser warmed before balance and model permissions were saved")
		}
		return adobeFixtureMediaRefresh(ctx, current)
	})
	s.refreshDueAdobeAccounts(context.Background())
	if reads.Load() != 2 || prepares.Load() != 1 {
		t.Fatalf("maintenance reads=%d prepares=%d", reads.Load(), prepares.Load())
	}
}

func TestAdobeMaintenanceBusyAccountsDoNotConsumeRefreshBudget(t *testing.T) {
	s := imageTaskTestServer(t)
	accounts := []adobe.Account{}
	for _, id := range []string{"busy-one", "busy-two", "busy-three", "busy-four", "available-fifth"} {
		a := adobeSchedulingAccount(t, s, id, time.Now().Add(-20*time.Minute))
		if err := s.adobeStore.UpdateQuota(a.ID, a.Revision, adobe.Quota{Status: "stale"}); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, a)
	}
	entered, release := make(chan struct{}, 4), make(chan struct{})
	var workers sync.WaitGroup
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		if current.ID != accounts[4].ID {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return adobe.Account{}, ctx.Err()
			}
		}
		return adobeFixtureMediaRefresh(ctx, current)
	})
	for _, a := range accounts[:4] {
		fixed, err := s.adobeAccountContext(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		workers.Add(1)
		go func(a adobe.Account, ctx context.Context) {
			defer workers.Done()
			_, _ = s.adobeStore.WarmMediaSession(ctx, a.ID, a.Revision)
		}(a, fixed)
	}
	defer func() { close(release); workers.Wait() }()
	for range accounts[:4] {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("fixture warmup never entered account gate")
		}
	}
	var reads atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClient(t, &reads, false)
	s.refreshDueAdobeAccounts(context.Background())
	fifth, err := s.adobeStore.Get(accounts[4].ID)
	if err != nil || fifth.Quota.Status != "known" || fifth.Quota.Generation == nil || reads.Load() != 2 {
		t.Fatalf("busy accounts starved fifth account: reads=%d status=%s err=%v", reads.Load(), fifth.Quota.Status, err)
	}
}

func TestAdobeMaintenanceSnapshotSkipsAccountRefreshedByForeground(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeSchedulingAccount(t, s, "recent", time.Now().Add(-time.Minute))
	if err := s.adobeStore.UpdateQuota(a.ID, a.Revision, adobe.Quota{Status: "stale"}); err != nil {
		t.Fatal(err)
	}
	stale, err := s.adobeStore.List()
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClient(t, &reads, false)
	if _, err = s.refreshAdobeAccount(context.Background(), a.ID, false); err != nil {
		t.Fatal(err)
	}
	s.refreshAdobeMaintenanceSnapshot(context.Background(), stale, time.Now())
	if reads.Load() != 2 {
		t.Fatalf("stale background snapshot duplicated completed foreground observations: %d", reads.Load())
	}
}

func TestAdobeMaintenanceRefreshesMostOverdueAccountsFirst(t *testing.T) {
	s := imageTaskTestServer(t)
	for i, id := range []string{"newest", "later", "middle", "older", "oldest"} {
		a := adobeSchedulingAccount(t, s, id, time.Now().Add(-2*time.Hour))
		if err := s.adobeStore.UpdateQuota(a.ID, a.Revision, adobe.Quota{Status: "known", CheckedAt: time.Now().Add(-time.Duration(i+2) * time.Hour).UTC().Format(time.RFC3339Nano), Generation: a.Quota.Generation}); err != nil {
			t.Fatal(err)
		}
	}
	var reads atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClient(t, &reads, false)
	s.refreshDueAdobeAccounts(context.Background())
	newest, _ := s.adobeStore.Get("adobe:newest")
	oldest, _ := s.adobeStore.Get("adobe:oldest")
	newestAt, _ := time.Parse(time.RFC3339Nano, newest.Quota.CheckedAt)
	oldestAt, _ := time.Parse(time.RFC3339Nano, oldest.Quota.CheckedAt)
	if reads.Load() != 8 || time.Since(newestAt) < time.Hour || time.Since(oldestAt) > time.Minute {
		t.Fatalf("refresh budget did not follow overdue ordering: reads=%d newest=%s oldest=%s", reads.Load(), newest.Quota.CheckedAt, oldest.Quota.CheckedAt)
	}
}

func TestAdobePreflightInvalidatesOnlyRejectedMediaSession(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		invalidate bool
	}{
		{"unauthorized", &adobe.Error{Stage: "preflight", Status: 401, Code: "authentication_error"}, true},
		{"session", &adobe.Error{Stage: "session", Code: "arp_mode_invalid"}, true},
		{"identity", adobe.ErrIdentity, true},
		{"quota", &adobe.Error{Stage: "preflight", Status: 429, Code: "free_quota_exhausted"}, false},
		{"entitlement", &adobe.Error{Stage: "preflight", Status: 403, Code: "free_entitlement_unavailable"}, false},
		{"transport", errors.New("read-only network error"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := imageTaskTestServer(t)
			a := adobeSchedulingAccount(t, s, "ready", time.Now().Add(-time.Minute))
			s.adobeStore.SetBrowserRefresher(adobeFixtureMediaRefresh)
			ctx, err := s.adobeAccountContext(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			a, err = s.adobeStore.WarmMediaSession(ctx, a.ID, a.Revision)
			if err != nil || !s.adobeStore.MediaSessionReady(ctx, a, 0) {
				t.Fatalf("warm session not ready: %v", err)
			}
			s.invalidateRejectedAdobeMediaSession(a, tc.err)
			if s.adobeStore.MediaSessionReady(ctx, a, 0) == tc.invalidate {
				t.Fatal("wrong media session invalidation decision")
			}
		})
	}
}

func TestAdobeMissingProxyManagerPinsDirectSelection(t *testing.T) {
	s := imageTaskTestServer(t)
	s.proxyManager = nil
	ctx, err := s.adobeAccountContext(context.Background(), adobe.Account{})
	endpoint, fixed := proxyruntime.URLSelectionFromContext(ctx)
	if err != nil || !fixed || endpoint != "" {
		t.Fatal("missing proxy manager did not pin explicit direct egress")
	}
}

// The complete public entrypoints must remain read-only until a local ready
// account is found, including stale rights while a browser refresh owns the gate.
func TestAdobeForegroundMediaDoesNotRefreshOrWaitForUnreadyAccount(t *testing.T) {
	for _, kind := range []string{"sync_image", "async_image", "video"} {
		for _, state := range []string{"cold", "expired", "maintenance"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				s := imageTaskTestServer(t)
				a := adobeFixtureAccount(t, s)
				if state == "expired" {
					claims, _ := json.Marshal(map[string]any{"user_id": a.AccountID, "exp": time.Now().Add(-time.Minute).Unix()})
					a.Token = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".expired"
					rows, err := s.adobeStore.Import([]adobe.Account{a})
					if err != nil {
						t.Fatal(err)
					}
					a = rows[0]
				}
				entered, release, warmDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				var browserCalls, upstreamCalls atomic.Int32
				s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
					if browserCalls.Add(1) == 1 && state == "maintenance" {
						close(entered)
					}
					if state == "maintenance" {
						select {
						case <-release:
						case <-ctx.Done():
							return adobe.Account{}, ctx.Err()
						}
					}
					return adobeFixtureMediaRefresh(ctx, current)
				})
				s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					upstreamCalls.Add(1)
					return nil, errors.New("unready foreground account must not call upstream")
				})})
				if state == "maintenance" {
					markAdobeFixturePermissionsStale(t, s, a, 10)
					fixed, err := s.adobeAccountContext(context.Background(), a)
					if err != nil {
						t.Fatal(err)
					}
					go func() { defer close(warmDone); _, _ = s.adobeStore.WarmMediaSession(fixed, a.ID, a.Revision) }()
					defer func() { unblock(); <-warmDone }()
					select {
					case <-entered:
					case <-time.After(time.Second):
						t.Fatal("background warmup did not start")
					}
				}
				var status string
				var route *mediaRoute
				finished := make(chan struct{})
				go func() { defer close(finished); status, route, _ = adobePreparationRequest(t, s, kind) }()
				select {
				case <-finished:
				case <-time.After(time.Second):
					unblock()
					<-finished
					t.Fatal("foreground request waited for background browser preparation")
				}
				expectedBrowsers := int32(0)
				if state == "maintenance" {
					expectedBrowsers = 1
				}
				if (status != "error" && status != "failed") || route != nil || browserCalls.Load() != expectedBrowsers || upstreamCalls.Load() != 0 {
					t.Fatalf("unready request started work: status=%s route=%+v browsers=%d upstream=%d", status, route, browserCalls.Load(), upstreamCalls.Load())
				}
				current, err := s.adobeStore.Get(a.ID)
				if err != nil || current.InFlight != "" {
					t.Fatal("unready request retained account reservation")
				}
			})
		}
	}
}

func TestAdobeReadyImagePreservesLaterCallerContext(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "value_and_deadline", true: "cancelled"}[cancelled], func(t *testing.T) {
			s := imageTaskTestServer(t)
			adobeReadyFixtureAccount(t, s)
			selected, err := s.selectImageImplementation(context.Background(), "gemini-3.1-flash-image", "", "1024x1024", "auto", 1, 0, provider.ImageOutputOptions{}, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			type callerKey struct{}
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), callerKey{}, "caller-value"), time.Minute)
			defer cancel()
			deadline, _ := ctx.Deadline()
			if cancelled {
				cancel()
			}
			var prepares, requests atomic.Int32
			s.adobeStore.SetBrowserRefresher(func(context.Context, adobe.Account) (adobe.Account, error) {
				prepares.Add(1)
				return adobe.Account{}, errors.New("ready foreground request must not refresh")
			})
			fixture := &adobeMediaCapture{output: adobeMediaPNG(t, 1024, 1024), inspectRequest: func(r *http.Request) {
				requests.Add(1)
				if r.Context().Value(callerKey{}) != "caller-value" {
					t.Error("selected context replaced later caller value")
				}
				observed, ok := r.Context().Deadline()
				if !ok || observed.After(deadline) {
					t.Error("selected context lost or extended caller deadline")
				}
			}}
			s.adobeClient = fixture.client(t)
			r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			_, err = s.generateAdobeImageData(r, ctx, selected, "a paper boat", "gemini-3.1-flash-image", "1024x1024", "url", "", 1, nil, provider.ImageOutputOptions{})
			if cancelled {
				if err == nil || !strings.Contains(err.Error(), "cancelled") || requests.Load() != 0 || fixture.preflights != 0 || fixture.submits != 0 {
					t.Fatalf("cancelled caller generated: err=%v requests=%d", err, requests.Load())
				}
			} else if err != nil || requests.Load() == 0 || fixture.preflights != 1 || fixture.submits != 1 {
				t.Fatalf("ready request lost caller context: err=%v requests=%d", err, requests.Load())
			}
			if prepares.Load() != 0 {
				t.Fatal("foreground request refreshed warmed session")
			}
		})
	}
}

func TestAdobeMaintenanceRefreshesExpiredPermissionsBeforeUsingBrowser(t *testing.T) {
	for _, status := range []string{"known", "stale"} {
		t.Run(status, func(t *testing.T) {
			s := imageTaskTestServer(t)
			a := adobeSchedulingAccount(t, s, "expired-model-permissions", time.Now().Add(-time.Minute))
			generation := *a.Quota.Generation
			generation.Status = status
			generation.CheckedAt = time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339Nano)
			if err := s.adobeStore.UpdateGenerationPermissions(a.ID, a.Revision, generation); err != nil {
				t.Fatal(err)
			}
			var reads, prepares atomic.Int32
			s.adobeClient = adobeReconciliationQuotaClient(t, &reads, false)
			s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
				prepares.Add(1)
				if reads.Load() != 2 || !adobeHasKnownMediaCapacity(current, time.Now()) {
					t.Error("browser occupied before expired permissions were refreshed")
				}
				return adobeFixtureMediaRefresh(ctx, current)
			})
			s.refreshAllAdobeMediaSessions(context.Background())
			if reads.Load() != 0 || prepares.Load() != 0 {
				t.Fatal("media-only scan refreshed an account with stale model permissions")
			}
			s.refreshDueAdobeAccounts(context.Background())
			if reads.Load() != 2 || prepares.Load() != 1 {
				t.Fatalf("maintenance did not prepare model-eligible account: quota=%d browser=%d", reads.Load(), prepares.Load())
			}
		})
	}
}
