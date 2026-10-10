package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func textOnlyConversation() map[string]any {
	return map[string]any{"current_node": "reply", "mapping": map[string]any{
		"input": map[string]any{"parent": nil, "message": map[string]any{"author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []any{"优化照片"}}}},
		"reply": map[string]any{"parent": "input", "message": map[string]any{"author": map[string]any{"role": "assistant"}, "recipient": "all", "status": "finished_successfully", "end_turn": true, "content": map[string]any{"content_type": "text", "parts": []any{"请上传需要优化的照片。"}}}},
	}}
}

func TestImageTextOnlyCompletionEndsPollingWithBusinessReason(t *testing.T) {
	for _, reason := range []string{"请上传需要优化的照片。", "无法按此要求生成，请调整描述。", "图片生成额度不足，请稍后重试。"} {
		t.Run(reason, func(t *testing.T) {
			value := textOnlyConversation()
			reply := value["mapping"].(map[string]any)["reply"].(map[string]any)["message"].(map[string]any)
			reply["content"] = map[string]any{"content_type": "text", "parts": []any{reason + " https://example.invalid/signed?token=secret jwt.header.payload"}}
			raw, _ := json.Marshal(value)
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			})}
			o := NewOpenAIImage("https://example.invalid", client, nil, 100*time.Millisecond)
			_, err := o.pollConversation(context.Background(), accounts.Account{Token: "jwt.header.payload"}, "test")
			var upstream *protocol.UpstreamError
			if !IsImageTerminalError(err) || !errors.As(err, &upstream) || upstream.Status != 422 || calls != 1 || !strings.Contains(err.Error(), reason) {
				t.Fatalf("terminal reason not returned promptly: calls=%d err=%v", calls, err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(ImageErrorDetails(err).Message, "jwt.header.payload") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestImageTextOnlyDetectionDoesNotStopActiveOrAmbiguousWork(t *testing.T) {
	for _, kind := range []string{"still-generating", "stream-fragment", "unfinished", "end-turn-false", "reasoning", "tool-call", "tool-output", "async-marker", "cycle", "missing-parent", "old-branch", "user-current"} {
		t.Run(kind, func(t *testing.T) {
			v := textOnlyConversation()
			mapping := v["mapping"].(map[string]any)
			n := mapping["reply"].(map[string]any)
			m := n["message"].(map[string]any)
			switch kind {
			case "still-generating":
				v["is_generating"] = true
			case "stream-fragment":
				v = map[string]any{"message": m}
			case "unfinished":
				m["status"] = "in_progress"
			case "end-turn-false":
				m["end_turn"] = false
			case "reasoning":
				m["channel"] = "analysis"
			case "tool-call":
				m["recipient"] = "imagegen"
			case "tool-output":
				n["parent"] = "tool"
				mapping["tool"] = map[string]any{"parent": "input", "message": map[string]any{"author": map[string]any{"role": "tool"}}}
			case "async-marker":
				m["metadata"] = map[string]any{"image_gen_async": map[string]any{"status": "running"}}
			case "cycle":
				n["parent"] = "reply"
			case "missing-parent":
				n["parent"] = "missing"
			case "old-branch":
				v["current_node"] = "new"
				mapping["new"] = map[string]any{"parent": "input", "message": map[string]any{"author": map[string]any{"role": "assistant"}, "status": "in_progress"}}
			case "user-current":
				v["current_node"] = "input"
			}
			if err := openAIImageTextOnlyError(v); err != nil {
				t.Fatalf("premature terminal: %v", err)
			}
		})
	}
}

func TestImageOutputTakesPrecedenceOverFinalText(t *testing.T) {
	v := textOnlyConversation()
	mapping := v["mapping"].(map[string]any)
	mapping["output"] = map[string]any{"message": map[string]any{"author": map[string]any{"role": "tool"}, "content": map[string]any{"asset_pointer": "file-service://file_generated123456"}}}
	raw, _ := json.Marshal(v)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	ids, err := NewOpenAIImage("https://example.invalid", client, nil, time.Second).pollConversation(context.Background(), accounts.Account{}, "test")
	if err != nil || len(ids) != 1 {
		t.Fatalf("generated image discarded: %v %v", ids, err)
	}
}

func TestImageExplicitFailuresPreferExplanation(t *testing.T) {
	for _, marker := range []string{"blocked", "refusal", "finish-filter", "error-flag"} {
		v := textOnlyConversation()
		m := v["mapping"].(map[string]any)["reply"].(map[string]any)["message"].(map[string]any)
		m["content"] = map[string]any{"content_type": "text", "parts": []any{"图片内容未通过审核，请调整描述。"}}
		switch marker {
		case "blocked":
			m["status"] = "blocked"
		case "refusal":
			m["content"].(map[string]any)["content_type"] = "refusal"
		case "finish-filter":
			m["metadata"] = map[string]any{"finish_details": map[string]any{"type": "content_filter"}}
		case "error-flag":
			m["metadata"] = map[string]any{"is_error": true}
		}
		if err := openAIImageTerminalError(m); err == nil || !strings.Contains(err.Error(), "图片内容未通过审核") {
			t.Fatalf("%s lost reason: %v", marker, err)
		}
	}
}

func TestStructuredImageFailurePreservesReasonBeforeStatus(t *testing.T) {
	for _, value := range []any{
		map[string]any{"status": "failed", "error": map[string]any{"message": "图片生成额度已用完"}},
		map[string]any{"state": "blocked", "message": "图片内容未通过审核"},
	} {
		for i := 0; i < 100; i++ {
			err := openAIImageTerminalError(value)
			if err == nil || (!strings.Contains(err.Error(), "额度") && !strings.Contains(err.Error(), "审核")) {
				t.Fatalf("business explanation lost: %v", err)
			}
		}
	}
}
