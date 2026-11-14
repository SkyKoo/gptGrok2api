package browserqueue

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestStartupTransitionsSurvivePollingAndTerminalResponse(t *testing.T) {
	for _, terminal := range []string{"ready", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			var posts, polls, closes atomic.Int32
			first := StartupEvent{Sequence: 1, Stage: "starting_browser", Status: "started"}
			second := StartupEvent{Sequence: 2, Stage: "starting_browser", Status: "completed", ElapsedMS: 37, DurationMS: 37}
			third := StartupEvent{Sequence: 3, Stage: "adobe_loading_account_page", Status: "started", ElapsedMS: 37}
			fourth := StartupEvent{Sequence: 4, Stage: third.Stage, Status: "completed", ElapsedMS: 1500, DurationMS: 1463}
			if terminal == "failed" {
				fourth.Status, fourth.ExceptionType = "failed", "timeout"
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				result := status{Status: "starting_browser", StartupEvents: []StartupEvent{first, second, third}}
				switch r.Method {
				case http.MethodPost:
					posts.Add(1)
				case http.MethodGet:
					if polls.Add(1) == 2 {
						result.Status = terminal
						result.StartupEvents = append(result.StartupEvents, fourth)
					}
				case http.MethodDelete:
					closes.Add(1)
				}
				_ = json.NewEncoder(w).Encode(result)
			}))
			defer s.Close()
			var seen []StartupEvent
			var heartbeats int
			lease, err := AcquireWithStartupProgress(context.Background(), s.Client(), s.URL, "", nil,
				func(stage string) error {
					if stage == third.Stage {
						heartbeats++
					}
					return nil
				},
				func(event StartupEvent) error { seen = append(seen, event); return nil })
			if (err != nil) != (terminal == "failed") {
				t.Fatalf("terminal %s: %v", terminal, err)
			}
			if lease != nil {
				lease.Release()
			}
			if !reflect.DeepEqual(seen, []StartupEvent{first, second, third, fourth}) {
				t.Fatalf("transitions lost or duplicated: %+v", seen)
			}
			if posts.Load() != 1 || closes.Load() != 1 || polls.Load() != 2 || heartbeats < 3 {
				t.Fatal("launch, cleanup or repeated cancellation checks changed")
			}
		})
	}
}

func TestStartupProgressGuardStopsAndClosesWithoutReplaying(t *testing.T) {
	var posts, closes atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		if r.Method == http.MethodDelete {
			closes.Add(1)
		}
		_ = json.NewEncoder(w).Encode(status{Status: "ready", StartupEvents: []StartupEvent{{Sequence: 1, Stage: "starting_browser", Status: "completed"}}})
	}))
	defer s.Close()
	stopped := errors.New("stopped")
	lease, err := AcquireWithStartupProgress(context.Background(), s.Client(), s.URL, "", nil, nil, func(StartupEvent) error { return stopped })
	if lease != nil || !errors.Is(err, stopped) || posts.Load() != 1 || closes.Load() != 1 {
		t.Fatal("progress guard did not release reservation")
	}
}

func TestStartupEventsAreAllowlistedAndBounded(t *testing.T) {
	base := StartupEvent{Sequence: 1, Stage: "starting_browser", Status: "started"}
	if !base.Valid() {
		t.Fatal("valid event rejected")
	}
	boundary := base
	boundary.ElapsedMS, boundary.DurationMS = 300000, 300000
	if !boundary.Valid() {
		t.Fatal("progress at the extended command deadline rejected")
	}
	for _, exception := range []string{"proxy_tunnel", "proxy_connection", "proxy_auth", "dns", "network_timeout", "tls", "network_connection"} {
		candidate := base
		candidate.ExceptionType = exception
		if !candidate.Valid() {
			t.Fatalf("network exception category rejected: %s", exception)
		}
	}
	for _, modify := range []func(*StartupEvent){
		func(e *StartupEvent) { e.Stage = "PRIVATE_URL" },
		func(e *StartupEvent) { e.ExceptionType = "SECRET" },
		func(e *StartupEvent) { e.Status = "SECRET" },
		func(e *StartupEvent) { e.Sequence = 0 },
		func(e *StartupEvent) { e.ElapsedMS = 300001 },
		func(e *StartupEvent) { e.DurationMS = -1 },
		func(e *StartupEvent) { e.DurationMS = 1 },
	} {
		candidate := base
		modify(&candidate)
		if candidate.Valid() {
			t.Fatalf("unsafe event accepted: %+v", candidate)
		}
	}
}
