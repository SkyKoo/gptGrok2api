package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
)

func adobeFreeVideoRequest(t *testing.T, fields url.Values, reference bool) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, values := range fields {
		for _, value := range values {
			if err := writer.WriteField(key, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	if reference {
		part, err := writer.CreateFormFile("input_reference", "frame.png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(adobeMediaPNG(t, 20, 20))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/videos", &body)
	r.Header.Set("Authorization", "Bearer api-secret")
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}

// Exercise the public contract through parsing, account selection, exact
// preflight and submission. The upstream is a fixture, never a live generation.
func TestAdobeVerifiedVideoOptionsReachUnifiedProtocol(t *testing.T) {
	for _, tc := range []struct {
		model, query, size, resolution, aspect string
		duration                               int
		audio                                  bool
	}{
		{"veo-3.1-lite", "seconds=4", "1920x1080", "1080p", "16:9", 4, false},
		{"veo-3.1-lite", "resolution_name=720p&aspect_ratio=9:16", "720x1280", "720p", "9:16", 8, false},
		{"veo-3.1-lite", "resolution_name=720p&aspect_ratio=9:16&generate_audio=true", "720x1280", "720p", "9:16", 8, true},
		{"veo-3.1-lite", "size=1080x1920&generate_audio=true", "1080x1920", "1080p", "9:16", 8, true},
		{"veo-3.1-fast", "seconds=6&size=1920x1080", "1920x1080", "1080p", "16:9", 6, false},
		{"veo-3.1-fast", "size=1080x1920", "1080x1920", "1080p", "9:16", 8, false},
		{"ray-3.14", "aspect_ratio=9:16", "720x1280", "720p", "9:16", 5, false},
		{"gemini-omni-flash-1.1", "aspect_ratio=9:16", "720x1280", "720p", "9:16", 5, true},
		{"gemini-omni-flash-1.2", "size=720x1280&generate_audio=true", "720x1280", "720p", "9:16", 5, true},
	} {
		t.Run(tc.model+"/"+tc.query, func(t *testing.T) {
			s := imageTaskTestServer(t)
			s.cfg.VideoDataDir = filepath.Join(s.cfg.DataDir, "videos")
			adobeFixtureAccount(t, s)
			fixture := &adobeMediaCapture{video: true}
			s.adobeClient = fixture.client(t)
			fields, _ := url.ParseQuery(tc.query)
			fields.Set("model", tc.model)
			fields.Set("prompt", "a blue ceramic mug")
			fields.Set("client_task_id", "verified-free-video")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, adobeFreeVideoRequest(t, fields, false))
			if w.Code != http.StatusOK {
				t.Fatalf("create: %d %s", w.Code, w.Body.String())
			}
			var created videoJob
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			got := waitAdobeVideoIdle(t, s, created.ID)
			if got.Model != tc.model || got.Size != tc.size || got.ResolutionName != tc.resolution || got.AspectRatio != tc.aspect || got.Seconds != fmt.Sprint(tc.duration) || got.GenerateAudio != tc.audio || got.ReferenceCount != 0 {
				t.Fatalf("request changed: %+v", got)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.preflights != 1 || fixture.submits != 1 || fixture.uploads != 0 {
				t.Fatalf("preflight=%d submit=%d upload=%d", fixture.preflights, fixture.submits, fixture.uploads)
			}
			p := fixture.payload
			if p["resolution"] != tc.resolution || p["duration"] != float64(tc.duration) || mapValue(p["generationSettings"])["aspectRatio"] != tc.aspect {
				t.Fatalf("upstream parameters changed: %+v", p)
			}
			if tc.model != "ray-3.14" && p["generateAudio"] != tc.audio {
				t.Fatal("upstream audio differs from the resolved request")
			}
			if _, exists := p["referenceBlobs"]; exists {
				t.Fatal("text-only variant gained reference blobs")
			}
		})
	}
}

func TestAdobeUnverifiedVideoCombinationsFailBeforeQueue(t *testing.T) {
	for _, tc := range []struct {
		query string
		ref   bool
	}{
		{"model=veo-3.1-fast&generate_audio=true", false},
		{"model=veo-3.1-fast&size=720x1280&generate_audio=true", false},
		{"model=veo-3.1-fast&size=1080x1920&generate_audio=true", false},
		{"model=veo-3.1-lite&seconds=4&generate_audio=true", false},
		{"model=veo-3.1-lite&resolution=720p&aspect_ratio=16:9&generate_audio=true", false},
		{"model=veo-3.1-lite&size=1080x1920&seconds=6", false},
		{"model=veo-3.1-lite&size=720x1280&seconds=4&generate_audio=true", false},
		{"model=veo-3.1-lite&size=1080x1920&seconds=6&generate_audio=true", false},
		{"model=veo-3.1-fast&resolution=720p&seconds=4", false},
		{"model=veo-3.1-lite&size=720x1280", true},
		{"model=veo-3.1-lite&size=720x1280&generate_audio=true", true},
		{"model=veo-3.1-lite&size=1080x1920&generate_audio=true", true},
		{"model=veo-3.1-lite&seconds=4", true},
		{"model=veo-3.1-fast&size=1080x1920", true},
		{"model=ray-3.14&aspect_ratio=9:16", true},
		{"model=veo-3.1-lite&size=1920x1080&resolution=720p", false},
		{"model=firefly-video&resolution=720p", false},
		{"model=gemini-omni-flash-1.1&aspect_ratio=9:16&generate_audio=false", false},
		{"model=gemini-omni-flash-1.2&aspect_ratio=9:16&generate_audio=false", false},
		{"model=gemini-omni-flash-1.1&aspect_ratio=9:16", true},
		{"model=gemini-omni-flash-1.2&size=720x1280", true},
		{"model=gemini-omni-flash-1.2&aspect_ratio=9:16&resolution=1080p", false},
		{"model=gemini-omni-flash-1.2&aspect_ratio=1:1", false},
		{"model=gemini-omni-flash-1.2&aspect_ratio=9:16&seconds=8", false},
	} {
		t.Run(fmt.Sprintf("%s/ref=%t", tc.query, tc.ref), func(t *testing.T) {
			s := imageTaskTestServer(t)
			adobeFixtureAccount(t, s)
			fixture := &adobeMediaCapture{video: true}
			s.adobeClient = fixture.client(t)
			fields, _ := url.ParseQuery(tc.query)
			fields.Set("prompt", "a blue ceramic mug")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, adobeFreeVideoRequest(t, fields, tc.ref))
			if w.Code != http.StatusBadRequest || len(s.videoJobs) != 0 || fixture.preflights != 0 || fixture.submits != 0 || fixture.uploads != 0 {
				t.Fatalf("invalid request progressed: status=%d jobs=%d preflight=%d submit=%d upload=%d body=%s", w.Code, len(s.videoJobs), fixture.preflights, fixture.submits, fixture.uploads, w.Body.String())
			}
		})
	}
}

func TestAdobeNewImageRatiosReachUnifiedProtocol(t *testing.T) {
	for _, tc := range []struct {
		model, size, aspect, resolution string
		width, height                   int
	}{
		{"gemini-3.1-flash-image-2.1", "1200x896", "4:3", "1K", 1200, 896},
		{"gemini-3.1-flash-image-2.1", "768x1376", "9:16", "1K", 768, 1376},
		{"humain-image-1", "2304x1792", "4:3", "2K", 2304, 1792},
	} {
		t.Run(tc.model+"/"+tc.aspect, func(t *testing.T) {
			s := imageTaskTestServer(t)
			adobeFixtureAccount(t, s)
			fixture := &adobeMediaCapture{output: adobeMediaPNG(t, tc.width, tc.height)}
			s.adobeClient = fixture.client(t)
			body, _ := json.Marshal(map[string]any{"model": tc.model, "prompt": "a blue ceramic mug", "size": tc.size, "n": 1})
			w := imageTaskCall(s, "POST", "/v1/images/generations", "api-secret", string(body))
			if w.Code != http.StatusOK {
				t.Fatalf("image: %d %s", w.Code, w.Body.String())
			}
			if fixture.preflights != 1 || fixture.submits != 1 || fixture.uploads != 0 || fixture.payload["resolution"] != tc.resolution || mapValue(fixture.payload["generationSettings"])["aspectRatio"] != tc.aspect {
				t.Fatal("logical size did not reach exact model parameters")
			}
			if _, exists := fixture.payload["size"]; exists {
				t.Fatal("resolution/aspect protocol must omit size")
			}
		})
	}
}

func TestAdobeUnverifiedImageOptionsFailBeforeUpstream(t *testing.T) {
	for _, id := range []string{"gemini-3.1-flash-image", "gemini-3.1-flash-image-2.1"} {
		for _, size := range []string{"2048x2048", "4096x4096"} {
			t.Run(id+"/"+size, func(t *testing.T) {
				s := imageTaskTestServer(t)
				adobeFixtureAccount(t, s)
				fixture := &adobeMediaCapture{}
				s.adobeClient = fixture.client(t)
				body, _ := json.Marshal(map[string]any{"model": id, "prompt": "a blue ceramic mug", "size": size, "n": 1})
				w := imageTaskCall(s, "POST", "/v1/images/generations", "api-secret", string(body))
				if w.Code != http.StatusBadRequest || fixture.preflights != 0 || fixture.uploads != 0 || fixture.submits != 0 {
					t.Fatalf("high-resolution request advanced: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
	// Non-square Nano 2.1 was checked without references. An edit must fail,
	// rather than dropping its image and submitting the text-only variant.
	s := imageTaskTestServer(t)
	adobeFixtureAccount(t, s)
	fixture := &adobeMediaCapture{}
	s.adobeClient = fixture.client(t)
	body, _ := json.Marshal(map[string]any{"model": "gemini-3.1-flash-image-2.1", "prompt": "a blue ceramic mug", "size": "1200x896", "images": []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(adobeMediaPNG(t, 20, 20))}})
	w := imageTaskCall(s, "POST", "/v1/images/edits", "api-secret", string(body))
	if w.Code != http.StatusBadRequest || fixture.preflights != 0 || fixture.uploads != 0 || fixture.submits != 0 {
		t.Fatalf("unverified reference combination advanced: %d %s", w.Code, w.Body.String())
	}
}
