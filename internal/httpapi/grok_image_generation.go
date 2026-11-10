package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

type grokImageGenerator func(context.Context, *accounts.Lease, int) ([]provider.ImageResult, error)

// Only called after both handlers have returned from their GPT branches.
// Keep successful members private until all outputs pass review; never regenerate.
func (s *Server) generateReviewedGrokImages(r *http.Request, prompt, model, size, format string, n int, inputs []provider.OpenAIImageInput, generate grokImageGenerator) ([]map[string]string, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	settings, err := s.store.Config()
	if err != nil {
		return nil, &protocol.UpstreamError{Status: 503, Message: "无法读取 Grok 内容核对设置，本次未提交生成"}
	}
	enabled, _ := settings["video_review_enabled"].(bool)
	s.enrichRequestMonitor(r, map[string]any{"metrics": map[string]any{"grok_media_review_enabled": enabled}})
	callID, _ := r.Context().Value(monitorCallIDKey{}).(string)
	requestHash := videoRequestFingerprint(model, prompt, size, "", 0, "", inputs)
	accepted := make([]provider.ImageResult, n)
	pending := make([]int, n)
	for i := range pending {
		pending[i] = i
	}
	for attempt := 1; attempt <= grokMediaGenerationAttempts; attempt++ {
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		s.stageRequestMonitor(r, "image_egress_waiting", 30, nil)
		accountStarted := time.Now()
		intent := accounts.Intent{Kind: "image", Model: model, ImageCount: len(pending), Uploads: len(inputs)}
		s.ensureGrokMediaQuotas(r.Context(), intent)
		lease, err := s.grokAccountPool.ReserveIntent(r.Context(), []string{"basic", "super", "heavy"}, nil, nil, 0, intent)
		if err != nil {
			return nil, &protocol.UpstreamError{Status: 429, Message: err.Error()}
		}
		if err := s.pinImageRoute(r.Context(), mediaRoute{Channel: "grok", AccountID: accounts.Identity(lease.Account), UpstreamModel: model, Stage: "selected"}); err != nil {
			s.grokAccountPool.Release(lease)
			return nil, err
		}
		defer s.grokAccountPool.Release(lease) // Release is idempotent; also cover panic/cancellation paths.
		s.enrichMonitorAccount(r, lease.Account)
		s.stageRequestMonitor(r, "image_getting_account", 35, map[string]any{"account_wait_ms": time.Since(accountStarted).Milliseconds()})
		s.stageRequestMonitor(r, "image_generating", 35, map[string]any{"grok_generation_attempt": attempt, "grok_requested_outputs": len(pending)})
		started := time.Now()
		images, generateErr := generate(r.Context(), lease, len(pending))
		if generateErr != nil {
			s.grokAccountPool.FeedbackIntent(lease, upstreamStatus(generateErr), generateErr)
			s.grokAccountPool.Release(lease)
			return nil, generateErr
		}
		s.stageRequestMonitor(r, "image_generating", 70, map[string]any{"conversation_stream_ms": time.Since(started).Milliseconds()})
		if len(images) < len(pending) {
			s.grokAccountPool.FeedbackIntent(lease, http.StatusOK, nil)
			s.grokAccountPool.Release(lease)
			return nil, &protocol.UpstreamError{Status: 502, Message: "Grok 返回的图片数量不足，未交付不完整结果"}
		}
		// Resolve into memory only; failed content must never enter public files/gallery.
		downloadStarted := time.Now()
		for i := range pending {
			resolved, resolveErr := s.mediaProvider.ResolveImage(r.Context(), lease.Account, images[i], "b64_json", "", "")
			if resolveErr != nil {
				s.grokAccountPool.FeedbackIntent(lease, upstreamStatus(resolveErr), resolveErr)
				s.grokAccountPool.Release(lease)
				return nil, resolveErr
			}
			// Clear the URL and ImagineFinal so final validated bytes are not fetched again.
			images[i] = provider.ImageResult{Base64: resolved["b64_json"], MIME: resolved["mime"]}
		}
		s.grokAccountPool.FeedbackIntent(lease, http.StatusOK, nil)
		s.grokAccountPool.Release(lease)
		s.stageRequestMonitor(r, "image_download_done", 80, map[string]any{"download_ms": time.Since(downloadStarted).Milliseconds()})
		retry := []int{}
		for i, index := range pending {
			if enabled {
				raw, err := base64.StdEncoding.DecodeString(images[i].Base64)
				if err != nil {
					return nil, err
				}
				hash, err := grokImageFingerprint(raw)
				if err != nil {
					return nil, err
				}
				s.stageRequestMonitor(r, "grok_media_content_review", 85, map[string]any{"grok_generation_attempt": attempt, "grok_review_output": index + 1})
				reviewStarted := time.Now()
				checked := s.reviewGrokMedia(r.Context(), callID, "image", requestHash, hash, prompt, inputs, raw, 0, attempt, index+1)
				s.stageRequestMonitor(r, "grok_media_content_reviewed", 90, map[string]any{"grok_media_review_ms": time.Since(reviewStarted).Milliseconds()})
				switch checked.Verdict {
				case "matched":
				case "mismatch":
					retry = append(retry, index)
					continue
				default:
					return nil, &protocol.UpstreamError{Status: 502, Message: "image_content_unverified: Grok 图片核对未能完成，已记录核对故障；未判定为占位图片"}
				}
			}
			accepted[index] = images[i]
		}
		if len(retry) == 0 {
			break
		}
		if attempt == grokMediaGenerationAttempts {
			return nil, &protocol.UpstreamError{Status: 502, Message: "image_content_mismatch: Grok 图片明显无关，已停止交付并保存核对证据；未重新生成"}
		}
		pending = retry
		s.stageRequestMonitor(r, "grok_media_regenerating", 35, map[string]any{"grok_generation_retry": 1, "grok_retry_outputs": len(pending)})
	}
	data := make([]map[string]string, 0, n)
	for i, img := range accepted {
		// Inline images have no upstream URL; unique task/position seed avoids same-second collisions.
		img.URL = fmt.Sprintf("cfm:%s:%s:%d", callID, randomID(), i)
		value, err := s.mediaProvider.ResolveImage(r.Context(), accounts.Account{}, img, format, s.cfg.ImageDataDir, requestPublicBase(r))
		if err != nil {
			return nil, err
		}
		s.recordGeneratedMedia(r.Context(), value)
		data = append(data, value)
	}
	s.stageRequestMonitor(r, "image_single_done", 99, map[string]any{"total_ms": s.requestMonitorElapsed(r), "response_ms": s.requestMonitorElapsed(r)})
	return data, nil
}
