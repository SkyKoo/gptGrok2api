package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func TestOpenAIChatPayloadUsesRawMessageText(t *testing.T) {
	payload := openAIChatPayload(protocol.ChatRequest{
		Model: "gpt-5-6",
		Messages: []protocol.Message{
			{Role: "user", Content: "请回复Go测试成功"},
		},
	})
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("unexpected messages: %#v", payload["messages"])
	}
	message, _ := messages[0].(map[string]any)
	content, _ := message["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 || parts[0] != "请回复Go测试成功" {
		t.Fatalf("message text was decorated: %#v", parts)
	}
	if strings.Contains(parts[0].(string), "[user]") {
		t.Fatalf("message contains protocol decoration: %q", parts[0])
	}
}

func TestOpenAIChatUploadsImagesForEachAccountAttempt(t *testing.T) {
	var uploads, conversations int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case r.URL.Path == "/":
			fmt.Fprint(w, `<html data-build="test"></html>`)
		case r.URL.Path == "/backend-api/files":
			uploads++
			_ = json.NewEncoder(w).Encode(map[string]any{"file_id": "file_" + account, "upload_url": serverURL(r) + "/blob"})
		case r.Method == http.MethodPut && r.URL.Path == "/blob":
			if r.Header.Get("Authorization") != "" {
				t.Error("credentials leaked to signed upload")
			}
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/uploaded"):
			if r.URL.Path != "/backend-api/files/file_"+account+"/uploaded" {
				t.Error("upload confirmation used a different account")
			}
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/chat-requirements/prepare"):
			fmt.Fprint(w, `{"prepare_token":"test"}`)
		case strings.HasSuffix(r.URL.Path, "/chat-requirements/finalize"):
			fmt.Fprint(w, `{"token":"test"}`)
		case r.URL.Path == "/backend-api/conversation":
			conversations++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			messages := payload["messages"].([]any)
			if messages[0].(map[string]any)["author"].(map[string]any)["role"] != "system" {
				t.Error("developer instruction role was lost")
			}
			message := messages[1].(map[string]any)
			content := message["content"].(map[string]any)
			parts := content["parts"].([]any)
			if content["content_type"] != "multimodal_text" || len(parts) != 3 || parts[0] != "before" || parts[2] != "after" || parts[1].(map[string]any)["asset_pointer"] != "file-service://file_"+account {
				t.Errorf("incorrect multimodal content: %#v", content)
			}
			if _, exists := message["metadata"].(map[string]any)["system_hints"]; exists {
				t.Error("text planning must not enable picture_v2")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["plan"]}}}`)
			fmt.Fprintln(w, `data: [DONE]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	chat := NewOpenAIChat(NewOpenAIImage(upstream.URL, upstream.Client(), nil, 5*time.Second))
	request := protocol.ChatRequest{Model: "auto", Messages: []protocol.Message{
		{Role: "developer", Content: "Only produce a plan"},
		{Role: "user", Content: []any{map[string]any{"type": "text", "text": "before"}, map[string]any{"type": "image_url"}, map[string]any{"type": "text", "text": "after"}}},
	}}
	input := OpenAIChatImage{MessageIndex: 1, PartIndex: 1, Input: OpenAIImageInput{Name: "reference.png", MIME: "image/png", Data: onePixelPNG(t)}}
	for _, token := range []string{"first", "second"} {
		text, _, err := chat.Complete(context.Background(), accounts.Account{Token: token}, request, input)
		if err != nil || text != "plan" {
			t.Fatalf("chat failed: text=%q err=%v", text, err)
		}
	}
	if uploads != 2 || conversations != 2 {
		t.Fatalf("references were reused across account attempts: uploads=%d conversations=%d", uploads, conversations)
	}
}

func TestOpenAIChatStateReturnsSSEDetailAsError(t *testing.T) {
	state := &openAIChatState{}
	_, err := state.event(map[string]any{"detail": "Invalid conversation body"})
	if err == nil || !strings.Contains(err.Error(), "Invalid conversation body") {
		t.Fatalf("expected upstream detail error, got %v", err)
	}
}

func TestOpenAIChatStreamTerminals(t *testing.T) {
	const text = `data: {"message":{"author":{"role":"assistant"},"content":{"parts":["hello"]}}}` + "\n"
	for _, tc := range []struct {
		name, body string
		failure    bool
	}{
		{"done", text + "data: [DONE]\n", false},
		{"truncated", text, true},
		{"empty", "data: [DONE]\n", true},
		{"rate limit", `data: {"error":{"code":"rate_limit","message":"wait"}}` + "\n", true},
		{"midstream", text + `data: {"error":{"message":"failed"}}` + "\n", true},
		{"malformed", text + "data: {bad\ndata: [DONE]\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := false
			err := scanOpenAIChat(strings.NewReader(tc.body), func(e OpenAIChatEvent) error { done = done || e.Done; return nil })
			if (err != nil) != tc.failure || done == tc.failure {
				t.Fatalf("error=%v done=%v", err, done)
			}
		})
	}
}

func TestOpenAIChatIgnoresNonFinalContent(t *testing.T) {
	body := `data: {"message":{"author":{"role":"assistant"},"channel":"analysis","content":{"parts":["hidden"]}}}
data: {"p":"/message/content/parts/0","o":"append","v":" hidden delta"}
data: {"message":{"author":{"role":"assistant"},"channel":"final","content":{"parts":["answer"]}}}
data: {"p":"/message/metadata/title","o":"append","v":"metadata"}
data: {"p":"/message/content/parts/0","o":"append","v":"!"}
data: [DONE]
`
	var output strings.Builder
	err := scanOpenAIChat(strings.NewReader(body), func(e OpenAIChatEvent) error { output.WriteString(e.Text); return nil })
	if err != nil || output.String() != "answer!" {
		t.Fatalf("output=%q err=%v", output.String(), err)
	}
}
