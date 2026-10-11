package provider

import (
	"fmt"
	"strings"
	"testing"
)

func TestImageGenerationRetryClassification(t *testing.T) {
	const generic = "We experienced an error when generating images."
	const full = generic + " Before doing anything else, please explicitly explain to the user that you were unable to generate images because of this. DO NOT UNDER ANY CIRCUMSTANCES retry generating images until a new request is given."
	for _, tc := range []struct {
		name, reason, marker, code string
		want                       bool
	}{
		{name: "generic", reason: generic, want: true},
		{name: "full-tool-message", reason: full, want: true},
		{name: "whitespace", reason: strings.ReplaceAll(full, " ", "\n"), want: true},
		{name: "policy", reason: "The image violates our content policy."},
		{name: "quota", reason: "Image generation quota exhausted."},
		{name: "missing-reference", reason: "Please upload the reference image."},
		{name: "unknown", reason: "Upstream image tool failed."},
		{name: "truncated", reason: generic + " Before doing anything else, please…"},
		{name: "extra-business-reason", reason: full + " This violates our policy."},
		{name: "structured-policy", reason: generic, marker: "content_filter"},
		{name: "structured-cancel", reason: generic, marker: "cancelled"},
		{name: "structured-quota", reason: generic, code: "quota_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := map[string]any{"author": map[string]any{"role": "tool"}, "metadata": map[string]any{"is_error": true, "finish_details": map[string]any{"type": tc.marker}}, "content": map[string]any{"content_type": "text", "parts": []any{tc.reason}}}
			if tc.code != "" {
				message["error"] = map[string]any{"message": tc.reason, "code": tc.code}
			}
			err := openAIImageTerminalError(message)
			if err == nil || !IsImageTerminalError(err) {
				t.Fatal("expected terminal error")
			}
			if got := IsRetryableImageGenerationError(fmt.Errorf("wrapped: %w", err)); got != tc.want {
				t.Fatalf("retryable=%v want %v", got, tc.want)
			}
		})
	}
	if IsRetryableImageGenerationError(fmt.Errorf("%s", generic)) || IsRetryableImageGenerationError(nil) {
		t.Fatal("untyped error became retryable")
	}
	if err := openAIImageTerminalError(map[string]any{"author": map[string]any{"role": "user"}, "error": generic}); err != nil {
		t.Fatal("user prompt treated as upstream failure")
	}
}
