package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestXAIBillingActualNestedConfig(t *testing.T) {
	cases := []struct {
		name, raw, status string
		remaining         any
		noQuota           bool
	}{
		{"zero_allowance", `{"config":{"monthlyLimit":{"val":0},"used":{"val":0},"onDemandCap":{"val":0},"billingPeriodStart":"2026-11-01T00:00:00+00:00","billingPeriodEnd":"2026-12-01T00:00:00+00:00"}}`, "success", nil, false},
		{"partially_used", `{"config":{"monthlyLimit":{"val":"200"},"used":{"val":"50"},"onDemandCap":{"val":0}}}`, "success", float64(75), false},
		{"missing", `{"config":{"billingPeriodEnd":"2026-12-01T00:00:00Z"}}`, "unavailable", nil, false},
		{"legacy_percent", `{"creditUsagePercent":23.5}`, "success", float64(76.5), false},
		{"missing_percent", `{}`, "unavailable", nil, false},
		{"on_demand", `{"config":{"monthlyLimit":{"val":0},"used":{"val":0},"onDemandCap":{"val":100}}}`, "success", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var input map[string]any
			json.Unmarshal([]byte(tc.raw), &input)
			got := parseXAIBilling(input)
			if got["status"] != tc.status || got["remaining_percent"] != tc.remaining {
				t.Fatalf("unexpected normalized quota: %#v", got)
			}
			if value, _ := got["no_quota"].(bool); value != tc.noQuota {
				t.Fatalf("wrong zero semantics: %#v", got)
			}
			if tc.name == "zero_allowance" && got["period"].(map[string]any)["end"] != "2026-12-01T00:00:00+00:00" {
				t.Fatal("period lost")
			}
		})
	}
}
func TestXAIQuotaQueryDoesNotCallModelAndPersistsRefreshedExpiry(t *testing.T) {
	var billing, refresh int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refresh++
			json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600})
		case "/billing":
			billing++
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Error("wrong access token")
			}
			w.Write([]byte(`{"creditUsagePercent":10}`))
		default:
			t.Errorf("unexpected model call: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	got := NewXAIProbe(upstream.URL, upstream.URL+"/token", upstream.Client()).QueryQuota(context.Background(), map[string]any{"access_token": "old", "refresh_token": "refresh", "expires_at": time.Now().Add(-time.Minute).Unix()})
	if got.Quota["remaining_percent"] != float64(90) || got.AccessToken != "new-access" || got.ExpiresAt < time.Now().Unix()+3500 || billing != 1 || refresh != 1 {
		t.Fatalf("query result incorrect: status=%v billing=%d refresh=%d", got.Quota, billing, refresh)
	}
}
func TestXAIQuotaQueryFailureIsNotZeroAndDoesNotFollowRedirect(t *testing.T) {
	for _, status := range []int{302, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/leak")
				w.WriteHeader(status)
				w.Write([]byte("SECRET must not escape"))
			}))
			defer server.Close()
			got := NewXAIProbe(server.URL, "", server.Client()).QueryQuota(context.Background(), map[string]any{"access_token": "access"})
			if got.Quota["status"] != "error" || got.Quota["remaining_percent"] != nil || calls != 1 {
				t.Fatalf("error treated as zero or retried: %#v calls=%d", got.Quota, calls)
			}
			if got.Quota["error"] == "SECRET must not escape" {
				t.Fatal("raw error exposed")
			}
		})
	}
}

func TestXAIQuotaRefreshDoesNotRedirectCredentials(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "/leak")
		w.WriteHeader(307)
	}))
	defer upstream.Close()
	result := NewXAIProbe(upstream.URL, upstream.URL+"/token", upstream.Client()).QueryQuota(context.Background(), map[string]any{"refresh_token": "SECRET"})
	if calls != 1 || result.Quota["code"] != "refresh_failed" {
		t.Fatalf("refresh redirect was followed or not diagnosed: calls=%d quota=%v", calls, result.Quota)
	}
}
