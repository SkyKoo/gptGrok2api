package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

func adobePreparationImageBody(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"client_task_id": "fresh-media", "model": "gemini-3.1-flash-image", "prompt": "a paper boat", "size": "1024x1024",
		"images": []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(adobeMediaPNG(t, 20, 20))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func adobePreparationRequest(t *testing.T, s *Server, kind string) (string, *mediaRoute, string) {
	t.Helper()
	if kind == "video" {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, adobeVideoMediaRequest(t, true, [][]byte{adobeMediaPNG(t, 20, 20)}))
		if w.Code != 200 {
			t.Fatalf("video accept: %d %s", w.Code, w.Body.String())
		}
		var created videoJob
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		job := waitAdobeVideoIdle(t, s, created.ID)
		return job.Status, job.Route, stringValue(job.Error["code"]) + " " + stringValue(job.Error["message"])
	}
	path := "/v1/images/edits"
	if kind == "async_image" {
		path = "/api/image-tasks/edits"
	}
	w := imageTaskCall(s, "POST", path, "api-secret", adobePreparationImageBody(t))
	if kind == "async_image" {
		if w.Code != 202 {
			t.Fatalf("image accept: %d %s", w.Code, w.Body.String())
		}
		task := waitAdobeImage(t, s, "fresh-media")
		return task.Status, task.Route, task.Error
	}
	var task *imageTaskState
	s.imageTaskMu.RLock()
	for _, current := range s.imageTasks {
		if current.Route != nil && current.Route.Channel == "adobe" {
			task = current
		}
	}
	s.imageTaskMu.RUnlock()
	if task == nil {
		t.Fatalf("sync task missing: %d %s", w.Code, w.Body.String())
	}
	return task.Status, task.Route, w.Body.String()
}

func TestAdobeNewMediaPreparesSelectedAccountBeforeAnyUpstreamRequest(t *testing.T) {
	for _, kind := range []string{"sync_image", "async_image", "video"} {
		t.Run(kind, func(t *testing.T) {
			s := imageTaskTestServer(t)
			a := adobeFixtureAccount(t, s)
			claims, _ := json.Marshal(map[string]any{"user_id": a.AccountID, "exp": time.Now().Add(time.Hour).Unix(), "session": "fresh"})
			freshToken := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
			fixture := &adobeMediaCapture{video: kind == "video", expectedBearer: freshToken, expectedARP: "fixture-fresh-media-session"}
			if kind != "video" {
				fixture.output = adobeMediaPNG(t, 1024, 1024)
			}
			s.adobeClient = fixture.client(t)
			var prepares atomic.Int32
			s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
				prepares.Add(1)
				if current.ID != a.ID || current.InFlight == "" {
					t.Error("media preparation did not use the reserved account")
				}
				fixture.mu.Lock()
				if fixture.preflights != 0 || fixture.uploads != 0 || fixture.submits != 0 {
					t.Error("upstream request preceded media preparation")
				}
				fixture.mu.Unlock()
				current, err := adobeFixtureMediaRefresh(ctx, current)
				current.Token = freshToken
				return current, err
			})
			status, route, detail := adobePreparationRequest(t, s, kind)
			if kind != "video" && status != "success" || kind == "video" && !strings.Contains(detail, "upstream_poll_failed") {
				t.Fatalf("unexpected outcome: %s %s", status, detail)
			}
			if route == nil || route.AccountID != a.ID || prepares.Load() != 1 {
				t.Fatalf("selection/preparation changed: route=%+v calls=%d", route, prepares.Load())
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.preflights != 1 || fixture.uploads != 1 || fixture.submits != 1 || fixture.polls != 1 {
				t.Fatalf("unexpected calls: preflight=%d upload=%d submit=%d poll=%d", fixture.preflights, fixture.uploads, fixture.submits, fixture.polls)
			}
		})
	}
}

func TestAdobeMediaPreparationFailureStopsWithoutSwitchingAccountOrReplay(t *testing.T) {
	for _, kind := range []string{"sync_image", "async_image", "video"} {
		t.Run(kind, func(t *testing.T) {
			s := imageTaskTestServer(t)
			first := adobeFixtureAccount(t, s)
			second := first
			second.AccountID = "alternative-adobe"
			second.Email = "alternative@example.test"
			claims, _ := json.Marshal(map[string]any{"user_id": second.AccountID, "exp": time.Now().Add(time.Hour).Unix()})
			second.Token = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
			rows, err := s.adobeStore.Import([]adobe.Account{second})
			if err != nil {
				t.Fatal(err)
			}
			second = adobeFixtureModelPermissions(t, s, rows[0])
			available := 10.0
			if err = s.adobeStore.UpdateQuota(second.ID, second.Revision, adobe.Quota{Status: "known", Available: &available, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
				t.Fatal(err)
			}
			fixture := &adobeMediaCapture{video: kind == "video"}
			s.adobeClient = fixture.client(t)
			var prepares atomic.Int32
			s.adobeStore.SetBrowserRefresher(func(_ context.Context, current adobe.Account) (adobe.Account, error) {
				prepares.Add(1)
				if current.ID != first.ID {
					t.Error("preparation switched to an alternative account")
				}
				return adobe.Account{}, &adobe.Error{Stage: "relogin", Code: "media_session_incomplete", MediaDiagnostic: &adobe.MediaPreparationDiagnostic{State: "pending", ReadinessReason: "vendors_pending", ValueKind: "PRIVATE_TOKEN"}}
			})
			status, route, detail := adobePreparationRequest(t, s, kind)
			if status != "error" && status != "failed" || !strings.Contains(detail, "media_session_prepare_failed") || !strings.Contains(detail, "session_incomplete [vendors_pending]") || strings.Contains(detail, "PRIVATE_TOKEN") {
				t.Fatalf("preparation error concealed: %s %s", status, detail)
			}
			monitor, marshalErr := json.Marshal(s.monitor.snapshot())
			if marshalErr != nil || !strings.Contains(string(monitor), `"media_session_diagnostic":{"state":"pending","readiness_reason":"vendors_pending"}`) || strings.Contains(string(monitor), "PRIVATE_TOKEN") {
				t.Fatalf("safe preparation diagnostic missing from monitor: %s %v", monitor, marshalErr)
			}
			if route == nil || route.AccountID != first.ID || route.Stage != "media_session_preparing" {
				t.Fatalf("failed task lost its selected route: %+v", route)
			}
			for _, id := range []string{first.ID, second.ID} {
				account, err := s.adobeStore.Get(id)
				if err != nil || account.InFlight != "" || account.SettlementUntil != 0 || account.Quota.Status != "known" {
					t.Fatalf("unsubmitted preparation changed account reservation/quota: %+v %v", account, err)
				}
			}
			if _, err = s.adobeStore.ReserveForModel(route.JobKey, "gemini-3.1-flash-image"); err == nil || errors.Is(err, adobe.ErrNoAccount) {
				t.Fatalf("failed task's durable selection was lost: %v", err)
			}
			if kind != "sync_image" {
				_, repeatedRoute, _ := adobePreparationRequest(t, s, kind)
				if repeatedRoute == nil || repeatedRoute.JobKey != route.JobKey {
					t.Fatal("duplicate request created another task")
				}
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if prepares.Load() != 1 || fixture.preflights != 0 || fixture.uploads != 0 || fixture.submits != 0 || fixture.polls != 0 {
				t.Fatalf("failed preparation reached upstream or replayed: prepares=%d preflight=%d upload=%d submit=%d poll=%d", prepares.Load(), fixture.preflights, fixture.uploads, fixture.submits, fixture.polls)
			}
		})
	}
}
