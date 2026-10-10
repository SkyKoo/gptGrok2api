package httpapi

import (
	"bytes"
	"image"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// Keep the requested size distinct from the provider's legacy preset mapping.
func openAIImageRequestedSize(size string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	if size == "" {
		return "auto"
	}
	return size
}

type imagePixelSize struct {
	Index  int    `json:"index"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Format string `json:"format,omitempty"`
}

func decodeImagePixelSize(raw []byte, index int) imagePixelSize {
	value := imagePixelSize{Index: index}
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		value.Width, value.Height, value.Format = cfg.Width, cfg.Height, format
	}
	return value
}

func knownImagePixelSizes(values []imagePixelSize) []imagePixelSize {
	known := make([]imagePixelSize, 0, len(values))
	for _, value := range values {
		if value.Width > 0 && value.Height > 0 {
			known = append(known, value)
		}
	}
	return known
}

func imageSizeRequestMetadata(size string, inputs []provider.OpenAIImageInput) map[string]any {
	requested := openAIImageRequestedSize(size)
	upstream := provider.NormalizeOpenAIImageSize(requested)
	dimensions := make([]imagePixelSize, 0, len(inputs))
	for index, input := range inputs {
		dimensions = append(dimensions, decodeImagePixelSize(input.Data, index+1))
	}
	meta := map[string]any{
		"requested_size":               requested,
		"upstream_size_field_included": upstream != "auto",
		"size_prompt_appended":         upstream != "auto",
		"input_dimensions":             dimensions,
	}
	if upstream != "auto" {
		meta["upstream_size"] = upstream
	}
	return meta
}
