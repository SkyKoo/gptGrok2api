package provider

import (
	"errors"
	"strings"
)

// IsRetryableImageGenerationError identifies a confirmed failed generation,
// never an ambiguous transport timeout or a failed download of an existing image.
func IsRetryableImageGenerationError(err error) bool {
	var terminal *imageTerminalError
	return errors.As(err, &terminal) && terminal.retryable
}

func retryableImageToolFailure(value any, reason string) bool {
	// Match the complete known tool message, not the truncated public summary.
	// Additional business explanations (quota, policy, missing input, etc.) must
	// not be mistaken for this generic transient failure.
	const generic = "we experienced an error when generating images."
	const explanation = " before doing anything else, please explicitly explain to the user that you were unable to generate images because of this."
	const suffix = " do not under any circumstances retry generating images until a new request is given."
	text := strings.ToLower(strings.Join(strings.Fields(reason), " "))
	if text != generic && text != generic+explanation && text != generic+explanation+suffix {
		return false
	}
	return !imageRetryBlocked(value)
}

// A structured refusal/quota/cancellation takes precedence over generic text.
func imageRetryBlocked(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if author, ok := v["author"].(map[string]any); ok {
			switch strings.ToLower(stringValue(author["role"])) {
			case "user", "system", "developer":
				return false
			}
		}
		for key, item := range v {
			switch strings.ToLower(key) {
			case "status", "state", "task_status", "generation_status", "content_type", "finish_type", "finish_reason", "code", "type":
				marker := strings.ToLower(strings.TrimSpace(stringValue(item)))
				switch marker {
				case "cancelled", "canceled", "blocked", "moderated", "refused", "rejected", "refusal", "content_filter", "safety", "moderation":
					return true
				}
				// Do not assume an unfamiliar structured error code is transient.
				if strings.EqualFold(key, "code") && marker != "" {
					return true
				}
			}
			if imageRetryBlocked(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if imageRetryBlocked(item) {
				return true
			}
		}
	}
	return false
}
