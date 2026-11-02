package httpapi

import (
	"bytes"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"image"
	"io"
	"net/http"
	"net/url"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

const maxVideoReferences = 7
const maxVideoReferenceBytes = 16 << 20

// Validate every attachment before queuing; multipart files are closed and
// copied so the worker never depends on request lifetime or temporary files.
func videoReferenceInputs(r *http.Request) ([]provider.OpenAIImageInput, error) {
	var inputs []provider.OpenAIImageInput
	if r.MultipartForm == nil {
		return nil, nil
	}
	count := 0
	for key, files := range r.MultipartForm.File {
		if key != "input_reference" && key != "input_reference[]" && len(files) > 0 {
			return nil, fmt.Errorf("视频任务只支持 input_reference 图片附件，不支持外部参考视频或其他文件字段")
		}
		count += len(files)
	}
	if count > maxVideoReferences {
		return nil, fmt.Errorf("视频参考图最多 %d 张", maxVideoReferences)
	}
	for _, key := range []string{"input_reference[]", "input_reference"} {
		for _, header := range r.MultipartForm.File[key] {
			if header.Size > maxVideoReferenceBytes {
				return nil, fmt.Errorf("每张视频参考图不能超过 16 MiB")
			}
			file, err := header.Open()
			if err != nil {
				return nil, fmt.Errorf("无法读取第 %d 张视频参考图", len(inputs)+1)
			}
			raw, readErr := io.ReadAll(io.LimitReader(file, maxVideoReferenceBytes+1))
			_ = file.Close()
			if readErr != nil || len(raw) == 0 {
				return nil, fmt.Errorf("第 %d 张视频参考图为空或读取失败", len(inputs)+1)
			}
			if len(raw) > maxVideoReferenceBytes {
				return nil, fmt.Errorf("每张视频参考图不能超过 16 MiB")
			}
			_, format, err := image.DecodeConfig(bytes.NewReader(raw))
			mime := map[string]string{"jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp", "bmp": "image/bmp", "tiff": "image/tiff"}[format]
			if err != nil || mime == "" {
				return nil, fmt.Errorf("第 %d 个附件不是有效图片；当前 Grok Web 通道不支持外部参考视频", len(inputs)+1)
			}
			inputs = append(inputs, provider.OpenAIImageInput{Name: safeImageInputName(header.Filename, mime, "reference.png"), MIME: mime, Data: raw})
		}
	}
	return inputs, nil
}

func videoReferenceURL(fileID, fileURI, userID string) string {
	ref := protocol.ResolveAssetReference(strings.TrimSpace(fileID), strings.TrimSpace(fileURI), userID)
	parsed, err := url.Parse(ref)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Path == "" || parsed.Path == "/" {
		return ""
	}
	return ref
}
