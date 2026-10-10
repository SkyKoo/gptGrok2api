package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func imageOutputRequest(t *testing.T, path, encoding string, fields map[string]any) *http.Request {
	t.Helper()
	var body bytes.Buffer
	contentType := "application/json"
	if encoding == "multipart" {
		writer := multipart.NewWriter(&body)
		for key, value := range fields {
			if text, ok := value.(string); ok {
				_ = writer.WriteField(key, text)
			}
		}
		file, err := writer.CreateFormFile("image[]", "reference.png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write(tinyPNG)
		_ = writer.Close()
		contentType = writer.FormDataContentType()
	} else {
		if strings.HasSuffix(path, "/edits") {
			fields["images"] = []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG)}
		}
		if err := json.NewEncoder(&body).Encode(fields); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest("POST", path, &body)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Authorization", "Bearer api-secret")
	return r
}

func TestImageOutputValidationBeforeAsyncAcceptanceOrGeneration(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits", "/api/image-tasks/generations", "/api/image-tasks/edits"} {
		for _, encoding := range []string{"json", "multipart"} {
			if encoding == "multipart" && !strings.HasSuffix(path, "/edits") {
				continue
			}
			for _, tc := range []struct{ param, value, model string }{
				{"background", "invalid", "gpt-image-2"},
				{"output_format", "jpeg", "gpt-image-2"},
				{"output_format", "jpg", "gpt-image-2"},
				{"output_format", "gif", "gpt-image-2"},
				{"background", "transparent", "grok-imagine-image-edit"},
			} {
				t.Run(path+"/"+encoding+"/"+tc.param+"/"+tc.value+"/"+tc.model, func(t *testing.T) {
					s := imageTaskTestServer(t)
					fields := map[string]any{"client_task_id": "invalid", "model": tc.model, "prompt": "test", tc.param: tc.value}
					if tc.value == "jpeg" {
						fields["background"] = "transparent"
					}
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, imageOutputRequest(t, path, encoding, fields))
					if w.Code != 400 || !strings.Contains(w.Body.String(), `"param":"`+tc.param+`"`) || !strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) {
						t.Fatalf("%d %s", w.Code, w.Body.String())
					}
					if len(s.imageTasks) != 0 {
						t.Fatal("invalid output options created a task")
					}
				})
			}
		}
	}
}

func TestImageOutputProtocolThroughWebAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, path, encoding, background, format string
		opaque, wantError                        bool
	}{
		{"jpeg-v1-images-generations-json", "/v1/images/generations", "json", "auto", "jpeg", false, false},
		{"jpeg-v1-images-edits-json", "/v1/images/edits", "json", "auto", "jpeg", false, false},
		{"jpeg-v1-images-edits-multipart", "/v1/images/edits", "multipart", "auto", "jpeg", false, false},
		{"jpeg-api-image-tasks-generations-json", "/api/image-tasks/generations", "json", "auto", "jpeg", false, false},
		{"jpeg-api-image-tasks-edits-json", "/api/image-tasks/edits", "json", "auto", "jpeg", false, false},
		{"jpeg-api-image-tasks-edits-multipart", "/api/image-tasks/edits", "multipart", "auto", "jpeg", false, false},
		{"webp-v1-images-generations-json", "/v1/images/generations", "json", "transparent", "webp", false, false},
		{"webp-v1-images-edits-json", "/v1/images/edits", "json", "transparent", "webp", false, false},
		{"webp-v1-images-edits-multipart", "/v1/images/edits", "multipart", "transparent", "webp", false, false},
		{"webp-api-image-tasks-generations-json", "/api/image-tasks/generations", "json", "transparent", "webp", false, false},
		{"webp-api-image-tasks-edits-json", "/api/image-tasks/edits", "json", "transparent", "webp", false, false},
		{"webp-api-image-tasks-edits-multipart", "/api/image-tasks/edits", "multipart", "transparent", "webp", false, false},
		{"transparent-default-format", "/v1/images/edits", "json", "transparent", "", false, false},
		{"async-default-format", "/api/image-tasks/edits", "json", "transparent", "", false, false},
		{"sync-generation", "/v1/images/generations", "json", "transparent", "png", false, false},
		{"sync-edit-json", "/v1/images/edits", "json", "transparent", "png", false, false},
		{"sync-edit-multipart", "/v1/images/edits", "multipart", "transparent", "png", false, false},
		{"async-generation", "/api/image-tasks/generations", "json", "transparent", "png", false, false},
		{"async-edit-json", "/api/image-tasks/edits", "json", "transparent", "png", false, false},
		{"async-edit-multipart", "/api/image-tasks/edits", "multipart", "transparent", "png", false, false},
		{"opaque-success", "/v1/images/edits", "json", "opaque", "png", true, false},
		{"auto-alpha", "/v1/images/edits", "json", "auto", "png", false, false},
		{"sync-mismatch", "/v1/images/edits", "json", "transparent", "png", true, true},
		{"async-mismatch", "/api/image-tasks/edits", "json", "transparent", "png", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := imageTaskTestServer(t)
			s.cfg.ChatMaxRetries = 2
			s.cfg.ChatRetryCodes = map[int]bool{502: true}
			_, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"access_token": "jwt.header.payload", "pool": "basic"}, {"access_token": "jwt.second.payload", "pool": "basic"}})
			if err != nil {
				t.Fatal(err)
			}
			canvas := image.NewNRGBA(image.Rect(0, 0, 4, 4))
			if tc.opaque {
				for y := 0; y < 4; y++ {
					for x := 0; x < 4; x++ {
						canvas.SetNRGBA(x, y, color.NRGBA{R: 100, A: 255})
					}
				}
			}
			canvas.SetNRGBA(1, 1, color.NRGBA{R: 200, A: 255})
			var output bytes.Buffer
			if err := png.Encode(&output, canvas); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var upstream *httptest.Server
			const fileID = "file_000000001234567890abcdef12345678"
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					io.WriteString(w, `<html data-build="test"></html>`)
				case "/backend-api/files":
					writeJSON(w, 200, map[string]any{"file_id": "file_reference", "upload_url": upstream.URL + "/upload"})
				case "/upload":
					w.WriteHeader(201)
				case "/backend-api/files/file_reference/uploaded":
					writeJSON(w, 200, map[string]any{"status": "success"})
				case "/backend-api/sentinel/chat-requirements/prepare":
					writeJSON(w, 200, map[string]any{"prepare_token": "test"})
				case "/backend-api/sentinel/chat-requirements/finalize":
					writeJSON(w, 200, map[string]any{"token": "test"})
				case "/backend-api/f/conversation/prepare":
					writeJSON(w, 200, map[string]any{"conduit_token": "test"})
				case "/backend-api/f/conversation":
					calls.Add(1)
					raw, _ := io.ReadAll(r.Body)
					if !bytes.Contains(raw, []byte("a red cat")) || !bytes.Contains(raw, []byte("Output file format: PNG")) {
						t.Error("missing original prompt or PNG requirement")
					}
					if tc.background == "transparent" && !bytes.Contains(raw, []byte("alpha=0")) {
						t.Error("transparent requirement lost before Web call")
					}
					if tc.background == "opaque" && !bytes.Contains(raw, []byte("fully opaque")) {
						t.Error("opaque requirement lost before Web call")
					}
					var payload map[string]any
					_ = json.Unmarshal(raw, &payload)
					for _, field := range []string{"background", "transparent_background", "output_format"} {
						if _, ok := payload[field]; ok {
							t.Error("unverified top-level Web field forwarded")
						}
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, `data: {"conversation_id":"conversation-1","message":{"content":{"parts":["file-service://`+fileID+`"]}}}`+"\n\ndata: [DONE]\n\n")
				case "/backend-api/conversation/conversation-1":
					writeHTTPAPIGeneratedImageConversation(w, fileID)
				case "/backend-api/files/" + fileID + "/download":
					writeJSON(w, 200, map[string]any{"download_url": upstream.URL + "/blob"})
				case "/blob":
					w.Header().Set("Content-Type", "image/png")
					w.Write(output.Bytes())
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			s.openAIImage = provider.NewOpenAIImage(upstream.URL, upstream.Client(), nil, 30*time.Second)
			fields := map[string]any{"client_task_id": "output-test", "model": "gpt-image-2", "prompt": "a red cat", "background": tc.background, "output_format": tc.format}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, imageOutputRequest(t, tc.path, tc.encoding, fields))
			async := strings.HasPrefix(tc.path, "/api/")
			if async {
				if w.Code != 202 {
					t.Fatalf("submit: %d %s", w.Code, w.Body.String())
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					w = imageTaskCall(s, "GET", "/api/image-tasks/output-test", "api-secret", "")
					var task imageTaskState
					_ = json.Unmarshal(w.Body.Bytes(), &task)
					if task.Status == "success" || task.Status == "error" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("task timeout")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("generation retried: %d", calls.Load())
			}
			if tc.wantError {
				if !strings.Contains(w.Body.String(), "did not satisfy background=transparent") {
					t.Fatal(w.Body.String())
				}
				if !async && w.Code != 502 {
					t.Fatalf("wrong output error status: %d", w.Code)
				}
				if async && !strings.Contains(w.Body.String(), `"status":"error"`) {
					t.Fatal(w.Body.String())
				}
				files, _ := os.ReadDir(s.cfg.ImageDataDir)
				if len(files) != 0 {
					t.Fatal("non-transparent result entered gallery")
				}
				return
			}
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			var result struct {
				Data         []map[string]string `json:"data"`
				Background   string              `json:"background"`
				OutputFormat string              `json:"output_format"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Data) != 1 {
				t.Fatal(w.Body.String())
			}
			expectedFormat := tc.format
			if expectedFormat == "" {
				expectedFormat = "png"
			}
			if result.OutputFormat != expectedFormat || (tc.background != "auto" && result.Background != tc.background) {
				t.Fatal("missing output metadata")
			}
			var raw []byte
			if async {
				preview := adminRequest(s.Handler(), "GET", result.Data[0]["url"], nil)
				if preview.Code != 200 || preview.Header().Get("Content-Type") != "image/"+expectedFormat {
					t.Fatal("missing saved image")
				}
				raw = preview.Body.Bytes()
				// The options survive restart and an identical request does not regenerate.
				reloaded := New(s.cfg)
				replay := httptest.NewRecorder()
				reloaded.Handler().ServeHTTP(replay, imageOutputRequest(t, tc.path, tc.encoding, fields))
				if replay.Code != 202 || !strings.Contains(replay.Body.String(), `"status":"success"`) {
					t.Fatal(replay.Body.String())
				}
				fields["output_format"] = "png"
				if expectedFormat == "png" {
					fields["output_format"] = "webp"
				}
				conflict := httptest.NewRecorder()
				reloaded.Handler().ServeHTTP(conflict, imageOutputRequest(t, tc.path, tc.encoding, fields))
				if conflict.Code != 409 {
					t.Fatalf("different output options reused old task: %d", conflict.Code)
				}
			} else {
				raw, _ = base64.StdEncoding.DecodeString(result.Data[0]["b64_json"])
			}
			decoded, actualFormat, err := image.Decode(bytes.NewReader(raw))
			if err != nil || actualFormat != expectedFormat || decoded.Bounds() != canvas.Bounds() {
				t.Fatalf("incorrect result format or dimensions: %s %v", actualFormat, err)
			}
			if expectedFormat == "png" && !bytes.Equal(raw, output.Bytes()) {
				t.Fatal("PNG bytes changed")
			}
			for y := 0; y < 4; y++ {
				for x := 0; x < 4; x++ {
					_, _, _, wantAlpha := canvas.At(x, y).RGBA()
					if expectedFormat == "jpeg" {
						wantAlpha = 65535
					}
					_, _, _, alpha := decoded.At(x, y).RGBA()
					if alpha != wantAlpha {
						t.Fatal("alpha changed during persistence/response")
					}
				}
			}
			extension := "." + expectedFormat
			if expectedFormat == "jpeg" {
				extension = ".jpg"
			}
			files, err := os.ReadDir(s.cfg.ImageDataDir)
			if err != nil {
				t.Fatal(err)
			}
			var images []string
			for _, file := range files {
				if !strings.HasSuffix(file.Name(), ".meta.json") {
					images = append(images, file.Name())
				}
			}
			if len(images) != 1 || filepath.Ext(images[0]) != extension {
				t.Fatalf("incorrect saved file type: %v %v", files, err)
			}
		})
	}
}

func TestImageOutputTaskHashPreservesLegacyRequests(t *testing.T) {
	task := &imageTaskState{Mode: "edit", Model: "gpt-image-2", N: 1, Size: "auto", Quality: "high", Prompt: "test", Images: [][]byte{[]byte("reference")}}
	raw, _ := json.Marshal([]any{task.Mode, task.Model, task.N, task.Size, task.Quality, task.Prompt, task.Images})
	digest := sha256.Sum256(raw)
	legacy := hex.EncodeToString(digest[:])
	if imageTaskHash(task) != legacy {
		t.Fatal("old task idempotency changed")
	}
	task.Background = "transparent"
	withBackground := imageTaskHash(task)
	if withBackground == legacy {
		t.Fatal("background absent from task hash")
	}
	task.OutputFormat = "png"
	if imageTaskHash(task) == withBackground {
		t.Fatal("format absent from task hash")
	}
}
