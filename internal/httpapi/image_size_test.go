package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
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

func sizeTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sizeTestRequest(t *testing.T, path, encoding string, fields map[string]any, refs [][]byte) *http.Request {
	t.Helper()
	var b bytes.Buffer
	contentType := "application/json"
	if encoding == "multipart" {
		writer := multipart.NewWriter(&b)
		for key, value := range fields {
			if err := writer.WriteField(key, fmt.Sprint(value)); err != nil {
				t.Fatal(err)
			}
		}
		for i, raw := range refs {
			file, err := writer.CreateFormFile("image[]", fmt.Sprintf("reference-%d.png", i))
			if err != nil {
				t.Fatal(err)
			}
			file.Write(raw)
		}
		writer.Close()
		contentType = writer.FormDataContentType()
	} else {
		if len(refs) > 0 {
			images := []string{}
			for _, raw := range refs {
				images = append(images, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(raw))
			}
			fields["images"] = images
		}
		if err := json.NewEncoder(&b).Encode(fields); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest("POST", path, &b)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Authorization", "Bearer api-secret")
	return r
}

func TestImageSizeProtocolAndLogs(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits", "/api/image-tasks/generations", "/api/image-tasks/edits"} {
		for _, encoding := range []string{"json", "multipart"} {
			if encoding == "multipart" && !strings.HasSuffix(path, "/edits") {
				continue
			}
			for _, size := range []string{"<omitted>", "auto", " AuTo ", "1024x1024", "1024x1365", "1536x1024"} {
				t.Run(path+"/"+encoding+"/"+size, func(t *testing.T) {
					s := imageTaskTestServer(t)
					if _, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"access_token": "jwt.header.payload", "pool": "basic"}}); err != nil {
						t.Fatal(err)
					}
					requested := size
					if size == "<omitted>" {
						requested = ""
					}
					requested = openAIImageRequestedSize(requested)
					normalized := provider.NormalizeOpenAIImageSize(requested)
					refs := [][]byte{}
					if strings.HasSuffix(path, "/edits") {
						refs = [][]byte{sizeTestPNG(t, 13, 19), sizeTestPNG(t, 8, 8)}
					}
					output := sizeTestPNG(t, 11, 17) // deliberately differs from all explicit requests
					var calls atomic.Int32
					const fileID = "file_000000001234567890abcdef12345678"
					var upstream *httptest.Server
					upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.URL.Path == "/":
							io.WriteString(w, `<html data-build="test"></html>`)
						case r.URL.Path == "/backend-api/files":
							var upload struct {
								Width int `json:"width"`
							}
							json.NewDecoder(r.Body).Decode(&upload)
							writeJSON(w, 200, map[string]any{"file_id": fmt.Sprintf("file_ref_%d", upload.Width), "upload_url": upstream.URL + "/upload"})
						case r.URL.Path == "/upload":
							w.WriteHeader(201)
						case strings.HasSuffix(r.URL.Path, "/uploaded"):
							writeJSON(w, 200, map[string]any{"status": "success"})
						case r.URL.Path == "/backend-api/sentinel/chat-requirements/prepare":
							writeJSON(w, 200, map[string]any{"prepare_token": "test"})
						case r.URL.Path == "/backend-api/sentinel/chat-requirements/finalize":
							writeJSON(w, 200, map[string]any{"token": "test"})
						case r.URL.Path == "/backend-api/f/conversation/prepare":
							writeJSON(w, 200, map[string]any{"conduit_token": "test"})
						case r.URL.Path == "/backend-api/f/conversation":
							calls.Add(1)
							var p map[string]any
							json.NewDecoder(r.Body).Decode(&p)
							field, present := p["image_generation_size"]
							if normalized == "auto" {
								if present {
									t.Error("auto sent fixed size", field)
								}
							} else if field != normalized {
								t.Error("wrong explicit size", field)
							}
							messages := p["messages"].([]any)
							msg := mapValue(messages[0])
							parts := mapValue(msg["content"])["parts"].([]any)
							prompt, _ := parts[len(parts)-1].(string)
							if !strings.Contains(prompt, "尺寸不变") || strings.Contains(prompt, "输出图片尺寸为") != (normalized != "auto") {
								t.Error("incorrect size prompt", prompt)
							}
							if normalized != "auto" && !strings.Contains(prompt, "输出图片尺寸为 "+normalized+"。") {
								t.Error("missing explicit size hint")
							}
							attachments, _ := mapValue(msg["metadata"])["attachments"].([]any)
							if len(attachments) != len(refs) || len(parts) != len(refs)+1 {
								t.Error("reference count changed")
							}
							for i, ref := range refs {
								want := decodeImagePixelSize(ref, i+1)
								part := mapValue(parts[i])
								attachment := mapValue(attachments[i])
								if part["width"] != float64(want.Width) || part["height"] != float64(want.Height) || attachment["width"] != float64(want.Width) || attachment["height"] != float64(want.Height) || attachment["id"] != fmt.Sprintf("file_ref_%d", want.Width) {
									t.Error("reference dimensions/order changed")
								}
							}
							w.Header().Set("Content-Type", "text/event-stream")
							io.WriteString(w, `data: {"conversation_id":"conversation-1","message":{"content":{"parts":["file-service://`+fileID+`"]}}}`+"\n\ndata: [DONE]\n\n")
						case r.URL.Path == "/backend-api/conversation/conversation-1":
							writeHTTPAPIGeneratedImageConversation(w, fileID)
						case r.URL.Path == "/backend-api/files/"+fileID+"/download":
							writeJSON(w, 200, map[string]any{"download_url": upstream.URL + "/blob"})
						case r.URL.Path == "/blob":
							w.Header().Set("Content-Type", "image/png")
							w.Write(output)
						default:
							http.NotFound(w, r)
						}
					}))
					defer upstream.Close()
					s.openAIImage = provider.NewOpenAIImage(upstream.URL, upstream.Client(), nil, 30*time.Second)
					fields := map[string]any{"client_task_id": "size-test", "model": "gpt-image-2", "prompt": "尺寸不变"}
					if size != "<omitted>" {
						fields["size"] = size
					}
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, sizeTestRequest(t, path, encoding, fields, refs))
					if strings.HasPrefix(path, "/api/") {
						if w.Code != 202 {
							t.Fatalf("submit %d %s", w.Code, w.Body.String())
						}
						deadline := time.Now().Add(5 * time.Second)
						for {
							w = imageTaskCall(s, "GET", "/api/image-tasks/size-test", "api-secret", "")
							var task imageTaskState
							json.Unmarshal(w.Body.Bytes(), &task)
							if task.Status == "success" || task.Status == "error" {
								if task.Status != "success" || task.Size != requested {
									t.Fatal(w.Body.String())
								}
								break
							}
							if time.Now().After(deadline) {
								t.Fatal("timeout")
							}
							time.Sleep(5 * time.Millisecond)
						}
					}
					if w.Code != 200 {
						t.Fatalf("result %d %s", w.Code, w.Body.String())
					}
					var result struct{ Data []map[string]string }
					if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Data) != 1 {
						t.Fatal("missing result")
					}
					raw, _ := base64.StdEncoding.DecodeString(result.Data[0]["b64_json"])
					if u := result.Data[0]["url"]; u != "" {
						raw = adminRequest(s.Handler(), "GET", u, nil).Body.Bytes()
					}
					if !bytes.Equal(raw, output) || calls.Load() != 1 {
						t.Fatal("result changed or generation repeated")
					}
					logs := imageTaskCall(s, "GET", "/api/logs?type=call&limit=20", "admin-secret", "")
					var logResult struct{ Items []map[string]any }
					json.Unmarshal(logs.Body.Bytes(), &logResult)
					if len(logResult.Items) != 1 {
						t.Fatal("missing call log")
					}
					detail := mapValue(logResult.Items[0]["detail"])
					assertLogInputBytes(t, s, detail, refs)
					meta := mapValue(mapValue(detail["request_meta"])["image_size"])
					if meta["requested_size"] != requested || meta["upstream_size_field_included"] != (normalized != "auto") || meta["size_prompt_appended"] != (normalized != "auto") {
						t.Fatal("wrong logged settings", meta)
					}
					if normalized == "auto" {
						if _, exists := meta["upstream_size"]; exists {
							t.Fatal("auto logged a sent size")
						}
					} else if meta["upstream_size"] != normalized {
						t.Fatal("wrong logged upstream size")
					}
					data, _ := json.Marshal(meta["input_dimensions"])
					var dimensions []imagePixelSize
					json.Unmarshal(data, &dimensions)
					if len(dimensions) != len(refs) {
						t.Fatal("missing input dimensions")
					}
					for i, ref := range refs {
						if dimensions[i] != decodeImagePixelSize(ref, i+1) {
							t.Fatal("wrong input dimensions")
						}
					}
					data, _ = json.Marshal(meta["output_dimensions"])
					json.Unmarshal(data, &dimensions)
					if len(dimensions) != 1 || dimensions[0].Width != 11 || dimensions[0].Height != 17 {
						t.Fatal("wrong actual output dimensions", string(data))
					}
					metadata, _ := json.Marshal(meta)
					if bytes.Contains(metadata, []byte("base64")) || bytes.Contains(metadata, []byte("jwt")) || bytes.Contains(metadata, []byte("http")) {
						t.Fatal("unsafe size metadata")
					}
				})
			}
		}
	}
}

func TestImageTaskSizeLegacyRetryAfterRestart(t *testing.T) {
	for _, mode := range []string{"generate", "edit"} {
		for _, version := range []int{0, 1} {
			for _, requestSize := range []string{"", "auto", "1024x1024", "1024x1365"} {
				t.Run(fmt.Sprintf("%s/v%d/%s", mode, version, requestSize), func(t *testing.T) {
					s := imageTaskTestServer(t)
					refs := [][]byte(nil)
					if mode == "edit" {
						refs = [][]byte{tinyPNG}
					}
					storedSize := requestSize
					if mode == "edit" && storedSize == "" {
						storedSize = "1024x1024"
					}
					if version == 1 {
						storedSize = openAIImageRequestedSize(requestSize)
					}
					now := time.Now().UTC().Format(time.RFC3339)
					task := &imageTaskState{ID: "legacy-size", OwnerID: "api", Mode: mode, Model: "gpt-image-2", N: 1, Size: storedSize, SizeVersion: version, Quality: "auto", Prompt: "test", Images: refs, Status: "success", CreatedAt: now, UpdatedAt: now, Data: []map[string]any{{"url": "/images/generated?id=old-result"}}}
					task.RequestHash = imageTaskHash(task)
					if err := s.saveImageTaskLocked(task); err != nil {
						t.Fatal(err)
					}
					file := filepath.Join(s.imageTasksDir(), imageTaskKey(task.OwnerID, task.ID)+".json")
					before, _ := os.ReadFile(file)
					reloaded := New(s.cfg)
					path := "/api/image-tasks/generations"
					if mode == "edit" {
						path = "/api/image-tasks/edits"
					}
					fields := map[string]any{"client_task_id": task.ID, "model": task.Model, "prompt": "test"}
					if requestSize != "" {
						fields["size"] = requestSize
					}
					for _, encoding := range []string{"json", "multipart"} {
						if encoding == "multipart" && mode != "edit" {
							continue
						}
						w := httptest.NewRecorder()
						reloaded.Handler().ServeHTTP(w, sizeTestRequest(t, path, encoding, fields, refs))
						if w.Code != 202 || !strings.Contains(w.Body.String(), "old-result") || !strings.Contains(w.Body.String(), `"status":"success"`) {
							t.Fatalf("legacy retry: %d %s", w.Code, w.Body.String())
						}
					}
					// New square and auto tasks must remain distinct despite the legacy fallback.
					if version == 1 {
						fields["size"] = "1024x1024"
						if storedSize == "1024x1024" {
							delete(fields, "size")
						}
						w := httptest.NewRecorder()
						reloaded.Handler().ServeHTTP(w, sizeTestRequest(t, path, "json", fields, refs))
						if w.Code != 409 {
							t.Fatal("new square/auto collision", w.Code)
						}
					}
					fields["prompt"] = "different"
					w := httptest.NewRecorder()
					reloaded.Handler().ServeHTTP(w, sizeTestRequest(t, path, "json", fields, refs))
					if w.Code != 409 {
						t.Fatal("changed payload accepted")
					}
					after, _ := os.ReadFile(file)
					if !bytes.Equal(before, after) {
						t.Fatal("legacy record rewritten")
					}
					if w := imageTaskCall(reloaded, "GET", "/api/image-tasks/legacy-size", "admin-secret", ""); w.Code != 404 {
						t.Fatal("owner isolation changed")
					}
				})
			}
		}
	}
}

func TestImageSizeValidationBeforeAcceptance(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits", "/api/image-tasks/generations", "/api/image-tasks/edits"} {
		s := imageTaskTestServer(t)
		fields := map[string]any{"client_task_id": "invalid", "model": "gpt-image-2", "prompt": "test", "size": "0x12"}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, imageOutputRequest(t, path, "json", fields))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid image size") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if len(s.imageTasks) != 0 {
			t.Fatal("invalid size created a task")
		}
	}
}
