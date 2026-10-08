package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIMultimodalNormalization(t *testing.T) {
	var request ResponsesRequest
	body := `{"model":"auto","instructions":"Plan only","input":[{"role":"user","content":[{"type":"input_text","text":"first"},{"type":"input_image","image_url":"data:image/png;base64,eA=="},{"type":"input_text","text":"second"}]},{"role":"assistant","content":[{"type":"output_text","text":"earlier plan"}]},{"role":"user","content":"revise"}],"store":false}`
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	chat, err := OpenAIResponsesChat(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 4 || chat.Messages[0].Role != "developer" || chat.Messages[2].Role != "assistant" {
		t.Fatalf("lost instructions/history: %#v", chat.Messages)
	}
	parts := chat.Messages[1].Content.([]any)
	if len(parts) != 3 || parts[0].(map[string]any)["text"] != "first" || parts[1].(map[string]any)["type"] != "image_url" || parts[2].(map[string]any)["text"] != "second" {
		t.Fatalf("lost part order: %#v", parts)
	}
}

func TestOpenAIInputValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		responses  bool
		wantError  bool
	}{
		{"plain", `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, false, false},
		{"image only", `{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png","detail":"auto"}}]}]}`, false, false},
		{"tool choice object", `{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function"}}`, false, true},
		{"format type object", `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":{}}}`, false, true},
		{"tools", `{"input":"hi","tools":[{"type":"function"}]}`, true, true},
		{"tool item", `{"input":[{"type":"function_call_output","call_id":"abc","output":"x"}]}`, true, true},
		{"stateful", `{"input":"hi","previous_response_id":"resp_test"}`, true, true},
		{"background", `{"input":"hi","background":true}`, true, true},
		{"strict JSON", `{"input":"hi","text":{"format":{"type":"json_schema","name":"plan","schema":{}}}}`, true, true},
		{"temperature", `{"input":"hi","temperature":1}`, true, true},
		{"missing input", `{"instructions":"hi","input":[]}`, true, true},
		{"file", `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file_test"}]}]}`, true, true},
		{"audio", `{"input":[{"role":"user","content":[{"type":"input_audio","data":"abc"}]}]}`, true, true},
		{"detail type", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png","detail":42}}]}]}`, false, true},
		{"response simple", `{"input":"hello","store":false,"background":false,"text":{"format":{"type":"text"}},"tool_choice":"none"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.responses {
				var r ResponsesRequest
				if e := json.Unmarshal([]byte(tc.body), &r); e != nil {
					t.Fatal(e)
				}
				_, err = OpenAIResponsesChat(r)
			} else {
				var r ChatRequest
				if e := json.Unmarshal([]byte(tc.body), &r); e != nil {
					t.Fatal(e)
				}
				_, err = NormalizeOpenAIChat(r)
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("want error=%v, got %v", tc.wantError, err)
			}
		})
	}
	parts := strings.Repeat(`{"type":"image_url","image_url":{"url":"https://example.test/a.png"}},`, 8)
	var r ChatRequest
	if err := json.Unmarshal([]byte(`{"messages":[{"role":"user","content":[`+strings.TrimSuffix(parts, ",")+`]}]}`), &r); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeOpenAIChat(r); err == nil {
		t.Fatal("too many images accepted")
	}
}
