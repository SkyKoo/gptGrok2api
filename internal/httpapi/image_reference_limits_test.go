package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestGrokImageReferenceLimitsAcrossEndpoints(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, jsonRequest := range []bool{false, true} {
			for _, count := range []int{5, 6} {
				t.Run(fmt.Sprintf("async=%t/json=%t/count=%d", async, jsonRequest, count), func(t *testing.T) {
					var uploads, generations atomic.Int32
					var upstream *httptest.Server
					upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/upload":
							i := uploads.Add(1)
							fmt.Fprintf(w, `{"fileUri":"/reference%d.png"}`, i)
						case "/post":
							fmt.Fprint(w, `{"post":{"id":"edit-post"}}`)
						case "/chat":
							generations.Add(1)
							var payload map[string]any
							if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
								t.Error(err)
							}
							models := mapValue(mapValue(mapValue(payload["responseMetadata"])["modelConfigOverride"])["modelMap"])
							refs := anyList(mapValue(models["imageEditModelConfig"])["imageReferences"])
							if len(refs) != 5 {
								t.Errorf("generation received %d references, want all 5", len(refs))
							}
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprintln(w, `data: {"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"`+upstream.URL+`/generated.png","progress":100}}}}`)
							fmt.Fprintln(w, `data: [DONE]`)
						case "/generated.png":
							w.Header().Set("Content-Type", "image/png")
							_, _ = w.Write(tinyPNG)
						default:
							t.Errorf("unexpected upstream request: %s", r.URL.Path)
							http.NotFound(w, r)
						}
					}))
					defer upstream.Close()
					s := New(adminTestConfig(t.TempDir()))
					s.mediaProvider = provider.NewMedia(upstream.Client(), upstream.URL+"/chat", upstream.URL+"/post", upstream.URL+"/upload", upstream.URL, s.cfg.RequestTimeout)
					if _, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"media_quota": testGrokMediaSnapshot(), "access_token": "test", "enabled": true, "status": "active"}}); err != nil {
						t.Fatal(err)
					}
					endpoint := "/v1/images/edits"
					if async {
						endpoint = "/api/image-tasks/edits"
					}
					var request *http.Request
					if jsonRequest {
						refs := make([]string, count-1)
						ref := "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG)
						// Invalid references verify over-limit rejection precedes decoding/download.
						if count > 5 {
							ref = "invalid-reference"
						}
						for i := range refs {
							refs[i] = ref
						}
						body, _ := json.Marshal(map[string]any{"client_task_id": "reference-limit", "model": "grok-imagine-image-edit", "prompt": "paper plane", "images": refs, "image_url": ref})
						request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
						request.Header.Set("Content-Type", "application/json")
					} else {
						var body bytes.Buffer
						writer := multipart.NewWriter(&body)
						_ = writer.WriteField("client_task_id", "reference-limit")
						_ = writer.WriteField("model", "grok-imagine-image-edit")
						_ = writer.WriteField("prompt", "paper plane")
						for i := 0; i < count; i++ {
							key := "image[]"
							if i%2 == 1 {
								key = "image"
							}
							part, _ := writer.CreateFormFile(key, fmt.Sprintf("reference%d.png", i))
							if count > 5 {
								_, _ = part.Write(nil)
							} else {
								_, _ = part.Write(tinyPNG)
							}
						}
						_ = writer.Close()
						request = httptest.NewRequest(http.MethodPost, endpoint, &body)
						request.Header.Set("Content-Type", writer.FormDataContentType())
					}
					request.Header.Set("Authorization", "Bearer api-secret")
					response := httptest.NewRecorder()
					s.Handler().ServeHTTP(response, request)
					if count > 5 {
						if response.Code != 400 || !strings.Contains(response.Body.String(), "最多支持 5 张") || uploads.Load() != 0 || generations.Load() != 0 || len(s.imageTasks) != 0 {
							t.Fatalf("over-limit request had effects: status=%d uploads=%d generation=%d tasks=%d body=%s", response.Code, uploads.Load(), generations.Load(), len(s.imageTasks), response.Body.String())
						}
						return
					}
					expected := http.StatusOK
					if async {
						expected = http.StatusAccepted
					}
					if response.Code != expected {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
					if async {
						deadline := time.Now().Add(3 * time.Second)
						for {
							s.imageTaskMu.Lock()
							status, failure := "", ""
							for _, task := range s.imageTasks {
								status, failure = task.Status, task.Error
							}
							s.imageTaskMu.Unlock()
							if status == "success" {
								break
							}
							if status == "error" || time.Now().After(deadline) {
								t.Fatalf("async task status=%s error=%s", status, failure)
							}
							time.Sleep(10 * time.Millisecond)
						}
					}
					if uploads.Load() != 5 || generations.Load() != 1 {
						t.Fatalf("uploads=%d generation=%d", uploads.Load(), generations.Load())
					}
				})
			}
		}
	}
}

func TestImageReferenceLimitCountsNestedAliasesBeforeReading(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	for _, contentType := range []string{"application/json", "multipart/form-data"} {
		t.Run(contentType, func(t *testing.T) {
			var request *http.Request
			if contentType == "application/json" {
				// Default synchronous model is Grok; combine all accepted aliases.
				body := `{"prompt":"test","images":[{"image_url":{"url":"invalid"}},["invalid","invalid"]],"image":"[\"invalid\",\"invalid\"]","image_url":"invalid"}`
				request = httptest.NewRequest("POST", "/v1/images/edits", strings.NewReader(body))
				request.Header.Set("Content-Type", contentType)
			} else {
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				_ = writer.WriteField("images", `["invalid","invalid","invalid","invalid","invalid"]`)
				part, _ := writer.CreateFormFile("image", "empty.png")
				_, _ = part.Write(nil)
				_ = writer.Close()
				request = httptest.NewRequest("POST", "/v1/images/edits", &body)
				request.Header.Set("Content-Type", writer.FormDataContentType())
			}
			_, err := s.parseImageEditRequest(request)
			if request.MultipartForm != nil {
				_ = request.MultipartForm.RemoveAll()
			}
			if err == nil || !strings.Contains(err.Error(), "最多支持 5 张") {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, model := range []string{"gpt-image-2", "codex-gpt-image-2"} {
		refs := make([]string, 7)
		for i := range refs {
			refs[i] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG)
		}
		body, _ := json.Marshal(map[string]any{"model": model, "prompt": "test", "images": refs})
		request := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		parsed, err := s.parseImageEditRequest(request)
		if err != nil || len(parsed.Inputs) != 7 {
			t.Fatalf("GPT parsing changed: model=%s count=%d err=%v", model, len(parsed.Inputs), err)
		}
	}
}

func TestVideoReferenceLimitPreservesSevenAndRejectsEight(t *testing.T) {
	for _, count := range []int{7, 8} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for i := 0; i < count; i++ {
			key := "input_reference"
			if i%2 == 1 {
				key = "input_reference[]"
			}
			part, _ := writer.CreateFormFile(key, fmt.Sprintf("ref%d.png", i))
			_, _ = part.Write(tinyPNG)
		}
		_ = writer.Close()
		request := httptest.NewRequest("POST", "/v1/videos", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		inputs, err := videoReferenceInputs(request)
		_ = request.MultipartForm.RemoveAll()
		if count == 7 && (err != nil || len(inputs) != 7) {
			t.Fatalf("seven references: count=%d err=%v", len(inputs), err)
		}
		if count == 8 && (err == nil || !strings.Contains(err.Error(), "最多 7 张")) {
			t.Fatalf("eight references: %v", err)
		}
	}
}
