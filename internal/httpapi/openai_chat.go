package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// runOpenAIChat owns the account lease for upload and conversation together.
// A retry is safe only before client-visible text has been emitted.
func (s *Server) runOpenAIChat(r *http.Request, request protocol.ChatRequest, route model.ChatRoute, images []provider.OpenAIChatImage, onEvent func(provider.OpenAIChatEvent) error) (resultErr error) {
	defer func() {
		if resultErr != nil {
			detail := openAIChatErrorObject(resultErr)
			s.enrichRequestMonitor(r, map[string]any{"error_status": openAITextChatStatus(resultErr), "error_code": detail["code"]})
			if outcome, ok := r.Context().Value(monitorOutcomeKey{}).(*monitorOutcome); ok {
				outcome.errorText = stringValue(detail["message"])
			}
		}
	}()
	inputs := make([]provider.OpenAIImageInput, len(images))
	for i, image := range images {
		inputs[i] = image.Input
	}
	s.recordInputImages(r, inputs)
	excluded := map[string]bool{}
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := r.Context().Err(); err != nil {
			return err
		}
		lease, err := s.accountPool.ReserveIntent(r.Context(), route.PoolCandidates, excluded, isOpenAIAccount, 0, accounts.Intent{Kind: "chat", Model: request.Model, Uploads: len(images)})
		if err != nil {
			if lastErr != nil && errors.Is(err, accounts.ErrUnavailable) {
				return lastErr
			}
			return err
		}
		s.enrichMonitorAccount(r, lease.Account)
		s.enrichRequestMonitor(r, map[string]any{"upstream_attempts": attempt + 1})
		emitted, clientFailed := false, false
		err = s.openAIChat.Stream(s.quotaContext(r.Context(), lease), lease.Account, request, func(event provider.OpenAIChatEvent) error {
			if event.Text != "" {
				emitted = true
			}
			callbackErr := onEvent(event)
			clientFailed = callbackErr != nil
			return callbackErr
		}, images...)
		s.accountPool.Release(lease)
		if err == nil {
			s.enrichRequestMonitor(r, map[string]any{"upstream_status": http.StatusOK})
			s.accountPool.FeedbackIntent(lease, http.StatusOK, nil)
			return nil
		}
		if r.Context().Err() != nil || clientFailed {
			return err
		}
		s.enrichRequestMonitor(r, map[string]any{"upstream_status": upstreamStatus(err)})
		s.accountPool.FeedbackIntent(lease, upstreamStatus(err), err)
		excluded[lease.Account.Token] = true
		lastErr = err
		if emitted || !s.quotaRetrySafe(lease, err) || !s.shouldRetry(upstreamStatus(err), attempt) {
			return err
		}
	}
}

func (s *Server) completeOpenAIChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	request, images, err := s.prepareOpenAIChat(r.Context(), request)
	if err != nil {
		writeOpenAITextChatError(w, err)
		return
	}
	var text strings.Builder
	err = s.runOpenAIChat(r, request, route, images, func(event provider.OpenAIChatEvent) error { text.WriteString(event.Text); return nil })
	if err != nil {
		writeOpenAITextChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": newChatID(), "object": "chat.completion", "created": time.Now().Unix(), "model": request.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text.String()}, "finish_reason": "stop"}},
		// The Web stream does not supply official API token usage.
		"usage": nil,
	})
}

func setOpenAIStreamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func writeOpenAIStreamData(w http.ResponseWriter, event string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err = fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (s *Server) streamOpenAIChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	request, images, err := s.prepareOpenAIChat(r.Context(), request)
	if err != nil {
		writeOpenAITextChatError(w, err)
		return
	}
	setOpenAIStreamHeaders(w)
	id, created, emitted := newChatID(), time.Now().Unix(), false
	send := func(delta map[string]any, finish any) error {
		return writeOpenAIStreamData(w, "", map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	}
	err = s.runOpenAIChat(r, request, route, images, func(event provider.OpenAIChatEvent) error {
		if event.Text == "" {
			return nil
		}
		if !emitted {
			if err := send(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
				return err
			}
			emitted = true
		}
		return send(map[string]any{"content": event.Text}, nil)
	})
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		_ = writeOpenAIStreamData(w, "", map[string]any{"error": openAIChatErrorObject(err)})
	} else {
		_ = send(map[string]any{}, "stop")
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) completeOpenAIImageChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest) {
	started := time.Now()
	imageContext, cancelImageRequest := s.imageTaskContext(r.Context())
	defer cancelImageRequest()
	releaseSlot, err := s.acquireImageSlot(imageContext, r)
	if err != nil {
		writeOpenAIChatError(w, err)
		return
	}
	defer releaseSlot()
	imageContext = s.monitorOpenAIImageContext(r, imageContext)
	s.enrichRequestMonitor(r, map[string]any{"model": request.Model})
	prompt := protocol.ExtractMessage(request.Messages)
	if strings.TrimSpace(prompt) == "" {
		writeError(w, http.StatusBadRequest, "messages contain no text", "invalid_request_error")
		return
	}
	excluded := map[string]bool{}
	var images []provider.ImageResult
	var selected accounts.Account
	var lastErr error
	inputStarted := time.Now()
	inputs := make([]provider.OpenAIImageInput, 0)
	for _, message := range request.Messages {
		// Only client-provided user messages may contribute reference images.
		// Assistant history can contain generated image URLs; treating those as
		// inputs causes subsequent /v1/chat/completions requests to feed the
		// previous output back downstream as a reference image.
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		parsed, err := s.imageInputsFromChatContent(r.Context(), message.Content)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		inputs = append(inputs, parsed...)
	}
	s.recordInputImages(r, inputs)
	s.stageRequestMonitor(r, "handler_queue_done", 10, nil)
	if len(inputs) > 0 {
		s.stageRequestMonitor(r, "image_uploading", 25, map[string]any{"upload_ms": time.Since(inputStarted).Milliseconds()})
	}
	size := strings.TrimSpace(request.Size)
	if size == "" {
		size = "1024x1024"
	}
	s.enrichRequestMonitor(r, map[string]any{"size": size, "image_url_parts": len(inputs), "data_url_images": len(inputs)})
	s.stageRequestMonitor(r, "image_egress_waiting", 30, map[string]any{"egress_wait_ms": 0})
	s.stageRequestMonitor(r, "image_getting_account", 35, nil)
	for attempt := 0; attempt <= s.cfg.ChatMaxRetries; attempt++ {
		accountStarted := time.Now()
		lease, err := s.accountPool.ReserveIntent(r.Context(), []string{"basic", "super", "heavy"}, excluded, isOpenAIAccount, s.cfg.ImageAccountLimit, accounts.Intent{Kind: "image", Model: request.Model, Uploads: len(inputs)})
		if err != nil {
			lastErr = err
			break
		}
		s.enrichMonitorAccount(r, lease.Account)
		s.stageRequestMonitor(r, "image_getting_account", 35, map[string]any{"account_wait_ms": time.Since(accountStarted).Milliseconds()})
		images, err = s.openAIImage.Generate(s.quotaContext(imageContext, lease), lease.Account, prompt, request.Model, size, "auto", inputs)
		selected = lease.Account
		s.accountPool.Release(lease)
		if err != nil {
			s.accountPool.FeedbackIntent(lease, upstreamStatus(err), err)
			excluded[lease.Account.Token] = true
			lastErr = err
			if s.quotaRetrySafe(lease, err) && s.shouldRetry(upstreamStatus(err), attempt) {
				continue
			}
			break
		}
		s.accountPool.FeedbackIntent(lease, http.StatusOK, nil)
		lastErr = nil
		break
	}
	s.stageRequestMonitor(r, "image_stream_resolve_start", 80, nil)
	if lastErr != nil {
		writeOpenAIChatError(w, lastErr)
		return
	}
	parts := make([]string, 0, len(images))
	for _, image := range images {
		item, localURL, resolveErr := s.openAIImage.Resolve(r.Context(), selected, image, "url", s.cfg.ImageDataDir, requestPublicBase(r))
		if resolveErr != nil {
			s.accountPool.Feedback(selected, upstreamStatus(resolveErr), resolveErr)
			writeError(w, upstreamStatus(resolveErr), resolveErr.Error(), "upstream_error")
			return
		}
		s.recordGeneratedMedia(r.Context(), map[string]string{"url": localURL})
		parts = append(parts, fmt.Sprintf("![image](%s)", item["url"]))
		// Chat image requests have no image-count parameter and return one
		// assistant image. Do not persist extra upstream references.
		break
	}
	s.stageRequestMonitor(r, "image_download_done", 95, map[string]any{"total_ms": time.Since(started).Milliseconds()})
	s.accountPool.Feedback(selected, http.StatusOK, nil)
	content := strings.Join(parts, "\n")
	writeJSON(w, http.StatusOK, map[string]any{
		"id": newChatID(), "object": "chat.completion", "created": time.Now().Unix(), "model": request.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   usageFor(prompt, content, ""),
	})
}

// Chat content mixes prompt text and image blocks. Only image-bearing blocks
// should be decoded; a plain prompt such as "hi" is not image data.
func (s *Server) imageInputsFromChatContent(ctx context.Context, value any) ([]provider.OpenAIImageInput, error) {
	switch typed := value.(type) {
	case nil, string:
		return nil, nil
	case []any:
		inputs := make([]provider.OpenAIImageInput, 0)
		for _, item := range typed {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			kind := strings.ToLower(strings.TrimSpace(stringValue(object["type"])))
			if kind != "image_url" && kind != "input_image" && kind != "image" {
				continue
			}
			parsed, err := s.imageInputsFromValue(ctx, object)
			if err != nil {
				return nil, err
			}
			inputs = append(inputs, parsed...)
		}
		return inputs, nil
	case map[string]any:
		kind := strings.ToLower(strings.TrimSpace(stringValue(typed["type"])))
		if kind == "image_url" || kind == "input_image" || kind == "image" {
			return s.imageInputsFromValue(ctx, typed)
		}
		return nil, nil
	default:
		return nil, nil
	}
}

func (s *Server) streamOpenAIImageChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest) {
	recorder := &responseCapture{header: make(http.Header)}
	request.Stream = false
	s.completeOpenAIImageChat(recorder, r, request)
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.body.Bytes(), &response); err != nil {
		writeError(w, http.StatusBadGateway, "invalid internal image response", "server_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writeSSE(w, map[string]any{"id": response["id"], "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": responseContent(response)}}}})
	writeSSE(w, map[string]any{"id": response["id"], "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func responseContent(response map[string]any) string {
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return stringValue(message["content"])
}

func openAIChatErrorObject(err error) map[string]any {
	// Account credentials belong to the gateway; do not report them as a bad
	// caller API key or echo upstream credential details to the caller.
	if upstreamStatus(err) == http.StatusUnauthorized || upstreamStatus(err) == http.StatusForbidden {
		return map[string]any{"message": "ChatGPT upstream account authentication or access failed; check the CFM account pool.", "type": "server_error", "param": nil, "code": "upstream_authentication_error"}
	}
	kind, code := "server_error", "server_error"
	if errors.Is(err, accounts.ErrUnavailable) || upstreamStatus(err) == http.StatusTooManyRequests {
		kind, code = "rate_limit_error", "rate_limit_exceeded"
	}
	return map[string]any{"message": err.Error(), "type": kind, "param": nil, "code": code}
}

func openAITextChatStatus(err error) int {
	if errors.Is(err, accounts.ErrUnavailable) {
		return http.StatusTooManyRequests
	}
	status := upstreamStatus(err)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return http.StatusBadGateway
	}
	return status
}

func writeOpenAITextChatError(w http.ResponseWriter, err error) {
	var input *protocol.InputError
	if errors.As(err, &input) {
		writeOpenAIInputError(w, input)
		return
	}
	writeJSON(w, openAITextChatStatus(err), map[string]any{"error": openAIChatErrorObject(err)})
}

// Preserve the existing error contract of the image handlers.
func writeOpenAIChatError(w http.ResponseWriter, err error) {
	status := upstreamStatus(err)
	if errors.Is(err, accounts.ErrUnavailable) {
		status = http.StatusTooManyRequests
	}
	writeError(w, status, err.Error(), "upstream_error")
}
