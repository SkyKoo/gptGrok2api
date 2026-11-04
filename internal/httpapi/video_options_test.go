package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestVideoOptions(t *testing.T) {
	for _, tc := range []struct {
		name, form string
		want       videoOptions
	}{
		{"defaults", "", videoOptions{6, "720x1280", "9:16", "720p", "custom"}},
		{"explicit settings", "seconds=6&size=1280x720&resolution_name=480p&preset=normal", videoOptions{6, "1280x720", "16:9", "480p", "normal"}},
		{"aliases", "seconds=6&aspect_ratio=3:2&resolution=480p&preset=fun", videoOptions{6, "1792x1024", "3:2", "480p", "fun"}},
		{"matching aliases", "size=1024x1792&aspect_ratio=2:3&resolution=720p&resolution_name=720p&quality=standard", videoOptions{6, "1024x1792", "2:3", "720p", "custom"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/videos", nil)
			r.Form, _ = url.ParseQuery(tc.form)
			got, err := parseVideoOptions(r)
			if err != nil || got != tc.want {
				t.Fatalf("got=%+v want=%+v err=%v", got, tc.want, err)
			}
		})
	}
}

func videoOptionsRequest(t *testing.T, fields url.Values) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("prompt", "a paper plane")
	for key, values := range fields {
		for _, value := range values {
			_ = writer.WriteField(key, value)
		}
	}
	_ = writer.Close()
	r := httptest.NewRequest("POST", "/v1/videos", &body)
	r.Header.Set("Authorization", "Bearer api-secret")
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}

func TestVideoInvalidOptionsNeverQueueOrCallUpstream(t *testing.T) {
	s := newVideoTestServer(adminTestConfig(t.TempDir()))
	for _, form := range []string{
		"seconds=10", "seconds=15", "seconds=0", "seconds=-1", "seconds=bad", "seconds=6.5", "seconds=5", "seconds=9999999999999999999999",
		"size=1920x1080", "aspect_ratio=auto", "aspect_ratio=4:3", "aspect_ratio=21:9", "size=720x1280&aspect_ratio=16:9",
		"resolution_name=1080p", "resolution_name=4k", "resolution_name=480p&resolution=720p", "resolution_name=480p&resolution_name=720p",
		"quality=high", "preset=unknown", "audio=false", "search=false", "duration=5", "seed=1", "input_reference=https://example.test/a.png",
	} {
		t.Run(form, func(t *testing.T) {
			fields, _ := url.ParseQuery(form)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, videoOptionsRequest(t, fields))
			s.videoMu.RLock()
			n := len(s.videoJobs)
			s.videoMu.RUnlock()
			if w.Code != http.StatusBadRequest || n != 0 {
				t.Fatalf("status=%d jobs=%d body=%s", w.Code, n, w.Body.String())
			}
		})
	}
}

func TestVideoOptionsReachUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, form, resolution, aspect, mode string
		lengths                              []int
	}{
		{"default", "", "720p", "9:16", "custom", []int{6}},
		{"480p landscape", "seconds=6&size=1280x720&resolution_name=480p&preset=normal", "480p", "16:9", "normal", []int{6}},
		{"480p square", "seconds=6&aspect_ratio=1:1&resolution=480p&preset=fun", "480p", "1:1", "extremely-crazy", []int{6}},
		{"720p wide", "seconds=6&aspect_ratio=3:2&resolution=720p&preset=spicy", "720p", "3:2", "extremely-spicy-or-crazy", []int{6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var payloads []map[string]any
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/post":
					fmt.Fprint(w, `{"post":{"id":"parent"}}`)
				case "/chat":
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					mu.Lock()
					payloads = append(payloads, payload)
					index := len(payloads)
					mu.Unlock()
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"result\":{\"response\":{\"streamingVideoGenerationResponse\":{\"progress\":100,\"videoPostId\":\"result-%d\",\"parentPostId\":\"parent\",\"videoUrl\":\"/video.mp4\"}}}}\n", index)
					fmt.Fprintln(w, "data: [DONE]")
				case "/video.mp4":
					w.Header().Set("Content-Type", "video/mp4")
					_, _ = w.Write(videoTestMP4("test-media", "metadata"))
				default:
					t.Errorf("unexpected upstream request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer up.Close()
			s := newVideoTestServer(adminTestConfig(t.TempDir()))
			s.mediaProvider = provider.NewMedia(up.Client(), up.URL+"/chat", up.URL+"/post", up.URL+"/upload", up.URL, s.cfg.RequestTimeout)
			_, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"media_quota": testGrokMediaSnapshot(), "access_token": "test", "enabled": true, "status": "active"}})
			if err != nil {
				t.Fatal(err)
			}
			fields, _ := url.ParseQuery(tc.form)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, videoOptionsRequest(t, fields))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var created videoJob
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if created.ResolutionName != tc.resolution || created.AspectRatio != tc.aspect {
				t.Fatalf("incorrect submitted metadata: %+v", created)
			}
			var result videoJob
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				s.videoMu.RLock()
				result = *s.videoJobs[created.ID]
				s.videoMu.RUnlock()
				if result.Status == "completed" || result.Status == "failed" {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if result.Status != "completed" {
				t.Fatalf("video result: %+v", result)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(payloads) != len(tc.lengths) {
				t.Fatalf("segments=%d", len(payloads))
			}
			for i, payload := range payloads {
				config := mapValue(mapValue(mapValue(mapValue(payload["responseMetadata"])["modelConfigOverride"])["modelMap"])["videoGenModelConfig"])
				if config["resolutionName"] != tc.resolution || config["aspectRatio"] != tc.aspect || config["videoLength"] != float64(tc.lengths[i]) {
					t.Fatalf("segment %d: %+v", i, config)
				}
				if !strings.HasSuffix(stringValue(payload["message"]), "--mode="+tc.mode) {
					t.Fatalf("segment %d lost preset: %v", i, payload["message"])
				}
			}
		})
	}
}

func TestVideoBothResolutionsRespectZeroSharedQuota(t *testing.T) {
	for _, resolution := range []string{"480p", "720p"} {
		t.Run(resolution, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("zero quota called upstream")
				http.Error(w, "unexpected", 500)
			}))
			defer up.Close()
			s := newVideoTestServer(adminTestConfig(t.TempDir()))
			s.mediaProvider = provider.NewMedia(up.Client(), up.URL+"/chat", up.URL+"/post", up.URL+"/upload", up.URL, s.cfg.RequestTimeout)
			snapshot := testGrokMediaSnapshot()
			mapValue(mapValue(snapshot["quotas"])["video720p"])["remaining"] = 0
			_, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"media_quota": snapshot, "access_token": "test", "enabled": true, "status": "active"}})
			if err != nil {
				t.Fatal(err)
			}
			job := &videoJob{ID: "zero-quota", Model: "grok-imagine-video"}
			s.runVideoJob(job, "paper plane", "16:9", resolution, 6, "custom", nil)
			if job.Status != "failed" || job.Error["code"] != "no_available_account" {
				t.Fatalf("zero quota accepted: %+v", job)
			}
		})
	}
}
