package httpapi

import (
	"context"
	"errors"
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
	return (size == "" || size == "auto" || size == m.Size) && count == 1 && refs == 0 && !masked &&
		(quality == "" || quality == "auto" || quality == "standard") &&
		(options.Background == "" || options.Background == "auto") && options.EffectiveFormat() == "png" &&
		options.FrameCount == nil && options.FrameDurationMS == nil && !options.Stream && options.PartialImages == nil && options.OutputCompression == nil
}

type imageSelection struct {
	Channel string
	Lease   *accounts.Lease
	Adobe   adobe.Account
	Key     string
}

func (s *Server) selectImageImplementation(ctx context.Context, id, conversation, size, quality string, count, refs int, options provider.ImageOutputOptions, masked bool, eligible func(accounts.Account) bool) (imageSelection, error) {
	order, err := s.channelOrder(id)
	if err != nil {
		return imageSelection{}, err
	}
	fits := false
	for _, channel := range order {
		switch channel {
		case "adobe":
			if !adobeImageFits(id, size, quality, count, refs, options, masked) {
				continue
			}
			fits = true
			if s.adobeStore == nil || s.adobeClient == nil {
				continue
			}
			s.ensureAdobeAccounts(ctx)
			key := imageExecutionKey(ctx)
			a, err := s.adobeStore.Reserve(key)
			if err != nil {
				if errors.Is(err, adobe.ErrNoAccount) || errors.Is(err, adobe.ErrBusy) {
					continue
				}
				return imageSelection{}, err
			}
			m, _ := model.AdobeModel(id)
			route := mediaRoute{Channel: channel, AccountID: a.ID, UpstreamModel: m.ModelID + "/" + m.ModelVersion, Stage: "selected", JobKey: key}
			if err = s.pinImageRoute(ctx, route); err != nil {
				s.adobeStore.Release(key)
				return imageSelection{}, err
			}
			return imageSelection{Channel: channel, Adobe: a, Key: key}, nil
		case "chatgpt":
			if !isOpenAIImageModel(id) || !validOpenAIImageSize(size) || refs > 7 {
				continue
			}
			fits = true
			l, err := s.accountPool.ReserveIntent(ctx, []string{"basic", "super", "heavy"}, nil, eligible, s.cfg.ImageAccountLimit, accounts.Intent{Kind: "image", Model: conversation, ImageCount: count, Uploads: count * (refs + boolInt(masked))})
			if err != nil {
				if errors.Is(err, accounts.ErrUnavailable) {
					continue
				}
				return imageSelection{}, err
			}
			if err = s.pinImageRoute(ctx, mediaRoute{Channel: channel, AccountID: accounts.Identity(l.Account), UpstreamModel: conversation, Stage: "selected"}); err != nil {
				s.accountPool.Release(l)
				return imageSelection{}, err
			}
			return imageSelection{Channel: channel, Lease: l}, nil
		}
	}
	if !fits {
		return imageSelection{}, &protocol.UpstreamError{Status: 400, Message: "no channel supports this complete model parameter combination"}
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
		if status < 400 {
			status = 502
		}
		return &protocol.UpstreamError{Status: status, Message: adobe.SafeError(err)}
	}
	if errors.Is(err, adobe.ErrNoAccount) || errors.Is(err, adobe.ErrBusy) {
		return &protocol.UpstreamError{Status: 429, Message: adobe.SafeError(err)}
	}
	return &protocol.UpstreamError{Status: 502, Message: adobe.SafeError(err)}
}
