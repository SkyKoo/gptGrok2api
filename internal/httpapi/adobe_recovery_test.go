package httpapi

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

func seedAdobeRecoveryVideo(t *testing.T, s *Server, stage string, withHandle bool) string {
	t.Helper()
	a := adobeFixtureAccount(t, s)
	id, key := "video_adobe_recovery", "video-video_adobe_recovery"
	if _, err := s.adobeStore.Reserve(key); err != nil {
		t.Fatal(err)
	}
	if withHandle {
		handle := adobe.Job{Kind: "video", Model: "veo-3.1-lite", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "SUBMITTED"}
		if err := s.adobeStore.SaveJob(key, handle); err != nil {
			t.Fatal(err)
		}
	}
	job := &videoJob{ID: id, Model: "veo-3.1-lite", Status: "in_progress", Route: &mediaRoute{Channel: "adobe", AccountID: a.ID, JobKey: key, Stage: stage}}
	s.videoJobs[id] = job
	s.monitor.start(id, "/v1/videos", job.Model, "fixture")
	if err := s.saveVideoCheckpointLocked(job); err != nil {
		t.Fatal(err)
	}
	return id
}

func waitAdobeVideoIdle(t *testing.T, s *Server, id string) videoJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.videoMu.RLock()
		copy := *s.videoJobs[id]
		s.videoMu.RUnlock()
		if !copy.recovering && (copy.Status == "completed" || copy.Status == "failed") {
			return copy
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("video recovery did not terminate")
	return videoJob{}
}

func TestAdobeVideoRepeatedRecoveryDoesNotInterruptActivePoll(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	id := seedAdobeRecoveryVideo(t, s, "polling", true)
	fresh := NewWithWorkers(s.cfg, false)
	started, release := make(chan struct{}, 1), make(chan struct{})
	var polls, submits atomic.Int32
	fresh.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			submits.Add(1)
		}
		if strings.Contains(r.URL.Path, "/v2/jobs/result/") {
			polls.Add(1)
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"FAILED"}`)), Request: r}, nil
	})})
	t.Cleanup(func() { close(release); waitAdobeVideoIdle(t, fresh, id) })
	fresh.recoverVideoTasks()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery never polled the existing handle")
	}
	fresh.recoverVideoTasks()
	fresh.videoMu.RLock()
	status, code := fresh.videoJobs[id].Status, stringValue(fresh.videoJobs[id].Error["code"])
	fresh.videoMu.RUnlock()
	if status != "in_progress" || code == "video_interrupted" {
		t.Fatalf("second recovery interrupted live poll: status=%s code=%s", status, code)
	}
	if submits.Load() != 0 || polls.Load() != 1 {
		t.Fatalf("recovery submitted=%d polled=%d", submits.Load(), polls.Load())
	}
}

func TestAdobeImageSavedHandleRecoversSubmitCheckpointGap(t *testing.T) {
	for _, withHandle := range []bool{true, false} {
		name := "without_handle"
		if withHandle {
			name = "with_handle"
		}
		t.Run(name, func(t *testing.T) {
			s := imageTaskTestServer(t)
			a := adobeFixtureAccount(t, s)
			key := "image-submit-gap"
			if _, err := s.adobeStore.Reserve(key); err != nil {
				t.Fatal(err)
			}
			handle := adobe.Job{Kind: "image", Model: "gemini-3.1-flash-image", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "SUBMITTED"}
			if withHandle {
				if err := s.adobeStore.SaveJob(key, handle); err != nil {
					t.Fatal(err)
				}
			}
			task := &imageTaskState{ID: "submit-gap", OwnerID: "owner", Model: handle.Model, Mode: "generate", N: 1, Status: "running", Route: &mediaRoute{Channel: "adobe", AccountID: a.ID, JobKey: key, Stage: "submit_started"}}
			if err := s.saveImageTaskLocked(task); err != nil {
				t.Fatal(err)
			}
			fresh := NewWithWorkers(s.cfg, false)
			var submits, polls atomic.Int32
			fresh.adobeClient = adobeFixtureClient(t, &submits, &polls, false)
			fresh.recoverAdobeImageTasks()
			got := waitAdobeImage(t, fresh, task.ID)
			want := "error"
			if withHandle {
				want = "success"
			}
			if got.Status != want {
				t.Fatalf("with_handle=%v status=%s error=%s", withHandle, got.Status, got.Error)
			}
			wantPolls := int32(0)
			if withHandle {
				wantPolls = 1
			}
			if submits.Load() != 0 || polls.Load() != wantPolls {
				t.Fatalf("with_handle=%v submitted=%d polled=%d", withHandle, submits.Load(), polls.Load())
			}
		})
	}
}

func TestAdobeVideoSavedHandleRecoversSubmitCheckpointGap(t *testing.T) {
	for _, withHandle := range []bool{true, false} {
		name := "without_handle"
		if withHandle {
			name = "with_handle"
		}
		t.Run(name, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			id := seedAdobeRecoveryVideo(t, s, "submit_started", withHandle)
			fresh := NewWithWorkers(s.cfg, false)
			var submits, polls atomic.Int32
			fresh.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					submits.Add(1)
				}
				if strings.Contains(r.URL.Path, "/v2/jobs/result/") {
					polls.Add(1)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"FAILED"}`)), Request: r}, nil
			})})
			fresh.recoverVideoTasks()
			job := waitAdobeVideoIdle(t, fresh, id)
			wantCode, wantPolls := "video_interrupted", int32(0)
			if withHandle {
				wantCode, wantPolls = "upstream_poll_failed", 1
			}
			if stringValue(job.Error["code"]) != wantCode {
				t.Fatalf("with_handle=%v status=%s code=%v", withHandle, job.Status, job.Error["code"])
			}
			if submits.Load() != 0 || polls.Load() != wantPolls {
				t.Fatalf("with_handle=%v submitted=%d polled=%d", withHandle, submits.Load(), polls.Load())
			}
		})
	}
}

func TestAdobeCompletedVideoRecoveryReleasesLeftoverReservation(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	id := seedAdobeRecoveryVideo(t, s, "polling", true)
	job := s.videoJobs[id]
	handle, err := s.adobeStore.LoadJob(job.Route.JobKey)
	if err != nil {
		t.Fatal(err)
	}
	handle.Status = "COMPLETED"
	if err := s.adobeStore.SaveJob(job.Route.JobKey, handle); err != nil {
		t.Fatal(err)
	}
	s.videoMu.Lock()
	job.Route.Stage = "completed"
	s.videoMu.Unlock()
	s.setVideoStatus(job, "completed", 100, nil)
	// The process stopped after the terminal task was durable but before Release.
	before, err := s.adobeStore.Get(job.Route.AccountID)
	if err != nil || before.InFlight == "" {
		t.Fatal("fixture has no pending reservation")
	}
	fresh := NewWithWorkers(s.cfg, false)
	var submits, polls atomic.Int32
	fresh.adobeClient = adobeFixtureClient(t, &submits, &polls, false)
	fresh.StartWorkers()
	after, err := fresh.adobeStore.Get(job.Route.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if after.InFlight != "" {
		t.Fatal("durably completed video kept account permanently busy")
	}
	if submits.Load() != 0 || polls.Load() != 0 {
		t.Fatalf("completed recovery contacted upstream: submitted=%d polled=%d", submits.Load(), polls.Load())
	}
	if fresh.resumeAdobeVideoJob(fresh.videoJobs[id]) {
		waitAdobeVideoIdle(t, fresh, id)
		t.Fatal("resume-poll reopened an already completed video")
	}
}

func TestAdobeSyncPollFailureReturnsResumableTaskWithoutResubmission(t *testing.T) {
	s := imageTaskTestServer(t)
	account := adobeFixtureAccount(t, s)
	var generated bytes.Buffer
	if err := png.Encode(&generated, image.NewRGBA(image.Rect(0, 0, 1024, 1024))); err != nil {
		t.Fatal(err)
	}
	var submits, polls, downloads atomic.Int32
	s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, status, mime := `{}`, 200, "application/json"
		switch {
		case strings.Contains(r.URL.Path, "generate-async"):
			submits.Add(1)
			body = `{"links":{"result":{"href":"https://bks-epo8552.adobe.io/v2/jobs/result/fixture"}}}`
		case strings.Contains(r.URL.Path, "/v2/jobs/result/"):
			if polls.Add(1) == 1 {
				status, body = 502, `{"code":"temporary_failure"}`
			} else {
				body = `{"status":"COMPLETED","outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/fixture.png"}}]}`
			}
		case r.URL.Hostname() == "pre-signed-firefly-prod.s3-accelerate.amazonaws.com":
			downloads.Add(1)
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("credentials sent to download host")
			}
			body, mime = generated.String(), "image/png"
		default:
			t.Errorf("unexpected recovery request path: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{mime}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
	first := imageTaskCall(s, "POST", "/v1/images/generations", "api-secret", `{"model":"gemini-3.1-flash-image","prompt":"one blue circle","size":"1024x1024","response_format":"url"}`)
	if first.Code != 502 {
		t.Fatalf("sync poll error: %d %s", first.Code, first.Body.String())
	}
	match := regexp.MustCompile(`task_id=([a-zA-Z0-9_-]+)`).FindStringSubmatch(first.Body.String())
	if len(match) != 2 {
		t.Fatal("sync failure omitted recoverable task ID")
	}
	taskID := match[1]
	pending, err := s.adobeStore.Get(account.ID)
	if err != nil || pending.InFlight == "" {
		t.Fatal("poll failure lost its pinned reservation")
	}
	resumed := imageTaskCall(s, "POST", "/api/image-tasks/"+taskID+"/resume-poll", "api-secret", "")
	if resumed.Code != 202 {
		t.Fatalf("resume: %d %s", resumed.Code, resumed.Body.String())
	}
	task := waitAdobeImage(t, s, taskID)
	if task.Status != "success" || len(task.Data) != 1 || stringValue(task.Data[0]["url"]) == "" {
		t.Fatalf("resume failed: status=%s error=%s", task.Status, task.Error)
	}
	released, err := s.adobeStore.Get(account.ID)
	if err != nil || released.InFlight != "" {
		t.Fatal("successful resume did not release account")
	}
	if submits.Load() != 1 || polls.Load() != 2 || downloads.Load() != 1 {
		t.Fatalf("submit=%d poll=%d download=%d", submits.Load(), polls.Load(), downloads.Load())
	}
}
