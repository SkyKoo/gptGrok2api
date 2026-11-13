package xaiquality

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func responseBody(answer, model, status string) []byte {
	raw, _ := json.Marshal(map[string]any{"model": model, "status": status, "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": answer}}}}, "usage": map[string]any{"output_tokens": 32, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}})
	return raw
}

func TestScoreRequiresCompletedRequestedModel(t *testing.T) {
	for _, tc := range []struct {
		name, answer, model, status, want string
		http                              int
	}{
		{"correct", `{"answer":53,"marker":"nonce"}`, Model, "completed", "pass", 200},
		{"incorrect", `{"answer":54,"marker":"nonce"}`, Model, "completed", "fail", 200},
		{"wrong marker", `{"answer":53,"marker":"fixed"}`, Model, "completed", "fail", 200},
		{"missing answer", `{"marker":"nonce"}`, Model, "completed", "fail", 200},
		{"no body", "", Model, "completed", "error", 200},
		{"refusal", "I cannot help", Model, "completed", "fail", 200},
		{"truncated", `{"answer":`, Model, "incomplete", "error", 200},
		{"different model", `{"answer":53,"marker":"nonce"}`, "grok-4.5", "completed", "error", 200},
		{"no model", `{"answer":53,"marker":"nonce"}`, "", "completed", "error", 200},
		{"credit exhaustion", "", Model, "completed", "error", 402},
		{"rate limit", "", Model, "completed", "error", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Sample{Expected: 53, Marker: "nonce", Status: "error"}
			score(&s, Response{Status: tc.http, Body: responseBody(tc.answer, tc.model, tc.status)})
			if s.Status != tc.want {
				t.Fatalf("got %s want %s", s.Status, tc.want)
			}
		})
	}
}

func TestScoreSSECompletionAndUsage(t *testing.T) {
	raw := responseBody(`{"answer":53,"marker":"nonce"}`, Model, "completed")
	for _, body := range []string{"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + string(raw) + "}\n\ndata: [DONE]\n", string(raw)} {
		s := Sample{Expected: 53, Marker: "nonce", Status: "error"}
		score(&s, Response{Status: 200, Body: []byte(body)})
		if s.Status != "pass" || s.ReasoningTokens == nil || *s.ReasoningTokens != 0 {
			t.Fatalf("zero reasoning must not be convicted: %+v", s)
		}
	}
}

func TestRepeatedFailureAndErrorNeverClearRisk(t *testing.T) {
	calls := 0
	report := Run(context.Background(), func(_ context.Context, payload []byte) (Response, error) {
		calls++
		var data map[string]any
		json.Unmarshal(payload, &data)
		if data["model"] != Model {
			t.Error("wrong model")
		}
		return Response{200, responseBody(`{"answer":-1,"marker":"wrong"}`, Model, "completed")}, nil
	}, nil)
	if calls != 6 || report.Status != "high_risk" || !report.Blocked || report.ConfirmedAt == 0 || len(report.RiskSamples) != 6 {
		t.Fatalf("missing independent confirmation: %+v", report)
	}
	failed := Run(context.Background(), func(context.Context, []byte) (Response, error) { return Response{Status: 401}, nil }, &report)
	if failed.Status != "error" || !failed.Blocked || failed.ConfirmedAt != report.ConfirmedAt || len(failed.RiskSamples) != 6 {
		t.Fatal("authentication failure erased quality evidence")
	}
	if Passed(&failed, time.Now()) {
		t.Fatal("error admitted")
	}
}

func TestChallengesUseFreshMarkers(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		s := challenge(i)
		if seen[s.Marker] || !strings.Contains(s.Prompt, s.Marker) {
			t.Fatal("challenge not unique")
		}
		seen[s.Marker] = true
		if s.Expected <= 0 {
			t.Fatal("invalid oracle")
		}
	}
}
