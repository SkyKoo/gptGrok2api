package register

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHMERequestPreservesRateLimitStageAndRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		code   string
		want   string
	}{
		{"rate limited", http.StatusTooManyRequests, "UPSTREAM_RATE_LIMITED", "upstream_rate_limited"},
		{"bad gateway is not rate limited", http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "upstream_unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": tt.code, "stage": "alias_generate", "upstream_status": tt.status})
			}))
			defer srv.Close()
			h, err := NewHME(HMEConfig{BaseURL: srv.URL, Password: "synthetic", AccountID: "acc"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			err = h.request(context.Background(), http.MethodPost, "/api/create", map[string]string{"label": "synthetic"}, nil)
			var failure *Failure
			if !errors.As(err, &failure) {
				t.Fatalf("error type lost: %v", err)
			}
			if failure.Code != tt.want || failure.Operation != "alias_create" || failure.UpstreamStage != "alias_generate" || failure.UpstreamStatus != tt.status || failure.RetryAfter != "120" {
				t.Fatalf("metadata lost: %+v", failure)
			}
		})
	}
}

func TestHMERequestDoesNotTrustArbitraryRetryAfterOrMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "private-cookie\r\nX-Leak: secret")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": "private-secret", "message": "password=secret"})
	}))
	defer srv.Close()
	h, _ := NewHME(HMEConfig{BaseURL: srv.URL, Password: "synthetic", AccountID: "acc"}, 0)
	defer h.Close()
	err := h.request(context.Background(), http.MethodPost, "/api/create", nil, nil)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "upstream_error" || failure.RetryAfter != "" {
		t.Fatalf("unsafe upstream data trusted: %+v", failure)
	}
	if failure.Error() == "" || len(failure.Error()) > 300 {
		t.Fatalf("unexpected diagnostic: %s", failure.Error())
	}
}
