package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"

	"github.com/HugoSmits86/nativewebp"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type adobeMediaCapture struct {
	mu                                  sync.Mutex
	uploads, submits, polls, preflights int
	payload                             map[string]any
	output                              []byte
	uploadDenied                        bool
	preflightPaid                       bool
	video                               bool
	expectedProxy                       string
	afterSubmit                         func()
}

func adobeMediaPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func (f *adobeMediaCapture) client(t *testing.T) *adobe.Client {
	t.Helper()
	return adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.expectedProxy != "" {
			endpoint, selected := proxyruntime.URLSelectionFromContext(r.Context())
			if !selected || endpoint != f.expectedProxy {
				t.Errorf("Adobe request %s did not use the selected daily proxy", r.URL.Path)
			}
		}
		status, body, mime := 200, `{}`, "application/json"
		switch {
		case r.URL.Path == "/v2/models/generation-preflight":
			f.preflights++
			body = adobeFreePreflightFixture(f.video)
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			for _, m := range adobe.FreeModels() {
				if request["modelId"] == m.ModelID && request["modelVersion"] == m.ModelVersion {
					var response map[string]any
					_ = json.Unmarshal([]byte(body), &response)
					response["featureId"] = m.FeatureID
					raw, _ := json.Marshal(response)
					body = string(raw)
					break
				}
			}
			if f.preflightPaid {
				body = strings.ReplaceAll(body, "firefly_complimentary_generations", "generative_credits")
			}
		case r.URL.Path == "/v2/storage/image":
			f.uploads++
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
			if err != nil {
				t.Errorf("invalid uploaded image: %v", err)
			}
			if f.video && (cfg.Width != 1920 || cfg.Height != 1080) {
				t.Errorf("video frame not fit: %dx%d", cfg.Width, cfg.Height)
			}
			if r.Header.Get("Content-Type") != "image/png" {
				t.Errorf("upload MIME %s", r.Header.Get("Content-Type"))
			}
			if f.uploadDenied {
				status, body = 403, `{"code":"access_error"}`
			} else {
				body = `{"images":[{"id":"fixture-reference-1"}]}`
			}
		case strings.Contains(r.URL.Path, "generate-async"):
			f.submits++
			if err := json.NewDecoder(r.Body).Decode(&f.payload); err != nil {
				t.Error(err)
			}
			body = `{"links":{"result":{"href":"https://bks-epo8552.adobe.io/v2/jobs/result/fixture"}}}`
			if f.afterSubmit != nil {
				f.afterSubmit()
			}
		case strings.Contains(r.URL.Path, "/v2/jobs/result/"):
			f.polls++
			if f.video && len(f.output) > 0 {
				body = `{"status":"COMPLETED","outputs":[{"video":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/fixture.mp4"}}]}`
			} else if f.video {
				body = `{"status":"FAILED"}`
			} else {
				body = `{"status":"COMPLETED","outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/fixture.png"}}]}`
			}
		case r.URL.Hostname() == "pre-signed-firefly-prod.s3-accelerate.amazonaws.com":
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("credentials sent to result storage")
			}
			body, mime = string(f.output), "image/png"
			if f.video {
				mime = "video/mp4"
			}
		default:
			t.Errorf("unexpected Adobe request path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{mime}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
}

func TestAdobeImageSizeAndReferencesReachProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, path  string
		edit, async bool
	}{
		{"generation", "/v1/images/generations", false, false},
		{"edit", "/v1/images/edits", true, false},
		{"async-edit", "/api/image-tasks/edits", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := imageTaskTestServer(t)
			adobeFixtureAccount(t, s)
			fixture := &adobeMediaCapture{output: adobeMediaPNG(t, 1200, 896), expectedProxy: "http://daily-default.invalid:8080"}
			s.proxyManager.SetDefault(fixture.expectedProxy, nil)
			fixture.afterSubmit = func() { s.proxyManager.SetDefault("http://next-default.invalid:8080", nil) }
			s.adobeClient = fixture.client(t)
			request := map[string]any{"model": "gemini-3.1-flash-image", "prompt": "a paper boat", "size": "1200x896", "n": 1, "response_format": "url"}
			if tc.edit {
				request["images"] = []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(adobeMediaPNG(t, 20, 20))}
			}
			if tc.async {
				request["client_task_id"] = "adobe-ref-size"
			}
			raw, _ := json.Marshal(request)
			w := imageTaskCall(s, "POST", tc.path, "api-secret", string(raw))
			var task *imageTaskState
			if tc.async {
				if w.Code != 202 {
					t.Fatalf("accept %d %s", w.Code, w.Body.String())
				}
				task = waitAdobeImage(t, s, "adobe-ref-size")
				if task.Status != "success" {
					t.Fatal(task.Error)
				}
			} else {
				if w.Code != 200 {
					t.Fatalf("sync %d %s", w.Code, w.Body.String())
				}
				for _, value := range s.imageTasks {
					task = value
				}
			}
			if task == nil || task.Size != "1200x896" || task.Route == nil {
				t.Fatal("requested dimensions not durably retained")
			}
			handle, err := s.adobeStore.LoadJob(task.Route.JobKey)
			if err != nil || handle.Width != 1200 || handle.Height != 896 {
				t.Fatalf("saved dimensions: %+v %v", handle, err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			size := mapValue(fixture.payload["size"])
			if size["width"] != float64(1200) || size["height"] != float64(896) {
				t.Fatalf("wrong submitted size: %v", size)
			}
			settings := mapValue(fixture.payload["modelSpecificPayload"])
			if settings["aspectRatio"] != "4:3" {
				t.Fatalf("rounded pixel dimensions lost named aspect: %v", settings)
			}
			refs, _ := fixture.payload["referenceBlobs"].([]any)
			wantRefs := 0
			if tc.edit {
				wantRefs = 1
			}
			if fixture.submits != 1 || fixture.uploads != wantRefs || len(refs) != wantRefs || handle.ReferenceCount != wantRefs {
				t.Fatalf("uploads=%d submits=%d refs=%d handleRefs=%d", fixture.uploads, fixture.submits, len(refs), handle.ReferenceCount)
			}
		})
	}
}

func TestAdobeReferenceFailureNeverGeneratesOrReplays(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	fixture := &adobeMediaCapture{uploadDenied: true}
	s.adobeClient = fixture.client(t)
	request := map[string]any{"client_task_id": "adobe-upload-failure", "model": "gemini-3.1-flash-image", "prompt": "a paper boat", "images": []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(adobeMediaPNG(t, 20, 20))}}
	raw, _ := json.Marshal(request)
	w := imageTaskCall(s, "POST", "/api/image-tasks/edits", "api-secret", string(raw))
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	task := waitAdobeImage(t, s, "adobe-upload-failure")
	if task.Status != "error" {
		t.Fatal("rejected upload succeeded")
	}
	again := imageTaskCall(s, "POST", "/api/image-tasks/edits", "api-secret", string(raw))
	if again.Code != 202 {
		t.Fatal(again.Body.String())
	}
	fixture.mu.Lock()
	uploads, submits := fixture.uploads, fixture.submits
	fixture.mu.Unlock()
	if uploads != 1 || submits != 0 {
		t.Fatalf("uploads=%d generation=%d", uploads, submits)
	}
	latest, err := s.adobeStore.Get(a.ID)
	if err != nil || latest.InFlight != "" {
		t.Fatal("definite upload failure kept account reserved")
	}
}

func adobeVideoMediaRequest(t *testing.T, audio bool, refs [][]byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{"model": "veo-3.1-lite", "prompt": "a paper boat on the sea", "seconds": "8", "size": "1920x1080", "resolution_name": "1080p", "generate_audio": fmt.Sprint(audio), "client_task_id": "adobe-audio-refs"} {
		_ = writer.WriteField(key, value)
	}
	for i, raw := range refs {
		part, err := writer.CreateFormFile("input_reference", fmt.Sprintf("frame-%d.png", i))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(raw)
	}
	_ = writer.Close()
	r := httptest.NewRequest("POST", "/v1/videos", &body)
	r.Header.Set("Authorization", "Bearer api-secret")
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}

func TestAdobeVideoAudioReferencesAndIdempotency(t *testing.T) {
	s := imageTaskTestServer(t)
	s.cfg.VideoDataDir = filepath.Join(s.cfg.DataDir, "videos")
	adobeFixtureAccount(t, s)
	fixture := &adobeMediaCapture{video: true, output: videoTestMP4("adobe-video", "fixture"), expectedProxy: "http://daily-default.invalid:8080"}
	s.proxyManager.SetDefault(fixture.expectedProxy, nil)
	fixture.afterSubmit = func() { s.proxyManager.SetDefault("http://next-default.invalid:8080", nil) }
	s.adobeClient = fixture.client(t)
	refs := [][]byte{adobeMediaPNG(t, 40, 30)}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adobeVideoMediaRequest(t, true, refs))
	if w.Code != 200 {
		t.Fatalf("accept %d %s", w.Code, w.Body.String())
	}
	var created videoJob
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	got := waitAdobeVideoIdle(t, s, created.ID)
	if !got.GenerateAudio || got.ReferenceCount != 1 || got.Status != "completed" {
		t.Fatalf("audio/reference settings lost or proxied video flow did not complete: %s %v", got.Status, got.Error)
	}
	fresh := NewWithWorkers(s.cfg, false)
	saved := fresh.videoJobs[created.ID]
	if saved == nil || !saved.GenerateAudio || saved.ReferenceCount != 1 {
		t.Fatal("audio/reference settings not persisted")
	}
	for _, server := range []*Server{s, fresh} {
		duplicate := httptest.NewRecorder()
		server.Handler().ServeHTTP(duplicate, adobeVideoMediaRequest(t, true, refs))
		if duplicate.Code != 200 {
			t.Fatalf("duplicate %d %s", duplicate.Code, duplicate.Body.String())
		}
		conflict := httptest.NewRecorder()
		server.Handler().ServeHTTP(conflict, adobeVideoMediaRequest(t, false, refs))
		if conflict.Code != 409 {
			t.Fatalf("audio conflict %d %s", conflict.Code, conflict.Body.String())
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.uploads != 1 || fixture.submits != 1 || fixture.payload["generateAudio"] != true {
		t.Fatalf("wrong video submission: uploads=%d submits=%d audio=%v", fixture.uploads, fixture.submits, fixture.payload["generateAudio"])
	}
	blobs, _ := fixture.payload["referenceBlobs"].([]any)
	if len(blobs) != 1 || mapValue(blobs[0])["usage"] != "frame" || mapValue(blobs[0])["order"] != float64(1) {
		t.Fatalf("frame binding %v", blobs)
	}
}

func TestVideoAudioFingerprintRetainsLegacySilentIdentity(t *testing.T) {
	refs := []provider.OpenAIImageInput{{Data: []byte("reference")}}
	legacy := videoRequestFingerprint("veo-3.1-lite", "boat", "16:9", "1080p", 8, "", refs)
	silent := videoRequestFingerprint("veo-3.1-lite", "boat", "16:9", "1080p", 8, "", refs, false)
	audio := videoRequestFingerprint("veo-3.1-lite", "boat", "16:9", "1080p", 8, "", refs, true)
	if legacy != silent || audio == silent {
		t.Fatal("audio identity broke legacy compatibility or aliases silent request")
	}
}

func TestAdobeInvalidReferencesFailBeforeQueue(t *testing.T) {
	for _, kind := range []string{"image", "video"} {
		t.Run(kind, func(t *testing.T) {
			s := imageTaskTestServer(t)
			adobeFixtureAccount(t, s)
			fixture := &adobeMediaCapture{}
			s.adobeClient = fixture.client(t)
			var w *httptest.ResponseRecorder
			if kind == "image" {
				body := `{"client_task_id":"bad-ref","model":"gemini-3.1-flash-image","prompt":"boat","images":["data:image/png;base64,bm90IGFuIGltYWdl"]}`
				w = imageTaskCall(s, "POST", "/api/image-tasks/edits", "api-secret", body)
			} else {
				w = httptest.NewRecorder()
				s.Handler().ServeHTTP(w, adobeVideoMediaRequest(t, true, [][]byte{[]byte("not an image")}))
			}
			if w.Code != 400 {
				t.Fatalf("invalid input accepted: %d %s", w.Code, w.Body.String())
			}
			if len(s.imageTasks) != 0 || len(s.videoJobs) != 0 || fixture.uploads != 0 || fixture.submits != 0 {
				t.Fatal("invalid image queued or contacted upstream")
			}
		})
	}
}

func adobeFreePreflightFixture(video bool) string {
	feature, pool, quota := "firefly_3p:external:nano_banana_3", "pool-images", 3
	if video {
		feature, pool, quota = "firefly_3p:external:veo_3_1_lite", "pool-video", 1
	}
	return fmt.Sprintf(`{"featureId":%q,"authorizationResult":{"result":"authorized","reasonCode":"quota_available"},"featureDetails":{},"selectedResource":{"resourceType":"firefly_complimentary_generations","computedCost":1,"effectiveQuotaReferenceId":%q},"effectiveQuotas":[{"effectiveQuotaReferenceId":%q,"resourceType":"firefly_complimentary_generations","provisionedQuota":%d,"consumedQuota":0,"refreshPeriod":"P1D"}]}`, feature, pool, pool, quota)
}

func TestAdobePaidPreflightStopsBeforeUploadOrGeneration(t *testing.T) {
	for _, video := range []bool{false, true} {
		t.Run(fmt.Sprint(video), func(t *testing.T) {
			s := imageTaskTestServer(t)
			a := adobeFixtureAccount(t, s)
			fixture := &adobeMediaCapture{video: video, preflightPaid: true}
			s.adobeClient = fixture.client(t)
			if video {
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, adobeVideoMediaRequest(t, true, [][]byte{adobeMediaPNG(t, 20, 20)}))
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				var created videoJob
				_ = json.Unmarshal(w.Body.Bytes(), &created)
				if got := waitAdobeVideoIdle(t, s, created.ID); got.Status != "failed" {
					t.Fatal("paid video allowed")
				}
			} else {
				payload := map[string]any{"model": "gemini-3.1-flash-image", "prompt": "boat", "images": []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(adobeMediaPNG(t, 20, 20))}}
				raw, _ := json.Marshal(payload)
				w := imageTaskCall(s, "POST", "/v1/images/edits", "api-secret", string(raw))
				if w.Code < 400 {
					t.Fatalf("paid image allowed: %d %s", w.Code, w.Body.String())
				}
			}
			fixture.mu.Lock()
			preflights, uploads, submits := fixture.preflights, fixture.uploads, fixture.submits
			fixture.mu.Unlock()
			if preflights != 1 || uploads != 0 || submits != 0 {
				t.Fatalf("preflight=%d upload=%d submit=%d", preflights, uploads, submits)
			}
			got, err := s.adobeStore.Get(a.ID)
			if err != nil || got.InFlight != "" {
				t.Fatal("read-only preflight rejection retained reservation")
			}
		})
	}
}

func TestAdobeRecoveryUsesRecordedImageDimensions(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	key := "image-rounded-aspect-recovery"
	if _, err := s.adobeStore.Reserve(key); err != nil {
		t.Fatal(err)
	}
	handle := adobe.Job{Kind: "image", Model: "gemini-3.1-flash-image", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "SUBMITTED", Width: 1200, Height: 896, AspectRatio: "4:3", Resolution: "1K", ReferenceCount: 1}
	if err := s.adobeStore.SaveJob(key, handle); err != nil {
		t.Fatal(err)
	}
	task := &imageTaskState{ID: "rounded-aspect-recovery", OwnerID: "owner", Mode: "edit", Model: handle.Model, Size: "1200x896", N: 1, Status: "running", Route: &mediaRoute{Channel: "adobe", AccountID: a.ID, JobKey: key, Stage: "polling"}}
	if err := s.saveImageTaskLocked(task); err != nil {
		t.Fatal(err)
	}
	fresh := NewWithWorkers(s.cfg, false)
	fixture := &adobeMediaCapture{output: adobeMediaPNG(t, 1200, 896)}
	fresh.adobeClient = fixture.client(t)
	fresh.recoverAdobeImageTasks()
	got := waitAdobeImage(t, fresh, task.ID)
	if got.Status != "success" {
		t.Fatalf("recorded-size recovery: %s", got.Error)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.preflights != 0 || fixture.uploads != 0 || fixture.submits != 0 || fixture.polls != 1 {
		t.Fatalf("recovery repeated work: %+v", fixture)
	}
}

func TestAdobeAsyncFourteenReferencesFollowModelCapability(t *testing.T) {
	s := imageTaskTestServer(t)
	adobeFixtureAccount(t, s)
	fixture := &adobeMediaCapture{output: adobeMediaPNG(t, 1024, 1024)}
	s.adobeClient = fixture.client(t)
	refs := make([]string, 14)
	for i := range refs {
		refs[i] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(adobeMediaPNG(t, 10+i, 10))
	}
	body := map[string]any{"client_task_id": "nano21-fourteen", "model": "gemini-3.1-flash-image-2.1", "prompt": "arrange these shapes", "images": refs}
	raw, _ := json.Marshal(body)
	w := imageTaskCall(s, "POST", "/api/image-tasks/edits", "api-secret", string(raw))
	if w.Code != 202 {
		t.Fatalf("14 refs rejected: %d %s", w.Code, w.Body.String())
	}
	got := waitAdobeImage(t, s, "nano21-fourteen")
	if got.Status != "success" {
		t.Fatal(got.Error)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.uploads != 14 || fixture.submits != 1 {
		t.Fatalf("uploads=%d submits=%d", fixture.uploads, fixture.submits)
	}
}

func TestAdobeRequiredAudioDefaultAndExplicitRejection(t *testing.T) {
	for _, id := range []string{"gemini-omni-flash-1.1", "gemini-omni-flash-1.2"} {
		r := httptest.NewRequest("POST", "/v1/videos", nil)
		r.Form = map[string][]string{"model": {id}}
		got, err := parseVideoOptions(r)
		if err != nil || !got.GenerateAudio {
			t.Fatalf("%s default audio: %+v %v", id, got, err)
		}
		r.Form.Set("generate_audio", "false")
		if _, err := parseVideoOptions(r); err == nil {
			t.Fatalf("%s silently accepted disabling required audio", id)
		}
	}
}

func TestAdobeOutputKeepsActualEncoding(t *testing.T) {
	for _, kind := range []string{"png", "jpeg", "webp"} {
		t.Run(kind, func(t *testing.T) {
			s := imageTaskTestServer(t)
			adobeFixtureAccount(t, s)
			var output bytes.Buffer
			canvas := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
			var err error
			switch kind {
			case "png":
				err = png.Encode(&output, canvas)
			case "jpeg":
				err = jpeg.Encode(&output, canvas, nil)
			case "webp":
				err = nativewebp.Encode(&output, canvas, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			fixture := &adobeMediaCapture{output: output.Bytes()}
			s.adobeClient = fixture.client(t)
			w := imageTaskCall(s, "POST", "/v1/images/generations", "api-secret", `{"model":"gemini-3.1-flash-image","prompt":"a paper boat","response_format":"b64_json"}`)
			if w.Code != 200 {
				t.Fatalf("format %s: %d %s", kind, w.Code, w.Body.String())
			}
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			rows, _ := response["data"].([]any)
			if len(rows) != 1 {
				t.Fatal("missing result")
			}
			item := mapValue(rows[0])
			wantMIME := "image/" + kind
			if response["output_format"] != kind || item["mime"] != wantMIME || item["output_format"] != kind {
				t.Fatalf("wrong actual metadata %v %v", response["output_format"], item["mime"])
			}
			decoded, err := base64.StdEncoding.DecodeString(stringValue(item["b64_json"]))
			if err != nil || !bytes.Equal(decoded, output.Bytes()) {
				t.Fatal("image was reencoded or base64 corrupted")
			}
			entries, err := os.ReadDir(s.cfg.ImageDataDir)
			if err != nil || len(entries) != 2 || entries[1].Name() != entries[0].Name()+".meta.json" {
				t.Fatalf("stored image/metadata files: %v %v", entries, err)
			}
			extension := kind
			if kind == "jpeg" {
				extension = "jpg"
			}
			if !strings.HasSuffix(entries[0].Name(), "."+extension) {
				t.Fatalf("wrong file encoding %s", entries[0].Name())
			}
			saved, err := os.ReadFile(filepath.Join(s.cfg.ImageDataDir, entries[0].Name()))
			if err != nil || !bytes.Equal(saved, output.Bytes()) {
				t.Fatal("stored bytes changed")
			}
		})
	}
}

func TestAdobeRejectsInvalidOrWrongSizeOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output []byte
	}{
		{"wrong-size", adobeMediaPNG(t, 1200, 896)},
		{"html", []byte("<html>not an image</html>")},
		{"truncated", adobeMediaPNG(t, 1024, 1024)[:50]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := imageTaskTestServer(t)
			adobeFixtureAccount(t, s)
			fixture := &adobeMediaCapture{output: tc.output}
			s.adobeClient = fixture.client(t)
			w := imageTaskCall(s, "POST", "/v1/images/generations", "api-secret", `{"model":"gemini-3.1-flash-image","prompt":"boat","size":"1024x1024"}`)
			if w.Code < 400 {
				t.Fatal("invalid output delivered")
			}
			entries, err := os.ReadDir(s.cfg.ImageDataDir)
			if err != nil && !os.IsNotExist(err) || len(entries) != 0 {
				t.Fatalf("invalid output persisted: %v %v", entries, err)
			}
			fixture.mu.Lock()
			submits := fixture.submits
			fixture.mu.Unlock()
			if submits != 1 {
				t.Fatalf("invalid output replayed generation: %d", submits)
			}
		})
	}
}

func TestAdobeSharedModelKeepsImageEditDefault(t *testing.T) {
	s := imageTaskTestServer(t)
	body := map[string]any{"client_task_id": "default-gpt-edit", "prompt": "boat", "images": []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(adobeMediaPNG(t, 20, 20))}}
	raw, _ := json.Marshal(body)
	w := imageTaskCall(s, "POST", "/api/image-tasks/edits", "api-secret", string(raw))
	if w.Code != 202 {
		t.Fatalf("default model rejected: %d %s", w.Code, w.Body.String())
	}
	got := waitAdobeImage(t, s, "default-gpt-edit")
	if got.Model != "gpt-image-2" {
		t.Fatalf("default model changed: %s", got.Model)
	}
}

func TestAdobeExplicitOutputEncodingCannotSilentlyChange(t *testing.T) {
	s := imageTaskTestServer(t)
	adobeFixtureAccount(t, s)
	var output bytes.Buffer
	if err := jpeg.Encode(&output, image.NewRGBA(image.Rect(0, 0, 1024, 1024)), nil); err != nil {
		t.Fatal(err)
	}
	fixture := &adobeMediaCapture{output: output.Bytes()}
	s.adobeClient = fixture.client(t)
	w := imageTaskCall(s, "POST", "/v1/images/generations", "api-secret", `{"model":"gemini-3.1-flash-image","prompt":"boat","output_format":"png"}`)
	if w.Code < 400 || !strings.Contains(w.Body.String(), "does not match the requested output_format") {
		t.Fatalf("explicit encoding ignored: %d %s", w.Code, w.Body.String())
	}
	for _, task := range s.imageTasks {
		if task.OutputFormat != "png" {
			t.Fatal("explicit encoding not retained for recovery")
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.submits != 1 {
		t.Fatal("encoding mismatch replayed generation")
	}
}
