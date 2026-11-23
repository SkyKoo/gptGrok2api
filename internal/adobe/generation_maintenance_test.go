package adobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestExpiredAdobeLoginFallsBackToBrowserAndPersistsBackoff(t *testing.T) {
	s, a := idleMediaStore(t)
	if err := s.update(a.ID, a.Revision, func(current *Account) {
		current.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		current.RefreshCookie = ""
	}); err != nil {
		t.Fatal(err)
	}
	calls, reads := 0, 0
	refresher := func(_ context.Context, current Account) (Account, error) {
		calls++
		if calls == 1 {
			return Account{}, &Error{Stage: "relogin", Code: "browser_unavailable"}
		}
		return freshMediaAccount(current), nil
	}
	s.SetBrowserRefresher(refresher)
	client := queryFixtureClient(t, func() { reads++ })
	if _, attempted, err := s.RefreshDueLogin(mediaTestContext(), client, a.ID, time.Now()); !attempted || err == nil || calls != 1 || reads != 0 {
		t.Fatalf("expired login did not attempt browser recovery: attempted=%v calls=%d reads=%d err=%v", attempted, calls, reads, err)
	}
	restarted := NewStore(s.path, s.secret)
	restarted.SetBrowserRefresher(refresher)
	if _, attempted, err := restarted.RefreshDueLogin(mediaTestContext(), client, a.ID, time.Now()); attempted || err != nil || calls != 1 {
		t.Fatalf("restart bypassed relogin backoff: attempted=%v calls=%d err=%v", attempted, calls, err)
	}
	if err := restarted.update(a.ID, a.Revision, func(current *Account) { current.NextRefreshAt = time.Now().Unix() - 1 }); err != nil {
		t.Fatal(err)
	}
	got, attempted, err := restarted.RefreshDueLogin(mediaTestContext(), client, a.ID, time.Now())
	if err != nil || !attempted || calls != 2 || reads != 1 || got.Revision <= a.Revision || got.LastError != "" {
		t.Fatalf("login did not recover after backoff: attempted=%v calls=%d reads=%d err=%v", attempted, calls, reads, err)
	}
}

func TestAdobeQuotaOnly401LeavesRecoveryToLoginScheduler(t *testing.T) {
	s, a := idleMediaStore(t)
	if err := s.update(a.ID, a.Revision, func(current *Account) { current.Quota.Status = "stale" }); err != nil {
		t.Fatal(err)
	}
	browser, requests := 0, 0
	s.SetBrowserRefresher(func(_ context.Context, a Account) (Account, error) {
		browser++
		return freshMediaAccount(a), nil
	})
	client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path != "/v1/credits/balance" {
			t.Errorf("quota polling attempted %s", r.URL.Path)
		}
		return jsonResponse(http.StatusUnauthorized, `{}`), nil
	})})
	got, attempted, err := s.RefreshDueQuota(mediaTestContext(), client, a.ID, time.Now())
	if !attempted || err == nil || requests != 1 || browser != 0 || !AutomaticReloginDue(got, time.Now()) {
		t.Fatalf("401 did not hand off to independent login recovery: attempted=%v requests=%d browser=%d err=%v", attempted, requests, browser, err)
	}
}

func TestQuotaMaintenanceIncludesIdleGenerationExpiry(t *testing.T) {
	now := time.Now().UTC()
	base := Account{Enabled: true, ExpiresAt: now.Add(2 * time.Hour).Unix(), LastUsedAt: now.Add(-2 * time.Hour).Unix(), Quota: Quota{Status: "known", CheckedAt: now.Format(time.RFC3339Nano)}}
	for _, tc := range []struct {
		name       string
		generation *GenerationEntitlements
		wantDue    bool
	}{
		{"missing", nil, true},
		{"stale", &GenerationEntitlements{Status: "stale", CheckedAt: now.Format(time.RFC3339Nano)}, true},
		{"expired", &GenerationEntitlements{Status: "known", CheckedAt: now.Add(-6 * time.Minute).Format(time.RFC3339Nano)}, true},
		{"refresh_before_expiry", &GenerationEntitlements{Status: "known", CheckedAt: now.Add(-GenerationObservationMaxAge + time.Minute).Format(time.RFC3339Nano)}, true},
		{"fresh", &GenerationEntitlements{Status: "known", CheckedAt: now.Format(time.RFC3339Nano)}, false},
		{"invalid_time", &GenerationEntitlements{Status: "known", CheckedAt: "invalid"}, true},
		{"future_time", &GenerationEntitlements{Status: "known", CheckedAt: now.Add(time.Minute).Format(time.RFC3339Nano)}, true},
		{"failed_observation_cooling", &GenerationEntitlements{Status: "error", CheckedAt: now.Format(time.RFC3339Nano)}, false},
		{"failed_observation_due", &GenerationEntitlements{Status: "error", CheckedAt: now.Add(-automaticQuotaFailureBackoff).Format(time.RFC3339Nano)}, true},
		{"unknown_observation_cooling", &GenerationEntitlements{Status: "unknown", CheckedAt: now.Format(time.RFC3339Nano)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			a.Quota.Generation = tc.generation
			if got := MaintenanceDue(a, now); got != tc.wantDue {
				t.Fatalf("idle generation due = %v, want %v", got, tc.wantDue)
			}
		})
	}
	fresh := strictSelectionGeneration(now, 1)
	a := base
	a.Quota.Generation = &fresh
	if got, want := QuotaRefreshDueAt(a, now), now.Add(GenerationObservationMaxAge-time.Minute); !got.Equal(want) {
		t.Fatalf("fresh generation refresh scheduled at %v, want %v", got, want)
	}
	// A fresh generation snapshot must not defer an earlier balance/token plan.
	a.LastUsedAt = now.Unix()
	a.Quota.CheckedAt = now.Add(-4 * time.Minute).Format(time.RFC3339Nano)
	if got := QuotaRefreshDueAt(a, now); !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("earlier balance refresh delayed: %v", got)
	}
	a = base
	a.Quota.Generation = &fresh
	a.ExpiresAt = now.Add(6 * time.Minute).Unix()
	if got, want := QuotaRefreshDueAt(a, now), time.Unix(a.ExpiresAt, 0).Add(-5*time.Minute); !got.Equal(want) {
		t.Fatalf("earlier credential refresh delayed: %v", got)
	}
	for _, gate := range []string{"disabled", "in_flight", "settlement", "retry"} {
		a := base // Missing generation is due unless the existing gate rejects it.
		switch gate {
		case "disabled":
			a.Enabled = false
		case "in_flight":
			a.InFlight = "accepted-task"
		case "settlement":
			a.SettlementUntil = now.Add(time.Minute).Unix()
		case "retry":
			a.LastError, a.NextRefreshAt = "adobe_refresh_failed", now.Add(15*time.Minute).Unix()
		}
		if MaintenanceDue(a, now) {
			t.Fatalf("generation expiry bypassed %s gate", gate)
		}
	}
}

func TestIdleGenerationMaintenanceUsesOnlyQuotaHTTP(t *testing.T) {
	s, a := idleMediaStore(t)
	if err := s.update(a.ID, a.Revision, func(current *Account) {
		current.LastUsedAt = time.Now().Add(-2 * time.Hour).Unix()
		generation := strictSelectionGeneration(time.Now().Add(-6*time.Minute), 1)
		current.Quota.Generation = &generation
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	browsers, reads := 0, 0
	s.SetBrowserRefresher(func(context.Context, Account) (Account, error) {
		browsers++
		return Account{}, errors.New("quota maintenance must not open browser for valid credentials")
	})
	client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		reads++
		switch r.URL.Path {
		case "/v1/credits/balance":
			return jsonResponse(200, `{"total":{"quota":{"available":10}}}`), nil
		case "/v1/feature-auth-profile":
			raw, _ := json.Marshal(entitlementFixture())
			return jsonResponse(200, string(raw)), nil
		default:
			t.Errorf("quota maintenance requested non-quota endpoint: %s", r.URL.Path)
			return jsonResponse(500, `{}`), nil
		}
	})})
	fresh, attempted, err := s.RefreshDueWithPermissions(mediaTestContext(), client, a.ID, time.Now())
	if err != nil || !attempted || reads != 2 || browsers != 0 || fresh.Quota.Generation.ModelSelectionState("veo-3.1-lite", time.Now()) != ModelUsable {
		t.Fatalf("idle account did not regain known entitlement using HTTP: attempted=%v reads=%d browsers=%d err=%v", attempted, reads, browsers, err)
	}
	if fresh.Token != before.Token || fresh.Cookie != before.Cookie || fresh.Revision != before.Revision {
		t.Fatal("quota-only maintenance changed credentials")
	}
	if _, attempted, err = s.RefreshDueWithPermissions(mediaTestContext(), client, a.ID, time.Now()); err != nil || attempted || reads != 2 {
		t.Fatalf("fresh model observation was immediately refreshed again: attempted=%v reads=%d err=%v", attempted, reads, err)
	}
}

func TestGenerationMaintenancePreservesFailureBackoff(t *testing.T) {
	s, a := idleMediaStore(t)
	reads := 0
	client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		reads++
		if r.URL.Path == "/v1/feature-auth-profile" {
			return jsonResponse(503, `{"error_code":"upstream_error"}`), nil
		}
		return jsonResponse(200, `{"total":{"quota":{"available":10}}}`), nil
	})})
	failed, attempted, err := s.RefreshDueWithPermissions(mediaTestContext(), client, a.ID, time.Now())
	if err != nil || !attempted || reads != 2 || failed.Quota.Generation == nil || failed.Quota.Generation.Status != "error" {
		t.Fatalf("failed generation observation was not recorded: attempted=%v reads=%d err=%v", attempted, reads, err)
	}
	for _, store := range []*Store{s, NewStore(s.path, s.secret)} {
		if _, attempted, err = store.RefreshDueWithPermissions(mediaTestContext(), client, a.ID, time.Now()); err != nil || attempted || reads != 2 {
			t.Fatalf("same/restarted scheduler bypassed failure retry window: attempted=%v reads=%d err=%v", attempted, reads, err)
		}
	}
	// Even an independently stale balance cannot bypass the current account's
	// model-observation cooldown or consume a background refresh attempt.
	if err = s.update(a.ID, a.Revision, func(current *Account) { current.Quota.Status = "stale" }); err != nil {
		t.Fatal(err)
	}
	if _, attempted, err = s.RefreshDueWithPermissions(mediaTestContext(), client, a.ID, time.Now()); err != nil || attempted || reads != 2 {
		t.Fatalf("due balance bypassed failed model observation: attempted=%v reads=%d err=%v", attempted, reads, err)
	}
	s.mu.Lock()
	failure := s.modelFailures[a.ID]
	failure.RetryAfter = time.Now().Add(-time.Second)
	s.modelFailures[a.ID] = failure
	s.mu.Unlock()
	if err = s.update(a.ID, a.Revision, func(current *Account) {
		current.Quota.Generation.CheckedAt = time.Now().Add(-automaticQuotaFailureBackoff - time.Second).Format(time.RFC3339Nano)
	}); err != nil {
		t.Fatal(err)
	}
	if _, attempted, err = s.RefreshDueWithPermissions(mediaTestContext(), client, a.ID, time.Now()); err != nil || !attempted || reads != 4 {
		t.Fatalf("expired failure backoff blocked background recovery: attempted=%v reads=%d err=%v", attempted, reads, err)
	}
}
