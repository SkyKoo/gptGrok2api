package browserqueue

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquirePreservesOnlyFixedStartupDiagnostics(t *testing.T) {
	for _, code := range []string{"state_missing", "run_email_mismatch", "run_locked", "unsafe_storage", "PRIVATE-RAW-ERROR"} {
		t.Run(code, func(t *testing.T) {
			var closes atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					closes.Add(1)
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed", "code": "browser_start_failed", "diagnostic_code": code})
			}))
			defer s.Close()
			_, err := Acquire(context.Background(), s.Client(), s.URL, "", nil, nil)
			var startup *StartupError
			if err == nil || err.Error() != "browser_start_failed" || closes.Load() != 1 {
				t.Fatal("startup error changed generic contract or leaked reservation")
			}
			if code == "PRIVATE-RAW-ERROR" {
				if errors.As(err, &startup) {
					t.Fatal("raw diagnostic exposed")
				}
			} else if !errors.As(err, &startup) || startup.Code != code {
				t.Fatal("fixed diagnostic lost")
			}
		})
	}
}

func TestAcquireWaitsBeforeReadyAndReleasesOnCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "cancelled"}[cancelled], func(t *testing.T) {
			var admissions, polls, closes atomic.Int32
			var id string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer private" {
					t.Error("missing authentication")
				}
				switch r.Method {
				case http.MethodPost:
					admissions.Add(1)
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					id, _ = body["id"].(string)
					if len(id) != 48 || body["daily_direct"] != true {
						t.Error("invalid reservation")
					}
					w.Write([]byte(`{"status":"waiting_browser"}`))
				case http.MethodGet:
					polls.Add(1)
					if !strings.HasSuffix(r.URL.Path, id) {
						t.Error("changed reservation")
					}
					w.Write([]byte(`{"status":"ready"}`))
				case http.MethodDelete:
					closes.Add(1)
					w.Write([]byte(`{"status":"released"}`))
				default:
					t.Error("unexpected business request")
				}
			}))
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			seen := 0
			lease, err := Acquire(ctx, upstream.Client(), upstream.URL, "private", map[string]any{"daily_direct": true}, func(stage string) error {
				if stage == "waiting_browser" {
					seen++
					if cancelled && seen > 1 {
						cancel()
					}
				}
				return nil
			})
			if cancelled {
				if err == nil || lease != nil || polls.Load() != 0 || closes.Load() != 1 {
					t.Fatalf("cancellation lost: %v polls=%d closes=%d", err, polls.Load(), closes.Load())
				}
			} else {
				if err != nil || lease == nil || polls.Load() != 1 {
					t.Fatalf("readiness failed: %v", err)
				}
				lease.Release()
				if closes.Load() != 1 {
					t.Fatal("not released")
				}
			}
			if admissions.Load() != 1 {
				t.Fatal("queue admission replayed")
			}
		})
	}
}

func TestAcquireNeverFollowsRedirectOrLeaksError(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/secret")
		w.WriteHeader(307)
		w.Write([]byte("SENSITIVE"))
	}))
	defer s.Close()
	_, err := Acquire(context.Background(), s.Client(), s.URL, "secret", nil, nil)
	if err == nil || strings.Contains(err.Error(), "SENSITIVE") || calls.Load() != 2 {
		t.Fatalf("redirect or release handling: err=%v calls=%d", err, calls.Load())
	}
}
