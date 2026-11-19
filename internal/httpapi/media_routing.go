package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// Only internal task checkpoints contain routing. Callers select logical models.
type mediaRoute struct {
	Channel       string `json:"channel"`
	AccountID     string `json:"account_id"`
	UpstreamModel string `json:"upstream_model"`
	Stage         string `json:"stage"`
	JobKey        string `json:"job_key,omitempty"`
}
type mediaTaskContextKey struct{}

func imageTaskFromContext(ctx context.Context) *imageTaskState {
	task, _ := ctx.Value(mediaTaskContextKey{}).(*imageTaskState)
	return task
}
func (s *Server) pinImageRoute(ctx context.Context, route mediaRoute) error {
	task := imageTaskFromContext(ctx)
	if task == nil {
		return nil
	}
	s.imageTaskMu.Lock()
	defer s.imageTaskMu.Unlock()
	if task.Route != nil {
		return errors.New("media route already selected; automatic resubmission disabled")
	}
	task.Route = &route
	return s.saveImageTaskLocked(task)
}
func (s *Server) imageRouteStage(ctx context.Context, stage string) error {
	task := imageTaskFromContext(ctx)
	if task == nil {
		return nil
	}
	s.imageTaskMu.Lock()
	defer s.imageTaskMu.Unlock()
	if task.Route == nil {
		return errors.New("missing media route")
	}
	task.Route.Stage = stage
	return s.saveImageTaskLocked(task)
}
func imageExecutionKey(ctx context.Context) string {
	if task := imageTaskFromContext(ctx); task != nil {
		return "image-" + imageTaskKey(task.OwnerID, task.ID)
	}
	return "image-sync-" + randomID()
}
func adobeImageFits(id, size, quality string, count, refs int, options provider.ImageOutputOptions, masked bool) bool {
	m, ok := model.AdobeModel(id)
	if !ok || m.Kind != "image" {
		return false
	}
	// auto is an explicit model default, never a silently downgraded exact size.
	width, height, validSize := adobeImageSize(id, size)
	_, fits := m.MatchImage(width, height, count, refs)
	return validSize && fits && !masked &&
		(quality == "" || quality == "auto" || quality == "standard") &&
		(options.Background == "" || options.Background == "auto") && options.EffectiveFormat() == "png" &&
		options.FrameCount == nil && options.FrameDurationMS == nil && !options.Stream && options.PartialImages == nil && options.OutputCompression == nil
}

type imageSelection struct {
	Channel string
	Lease   *accounts.Lease
	Adobe   adobe.Account
	Key     string
	Context context.Context
}

type imageSelectionDiagnostics struct {
	Model                   string
	ConversationModel       string
	ChannelOrder            []string
	ChatGPT                 accounts.SelectionSummary
	ChatGPTObserved         bool
	AdobeCandidateCount     int
	AdobeRejectionReason    string
	AdobeParameterSupported bool
}

func (d imageSelectionDiagnostics) fields() map[string]any {
	channel := strings.Join(d.ChannelOrder, ",")
	if len(d.ChannelOrder) == 0 {
		channel = "none"
	}
	fields := map[string]any{
		"model":                 d.Model,
		"channel":               channel,
		"conversation_model":    d.ConversationModel,
		"channel_order":         append([]string(nil), d.ChannelOrder...),
		"adobe_candidate_count": d.AdobeCandidateCount,
	}
	if d.ChatGPTObserved {
		fields["chatgpt"] = d.ChatGPT.Fields()
	}
	if d.AdobeRejectionReason != "" {
		fields["adobe_rejection_reason"] = d.AdobeRejectionReason
	}
	if d.AdobeParameterSupported {
		fields["adobe_parameter_supported"] = true
	}
	return fields
}

func (s *Server) recordImageSelectionFailure(r *http.Request, diagnostics imageSelectionDiagnostics) {
	fields := diagnostics.fields()
	if r != nil {
		s.enrichRequestMonitor(r, map[string]any{"metrics": map[string]any{"image_account_selection": fields}})
	}
	raw, err := json.Marshal(fields)
	if err == nil {
		log.Printf("image account selection failed: %s", raw)
	}
}

// selectImageImplementation keeps the test and internal callers that do not
// have an HTTP request compatible. Request handlers use the request-aware form
// so selection diagnostics reach the call log.
func (s *Server) selectImageImplementation(ctx context.Context, id, conversation, size, quality string, count, refs int, options provider.ImageOutputOptions, masked bool, eligible func(accounts.Account) bool) (imageSelection, error) {
	return s.selectImageImplementationForRequest(nil, ctx, id, conversation, size, quality, count, refs, options, masked, eligible)
}

func (s *Server) selectImageImplementationForRequest(r *http.Request, ctx context.Context, id, conversation, size, quality string, count, refs int, options provider.ImageOutputOptions, masked bool, eligible func(accounts.Account) bool) (imageSelection, error) {
	order, err := s.channelOrder(id)
	if err != nil {
		return imageSelection{}, err
	}
	diagnostics := imageSelectionDiagnostics{Model: id, ConversationModel: conversation, ChannelOrder: order}
	fits := false
	for _, channel := range order {
		switch channel {
		case "adobe":
			if !adobeImageFits(id, size, quality, count, refs, options, masked) {
				if diagnostics.AdobeRejectionReason == "" {
					diagnostics.AdobeRejectionReason = "parameter_mismatch"
				}
				continue
			}
			fits = true
			diagnostics.AdobeParameterSupported = true
			if s.adobeStore == nil || s.adobeClient == nil {
				diagnostics.AdobeRejectionReason = "adobe_service_unavailable"
				continue
			}
			if err := s.ensureAdobeAccounts(ctx, id); err != nil {
				if errors.Is(err, adobe.ErrNoAccount) || errors.Is(err, adobe.ErrBusy) {
					diagnostics.AdobeRejectionReason = "no_adobe_candidate"
					continue
				}
				if errors.Is(err, adobe.ErrMediaSessionNotReady) {
					diagnostics.AdobeRejectionReason = "media_session_not_ready"
					continue
				}
				diagnostics.AdobeRejectionReason = adobe.SafeError(err)
				s.recordImageSelectionFailure(r, diagnostics)
				return imageSelection{}, adobeAPIError(err)
			}
			diagnostics.AdobeCandidateCount++
			key := imageExecutionKey(ctx)
			a, selectedContext, err := s.adobeStore.ReserveReadyMediaForModel(ctx, key, id, s.adobeAccountContext)
			if err != nil {
				if errors.Is(err, adobe.ErrNoAccount) || errors.Is(err, adobe.ErrBusy) {
					diagnostics.AdobeRejectionReason = "no_adobe_candidate"
					continue
				}
				if errors.Is(err, adobe.ErrMediaSessionNotReady) {
					diagnostics.AdobeRejectionReason = "media_session_not_ready"
					continue
				}
				diagnostics.AdobeRejectionReason = adobe.SafeError(err)
				s.recordImageSelectionFailure(r, diagnostics)
				return imageSelection{}, adobeAPIError(err)
			}
			m, _ := model.AdobeModel(id)
			route := mediaRoute{Channel: channel, AccountID: a.ID, UpstreamModel: m.ModelID + "/" + m.ModelVersion, Stage: "selected", JobKey: key}
			if err = s.pinImageRoute(ctx, route); err != nil {
				s.adobeStore.ReleaseUnsubmittedTask(key, a.ID)
				s.recordImageSelectionFailure(r, diagnostics)
				return imageSelection{}, err
			}
			return imageSelection{Channel: channel, Adobe: a, Key: key, Context: selectedContext}, nil
		case "chatgpt":
			if !isOpenAIImageModel(id) || !validOpenAIImageSize(size) || refs > 7 {
				continue
			}
			fits = true
			intent := accounts.Intent{Kind: "image", Model: conversation, ImageCount: count, Uploads: count * (refs + boolInt(masked))}
			l, summary, err := s.accountPool.ReserveIntentWithSummary(ctx, []string{"basic", "super", "heavy"}, nil, eligible, s.cfg.ImageAccountLimit, intent)
			diagnostics.ChatGPT, diagnostics.ChatGPTObserved = summary, true
			if err != nil {
				if errors.Is(err, accounts.ErrUnavailable) {
					continue
				}
				s.recordImageSelectionFailure(r, diagnostics)
				return imageSelection{}, err
			}
			if err = s.pinImageRoute(ctx, mediaRoute{Channel: channel, AccountID: accounts.Identity(l.Account), UpstreamModel: conversation, Stage: "selected"}); err != nil {
				s.accountPool.Release(l)
				s.recordImageSelectionFailure(r, diagnostics)
				return imageSelection{}, err
			}
			return imageSelection{Channel: channel, Lease: l}, nil
		}
	}
	if !fits {
		diagnostics.AdobeRejectionReason = firstNonEmpty(diagnostics.AdobeRejectionReason, "no_channel_supports_parameters")
		s.recordImageSelectionFailure(r, diagnostics)
		return imageSelection{}, &protocol.UpstreamError{Status: 400, Message: "no channel supports this complete model parameter combination"}
	}
	s.recordImageSelectionFailure(r, diagnostics)
	if diagnostics.AdobeRejectionReason == "media_session_not_ready" {
		return imageSelection{}, &protocol.UpstreamError{Status: http.StatusServiceUnavailable, Message: "Adobe media session is warming; retry later"}
	}
	return imageSelection{}, &protocol.UpstreamError{Status: 429, Message: "no eligible account with confirmed quota for this model"}
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func adobeAPIError(err error) error {
	var e *adobe.Error
	if errors.As(err, &e) {
		status := e.Status
		if e.Stage == "preflight" {
			switch e.Code {
			case "free_quota_exhausted":
				status = 429
			case "free_entitlement_unavailable":
				status = 403
			}
		}
		if status < 400 {
			status = 502
		}
		return &protocol.UpstreamError{Status: status, Message: adobe.SafeError(err)}
	}
	if errors.Is(err, adobe.ErrNoAccount) || errors.Is(err, adobe.ErrBusy) {
		return &protocol.UpstreamError{Status: 429, Message: adobe.SafeError(err)}
	}
	if errors.Is(err, adobe.ErrMediaSessionNotReady) {
		return &protocol.UpstreamError{Status: http.StatusServiceUnavailable, Message: adobe.SafeError(err)}
	}
	return &protocol.UpstreamError{Status: 502, Message: adobe.SafeError(err)}
}
