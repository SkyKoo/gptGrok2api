package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestXAIProbeRefreshesAfterUnauthorizedAndReadsBilling(t *testing.T) {
	responseCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			responseCalls++
			if r.Header.Get("Authorization") == "Bearer old-token" {
				http.Error(w, `{"error":{"code":"invalid_token","message":"expired"}}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp-1", "usage": map[string]any{"total_tokens": 1}})
		case "/billing":
			_ = json.NewEncoder(w).Encode(map[string]any{"creditUsagePercent": 23.5})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "new-token", "refresh_token": "new-refresh", "id_token": "new-id"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result := NewXAIProbe(server.URL, server.URL+"/token", server.Client()).Probe(context.Background(), map[string]any{"access_token": "old-token", "refresh_token": "refresh"})
	if result.Status != "valid" || result.AccessToken != "new-token" || result.RefreshToken != "new-refresh" || responseCalls != 2 {
		t.Fatalf("unexpected probe result: %#v calls=%d", result, responseCalls)
	}
	if result.Quota["used_percent"] != 23.5 || result.Quota["remaining_percent"] != 76.5 {
		t.Fatalf("unexpected quota: %#v", result.Quota)
	}
}

func TestXAIProbeSupportsZeroBillingWithRealCallAndCurrentHeaders(t *testing.T) {
	for _, code := range []int{200, 403, 426, 429} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-grok-client-version") != "1.0.70" || r.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" || r.Header.Get("User-Agent") != "xai-grok-workspace/1.0.70" {
					t.Error("obsolete CLI protocol")
				}
				if r.URL.Path == "/billing" {
					w.Write([]byte(`{"config":{"monthlyLimit":{"val":0},"used":{"val":0},"onDemandCap":{"val":0}}}`))
					return
				}
				if r.URL.Path != "/responses" {
					t.Error("permission rejection must not refresh or retry")
				}
				calls++
				w.WriteHeader(code)
				if code == 200 {
					w.Write([]byte(`{"status":"completed","output":[{"type":"message"}]}`))
				} else {
					w.Write([]byte(`{"error":{"code":"permission_denied"}}`))
				}
			}))
			defer server.Close()
			t.Setenv("CFM_XAI_CLI_CLIENT_VERSION", "")
			result := NewXAIProbe(server.URL, server.URL+"/token", server.Client()).Probe(context.Background(), map[string]any{"access_token": "a", "refresh_token": "r"})
			if calls != 1 {
				t.Fatal("model request replayed")
			}
			if code == 200 && (result.Status != "valid" || result.Quota["monthly_zero"] != true || result.Quota["no_quota"] == true || result.Quota["remaining_percent"] != nil) {
				t.Fatalf("zero billing incorrectly disables valid credential: %+v", result)
			}
			if code == 403 && result.Status != "denied" {
				t.Fatal("permission denial treated as invalid token")
			}
			if code == 426 && result.Status != "unknown" {
				t.Fatal("protocol rejection treated as exhausted quota")
			}
		})
	}
}
