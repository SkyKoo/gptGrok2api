package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
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

func gifProtocolFixture(t *testing.T) []byte {
	t.Helper()
	a := &gif.GIF{LoopCount: 0}
	for i := 0; i < 3; i++ {
		p := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
		p.SetColorIndex(i%2, 0, 1)
		a.Image = append(a.Image, p)
		a.Delay = append(a.Delay, 12)
	}
	var b bytes.Buffer
	if e := gif.EncodeAll(&b, a); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestImage26GIFSyncAsyncAndRestart(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, wrongOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("async=%v/wrong=%v", async, wrongOutput), func(t *testing.T) {
				s := imageTaskTestServer(t)
				s.cfg.ChatMaxRetries = 2
				s.cfg.ChatRetryCodes = map[int]bool{502: true}
				_, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"access_token": "jwt.header.payload", "email": "animation@example.test", "pool": "basic"}})
				if err != nil {
					t.Fatal(err)
				}
				_, _ = s.store.UpdateConfig("openai_routing", map[string]any{"image_conversation_model": "gpt-6"})
				var calls, downloads atomic.Int32
				raw := gifProtocolFixture(t)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/":
						io.WriteString(w, `<html data-build="test"></html>`)
					case "/backend-api/sentinel/chat-requirements/prepare":
						writeJSON(w, 200, map[string]any{"prepare_token": "test"})
					case "/backend-api/sentinel/chat-requirements/finalize":
						writeJSON(w, 200, map[string]any{"token": "test"})
					case "/backend-api/f/conversation/prepare":
						writeJSON(w, 200, map[string]any{"status": "ok", "conduit_token": nil})
					case "/backend-api/f/conversation":
						calls.Add(1)
						var body map[string]any
						_ = json.NewDecoder(r.Body).Decode(&body)
						b, _ := json.Marshal(body)
						if body["model"] != "gpt-image-2.6-sunburst" || !bytes.Contains(b, []byte("3 frames")) || !bytes.Contains(b, []byte("120 milliseconds")) {
							t.Error("model or animation options lost", string(b))
						}
						for _, key := range []string{"output_format", "frame_count", "frame_duration_ms"} {
							if _, ok := body[key]; ok {
								t.Error("unverified top-level Web parameter", key)
							}
						}
						if _, ok := r.Header["X-Conduit-Token"]; ok {
							t.Error("empty conduit header sent")
						}
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"conversation_id\":\"conversation-1\"}\n\ndata: [DONE]\n\n")
					case "/backend-api/conversation/conversation-1":
						fixture, e := os.ReadFile("../provider/testdata/image26-animation.json")
						if e != nil {
							t.Error(e)
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(fixture)
					case "/backend-api/conversation/conversation-1/attachment/animation-primary/download":
						downloads.Add(1)
						if wrongOutput {
							w.Header().Set("Content-Type", "image/png")
							_, _ = w.Write(tinyPNG)
						} else {
							w.Header().Set("Content-Type", "image/gif")
							_, _ = w.Write(raw)
						}
					default:
						t.Error("unexpected endpoint", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer upstream.Close()
				s.openAIImage = provider.NewOpenAIImage(upstream.URL, upstream.Client(), nil, 30*time.Second)
				path := "/v1/images/generations"
				if async {
					path = "/api/image-tasks/generations"
				}
				body := `{"client_task_id":"gif-test","model":"gpt-image-2.6-sunburst","prompt":"blue ball","n":1,"output_format":"gif","frame_count":3,"frame_duration_ms":120}`
				w := imageTaskCall(s, "POST", path, "api-secret", body)
				if async {
					if w.Code != 202 {
						t.Fatalf("submit %d %s", w.Code, w.Body.String())
					}
					deadline := time.Now().Add(5 * time.Second)
					for {
						w = imageTaskCall(s, "GET", "/api/image-tasks/gif-test", "api-secret", "")
						var task imageTaskState
						_ = json.Unmarshal(w.Body.Bytes(), &task)
						if task.Status == "success" || task.Status == "error" {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("async timeout")
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				if calls.Load() != 1 || downloads.Load() != 1 {
					t.Fatalf("generation duplicated or spritesheet downloaded: %d/%d", calls.Load(), downloads.Load())
				}
				if wrongOutput {
					if !strings.Contains(w.Body.String(), "static previews") {
						t.Fatal("wrong-output contract lost", w.Body.String())
					}
					files, _ := os.ReadDir(s.cfg.ImageDataDir)
					if len(files) != 0 {
						t.Fatal("invalid GIF stored")
					}
					return
				}
				if w.Code != 200 {
					t.Fatalf("result %d %s", w.Code, w.Body.String())
				}
				var result struct {
					Data          []map[string]string `json:"data"`
					OutputFormat  string              `json:"output_format"`
					FrameCount    int                 `json:"frame_count"`
					FrameDuration int                 `json:"frame_duration_ms"`
				}
				if json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Data) != 1 || result.OutputFormat != "gif" {
					t.Fatal(w.Body.String())
				}
				var saved []byte
				if async {
					if result.FrameCount != 3 || result.FrameDuration != 120 {
						t.Fatal("task parameters missing")
					}
					preview := adminRequest(s.Handler(), "GET", result.Data[0]["url"], nil)
					if preview.Code != 200 || preview.Header().Get("Content-Type") != "image/gif" {
						t.Fatal("GIF MIME lost")
					}
					saved = preview.Body.Bytes()
					reloaded := New(s.cfg)
					replay := imageTaskCall(reloaded, "POST", path, "api-secret", body)
					if replay.Code != 200 && replay.Code != 202 {
						t.Fatal("restart replay failed", replay.Body.String())
					}
					conflict := imageTaskCall(reloaded, "POST", path, "api-secret", strings.Replace(body, `"frame_count":3`, `"frame_count":4`, 1))
					if conflict.Code != 409 {
						t.Fatal("different frame count reused an old animation", conflict.Code)
					}
					conflict = imageTaskCall(reloaded, "POST", path, "api-secret", strings.Replace(body, `"frame_duration_ms":120`, `"frame_duration_ms":200`, 1))
					if conflict.Code != 409 {
						t.Fatal("different frame timing reused an old animation", conflict.Code)
					}
					if calls.Load() != 1 {
						t.Fatal("replay regenerated")
					}
				} else {
					saved, err = base64.StdEncoding.DecodeString(result.Data[0]["b64_json"])
					if err != nil {
						t.Fatal(err)
					}
				}
				if !bytes.Equal(saved, raw) {
					t.Fatal("animation bytes or frames changed")
				}
				files, _ := filepath.Glob(filepath.Join(s.cfg.ImageDataDir, "*.gif"))
				if len(files) != 1 {
					t.Fatal("wrong persisted animation count", files)
				}
			})
		}
	}
}
func TestGIFInvalidRequestsNeverEnqueue(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/api/image-tasks/generations", "/v1/images/edits", "/api/image-tasks/edits"} {
		for _, extra := range []string{`,"frame_count":0`, `,"frame_count":17`, `,"frame_duration_ms":10`, `,"stream":true`, `,"partial_images":0`, `,"output_compression":80`} {
			s := imageTaskTestServer(t)
			body := `{"client_task_id":"invalid","model":"gpt-image-2.6-sunburst","prompt":"ball","output_format":"gif"` + extra + `}`
			w := imageTaskCall(s, "POST", path, "api-secret", body)
			if w.Code != 400 || len(s.imageTasks) != 0 {
				t.Fatalf("accepted %s %s: %d", path, extra, w.Code)
			}
		}
	}
	for _, path := range []string{"/v1/images/edits", "/api/image-tasks/edits"} {
		for _, encoding := range []string{"json", "multipart"} {
			s := imageTaskTestServer(t)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, imageOutputRequest(t, path, encoding, map[string]any{"client_task_id": "edit", "model": "gpt-image-2.6-sunburst", "prompt": "ball", "output_format": "gif"}))
			if w.Code != 400 || !strings.Contains(w.Body.String(), "generation only") || len(s.imageTasks) != 0 {
				t.Fatalf("GIF edit accepted %s %s: %s", path, encoding, w.Body.String())
			}
		}
	}
}
