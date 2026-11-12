package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

func TestAdobeFailedSubmissionRetainsTraceWithoutMakingItResumable(t *testing.T) {
	for _, mode := range []string{"sync_image", "async_image", "video"} {
		for _, failure := range []string{"upstream_timeout", "transport", "rejected"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				s := imageTaskTestServer(t)
				a := adobeFixtureAccount(t, s)
				var submits, polls atomic.Int32
				var requestID atomic.Value
				s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					status, body := http.StatusOK, adobeFreePreflightFixture(mode == "video")
					switch {
					case r.URL.Path == "/v2/models/generation-preflight":
					case strings.HasSuffix(r.URL.Path, "/generate-async"):
						submits.Add(1)
						requestID.Store(r.Header.Get("x-request-id"))
						switch failure {
						case "transport":
							return nil, errors.New("fixture connection interrupted")
						case "rejected":
							status, body = http.StatusForbidden, `{"error_code":"access_error"}`
						default:
							status, body = http.StatusRequestTimeout, `{"error_code":"timeout_error"}`
						}
					default:
						polls.Add(1)
						t.Errorf("unexpected upstream request: %s", r.URL.Path)
					}
					return &http.Response{StatusCode: status, ProtoMajor: 2, Header: http.Header{"Content-Type": []string{"application/json"}, "X-Access-Error": []string{"taste_exhausted"}, "X-Access-User-Context": []string{"PRIVATE_CONTEXT"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})})

				var route *mediaRoute
				var imageTask *imageTaskState
				var videoTask videoJob
				kind, modelID := "image", "gemini-3.1-flash-image"
				if mode == "video" {
					kind, modelID = "video", "veo-3.1-lite"
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, adobeVideoMediaRequest(t, true, nil))
					if w.Code != http.StatusOK {
						t.Fatalf("video creation: %d %s", w.Code, w.Body.String())
					}
					var created videoJob
					if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
						t.Fatal(err)
					}
					videoTask = waitAdobeVideoIdle(t, s, created.ID)
					if videoTask.Status != "failed" {
						t.Fatalf("video status: %s", videoTask.Status)
					}
					route = videoTask.Route
				} else {
					path := "/v1/images/generations"
					if mode == "async_image" {
						path = "/api/image-tasks/generations"
					}
					w := imageTaskCall(s, "POST", path, "api-secret", `{"client_task_id":"trace-once","model":"gemini-3.1-flash-image","prompt":"one blue circle","size":"1024x1024"}`)
					if mode == "async_image" {
						if w.Code != http.StatusAccepted {
							t.Fatalf("image creation: %d %s", w.Code, w.Body.String())
						}
						imageTask = waitAdobeImage(t, s, "trace-once")
					} else {
						want := map[string]int{"upstream_timeout": 408, "transport": 502, "rejected": 403}[failure]
						if w.Code != want {
							t.Fatalf("sync image failure: %d %s", w.Code, w.Body.String())
						}
						s.imageTaskMu.RLock()
						for _, task := range s.imageTasks {
							copy := *task
							imageTask = &copy
						}
						s.imageTaskMu.RUnlock()
					}
					if imageTask == nil || imageTask.Status != "error" {
						t.Fatal("missing failed image checkpoint")
					}
					route = imageTask.Route
				}
				if route == nil {
					t.Fatal("missing pinned route")
				}
				handle, err := s.adobeStore.LoadJob(route.JobKey)
				if err != nil {
					t.Fatalf("request trace was not persisted: %v", err)
				}
				wantStatus := "SUBMISSION_UNKNOWN"
				if failure == "rejected" {
					wantStatus = "REJECTED"
				}
				if handle.RequestID == "" || handle.RequestID != requestID.Load() || handle.Status != wantStatus || handle.PollURL != "" || handle.Model != modelID || handle.Kind != kind || handle.ModelID == "" || handle.ModelVersion == "" {
					t.Fatalf("incorrect request trace: %+v", handle)
				}
				diagnostic := handle.SubmissionDiagnostic
				if diagnostic == nil || diagnostic.ResponseReceived != (failure != "transport") {
					t.Fatal("failed submit lost response diagnostic")
				}
				if failure != "transport" && (diagnostic.HTTPProto != "2" || diagnostic.AccessError != "taste_exhausted") {
					t.Fatal("failed submit lost fixed response metadata")
				}
				rawHandle, _ := json.Marshal(handle)
				if strings.Contains(string(rawHandle), "PRIVATE_CONTEXT") {
					t.Fatal("raw response header leaked into persisted job")
				}
				if kind == "image" && (handle.Width != 1024 || handle.Height != 1024 || handle.AspectRatio != "1:1" || handle.Resolution != "1K") {
					t.Fatalf("image parameters were lost: %+v", handle)
				}
				if kind == "video" && (handle.Width != 1920 || handle.Height != 1080 || handle.Duration != 8 || !handle.GenerateAudio) {
					t.Fatalf("video parameters were lost: %+v", handle)
				}

				fresh := NewWithWorkers(s.cfg, false)
				fresh.adobeClient = s.adobeClient
				persisted, err := fresh.adobeStore.LoadJob(route.JobKey)
				if err != nil || persisted.RequestID != handle.RequestID || !reflect.DeepEqual(persisted.SubmissionDiagnostic, handle.SubmissionDiagnostic) {
					t.Fatal("restart lost the request ID or safe diagnostic")
				}
				if fresh.adobeRouteCanResume(route, modelID, kind) {
					t.Fatal("request ID alone authorized recovery")
				}
				if _, err = fresh.adobeStore.Resume(route.JobKey, a.ID); err == nil {
					t.Fatal("request ID alone authorized store resume")
				}
				if kind == "video" {
					if fresh.resumeAdobeVideoJob(fresh.videoJobs[videoTask.ID]) {
						t.Fatal("failed video without poll URL was reopened")
					}
				} else {
					loaded := fresh.imageTasks[imageTaskKey(imageTask.OwnerID, imageTask.ID)]
					if loaded == nil || fresh.resumeAdobeImageTask(loaded) {
						t.Fatal("failed image without poll URL was reopened")
					}
				}
				account, err := fresh.adobeStore.Get(a.ID)
				if err != nil || (failure != "rejected" && (account.InFlight != route.JobKey || account.Status != "submission_unknown")) || (failure == "rejected" && account.InFlight != "") {
					t.Fatalf("failure classification changed: status=%s in_flight=%s error=%v", account.Status, account.InFlight, err)
				}
				if submits.Load() != 1 || polls.Load() != 0 {
					t.Fatalf("submit=%d poll=%d", submits.Load(), polls.Load())
				}
			})
		}
	}
}

func TestAdobeFailedTraceWriteStillPreservesUnknownReservation(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	var submits, polls atomic.Int32
	s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/v2/models/generation-preflight":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(adobeFreePreflightFixture(false))), Request: r}, nil
		case strings.HasSuffix(r.URL.Path, "/generate-async"):
			submits.Add(1)
			// Preparation must first validate the intact journal. Simulate the
			// disk failure only after the generation request has left the client.
			if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "adobe_jobs"), []byte("fixture blocks directory creation"), 0600); err != nil {
				t.Error(err)
			}
			return nil, errors.New("fixture connection interrupted after submit")
		default:
			polls.Add(1)
			t.Errorf("unexpected upstream request: %s", r.URL.Path)
			return nil, errors.New("unexpected upstream request")
		}
	})})
	w := imageTaskCall(s, "POST", "/v1/images/generations", "api-secret", `{"model":"gemini-3.1-flash-image","prompt":"one blue circle","size":"1024x1024"}`)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "adobe_submit_transport_error") || !strings.Contains(w.Body.String(), "request state could not be saved") || strings.Contains(w.Body.String(), s.cfg.DataDir) {
		t.Fatalf("unsafe or missing persistence failure: %d %s", w.Code, w.Body.String())
	}
	account, err := s.adobeStore.Get(a.ID)
	if err != nil || account.Status != "submission_unknown" || account.InFlight == "" {
		t.Fatal("trace write failure released the unknown task")
	}
	if submits.Load() != 1 || polls.Load() != 0 {
		t.Fatalf("submit=%d poll=%d", submits.Load(), polls.Load())
	}
}
