package provider

import (
	"net/http"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// Match explicit upstream moderation signals, not an arbitrary mention of moderation.
func isMediaModerationError(message string) bool {
	text := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(text, "content moderated") ||
		strings.Contains(text, "content-moderated") ||
		strings.Contains(text, "content_moderated") ||
		strings.Contains(text, "moderation_blocked")
}

func imagineModerationError() error {
	return &protocol.UpstreamError{
		Status:   http.StatusUnprocessableEntity,
		Message:  "Grok 图片被上游内容审核拦截（image_gen）",
		Endpoint: "/ws/imagine/listen",
	}
}

// JSON protocols may include an unset error field on successful progress events.
func hasMediaError(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return strings.TrimSpace(typed) != ""
	case float64:
		return typed != 0
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return true
	}
}
