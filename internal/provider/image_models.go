package provider

import "strings"

// IsDirectOpenAIImageModel identifies explicit Web image routes. These names
// bypass the legacy chat-model mapping and need not appear in the chat picker.
func IsDirectOpenAIImageModel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "gpt-image-2.5-sunburst", "gpt-image-2.5-flare",
		"gpt-image-2.6-sunburst", "gpt-image-2.6-flare":
		return true
	}
	return false
}

// SupportsOpenAIImageGIF is independent of direct model routing. Static-image
// support alone does not establish that a Web model can return an animation.
func SupportsOpenAIImageGIF(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "gpt-image-2.6-sunburst", "gpt-image-2.6-flare":
		return true
	}
	return false
}
