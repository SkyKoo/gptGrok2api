package httpapi

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func invalidMask(message string) error {
	return &provider.ImageOptionError{Param: "mask", Message: message}
}

func prepareImageMask(model string, raw []byte, inputs []provider.OpenAIImageInput) (*provider.ImageMask, error) {
	if raw == nil {
		return nil, nil
	}
	if !isOpenAIImageModel(model) {
		return nil, invalidMask("mask is only supported for GPT image models")
	}
	return provider.ParseImageMask(raw, inputs)
}

func (s *Server) parseMultipartImageMask(r *http.Request) ([]byte, error) {
	var raw []byte
	for _, field := range imageEditMaskFields {
		for _, header := range r.MultipartForm.File[field] {
			if raw != nil {
				return nil, invalidMask("only one mask is supported")
			}
			if header.Size > provider.MaxImageMaskBytes {
				return nil, invalidMask("mask exceeds 4 MiB")
			}
			file, err := header.Open()
			if err != nil {
				return nil, invalidMask("invalid mask upload")
			}
			raw, err = io.ReadAll(io.LimitReader(file, provider.MaxImageMaskBytes+1))
			_ = file.Close()
			if err != nil || len(raw) == 0 || len(raw) > provider.MaxImageMaskBytes {
				return nil, invalidMask("mask must contain 1 byte to 4 MiB")
			}
		}
		for _, value := range r.MultipartForm.Value[field] {
			if raw != nil {
				return nil, invalidMask("only one mask is supported")
			}
			var err error
			raw, err = s.imageMaskFromValue(r.Context(), value)
			if err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}

// Resolve one mask without the reference parser's eager image decode. PNG
// dimensions and memory limits are checked before pixel decoding by the provider.
func (s *Server) imageMaskFromValue(ctx context.Context, value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	var text string
	switch v := value.(type) {
	case string:
		text = v
	case map[string]any:
		for _, key := range []string{"b64_json", "base64", "image_url", "url"} {
			if entry, exists := v[key]; exists {
				if text != "" {
					return nil, invalidMask("mask must contain a single image reference")
				}
				if nested, ok := entry.(map[string]any); ok && key == "image_url" {
					entry = nested["url"]
				}
				var ok bool
				text, ok = entry.(string)
				if !ok || strings.TrimSpace(text) == "" {
					return nil, invalidMask("invalid mask reference")
				}
			}
		}
	default:
		return nil, invalidMask("mask must be a PNG reference or null")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, invalidMask("mask must not be empty")
	}
	var raw []byte
	var err error
	lower := strings.ToLower(text)
	switch {
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		downloadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		input, fetchErr := s.downloadImageInputWithLimit(downloadCtx, text, provider.MaxImageMaskBytes)
		if fetchErr != nil {
			return nil, invalidMask("invalid or inaccessible mask image")
		}
		raw = input.Data
	default:
		encoded := text
		if strings.HasPrefix(lower, "data:") {
			header, body, ok := strings.Cut(text, ",")
			if !ok {
				return nil, invalidMask("invalid mask data URL")
			}
			if strings.Contains(strings.ToLower(header), ";base64") {
				encoded = body
			} else {
				if len(body) > provider.MaxImageMaskBytes*3 {
					return nil, invalidMask("mask exceeds 4 MiB")
				}
				decoded, decodeErr := url.PathUnescape(body)
				raw, err = []byte(decoded), decodeErr
				encoded = ""
			}
		}
		if encoded != "" {
			if len(encoded) > base64.StdEncoding.EncodedLen(provider.MaxImageMaskBytes)+4096 {
				return nil, invalidMask("mask exceeds 4 MiB")
			}
			encoded = strings.ReplaceAll(strings.ReplaceAll(encoded, "\r", ""), "\n", "")
			raw, err = base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(encoded, "="))
			}
		}
	}
	if err != nil || len(raw) == 0 {
		return nil, invalidMask("invalid mask image")
	}
	if len(raw) > provider.MaxImageMaskBytes {
		return nil, invalidMask("mask exceeds 4 MiB")
	}
	return raw, nil
}
