package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func openAIImagesResult(data []map[string]string, options provider.ImageOutputOptions) map[string]any {
	value := map[string]any{"created": time.Now().Unix(), "data": data}
	value["output_format"] = options.EffectiveFormat()
	if options.Background == "transparent" || options.Background == "opaque" {
		value["background"] = options.Background
	}
	return value
}

func writeImageOptionError(w http.ResponseWriter, err error) bool {
	var option *provider.ImageOptionError
	if !errors.As(err, &option) {
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": option.Message, "type": "invalid_request_error", "param": option.Param, "code": nil,
	}})
	return true
}

func validateImageOutput(w http.ResponseWriter, model string, options provider.ImageOutputOptions) bool {
	if err := options.Validate(); err != nil {
		writeImageOptionError(w, err)
		return false
	}
	if options.OutputFormat == "gif" && !provider.SupportsOpenAIImageGIF(model) {
		writeImageOptionError(w, &provider.ImageOptionError{Param: "output_format", Message: "GIF output requires gpt-image-2.6-sunburst or gpt-image-2.6-flare"})
		return false
	}
	if !isOpenAIImageModel(model) && (options.Background != "" || options.OutputFormat != "") {
		param := "background"
		if options.Background == "" {
			param = "output_format"
		}
		writeImageOptionError(w, &provider.ImageOptionError{Param: param, Message: param + " is only supported for GPT image models"})
		return false
	}
	return true
}

func parseImageOutputJSON(body map[string]any) (provider.ImageOutputOptions, error) {
	var options provider.ImageOutputOptions
	for _, field := range []struct {
		name string
		dst  *string
	}{{"background", &options.Background}, {"output_format", &options.OutputFormat}} {
		value := body[field.name]
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return options, &provider.ImageOptionError{Param: field.name, Message: field.name + " must be a string or null"}
		}
		*field.dst = text
	}
	for _, field := range []struct {
		name string
		dst  any
	}{{"frame_count", &options.FrameCount}, {"frame_duration_ms", &options.FrameDurationMS}, {"stream", &options.Stream}, {"partial_images", &options.PartialImages}, {"output_compression", &options.OutputCompression}} {
		if value, ok := body[field.name]; ok && value != nil {
			raw, err := json.Marshal(value)
			if err != nil || json.Unmarshal(raw, field.dst) != nil {
				return options, &provider.ImageOptionError{Param: field.name, Message: field.name + " has an invalid type"}
			}
		}
	}
	return options, options.Validate()
}

func validateImageEditOutput(w http.ResponseWriter, model string, options provider.ImageOutputOptions) bool {
	if !validateImageOutput(w, model, options) {
		return false
	}
	if options.OutputFormat == "gif" {
		writeImageOptionError(w, &provider.ImageOptionError{Param: "output_format", Message: "GIF output is supported for generation only, not image edits"})
		return false
	}
	return true
}
