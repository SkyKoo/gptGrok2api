package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestImageErrorDiagnosticsSurviveSyncAsyncAndRestart(t *testing.T) {
	for _, mode := range []string{"generations", "edits"} {
		for _, async := range []bool{false, true} {
			name := mode + "/sync"
			if async {
				name = mode + "/async"
			}
			t.Run(name, func(t *testing.T) {
				s := imageTaskTestServer(t)
				s.cfg.ChatMaxRetries = 2
				s.cfg.ChatRetryCodes = map[int]bool{422: true}
				if _, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"access_token": "jwt.header.payload", "pool": "basic"}}); err != nil {
					t.Fatal(err)
				}
				reason := "Upstream image tool failed.\nCookie: session=private-cookie\nAccount jwt.header.payload person@example.test\n" + strings.Repeat("long diagnostic context. ", 500) + "TAIL_CAUSE: renderer unavailable"
				message := map[string]any{"author": map[string]any{"role": "tool"}, "metadata": map[string]any{"is_error": true}, "status": "finished_successfully", "content": map[string]any{"content_type": "text", "parts": []any{reason}}}
				var starts atomic.Int32
				client := &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					raw := `{}`
					switch r.URL.Path {
					case "/":
						raw = `<html data-build="test"></html>`
					case "/backend-api/files":
						raw = `{"file_id":"file_reference","upload_url":"https://upstream.invalid/reference-upload"}`
					case "/reference-upload", "/backend-api/files/file_reference/uploaded":
					case "/backend-api/sentinel/chat-requirements/prepare":
						raw = `{"prepare_token":"test"}`
					case "/backend-api/sentinel/chat-requirements/finalize":
						raw = `{"token":"test"}`
					case "/backend-api/f/conversation/prepare":
						raw = `{"conduit_token":"test"}`
					case "/backend-api/f/conversation":
						starts.Add(1)
						if mode == "edits" {
							raw = "data: {\"conversation_id\":\"test-conversation\"}\n\ndata: [DONE]\n\n"
						} else {
							b, _ := json.Marshal(map[string]any{"message": message})
							raw = "data: " + string(b) + "\n\ndata: [DONE]\n\n"
						}
					case "/backend-api/conversation/test-conversation":
						b, _ := json.Marshal(map[string]any{"message": message})
						raw = string(b)
					default:
						t.Errorf("unexpected upstream call %s", r.URL.Path)
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
				})}
				s.openAIImage = provider.NewOpenAIImage("https://upstream.invalid", client, nil, time.Second)
				var task *imageTaskState
				if async {
					task = &imageTaskState{ID: "diagnostic-task", OwnerID: "api", Mode: "generate", Model: "gpt-image-2", N: 1, Prompt: "test", Status: "queued"}
					if mode == "edits" {
						task.Mode = "edit"
						task.Images = [][]byte{tinyPNG}
					}
					s.runImageTask(context.Background(), task, "Bearer api-secret", "")
					if task.Status != "error" || !strings.HasSuffix(task.ErrorDetail, "TAIL_CAUSE: renderer unavailable") || task.ErrorDetailTruncated {
						t.Fatal("async full reason lost")
					}
					reloaded := New(s.cfg)
					persisted := reloaded.imageTasks[imageTaskKey("api", task.ID)]
					if persisted == nil || persisted.ErrorDetail != task.ErrorDetail {
						t.Fatal("diagnostic did not survive restart")
					}
					public := imageTaskCall(reloaded, "GET", "/api/image-tasks/"+task.ID, "api-secret", "")
					if public.Code != 200 || strings.Contains(public.Body.String(), "TAIL_CAUSE") || strings.Contains(public.Body.String(), "error_detail") {
						t.Fatal("private diagnostic leaked into task API")
					}
					raw, err := os.ReadFile(filepath.Join(s.imageTasksDir(), imageTaskKey("api", task.ID)+".json"))
					if err != nil {
						t.Fatal(err)
					}
					assertNoImageErrorSecrets(t, string(raw))
				} else {
					request := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"test","n":1}`))
					request.Header.Set("Content-Type", "application/json")
					if mode == "edits" {
						var b bytes.Buffer
						w := multipart.NewWriter(&b)
						_ = w.WriteField("model", "gpt-image-2")
						_ = w.WriteField("prompt", "test")
						f, _ := w.CreateFormFile("image[]", "test.png")
						_, _ = f.Write(tinyPNG)
						_ = w.Close()
						request = httptest.NewRequest("POST", "/v1/images/edits", &b)
						request.Header.Set("Content-Type", w.FormDataContentType())
					}
					request.Header.Set("Authorization", "Bearer api-secret")
					response := httptest.NewRecorder()
					s.Handler().ServeHTTP(response, request)
					if response.Code != 422 || strings.Contains(response.Body.String(), "TAIL_CAUSE") || response.Body.Len() > 1024 {
						t.Fatalf("sync contract changed: %d", response.Code)
					}
					assertNoImageErrorSecrets(t, response.Body.String())
				}
				if starts.Load() != 1 {
					t.Fatalf("terminal failure generated %d times", starts.Load())
				}
				raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "logs.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				assertNoImageErrorSecrets(t, string(raw))
				var row map[string]any
				if err = json.Unmarshal(bytes.TrimSpace(raw), &row); err != nil {
					t.Fatal(err)
				}
				d := mapValue(row["detail"])
				full := stringValue(d["raw_error"])
				if len(full) < 8<<10 || !strings.HasSuffix(full, "TAIL_CAUSE: renderer unavailable") || full != stringValue(d["upstream_error"]) || d["error_detail_redacted"] != true || d["error_detail_truncated"] != false {
					t.Fatal("full log diagnostic missing")
				}
				if strings.Contains(stringValue(d["error"]), "TAIL_CAUSE") {
					t.Fatal("log summary includes full error")
				}
				reloaded := New(s.cfg)
				admin := imageTaskCall(reloaded, "GET", "/api/logs?type=call", "admin-secret", "")
				if admin.Code != 200 || !strings.Contains(admin.Body.String(), "TAIL_CAUSE") {
					t.Fatal("admin cannot retrieve persistent full reason")
				}
				denied := imageTaskCall(reloaded, "GET", "/api/logs?type=call", "api-secret", "")
				if denied.Code == 200 || strings.Contains(denied.Body.String(), "TAIL_CAUSE") {
					t.Fatal("API key can read admin diagnostic")
				}
			})
		}
	}
}

func assertNoImageErrorSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{"private-cookie", "jwt.header.payload", "person@example.test", "api-secret"} {
		if strings.Contains(raw, secret) {
			t.Errorf("credential leaked into error data: %s", secret)
		}
	}
}
