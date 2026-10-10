package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestTextOnlyImageFailureReachesSyncAndPersistedAsyncAPI(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			s := imageTaskTestServer(t)
			s.cfg.ChatMaxRetries = 2
			s.cfg.ChatRetryCodes = map[int]bool{422: true}
			if _, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"access_token": "jwt.header.payload", "pool": "basic"}}); err != nil {
				t.Fatal(err)
			}
			var starts, polls atomic.Int32
			client := &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				raw := `{}`
				switch r.URL.Path {
				case "/":
					raw = `<html data-build="test"></html>`
				case "/backend-api/sentinel/chat-requirements/prepare":
					raw = `{"prepare_token":"test"}`
				case "/backend-api/sentinel/chat-requirements/finalize":
					raw = `{"token":"test"}`
				case "/backend-api/f/conversation/prepare":
					raw = `{"conduit_token":"test"}`
				case "/backend-api/f/conversation":
					starts.Add(1)
					raw = "data: {\"conversation_id\":\"text-only\"}\n\ndata: [DONE]\n\n"
				case "/backend-api/conversation/text-only":
					polls.Add(1)
					raw = `{"current_node":"reply","mapping":{"input":{"parent":null,"message":{"author":{"role":"user"},"content":{"content_type":"text","parts":["edit a photo"]}}},"reply":{"parent":"input","message":{"author":{"role":"assistant"},"recipient":"all","status":"finished_successfully","end_turn":true,"content":{"content_type":"text","parts":["请上传需要优化的照片。"]}}}}}`
				default:
					t.Errorf("unexpected upstream request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
			})}
			s.openAIImage = provider.NewOpenAIImage("https://upstream.invalid", client, nil, time.Second)
			if async {
				task := &imageTaskState{ID: "text-only-task", OwnerID: "api", Mode: "generate", Model: "gpt-image-2", N: 1, Prompt: "test", Status: "queued"}
				s.runImageTask(context.Background(), task, "Bearer api-secret", "")
				if task.Status != "error" || !strings.Contains(task.Error, "请上传需要优化的照片") {
					t.Fatalf("failed task lost reason: %+v", task)
				}
				restarted := New(s.cfg)
				response := imageTaskCall(restarted, "GET", "/api/image-tasks/"+task.ID, "api-secret", "")
				if response.Code != 200 || !strings.Contains(response.Body.String(), "请上传需要优化的照片") {
					t.Fatal("business reason lost after restart", response.Body.String())
				}
			} else {
				response := imageTaskCall(s, "POST", "/v1/images/generations", "api-secret", `{"model":"gpt-image-2","prompt":"test","n":1}`)
				if response.Code != 422 || !strings.Contains(response.Body.String(), "请上传需要优化的照片") {
					t.Fatalf("sync rejected without useful reason: %d %s", response.Code, response.Body.String())
				}
			}
			if starts.Load() != 1 || polls.Load() != 1 {
				t.Fatalf("terminal work retried or waited: start=%d poll=%d", starts.Load(), polls.Load())
			}
		})
	}
}
