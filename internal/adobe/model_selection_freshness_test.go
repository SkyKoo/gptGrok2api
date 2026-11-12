package adobe

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func strictSelectionGeneration(now time.Time, available float64) GenerationEntitlements {
	one := 1.0
	return GenerationEntitlements{Status: "known", CheckedAt: now.UTC().Format(time.RFC3339Nano),
		Models: []ModelEntitlement{{Model: "veo-3.1-lite", Authorized: true, ResourceType: "firefly_complimentary_generations", PoolID: "video", Cost: &one}},
		Pools:  []GenerationPool{{ID: "video", ResourceType: "firefly_complimentary_generations", Available: &available}},
	}
}

func TestRefreshModelPermissionsSharesConcurrentFailedObservation(t *testing.T) {
	s, a := idleMediaStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		reads.Add(1)
		if r.URL.Path == "/v1/feature-auth-profile" {
			close(entered)
			<-release
			return jsonResponse(503, `{"error_code":"upstream_error"}`), nil
		}
		return jsonResponse(200, `{"total":{"quota":{"available":0}}}`), nil
	})})
	first := make(chan error, 1)
	go func() {
		_, attempted, err := s.RefreshModelPermissions(mediaTestContext(), client, a.ID, "veo-3.1-lite")
		if !attempted {
			first <- errors.New("first query not attempted")
			return
		}
		first <- err
	}()
	<-entered
	second := make(chan error, 1)
	go func() {
		_, attempted, err := s.RefreshModelPermissions(mediaTestContext(), client, a.ID, "veo-3.1-lite")
		if attempted {
			second <- errors.New("waiter duplicated query")
			return
		}
		second <- err
	}()
	select {
	case <-second:
		t.Fatal("waiter did not wait for current observation")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-first; SafeError(err) != "adobe_entitlements_observation_incomplete" {
		t.Fatal(err)
	}
	if err := <-second; SafeError(err) != "adobe_entitlements_observation_incomplete" {
		t.Fatal(err)
	}
	if reads.Load() != 2 {
		t.Fatalf("concurrent quota refresh made %d reads", reads.Load())
	}
	if _, attempted, err := s.RefreshModelPermissions(mediaTestContext(), client, a.ID, "veo-3.1-lite"); attempted || SafeError(err) != "adobe_entitlements_observation_incomplete" || reads.Load() != 2 {
		t.Fatal("immediate independent request bypassed failure backoff", err)
	}
	// Simulate the publication window: the failed quota is already on disk,
	// but its reader still holds the maintenance gate. A new waiter sees the
	// same QuotaRevision before/after the wait and must still share the failure.
	s.mu.Lock()
	s.maintenance[a.ID] = true
	s.mu.Unlock()
	window := make(chan error, 1)
	go func() {
		_, attempted, err := s.RefreshModelPermissions(mediaTestContext(), client, a.ID, "veo-3.1-lite")
		if attempted {
			window <- errors.New("publication-window waiter repeated query")
			return
		}
		window <- err
	}()
	select {
	case <-window:
		t.Fatal("publication-window waiter skipped gate")
	case <-time.After(25 * time.Millisecond):
	}
	s.endAccountMaintenance(a.ID)
	if err := <-window; SafeError(err) != "adobe_entitlements_observation_incomplete" || reads.Load() != 2 {
		t.Fatal("published failure was not shared", err)
	}
}

func TestModelPermissionFailureBackoffExpiresAndFollowsIdentityAndEgress(t *testing.T) {
	for _, mode := range []string{"expiry", "credentials", "egress"} {
		t.Run(mode, func(t *testing.T) {
			s, a := idleMediaStore(t)
			reads := 0
			client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				reads++
				if r.URL.Path == "/v1/feature-auth-profile" {
					return jsonResponse(503, `{"error_code":"upstream_error"}`), nil
				}
				return jsonResponse(200, `{"total":{"quota":{"available":0}}}`), nil
			})})
			if _, attempted, err := s.RefreshModelPermissions(mediaTestContext(), client, a.ID, "veo-3.1-lite"); !attempted || err == nil {
				t.Fatal("failed observation not recorded", err)
			}
			ctx := mediaTestContext()
			switch mode {
			case "expiry":
				s.mu.Lock()
				failure := s.modelFailures[a.ID]
				failure.RetryAfter = time.Now().Add(-time.Second)
				s.modelFailures[a.ID] = failure
				s.mu.Unlock()
			case "credentials":
				current, _ := s.Get(a.ID)
				if _, err := s.ReplaceCredentials(current, current); err != nil {
					t.Fatal(err)
				}
			case "egress":
				ctx = proxyruntime.WithURL(context.Background(), "http://new-exit.invalid:8080")
			}
			if _, attempted, err := s.RefreshModelPermissions(ctx, client, a.ID, "veo-3.1-lite"); !attempted || err == nil || reads != 4 {
				t.Fatal("expired/stale-context failure incorrectly blocked fresh observation", err, reads)
			}
		})
	}
}

func TestModelSelectionRequiresFreshCompleteFreeEntitlement(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		mutate func(*GenerationEntitlements)
		want   ModelSelectionState
	}{
		{"fresh available", nil, ModelUsable},
		{"fresh zero", func(e *GenerationEntitlements) { v := 0.0; e.Pools[0].Available = &v }, ModelUnavailable},
		{"unauthorized", func(e *GenerationEntitlements) { e.Models[0].Authorized = false }, ModelUnavailable},
		{"stale", func(e *GenerationEntitlements) { e.Status = "stale" }, ModelNeedsRefresh},
		{"old", func(e *GenerationEntitlements) { e.CheckedAt = now.Add(-6 * time.Minute).Format(time.RFC3339Nano) }, ModelNeedsRefresh},
		{"future", func(e *GenerationEntitlements) { e.CheckedAt = now.Add(time.Minute).Format(time.RFC3339Nano) }, ModelNeedsRefresh},
		{"error preserves old number", func(e *GenerationEntitlements) { e.Status = "error" }, ModelNeedsRefresh},
		{"no checked timestamp", func(e *GenerationEntitlements) { e.CheckedAt = "" }, ModelNeedsRefresh},
		{"missing model", func(e *GenerationEntitlements) { e.Models = nil }, ModelNeedsRefresh},
		{"missing pool", func(e *GenerationEntitlements) { e.Pools = nil }, ModelNeedsRefresh},
		{"missing counter", func(e *GenerationEntitlements) { e.Pools[0].Available = nil }, ModelNeedsRefresh},
		{"missing cost", func(e *GenerationEntitlements) { e.Models[0].Cost = nil }, ModelNeedsRefresh},
		{"invalid counter", func(e *GenerationEntitlements) { v := math.NaN(); e.Pools[0].Available = &v }, ModelNeedsRefresh},
		{"negative counter", func(e *GenerationEntitlements) { v := -1.0; e.Pools[0].Available = &v }, ModelNeedsRefresh},
		{"missing resource", func(e *GenerationEntitlements) { e.Models[0].ResourceType = "" }, ModelNeedsRefresh},
		{"paid resource", func(e *GenerationEntitlements) { e.Models[0].ResourceType = "firefly_paid_credits" }, ModelNeedsRefresh},
		{"resource mismatch", func(e *GenerationEntitlements) { e.Pools[0].ResourceType = "firefly_limited_taste" }, ModelNeedsRefresh},
		{"fair use explicit authorization", func(e *GenerationEntitlements) {
			e.Models[0].ResourceType = "firefly_fair_use"
			e.Models[0].PoolID = ""
			e.Models[0].Cost = nil
			e.Pools = nil
		}, ModelUsable},
		{"fair use denied", func(e *GenerationEntitlements) {
			e.Models[0].ResourceType = "firefly_fair_use"
			e.Models[0].Authorized = false
			e.Pools = nil
		}, ModelUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := strictSelectionGeneration(now, 1)
			if tc.mutate != nil {
				tc.mutate(&e)
			}
			if got := e.ModelSelectionState("veo-3.1-lite", now); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
	var missing *GenerationEntitlements
	if missing.ModelSelectionState("veo-3.1-lite", now) != ModelNeedsRefresh {
		t.Fatal("missing observation admitted")
	}
}

func TestReserveMediaForModelRejectsStaleQuotaEvenWhenMediaReady(t *testing.T) {
	s, a := idleMediaStore(t)
	s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) { return freshMediaAccount(a), nil })
	a, err := s.WarmMediaSession(mediaTestContext(), a.ID, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	e := strictSelectionGeneration(time.Now().Add(-time.Hour), 0)
	e.Status = "stale"
	if err = s.UpdateGenerationPermissions(a.ID, a.Revision, e); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReserveMediaForModel(context.Background(), "stale-quota", "veo-3.1-lite", mediaDirectResolver); !errors.Is(err, ErrNoAccount) {
		t.Fatal("stale zero bound because media was ready", err)
	}
	a, _ = s.Get(a.ID)
	if a.InFlight != "" {
		t.Fatal("stale observation reserved account")
	}
	e = strictSelectionGeneration(time.Now(), 1)
	if err = s.UpdateGenerationPermissions(a.ID, a.Revision, e); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReserveMediaForModel(context.Background(), "fresh-quota", "veo-3.1-lite", mediaDirectResolver); err != nil {
		t.Fatal("fresh positive observation could not bind", err)
	}
}

func TestReserveMediaForModelRechecksQuotaAfterResolver(t *testing.T) {
	for _, change := range []string{"zero", "stale", "missing-model"} {
		t.Run(change, func(t *testing.T) {
			s, a := idleMediaStore(t)
			e := strictSelectionGeneration(time.Now(), 1)
			if err := s.UpdateGenerationPermissions(a.ID, a.Revision, e); err != nil {
				t.Fatal(err)
			}
			calls := 0
			_, _, err := s.ReserveMediaForModel(context.Background(), "quota-cas", "veo-3.1-lite", func(ctx context.Context, current Account) (context.Context, error) {
				calls++
				next := strictSelectionGeneration(time.Now(), 1)
				switch change {
				case "zero":
					next = strictSelectionGeneration(time.Now(), 0)
				case "stale":
					next.Status = "stale"
				case "missing-model":
					next.Models = nil
				}
				if err := s.UpdateGenerationPermissions(a.ID, a.Revision, next); err != nil {
					t.Fatal(err)
				}
				return mediaDirectResolver(ctx, current)
			})
			if !errors.Is(err, ErrNoAccount) || calls != 1 {
				t.Fatal("changed quota passed final binding check", err, calls)
			}
			d, _ := s.load()
			if _, bound := d.Bindings["quota-cas"]; bound {
				t.Fatal("failed quota CAS persisted binding")
			}
		})
	}
}

func TestReserveMediaForModelWaitsForCurrentStaleQuotaObservation(t *testing.T) {
	s, a := idleMediaStore(t)
	e := strictSelectionGeneration(time.Now().Add(-time.Hour), 0)
	e.Status = "stale"
	if err := s.UpdateGenerationPermissions(a.ID, a.Revision, e); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.maintenance[a.ID] = true
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, _, err := s.ReserveMediaForModel(context.Background(), "wait-for-current-quota", "veo-3.1-lite", mediaDirectResolver)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("stale observation rejected while refresh active: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := s.UpdateGenerationPermissions(a.ID, a.Revision, strictSelectionGeneration(time.Now(), 1)); err != nil {
		t.Fatal(err)
	}
	s.endAccountMaintenance(a.ID)
	if err := <-done; err != nil {
		t.Fatal("fresh observation did not unblock binding", err)
	}
}
