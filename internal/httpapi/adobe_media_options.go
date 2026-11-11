package httpapi

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func adobeImageSize(id, size string) (int, int, bool) {
	m, ok := adobe.FindModel(id)
	if !ok {
		return 0, 0, false
	}
	size = strings.TrimSpace(size)
	if size == "" || size == "auto" {
		size = m.Size
	}
	return adobePixelSize(size)
}

func adobePixelSize(size string) (int, int, bool) {
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	width, e1 := strconv.Atoi(parts[0])
	height, e2 := strconv.Atoi(parts[1])
	return width, height, e1 == nil && e2 == nil && width > 0 && height > 0 && fmt.Sprintf("%dx%d", width, height) == size
}

func adobeReferenceImages(inputs []provider.OpenAIImageInput) []adobe.ReferenceImage {
	refs := make([]adobe.ReferenceImage, len(inputs))
	for i, input := range inputs {
		mime := input.MIME
		if mime == "" || mime == "application/octet-stream" {
			mime = http.DetectContentType(input.Data)
		}
		refs[i] = adobe.ReferenceImage{Data: input.Data, MIME: mime}
	}
	return refs
}

func adobeImageRequest(id, prompt, size string, count int, inputs []provider.OpenAIImageInput) (adobe.ImageRequest, error) {
	width, height, ok := adobeImageSize(id, size)
	if !ok {
		return adobe.ImageRequest{}, fmt.Errorf("unsupported image size for this model")
	}
	request := adobe.ImageRequest{Model: id, Prompt: prompt, Width: width, Height: height, N: count, References: adobeReferenceImages(inputs)}
	if _, _, err := adobe.ValidateImageRequest(request); err != nil {
		return request, fmt.Errorf("%s", adobe.SafeError(err))
	}
	return request, nil
}

func adobeVideoRequest(id, prompt string, options videoOptions, inputs []provider.OpenAIImageInput) (adobe.VideoRequest, error) {
	width, height, ok := adobePixelSize(options.Size)
	if !ok {
		return adobe.VideoRequest{}, fmt.Errorf("unsupported video size for this model")
	}
	request := adobe.VideoRequest{Model: id, Prompt: prompt, Width: width, Height: height, Duration: options.Seconds, Resolution: options.Resolution, AspectRatio: options.Aspect, GenerateAudio: options.GenerateAudio, References: adobeReferenceImages(inputs)}
	if _, _, err := adobe.ValidateVideoRequest(request); err != nil {
		return request, fmt.Errorf("%s", adobe.SafeError(err))
	}
	return request, nil
}

// Only static outputs belong to these image routes. Inspect structured container
// flags rather than matching arbitrary bytes inside compressed pixel data.
func adobeOutputAnimated(raw []byte, kind string) bool {
	if kind == "webp" {
		return len(raw) >= 21 && string(raw[12:16]) == "VP8X" && raw[20]&2 != 0
	}
	if kind == "png" {
		for offset := 8; offset+12 <= len(raw); {
			length := uint64(binary.BigEndian.Uint32(raw[offset : offset+4]))
			if length > uint64(len(raw)-offset-12) {
				return false
			}
			if string(raw[offset+4:offset+8]) == "acTL" {
				return true
			}
			offset += int(length) + 12
		}
	}
	return false
}
