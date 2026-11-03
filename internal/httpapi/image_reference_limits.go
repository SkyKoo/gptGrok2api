package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Grok Imagine Web uses separate attachment limits for image editing and video.
const maxGrokImageReferences = 5

func validateGrokImageReferenceCount(model string, count int) error {
	if strings.HasPrefix(strings.TrimSpace(model), "grok-") && count > maxGrokImageReferences {
		return fmt.Errorf("Grok 生图最多支持 %d 张参考图，当前 %d 张，请减少参考图数量", maxGrokImageReferences, count)
	}
	return nil
}

// Count the formats accepted by imageInputsFromValue before downloading or
// decoding them. Invalid values are still rejected by the normal parser.
func imageReferenceValueCount(value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return 0
		}
		if strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{") {
			var nested any
			if json.Unmarshal([]byte(text), &nested) == nil {
				return imageReferenceValueCount(nested)
			}
		}
		return 1
	case []any:
		count := 0
		for _, item := range typed {
			count += imageReferenceValueCount(item)
		}
		return count
	case map[string]any:
		if firstNonEmpty(stringValue(typed["file_id"]), stringValue(typed["b64_json"]), stringValue(typed["base64"])) != "" {
			return 1
		}
		imageURL := typed["image_url"]
		if imageURL == nil {
			imageURL = typed["url"]
		}
		if nested, ok := imageURL.(map[string]any); ok {
			imageURL = nested["url"]
		}
		return imageReferenceValueCount(imageURL)
	default:
		return 1
	}
}
