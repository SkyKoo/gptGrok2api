package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

func resolutionRequestBody(a adobe.Account, key string, confirmed bool) string {
	raw, _ := json.Marshal(map[string]any{"id": a.ID, "expected_task_key": key, "expected_credential_revision": a.Revision, "confirmed": confirmed, "reason": "Reviewed Adobe history; release only local occupancy"})
	return string(raw)
}

func TestAdobeAccountResolveAdminReviewAndResumeGuard(t *testing.T) {
	for _, kind := range []string{"image", "video", "missing"} {
		t.Run(kind, func(t *testing.T) {
			s := imageTaskTestServer(t)
			a := adobeFixtureAccount(t, s)
			key := "old-unknown-task"
			_, _ = s.adobeStore.Reserve(key)
			_ = s.adobeStore.MarkUnknown(key)
			route := &mediaRoute{Channel: "adobe", AccountID: a.ID, JobKey: key, Stage: "polling"}
			handle := adobe.Job{Kind: "image", Model: "gemini-3.1-flash-image", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "SUBMITTED"}
			imageTask := &imageTaskState{ID: "old", OwnerID: "owner", Status: "error", Error: "original upstream timeout", Model: handle.Model, Route: route}
			videoTask := &videoJob{ID: "video_old", Status: "failed", Error: map[string]any{"code": "original_timeout"}, Model: "veo-3.1-lite", Route: route}
			if kind == "image" {
				s.imageTasks[imageTaskKey(imageTask.OwnerID, imageTask.ID)] = imageTask
				_ = s.saveImageTaskLocked(imageTask)
			}
			if kind == "video" {
				s.videoJobs[videoTask.ID] = videoTask
				handle.Kind = "video"
				handle.Model = videoTask.Model
				_ = s.saveVideoCheckpointLocked(videoTask)
			}
			_ = s.adobeStore.SaveJob(key, handle)
			body := resolutionRequestBody(a, key, true)
			denied := imageTaskCall(s, "POST", "/api/adobe/accounts/resolve-submission", "api-secret", body)
			if denied.Code != 401 && denied.Code != 403 {
				t.Fatal("non-admin resolved reservation")
			}
			unconfirmed := imageTaskCall(s, "POST", "/api/adobe/accounts/resolve-submission", "admin-secret", resolutionRequestBody(a, key, false))
			if unconfirmed.Code != 400 {
				t.Fatalf("missing confirmation: %d", unconfirmed.Code)
			}
			if kind != "missing" {
				if kind == "image" {
					imageTask.recovering = true
				} else {
					videoTask.recovering = true
				}
				busy := imageTaskCall(s, "POST", "/api/adobe/accounts/resolve-submission", "admin-secret", body)
				if busy.Code != 409 {
					t.Fatal("released active recovery")
				}
				imageTask.recovering = false
				videoTask.recovering = false
			}
			var submits, polls atomic.Int32
			s.adobeClient = adobeFixtureClient(t, &submits, &polls, false)
			result := imageTaskCall(s, "POST", "/api/adobe/accounts/resolve-submission", "admin-secret", body)
			if result.Code != 200 {
				t.Fatalf("resolve %d %s", result.Code, result.Body.String())
			}
			repeated := imageTaskCall(s, "POST", "/api/adobe/accounts/resolve-submission", "admin-secret", body)
			if repeated.Code != 409 {
				t.Fatal("stale confirmation reused")
			}
			if imageTask.Error != "original upstream timeout" || stringValue(videoTask.Error["code"]) != "original_timeout" {
				t.Fatal("rewrote historical error")
			}
			fresh := NewWithWorkers(s.cfg, false)
			fresh.adobeClient = adobeFixtureClient(t, &submits, &polls, false)
			if fresh.adobeRouteCanResume(route, handle.Model, handle.Kind) || fresh.resumeAdobeImageTask(imageTask) || fresh.resumeAdobeVideoJob(videoTask) {
				t.Fatal("resolved task reopened")
			}
			if submits.Load() != 0 || polls.Load() != 0 {
				t.Fatal("manual review made upstream requests")
			}
			records, err := fresh.adobeStore.SubmissionResolutions(a.ID)
			wantState := "terminal"
			if kind == "missing" {
				wantState = "missing"
			}
			if err != nil || len(records) != 1 || records[0].TaskRecordState != wantState {
				t.Fatal("audit did not record missing task history")
			}
		})
	}
}

func TestAdobeAccountQuotaRefreshUsesReadOnlyPathWhileUnknown(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	_, _ = s.adobeStore.Reserve("unknown-task")
	_ = s.adobeStore.MarkUnknown("unknown-task")
	before, _ := s.adobeStore.Get(a.ID)
	calls := 0
	s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/v1/credits/balance" && r.URL.Path != "/v1/feature-auth-profile" {
			t.Error("quota endpoint attempted refresh or generation")
		}
		return &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})})
	raw, _ := json.Marshal(map[string]any{"ids": []string{a.ID}, "credentials": false})
	response := imageTaskCall(s, "POST", "/api/adobe/accounts/refresh", "admin-secret", string(raw))
	if response.Code != 200 || calls != 2 || strings.Contains(response.Body.String(), "adobe_account_busy") {
		t.Fatal("unknown reservation still blocks quota read")
	}
	after, _ := s.adobeStore.Get(a.ID)
	after.Quota, after.QuotaRevision = before.Quota, before.QuotaRevision
	if !reflect.DeepEqual(before, after) {
		t.Fatal("quota failure changed unknown task state")
	}
}

func TestAdobePinnedImageHistorySurvivesRetentionCleanup(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	_, _ = s.adobeStore.Reserve("unknown-task")
	_ = s.adobeStore.MarkUnknown("unknown-task")
	task := &imageTaskState{ID: "old", OwnerID: "owner", Status: "error", Error: "original timeout", UpdatedAt: time.Now().Add(-365 * 24 * time.Hour).UTC().Format(time.RFC3339), Route: &mediaRoute{Channel: "adobe", AccountID: a.ID, JobKey: "unknown-task", Stage: "submit_started"}}
	key := imageTaskKey(task.OwnerID, task.ID)
	s.imageTasks[key] = task
	if err := s.saveImageTaskLocked(task); err != nil {
		t.Fatal(err)
	}
	s.cleanupExpiredImageTasks()
	if s.imageTasks[key] == nil {
		t.Fatal("cleanup deleted unresolved task")
	}
	if _, err := os.Stat(filepath.Join(s.imageTasksDir(), key+".json")); err != nil {
		t.Fatal("cleanup removed unresolved disk record")
	}
}

func TestAdobeManualResolveRealSyncAndAsyncSubmissionNeverReplays(t *testing.T) {
	for _, mode := range []string{"sync", "async"} {
		t.Run(mode, func(t *testing.T) {
			s := imageTaskTestServer(t)
			a := adobeFixtureAccount(t, s)
			var submits, polls atomic.Int32
			s.adobeClient = adobeFixtureClient(t, &submits, &polls, true)
			path := "/v1/images/generations"
			body := `{"client_task_id":"review-once","model":"gemini-3.1-flash-image","prompt":"one blue circle","size":"1024x1024","response_format":"url"}`
			if mode == "async" {
				path = "/api/image-tasks/generations"
			}
			initial := imageTaskCall(s, "POST", path, "api-secret", body)
			if mode == "sync" && initial.Code != 502 || mode == "async" && initial.Code != 202 {
				t.Fatalf("unexpected initial status: %d", initial.Code)
			}
			if mode == "async" {
				waitAdobeImage(t, s, "review-once")
			}
			pending, err := s.adobeStore.Get(a.ID)
			if err != nil || pending.Status != "submission_unknown" || pending.InFlight == "" {
				t.Fatal("fixture failed to retain unknown submission")
			}
			var taskID string
			s.imageTaskMu.RLock()
			for _, task := range s.imageTasks {
				if task.Route != nil && task.Route.JobKey == pending.InFlight {
					taskID = task.ID
				}
			}
			s.imageTaskMu.RUnlock()
			if taskID == "" {
				t.Fatal("unknown task history missing")
			}
			result := imageTaskCall(s, "POST", "/api/adobe/accounts/resolve-submission", "admin-secret", resolutionRequestBody(pending, pending.InFlight, true))
			if result.Code != 200 {
				t.Fatalf("manual resolve: %d %s", result.Code, result.Body.String())
			}
			resumed := imageTaskCall(s, "POST", "/api/image-tasks/"+taskID+"/resume-poll", "api-secret", "")
			if resumed.Code != 200 || !strings.Contains(resumed.Body.String(), `"status":"error"`) {
				t.Fatal("resolved task did not retain historical error")
			}
			if mode == "async" {
				_ = imageTaskCall(s, "POST", path, "api-secret", body)
			}
			if submits.Load() != 1 || polls.Load() != 0 {
				t.Fatalf("resolution replayed submit=%d poll=%d", submits.Load(), polls.Load())
			}
		})
	}
}

func TestAdobeTerminalHistoryRetentionDistinguishesDeletedAccountAndReadError(t *testing.T) {
	for _, state := range []string{"deleted_account", "storage_read_error"} {
		for _, status := range []string{"success", "error"} {
			t.Run(state+"/"+status, func(t *testing.T) {
				s := imageTaskTestServer(t)
				a := adobeFixtureAccount(t, s)
				task := &imageTaskState{ID: "expired", OwnerID: "owner", Status: status, UpdatedAt: time.Now().Add(-365 * 24 * time.Hour).UTC().Format(time.RFC3339), Route: &mediaRoute{Channel: "adobe", AccountID: a.ID, JobKey: "old-task", Stage: "completed"}}
				key := imageTaskKey(task.OwnerID, task.ID)
				s.imageTasks[key] = task
				if err := s.saveImageTaskLocked(task); err != nil {
					t.Fatal(err)
				}
				if state == "deleted_account" {
					if _, err := s.adobeStore.Delete([]string{a.ID}); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "adobe_accounts.json.enc"), []byte("corrupt fixture"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				s.cleanupExpiredImageTasks()
				_, retained := s.imageTasks[key]
				_, diskErr := os.Stat(filepath.Join(s.imageTasksDir(), key+".json"))
				if state == "deleted_account" {
					if retained || !os.IsNotExist(diskErr) {
						t.Fatal("deleted account left expired task history permanently retained")
					}
				} else if !retained || diskErr != nil {
					t.Fatal("unreadable account store caused task evidence to be deleted")
				}
			})
		}
	}
}
