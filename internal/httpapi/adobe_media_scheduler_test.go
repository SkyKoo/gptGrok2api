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

func TestAdobeImageSelectionPreservesLaterCallerContext(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "value_and_deadline", true: "cancelled"}[cancelled], func(t *testing.T) {
			s := imageTaskTestServer(t)
			adobeSchedulingAccount(t, s, "cold", time.Now())
			selected, err := s.selectImageImplementation(context.Background(), "gemini-3.1-flash-image", "", "1024x1024", "auto", 1, 0, provider.ImageOutputOptions{}, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			type callerKey struct{}
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), callerKey{}, "caller-value"), time.Minute)
			defer cancel()
			if cancelled {
				cancel()
			}
			var prepares atomic.Int32
			s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
				prepares.Add(1)
				if ctx.Value(callerKey{}) != "caller-value" {
					t.Error("selected context replaced the caller's later value")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("selected context replaced the caller's later deadline")
				}
				return adobeFixtureMediaRefresh(ctx, current)
			})
			fixture := &adobeMediaCapture{output: adobeMediaPNG(t, 1024, 1024)}
			s.adobeClient = fixture.client(t)
			r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			_, err = s.generateAdobeImageData(r, ctx, selected, "a paper boat", "gemini-3.1-flash-image", "1024x1024", "url", "", 1, nil, provider.ImageOutputOptions{})
			if cancelled {
				if err == nil || !strings.Contains(err.Error(), "cancelled") || prepares.Load() != 0 || fixture.preflights != 0 || fixture.submits != 0 {
					t.Fatalf("cancelled caller still generated: err=%v prepare=%d preflight=%d submit=%d", err, prepares.Load(), fixture.preflights, fixture.submits)
				}
			} else if err != nil || prepares.Load() != 1 {
				t.Fatalf("caller context request failed: err=%v prepares=%d", err, prepares.Load())
			}
		})
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
		entered <- struct{}{}
		<-release
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
		if err := s.adobeStore.UpdateQuota(a.ID, a.Revision, adobe.Quota{Status: "known", CheckedAt: time.Now().Add(-time.Duration(i+2) * time.Hour).UTC().Format(time.RFC3339Nano)}); err != nil {
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
