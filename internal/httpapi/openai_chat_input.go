package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"net/http"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// Decode once before selecting an account. Only the bytes are reusable across
// retries; account-owned upload IDs are created inside each provider attempt.
func (s *Server) prepareOpenAIChat(ctx context.Context, request protocol.ChatRequest) (protocol.ChatRequest, []provider.OpenAIChatImage, error) {
	request, err := protocol.NormalizeOpenAIChat(request)
	if err != nil {
		return request, nil, err
	}
	images := []provider.OpenAIChatImage{}
	total := 0
	for mi, message := range request.Messages {
		for pi, raw := range message.Content.([]any) {
			part := raw.(map[string]any)
			if part["type"] != "image_url" {
				continue
			}
			param := fmt.Sprintf("messages[%d].content[%d].image_url", mi, pi)
			input, err := s.imageInputFromString(ctx, part["image_url"].(map[string]any)["url"].(string))
			if err != nil {
				if ctx.Err() != nil {
					return request, nil, ctx.Err()
				}
				// Fetch errors may contain signed URLs or data URLs. Never echo them.
				return request, nil, &protocol.InputError{Param: param, Message: "image could not be fetched or decoded; use valid image data or a reachable public HTTP(S) URL (maximum 50 MiB)"}
			}
			total += len(input.Data)
			if total > maxImageEditReferenceBytes {
				return request, nil, &protocol.InputError{Param: param, Message: "combined image data exceeds 50 MiB"}
			}
			cfg, format, err := image.DecodeConfig(bytes.NewReader(input.Data))
			if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
				return request, nil, &protocol.InputError{Param: param, Message: "invalid image or image exceeds 40 megapixels"}
			}
			switch format {
			case "png", "jpeg", "webp", "gif":
			default:
				return request, nil, &protocol.InputError{Param: param, Message: "supported image formats are PNG, JPEG, WebP and GIF"}
			}
			if _, _, err := image.Decode(bytes.NewReader(input.Data)); err != nil {
				return request, nil, &protocol.InputError{Param: param, Message: "image data is truncated or corrupt"}
			}
			input.MIME = "image/" + format
			input.Name = fmt.Sprintf("reference-%d.%s", len(images)+1, format)
			images = append(images, provider.OpenAIChatImage{MessageIndex: mi, PartIndex: pi, Input: input})
		}
	}
	return request, images, nil
}

func writeOpenAIInputError(w http.ResponseWriter, err *protocol.InputError) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Message, "type": "invalid_request_error", "param": err.Param, "code": "invalid_value"}})
}
