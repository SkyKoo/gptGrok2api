package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func chatImageFixture(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}

// The mock only exposes ChatGPT Web paths. A regression routing Responses to
// Grok fails here instead of accidentally passing through another provider.
func multimodalTestServer(t *testing.T, conversation http.HandlerFunc) (*Server, *atomic.Int32) {
	t.Helper()
	uploads := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case r.URL.Path == "/":
			fmt.Fprint(w, `<html data-build="test"></html>`)
		case r.URL.Path == "/backend-api/files":
			id := fmt.Sprintf("%s-%d", token, uploads.Add(1))
			_ = json.NewEncoder(w).Encode(map[string]any{"file_id": id, "upload_url": "http://" + r.Host + "/blob"})
		case r.Method == http.MethodPut && r.URL.Path == "/blob":
			if token != "" {
				t.Error("authorization leaked to signed upload")
			}
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/uploaded"):
			if !strings.Contains(r.URL.Path, "/"+token+"-") {
				t.Error("upload used a different account")
			}
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/chat-requirements/prepare"):
			fmt.Fprint(w, `{"prepare_token":"test"}`)
		case strings.HasSuffix(r.URL.Path, "/chat-requirements/finalize"):
			fmt.Fprint(w, `{"token":"test"}`)
		case r.URL.Path == "/backend-api/conversation":
			conversation(w, r)
		default:
			t.Errorf("wrong upstream route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	cfg := chatTestConfig(t.TempDir(), upstream.URL+"/wrong-grok-route")
	cfg.OpenAIBaseURL = upstream.URL
	cfg.RequestTimeout = 5 * time.Second
	server := New(cfg)
	for _, token := range []string{"first", "second"} {
		if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{{"access_token": token, "source_type": "chatgpt_web", "type": "free"}}); err != nil {
			t.Fatal(err)
		}
	}
	return server, uploads
}

func multimodalRequestBody(t *testing.T, responses, stream, images bool) string {
	t.Helper()
	parts := []any{}
	textType, imageType := "text", "image_url"
	if responses {
		textType, imageType = "input_text", "input_image"
	}
	parts = append(parts, map[string]any{"type": textType, "text": "before"})
	if images {
		for i := 0; i < 2; i++ {
			var url any = map[string]any{"url": chatImageFixture(t)}
			if responses {
				url = chatImageFixture(t)
			}
			parts = append(parts, map[string]any{"type": imageType, "image_url": url})
		}
	}
	parts = append(parts, map[string]any{"type": textType, "text": "after"})
	messages := []any{map[string]any{"role": "developer", "content": "plan only"}, map[string]any{"role": "assistant", "content": "history"}, map[string]any{"role": "user", "content": parts}}
	body := map[string]any{"model": "auto", "stream": stream}
	if responses {
		body["input"] = messages
		body["store"] = false
	} else {
		body["messages"] = messages
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func invokeMultimodal(server *Server, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer api-secret")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	return w
}

func parseResponseEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	events := []map[string]any{}
	name := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			name = strings.TrimPrefix(line, "event: ")
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &item); err != nil {
			t.Fatal(err)
		}
		if item["type"] != name || item["sequence_number"] != float64(len(events)) {
			t.Fatalf("bad event name/sequence: %#v", item)
		}
		events = append(events, item)
	}
	return events
}

func TestOpenAIMultimodalEndpoints(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, images := range []bool{false, true} {
				t.Run(fmt.Sprintf("responses=%v/stream=%v/images=%v", responses, stream, images), func(t *testing.T) {
					server, uploads := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							return
						}
						messages := body["messages"].([]any)
						if len(messages) != 3 || messages[0].(map[string]any)["author"].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["author"].(map[string]any)["role"] != "assistant" {
							t.Errorf("lost role/history: %#v", messages)
						}
						content := messages[2].(map[string]any)["content"].(map[string]any)
						parts := content["parts"].([]any)
						if parts[0] != "before" || parts[len(parts)-1] != "after" {
							t.Error("text order changed")
						}
						if images {
							token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
							if len(parts) != 4 || content["content_type"] != "multimodal_text" {
								t.Errorf("lost image input: %#v", content)
							}
							for _, part := range parts[1:3] {
								if !strings.HasPrefix(part.(map[string]any)["asset_pointer"].(string), "file-service://"+token+"-") {
									t.Error("wrong account image")
								}
							}
						}
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["hello "]}}}`)
						fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["hello world"]}}}`)
						fmt.Fprintln(w, "data: [DONE]")
					})
					path := "/v1/chat/completions"
					if responses {
						path = "/v1/responses"
					}
					result := invokeMultimodal(server, path, multimodalRequestBody(t, responses, stream, images))
					if result.Code != 200 {
						t.Fatalf("status=%d %s", result.Code, result.Body.String())
					}
					wantUploads := int32(0)
					if images {
						wantUploads = 2
					}
					if uploads.Load() != wantUploads {
						t.Fatalf("uploads=%d", uploads.Load())
					}
					if responses && stream {
						events := parseResponseEvents(t, result.Body.String())
						expected := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}
						if len(events) != len(expected) {
							t.Fatalf("unexpected events: %s", result.Body.String())
						}
						for i, kind := range expected {
							if events[i]["type"] != kind {
								t.Fatalf("event %d: %#v", i, events[i])
							}
						}
						final := events[len(events)-1]["response"].(map[string]any)
						if final["status"] != "completed" || final["usage"] != nil || strings.Contains(result.Body.String(), "[DONE]") {
							t.Fatalf("invalid terminal response: %#v", final)
						}
						added := events[2]["item"].(map[string]any)
						if added["id"] != events[4]["item_id"] || added["id"] != final["output"].([]any)[0].(map[string]any)["id"] {
							t.Fatal("message IDs changed")
						}
					} else if !stream {
						var resultBody map[string]any
						if err := json.Unmarshal(result.Body.Bytes(), &resultBody); err != nil {
							t.Fatal(err)
						}
						if !strings.Contains(result.Body.String(), "hello world") || resultBody["usage"] != nil {
							t.Fatal(result.Body.String())
						}
						if responses {
							if resultBody["object"] != "response" || resultBody["status"] != "completed" {
								t.Fatal(result.Body.String())
							}
						} else if resultBody["object"] != "chat.completion" {
							t.Fatal(result.Body.String())
						}
					} else if !strings.Contains(result.Body.String(), `"finish_reason":"stop"`) || !strings.Contains(result.Body.String(), "data: [DONE]") {
						t.Fatal(result.Body.String())
					}
				})
			}
		}
	}
}

func TestOpenAIChatRetriesReuploadAndFailsPartialStream(t *testing.T) {
	for _, failure := range []string{"rate_limit", "upload", "truncated", "midstream", "invalid_request"} {
		for _, responses := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/responses=%v", failure, responses), func(t *testing.T) {
				var calls atomic.Int32
				server, uploads := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					n := calls.Add(1)
					if n == 1 && failure == "rate_limit" {
						http.Error(w, "limited", 429)
						return
					}
					if failure == "invalid_request" {
						http.Error(w, "bad payload", 400)
						return
					}
					fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["partial"]}}}`)
					if failure == "midstream" {
						fmt.Fprintln(w, `data: {"error":{"message":"disconnected"}}`)
						return
					}
					if failure != "truncated" {
						fmt.Fprintln(w, "data: [DONE]")
					}
				})
				if failure == "upload" {
					// Fail the whole transport before conversation: the provider must not
					// create a conversation with unuploaded image references.
					server.openAIChat.Image.BaseURL = "http://127.0.0.1:1"
				}
				path := "/v1/chat/completions"
				if responses {
					path = "/v1/responses"
				}
				result := invokeMultimodal(server, path, multimodalRequestBody(t, responses, true, true))
				success := failure == "rate_limit"
				body := result.Body.String()
				terminal := `"finish_reason":"stop"`
				if responses {
					terminal = "event: response.completed"
				}
				if strings.Contains(body, terminal) != success {
					t.Fatal(body)
				}
				if success {
					if calls.Load() != 2 || uploads.Load() != 4 {
						t.Fatalf("retry uploads=%d calls=%d", uploads.Load(), calls.Load())
					}
				} else if failure != "upload" && calls.Load() != 1 {
					t.Fatalf("retried unsafe or nonretryable response: %d", calls.Load())
				}
				if responses && !success {
					events := parseResponseEvents(t, body)
					if events[len(events)-1]["type"] != "response.failed" {
						t.Fatal(body)
					}
				}
			})
		}
	}
}

func TestOpenAIInvalidImagesAndUnsupportedOptions(t *testing.T) {
	var calls atomic.Int32
	server, uploads := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "must not reach upstream", 500)
	})
	for _, body := range []string{
		`{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,bm90YW5pbWFnZQ=="}}]}]}`,
		`{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://127.0.0.1/private?secret=do-not-echo"}}]}]}`,
		`{"model":"auto","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema"}}`,
	} {
		result := invokeMultimodal(server, "/v1/chat/completions", body)
		if result.Code != 400 || !strings.Contains(result.Body.String(), `"param":`) || strings.Contains(result.Body.String(), "do-not-echo") {
			t.Fatalf("invalid error: %d %s", result.Code, result.Body.String())
		}
	}
	if calls.Load() != 0 || uploads.Load() != 0 {
		t.Fatal("invalid input consumed an account attempt")
	}
	result := invokeMultimodal(server, "/v1/responses", `{"model":"gpt-image-2","input":"draw"}`)
	if result.Code != 400 {
		t.Fatalf("image model routed as ordinary chat: %d", result.Code)
	}
}

func TestOpenAIResponsesStreamsBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["first"]}}}`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprintln(w, "data: [DONE]")
	})
	api := httptest.NewServer(server.Handler())
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", api.URL+"/v1/responses", strings.NewReader(`{"model":"auto","input":"hi","stream":true}`))
	request.Header.Set("Authorization", "Bearer api-secret")
	response, err := api.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("no delta while upstream is pending: %v", err)
		}
		if strings.Contains(line, `"delta":"first"`) {
			break
		}
	}
	unblock()
	rest, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(rest), "response.completed") {
		t.Fatalf("missing terminal event: %s %v", rest, err)
	}
}

func TestOpenAIChatRemoteImageAndCancellation(t *testing.T) {
	var calls atomic.Int32
	server, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["ok"]}}}`)
		fmt.Fprintln(w, "data: [DONE]")
	})
	imageData := strings.SplitN(chatImageFixture(t), ",", 2)[1]
	raw, _ := base64.StdEncoding.DecodeString(imageData)
	fetched := false
	server.requestClient = &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		fetched = true
		if r.URL.String() != "https://93.184.216.34/reference.png" || r.Header.Get("Authorization") != "" {
			t.Fatal("unexpected or authenticated image fetch")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})}
	body := `{"model":"auto","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://93.184.216.34/reference.png"}]}]}`
	result := invokeMultimodal(server, "/v1/responses", body)
	if result.Code != 200 || !fetched {
		t.Fatalf("URL/image-only request failed: %d %s", result.Code, result.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"auto","input":"hi","stream":true}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer api-secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if calls.Load() != 1 || strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatal("cancelled request reached upstream or reported success")
	}
}

func TestOpenAIResponsesDisconnectCancelsUpstream(t *testing.T) {
	stopped := make(chan struct{})
	var calls atomic.Int32
	server, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["first"]}}}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	})
	api := httptest.NewServer(server.Handler())
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", api.URL+"/v1/responses", strings.NewReader(`{"model":"auto","input":"hi","stream":true}`))
	request.Header.Set("Authorization", "Bearer api-secret")
	response, err := api.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, `"delta":"first"`) {
			break
		}
	}
	cancel()
	response.Body.Close()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel upstream")
	}
	if calls.Load() != 1 {
		t.Fatal("client disconnect retried")
	}
}

func TestOpenAIUpstreamAuthMappingAndLogs(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, status := range []int{401, 403} {
				t.Run(fmt.Sprintf("responses=%v/stream=%v/status=%d", responses, stream, status), func(t *testing.T) {
					var calls atomic.Int32
					server, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						http.Error(w, "token_revoked private-upstream-detail", status)
					})
					path := "/v1/chat/completions"
					if responses {
						path = "/v1/responses"
					}
					result := invokeMultimodal(server, path, multimodalRequestBody(t, responses, stream, false))
					wantHTTP := 502
					if stream {
						wantHTTP = 200
					}
					if result.Code != wantHTTP || !strings.Contains(result.Body.String(), "upstream_authentication_error") || strings.Contains(result.Body.String(), "private-upstream-detail") {
						t.Fatalf("bad auth error: %d %s", result.Code, result.Body.String())
					}
					if calls.Load() < 1 || calls.Load() > int32(server.cfg.ChatMaxRetries+1) {
						t.Fatalf("retry limit: %d", calls.Load())
					}
					logs := server.loadCallLogs()
					if len(logs) != 1 {
						t.Fatalf("logs=%d", len(logs))
					}
					detail := mapValue(logs[0]["detail"])
					meta := mapValue(detail["request_meta"])
					if detail["endpoint"] != path || detail["status"] != "failed" || intValue(meta["upstream_status"]) != status || intValue(meta["error_status"]) != 502 || intValue(meta["upstream_attempts"]) != int(calls.Load()) {
						t.Fatalf("bad log: %#v", detail)
					}
					if !strings.Contains(stringValue(detail["request_text_full"]), "before") {
						t.Fatalf("lost request summary: %#v", detail)
					}
					// Caller auth still fails before an upstream attempt.
					r := httptest.NewRequest("POST", path, strings.NewReader(multimodalRequestBody(t, responses, false, false)))
					r.Header.Set("Authorization", "Bearer wrong-key")
					w := httptest.NewRecorder()
					server.Handler().ServeHTTP(w, r)
					if w.Code != 401 {
						t.Fatalf("caller auth changed: %d", w.Code)
					}
				})
			}
		}
	}
}

func TestResponsesLogsSuccessAndLateStreamFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			server, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				// Error arrives after the response capture's 8 KiB limit.
				raw, _ := json.Marshal(map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "content": map[string]any{"parts": []string{strings.Repeat("a", 10000)}}}})
				fmt.Fprintf(w, "data: %s\n\n", raw)
				if !fail {
					fmt.Fprintln(w, "data: [DONE]")
				}
			})
			body := multimodalRequestBody(t, true, true, true)
			result := invokeMultimodal(server, "/v1/responses", body)
			if result.Code != 200 {
				t.Fatal(result.Body.String())
			}
			logs := server.loadCallLogs()
			if len(logs) != 1 {
				t.Fatalf("logs=%d", len(logs))
			}
			detail := mapValue(logs[0]["detail"])
			shape := mapValue(detail["request_shape"])
			want := "success"
			if fail {
				want = "failed"
			}
			if detail["status"] != want || intValue(shape["image_url_parts"]) != 2 || intValue(shape["data_url_images"]) != 2 {
				t.Fatalf("bad outcome/shape: %#v", detail)
			}
			raw, _ := json.Marshal(logs)
			if strings.Contains(string(raw), "data:image/") || strings.Contains(string(raw), "api-secret") {
				t.Fatal("request binary or key leaked to logs")
			}
		})
	}
}

func TestOpenAIExpiredAccountsDoNotConsumeRetries(t *testing.T) {
	var calls atomic.Int32
	server, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer valid" {
			t.Error("expired token reached upstream")
		}
		fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["ok"]}}}`)
		fmt.Fprintln(w, "data: [DONE]")
	})
	items := []map[string]any{}
	for i := 0; i < 6; i++ {
		token := "header." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, i))) + ".signature"
		items = append(items, map[string]any{"access_token": token, "source_type": "chatgpt_web", "status": "正常"})
	}
	items = append(items, map[string]any{"access_token": "valid", "source_type": "chatgpt_web", "status": "正常"})
	if err := server.store.SaveAccounts(items); err != nil {
		t.Fatal(err)
	}
	server.cfg.ChatMaxRetries = 0
	result := invokeMultimodal(server, "/v1/responses", `{"model":"auto","input":"hi"}`)
	if result.Code != 200 || calls.Load() != 1 {
		t.Fatalf("expired accounts consumed retry: %d calls=%d", result.Code, calls.Load())
	}
}
