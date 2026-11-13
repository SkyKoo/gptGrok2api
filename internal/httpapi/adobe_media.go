package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"image"
	"net/http"
	"strings"
	"time"
)

func (s *Server) generateAdobeImageData(r *http.Request, ctx context.Context, selected imageSelection, prompt, id, size, format, publicBase string, count int, inputs []provider.OpenAIImageInput, options provider.ImageOutputOptions) (result []map[string]string, resultErr error) {
	if selected.Context != nil {
		if endpoint, fixed := proxyruntime.URLSelectionFromContext(selected.Context); fixed {
			ctx = proxyruntime.WithURL(ctx, endpoint)
		} else {
			s.adobeStore.ReleaseUnsubmittedTask(selected.Key, selected.Adobe.ID)
			return nil, adobeAPIError(&adobe.Error{Stage: "media_prepare", Code: "egress_required"})
		}
	}
	request, validationErr := adobeImageRequest(id, prompt, size, count, inputs)
	if validationErr != nil {
		s.adobeStore.ReleaseUnsubmittedTask(selected.Key, selected.Adobe.ID)
		return nil, &protocol.UpstreamError{Status: 400, Message: validationErr.Error()}
	}
	// Synchronous calls also need a durable owner-scoped recovery handle.
	if imageTaskFromContext(ctx) == nil {
		m, _ := model.AdobeModel(id)
		owner := "internal"
		if r != nil {
			owner = s.authIdentity(r)
		}
		mode := "generate"
		if len(inputs) > 0 {
			mode = "edit"
		}
		task := &imageTaskState{ImageOutputOptions: options, ID: selected.Key, OwnerID: owner, Status: "running", Mode: mode, Model: id, N: count, Size: fmt.Sprintf("%dx%d", request.Width, request.Height), CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339), recovering: true, Route: &mediaRoute{Channel: "adobe", AccountID: selected.Adobe.ID, UpstreamModel: m.ModelID + "/" + m.ModelVersion, JobKey: selected.Key, Stage: "selected"}}
		s.imageTaskMu.Lock()
		if s.imageTasks[imageTaskKey(owner, task.ID)] != nil {
			s.imageTaskMu.Unlock()
			return nil, &protocol.UpstreamError{Status: 409, Message: "media task already exists; automatic resubmission disabled"}
		}
		err := s.saveImageTaskLocked(task)
		if err == nil {
			s.imageTasks[imageTaskKey(owner, task.ID)] = task
		}
		s.imageTaskMu.Unlock()
		if err != nil {
			s.adobeStore.ReleaseUnsubmittedTask(selected.Key, selected.Adobe.ID)
			return nil, err
		}
		ctx = context.WithValue(ctx, mediaTaskContextKey{}, task)
		defer func() {
			if resultErr != nil {
				s.finishImageTaskError(task, resultErr.Error())
				message := fmt.Sprintf("%v; task_id=%s", resultErr, task.ID)
				if s.adobeRouteCanResume(task.Route, task.Model, "image") {
					message += fmt.Sprintf(" (resume existing task via /api/image-tasks/%s/resume-poll)", task.ID)
				}
				resultErr = &protocol.UpstreamError{Status: upstreamStatus(resultErr), Message: message}
			} else {
				s.imageTaskMu.Lock()
				task.Status = "success"
				task.Data = nil
				for _, item := range result {
					task.Data = append(task.Data, map[string]any{"url": item["url"], "mime": item["mime"], "output_format": item["output_format"]})
				}
				task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				s.persistFinishedImageTaskLocked(task)
				s.imageTaskMu.Unlock()
			}
			s.imageTaskMu.Lock()
			task.recovering = false
			s.imageTaskMu.Unlock()
		}()
	}

	var egressErr error
	ctx, egressErr = s.adobeAccountContext(ctx, selected.Adobe)
	if egressErr != nil {
		s.adobeStore.ReleaseUnsubmittedTask(selected.Key, selected.Adobe.ID)
		return nil, adobeAPIError(egressErr)
	}

	if format != "url" && format != "b64_json" {
		s.adobeStore.ReleaseUnsubmittedTask(selected.Key, selected.Adobe.ID)
		return nil, &protocol.UpstreamError{Status: 400, Message: "response_format must be url or b64_json"}
	}
	m, _ := model.AdobeModel(id)
	s.enrichRequestMonitor(r, map[string]any{"media_channel": "adobe", "upstream_model": m.ModelID + "/" + m.ModelVersion})
	if err := s.beginAdobeImagePreparation(ctx, selected); err != nil {
		return nil, err
	}
	s.stageRequestMonitor(r, "media_session_preparing", 15, nil)
	prepareStarted := time.Now()
	wasReady := s.adobeStore.MediaSessionReady(ctx, selected.Adobe, 0)
	fresh, err := s.adobeStore.PrepareMediaForTask(ctx, selected.Key, selected.Adobe.ID, selected.Adobe.Revision)
	s.enrichRequestMonitor(r, map[string]any{"metrics": map[string]any{"media_session_cache_hit": wasReady, "media_session_prepare_ms": time.Since(prepareStarted).Milliseconds()}})
	if err != nil {
		if diagnostic := adobe.MediaDiagnosticFromError(err); diagnostic != nil {
			s.enrichRequestMonitor(r, map[string]any{"metrics": map[string]any{"media_session_diagnostic": diagnostic}})
		}
		return nil, s.adobeMediaPreparationError(selected.Key, selected.Adobe.ID, err)
	}
	selected.Adobe = fresh
	if _, err := s.adobeClient.PreflightImage(ctx, selected.Adobe, request); err != nil {
		if diagnostic := adobe.PreflightDiagnosticFromError(err); diagnostic != nil {
			s.enrichRequestMonitor(r, map[string]any{"metrics": map[string]any{"adobe_preflight_diagnostic": diagnostic}})
		}
		s.invalidateRejectedAdobeMediaSession(selected.Adobe, err)
		s.adobeStore.Release(selected.Key)
		return nil, adobeAPIError(err)
	}
	if err := s.imageRouteStage(ctx, "submit_started"); err != nil {
		s.adobeStore.Release(selected.Key)
		return nil, err
	}
	s.stageRequestMonitor(r, "image_starting_generation", 35, nil)
	job, err := s.adobeClient.SubmitImage(ctx, selected.Adobe, request)
	if err != nil {
		s.adobeStore.InvalidateMediaSession(selected.Adobe.ID, selected.Adobe.Revision)
		if stateErr := s.adobeSubmissionFailed(selected.Key, job, err); stateErr != nil {
			return nil, &protocol.UpstreamError{Status: upstreamStatus(adobeAPIError(err)), Message: adobe.SafeError(err) + "; upstream request state could not be saved; automatic resubmission disabled"}
		}
		return nil, adobeAPIError(err)
	}
	if err = s.adobeStore.SaveJob(selected.Key, job); err != nil {
		s.adobeStore.MarkUnknown(selected.Key)
		return nil, fmt.Errorf("could not save upstream task handle; automatic resubmission disabled")
	}
	if err = s.imageRouteStage(ctx, "polling"); err != nil {
		return nil, err
	}
	s.stageRequestMonitor(r, "image_generating", 60, nil)
	return s.finishAdobeImage(ctx, selected.Adobe, selected.Key, job, format, publicBase)
}

func (s *Server) beginAdobeImagePreparation(ctx context.Context, selected imageSelection) error {
	task := imageTaskFromContext(ctx)
	if task == nil {
		return errors.New("media task missing")
	}
	s.imageTaskMu.Lock()
	defer s.imageTaskMu.Unlock()
	if task.Route == nil || task.Route.Channel != "adobe" || task.Route.AccountID != selected.Adobe.ID || task.Route.JobKey != selected.Key || task.Route.Stage != "selected" {
		return &protocol.UpstreamError{Status: 409, Message: "media task already started; automatic resubmission disabled"}
	}
	task.Route.Stage = "media_session_preparing"
	if err := s.saveImageTaskLocked(task); err != nil {
		return s.adobeMediaPreparationError(selected.Key, selected.Adobe.ID, err)
	}
	return nil
}

func (s *Server) adobeMediaPreparationError(key, accountID string, err error) error {
	message := "media_session_prepare_failed: " + adobe.SafeError(err)
	if releaseErr := s.adobeStore.ReleaseUnsubmittedTask(key, accountID); releaseErr != nil {
		message += "; reservation retained: " + adobe.SafeError(releaseErr)
	}
	return &protocol.UpstreamError{Status: upstreamStatus(adobeAPIError(err)), Message: message}
}

func (s *Server) invalidateRejectedAdobeMediaSession(a adobe.Account, err error) {
	var upstream *adobe.Error
	if errors.Is(err, adobe.ErrIdentity) || errors.As(err, &upstream) && (upstream.Status == http.StatusUnauthorized || upstream.Code == "authentication_error" || upstream.Stage == "session" || upstream.Code == "session_incomplete") {
		s.adobeStore.InvalidateMediaSession(a.ID, a.Revision)
	}
}

func (s *Server) adobeSubmissionFailed(key string, job adobe.Job, err error) error {
	// Even a rejected or ambiguous submission has a useful request ID. Persist
	// that evidence without inventing a poll URL or permitting another submit.
	var saveErr error
	if job.RequestID != "" {
		saveErr = s.adobeStore.SaveJob(key, job)
	}
	var upstream *adobe.Error
	if errors.As(err, &upstream) && !upstream.SubmissionUnknown {
		return errors.Join(saveErr, s.adobeStore.Release(key))
	}
	return errors.Join(saveErr, s.adobeStore.MarkUnknown(key))
}
func adobeTerminal(status string) bool {
	switch strings.ToUpper(status) {
	case "COMPLETED", "FAILED", "ERROR", "CANCELLED", "CANCELED", "REJECTED":
		return true
	}
	return false
}
func (s *Server) pollAdobeJob(ctx context.Context, a adobe.Account, key string, job adobe.Job, progress func(int)) (adobe.Job, error) {
	refreshed := false
	if !adobeTerminal(job.Status) && a.ExpiresAt <= time.Now().Add(time.Minute).Unix() {
		var err error
		a, err = s.adobeStore.RefreshTask(ctx, s.adobeClient, key, a.ID)
		if err != nil {
			return job, adobeAPIError(err)
		}
		refreshed = true
	}

	for {
		if adobeTerminal(job.Status) {
			if job.Status != "COMPLETED" {
				if err := s.adobeStore.Release(key); err != nil {
					return job, fmt.Errorf("could not release terminal Adobe task reservation")
				}
				return job, adobeAPIError(adobe.TerminalPollError(job))
			}
			return job, nil
		}
		next, err := s.adobeClient.Poll(ctx, a, job)
		if err != nil && !refreshed {
			var upstream *adobe.Error
			if errors.As(err, &upstream) && upstream.Status == 401 {
				refreshed = true
				fresh, refreshErr := s.adobeStore.RefreshTask(ctx, s.adobeClient, key, a.ID)
				if refreshErr != nil {
					return job, adobeAPIError(refreshErr)
				}
				a = fresh
				continue
			}
		}
		if err != nil {
			return job, adobeAPIError(err)
		}
		job = next
		if err = s.adobeStore.SaveJob(key, job); err != nil {
			return job, fmt.Errorf("could not persist upstream task status")
		}
		if progress != nil {
			progress(min(99, max(1, int(job.Progress))))
		}
		if adobeTerminal(job.Status) {
			continue
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return job, ctx.Err()
		case <-timer.C:
		}
	}
}
func (s *Server) finishAdobeImage(ctx context.Context, a adobe.Account, key string, job adobe.Job, format, publicBase string) ([]map[string]string, error) {
	var egressErr error
	ctx, egressErr = s.adobeAccountContext(ctx, a)
	if egressErr != nil {
		return nil, adobeAPIError(egressErr)
	}

	job, err := s.pollAdobeJob(ctx, a, key, job, nil)
	if err != nil {
		return nil, err
	}
	if len(job.OutputURLs) != 1 {
		return nil, fmt.Errorf("completed image task did not return exactly one output")
	}
	if err = s.imageRouteStage(ctx, "downloading"); err != nil {
		return nil, err
	}
	raw, _, err := s.adobeClient.Download(ctx, job.OutputURLs[0], 32<<20)
	if err != nil {
		return nil, adobeAPIError(err)
	}
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(raw))
	mime := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp"}[kind]
	if err != nil || mime == "" || http.DetectContentType(raw) != mime || adobeOutputAnimated(raw, kind) || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
		return nil, fmt.Errorf("invalid Adobe image output")
	}
	// New handles retain the chosen dimensions. Legacy handles only used the
	// original square tier, so their zero values resolve to that model default.
	width, height := job.Width, job.Height
	if width == 0 && height == 0 {
		width, height, _ = adobeImageSize(job.Model, "")
	}
	if cfg.Width != width || cfg.Height != height {
		return nil, fmt.Errorf("Adobe image dimensions do not match the selected Free tier")
	}
	if _, _, err = image.Decode(bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("incomplete Adobe image output")
	}
	if task := imageTaskFromContext(ctx); task != nil && task.OutputFormat != "" && task.OutputFormat != kind {
		return nil, fmt.Errorf("Adobe image encoding does not match the requested output_format")
	}
	value, err := s.mediaProvider.ResolveImage(ctx, accounts.Account{}, provider.ImageResult{URL: "cfm:" + key, Base64: base64.StdEncoding.EncodeToString(raw), MIME: mime}, "url", s.cfg.ImageDataDir, publicBase)
	if err != nil {
		return nil, err
	}
	value["mime"] = mime
	value["output_format"] = kind
	s.recordGeneratedMedia(ctx, value)
	if err = s.imageRouteStage(ctx, "completed"); err != nil {
		return nil, err
	}
	s.adobeStore.Release(key)
	if format == "b64_json" {
		value["b64_json"] = base64.StdEncoding.EncodeToString(raw)
		value["mime"] = mime
	}
	return []map[string]string{value}, nil
}
func (s *Server) recoverAdobeImageTasks() {
	s.imageTaskMu.RLock()
	pending := []*imageTaskState{}
	for _, task := range s.imageTasks {
		if (task.Status == "running" || task.Status == "queued") && task.Route != nil && task.Route.Channel == "adobe" {
			pending = append(pending, task)
		}
	}
	s.imageTaskMu.RUnlock()
	for _, task := range pending {
		s.resumeAdobeImageTask(task)
	}
}
func (s *Server) resumeAdobeImageTask(task *imageTaskState) bool {
	s.imageTaskMu.Lock()
	if task.Status == "success" || task.recovering || !s.adobeRouteCanResume(task.Route, task.Model, "image") {
		s.imageTaskMu.Unlock()
		return false
	}
	route := *task.Route
	task.recovering = true
	task.Status = "running"
	task.Error = ""
	task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if s.saveImageTaskLocked(task) != nil {
		task.recovering = false
		s.imageTaskMu.Unlock()
		return false
	}
	s.imageTaskMu.Unlock()
	s.launchDeploymentWork(func() {
		defer func() { s.imageTaskMu.Lock(); task.recovering = false; s.imageTaskMu.Unlock() }()
		ctx, cancel := s.imageTaskContext(context.Background())
		defer cancel()
		ctx = context.WithValue(ctx, mediaTaskContextKey{}, task)
		job, err := s.adobeStore.LoadJob(route.JobKey)
		if err != nil {
			s.finishImageTaskError(task, "upstream task handle unavailable; automatic resubmission disabled")
			return
		}
		a, err := s.adobeStore.Resume(route.JobKey, route.AccountID)
		if err != nil {
			s.finishImageTaskError(task, adobe.SafeError(err))
			return
		}
		data, err := s.finishAdobeImage(ctx, a, route.JobKey, job, "url", "")
		if err != nil {
			s.finishImageTaskError(task, err.Error())
			return
		}
		s.imageTaskMu.Lock()
		defer s.imageTaskMu.Unlock()
		task.Status = "success"
		task.Error = ""
		task.Data = nil
		for _, item := range data {
			row := map[string]any{}
			for k, v := range item {
				row[k] = v
			}
			task.Data = append(task.Data, row)
		}
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		s.persistFinishedImageTaskLocked(task)
	})
	return true
}

func (s *Server) runAdobeVideoJob(job *videoJob, prompt string, options videoOptions, inputs []provider.OpenAIImageInput) {
	s.videoMu.Lock()
	if job.recovering || job.Route != nil {
		s.videoMu.Unlock()
		return
	}
	job.recovering = true
	s.videoMu.Unlock()
	defer func() { s.videoMu.Lock(); job.recovering = false; s.videoMu.Unlock() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	request, err := adobeVideoRequest(job.Model, prompt, options, inputs)
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "invalid_request", "message": err.Error()})
		return
	}
	if err := s.ensureAdobeAccounts(ctx, job.Model); err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "account_entitlements_unavailable", "message": adobe.SafeError(err)})
		return
	}
	key := "video-" + job.ID
	a, selectedContext, err := s.adobeStore.ReserveMediaForModel(ctx, key, job.Model, s.adobeAccountContext)
	if err != nil {
		code := "no_available_account"
		var upstream *adobe.Error
		if errors.As(err, &upstream) && upstream.Stage == "routing" {
			code = "account_proxy_unavailable"
		}
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": code, "message": adobe.SafeError(err)})
		return
	}
	ctx = selectedContext
	m, _ := model.AdobeModel(job.Model)
	s.videoMu.Lock()
	job.Route = &mediaRoute{Channel: "adobe", AccountID: a.ID, UpstreamModel: m.ModelID + "/" + m.ModelVersion, Stage: "selected", JobKey: key}
	err = s.saveVideoCheckpointLocked(job)
	s.videoMu.Unlock()
	if err != nil {
		s.adobeStore.ReleaseUnsubmittedTask(key, a.ID)
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "could not persist selected channel"})
		return
	}
	s.videoMu.Lock()
	job.Route.Stage = "media_session_preparing"
	s.videoMu.Unlock()
	if !s.videoStage(job, "media_session_preparing", nil) {
		err = errors.New("could not persist media preparation state")
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": s.adobeMediaPreparationError(key, a.ID, err).Error()})
		return
	}
	prepareStarted := time.Now()
	wasReady := s.adobeStore.MediaSessionReady(ctx, a, 0)
	fresh, err := s.adobeStore.PrepareMediaForTask(ctx, key, a.ID, a.Revision)
	s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"media_session_cache_hit": wasReady, "media_session_prepare_ms": time.Since(prepareStarted).Milliseconds()}})
	if err != nil {
		if diagnostic := adobe.MediaDiagnosticFromError(err); diagnostic != nil {
			s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"media_session_diagnostic": diagnostic}})
		}
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "media_session_prepare_failed", "message": s.adobeMediaPreparationError(key, a.ID, err).Error()})
		return
	}
	a = fresh
	if _, err := s.adobeClient.PreflightVideo(ctx, a, request); err != nil {
		if diagnostic := adobe.PreflightDiagnosticFromError(err); diagnostic != nil {
			s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"adobe_preflight_diagnostic": diagnostic}})
		}
		s.invalidateRejectedAdobeMediaSession(a, err)
		s.adobeStore.Release(key)
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "free_generation_unavailable", "message": adobe.SafeError(err)})
		return
	}
	s.videoMu.Lock()
	job.Route.Stage = "submit_started"
	err = s.saveVideoCheckpointLocked(job)
	s.videoMu.Unlock()
	if err != nil {
		s.adobeStore.Release(key)
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "could not persist submission state"})
		return
	}
	s.monitor.enrich(job.ID, map[string]any{"media_channel": "adobe", "upstream_model": m.ModelID + "/" + m.ModelVersion, "generate_audio": options.GenerateAudio, "reference_count": len(inputs)})
	s.setVideoStatus(job, "in_progress", 1, nil)
	handle, err := s.adobeClient.SubmitVideo(ctx, a, request)
	if err != nil {
		s.adobeStore.InvalidateMediaSession(a.ID, a.Revision)
		if stateErr := s.adobeSubmissionFailed(key, handle, err); stateErr != nil {
			s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": adobe.SafeError(err) + "; upstream request state could not be saved; automatic resubmission disabled"})
			return
		}
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "upstream_submit_failed", "message": adobe.SafeError(err)})
		return
	}
	if err = s.adobeStore.SaveJob(key, handle); err != nil {
		s.adobeStore.MarkUnknown(key)
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "upstream task handle could not be saved; automatic resubmission disabled"})
		return
	}
	s.videoMu.Lock()
	job.Route.Stage = "polling"
	err = s.saveVideoCheckpointLocked(job)
	s.videoMu.Unlock()
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "could not persist task state"})
		return
	}
	s.finishAdobeVideo(ctx, job, a, handle)
}
func (s *Server) finishAdobeVideo(ctx context.Context, job *videoJob, a adobe.Account, handle adobe.Job) {
	// A persisted rejection only needs local settlement. Completed jobs still
	// require the selected egress for downloading, as do all nonterminal polls.
	if !adobeTerminal(handle.Status) || handle.Status == "COMPLETED" {
		var egressErr error
		ctx, egressErr = s.adobeAccountContext(ctx, a)
		if egressErr != nil {
			s.setVideoStatus(job, "failed", 0, map[string]any{"code": "account_proxy_unavailable", "message": adobe.SafeError(egressErr)})
			return
		}
	}

	key := job.Route.JobKey
	handle, err := s.pollAdobeJob(ctx, a, key, handle, func(progress int) { s.setVideoStatus(job, "in_progress", progress, nil) })
	if err != nil {
		code := "upstream_poll_failed"
		if handle.Status == "FAILED" && adobe.TerminalPollError(handle).Code == "video_unsafe" {
			code = "video_unsafe"
			s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"adobe_poll_diagnostic": handle.PollDiagnostic}})
		}
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": code, "message": err.Error()})
		return
	}
	if len(handle.OutputURLs) != 1 {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "invalid_output", "message": "completed task did not return exactly one video"})
		return
	}
	s.videoMu.Lock()
	job.Route.Stage = "downloading"
	err = s.saveVideoCheckpointLocked(job)
	s.videoMu.Unlock()
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "could not persist task state"})
		return
	}
	raw, mime, err := s.adobeClient.Download(ctx, handle.OutputURLs[0], 128<<20)
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "download_failed", "message": adobe.SafeError(err)})
		return
	}
	path, _, err := s.storeVideoResultWithPolicy(job.ID, job.RequestHash, raw, mime, false, videoVerification{Verdict: "skipped", Reason: "disabled"})
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": err.Error()})
		return
	}
	s.videoMu.Lock()
	job.Route.Stage = "completed"
	job.CompletedAt = time.Now().Unix()
	job.ContentPath = path
	s.videoMu.Unlock()
	s.setVideoStatus(job, "completed", 100, nil)
	s.adobeStore.Release(key)
}
func (s *Server) resumeAdobeVideoJob(job *videoJob) bool {
	s.videoMu.Lock()
	if job.Status == "completed" || job.recovering || !s.adobeRouteCanResume(job.Route, job.Model, "video") {
		s.videoMu.Unlock()
		return false
	}
	route := *job.Route
	job.Status = "in_progress"
	job.Error = nil
	job.logWritten = false
	if job.savedRecord != nil {
		saved := *job.savedRecord
		s.monitor.mu.Lock()
		s.monitor.active[job.ID] = &saved
		s.monitor.mu.Unlock()
		job.savedRecord = nil
	}
	job.recovering = true
	s.videoMu.Unlock()
	s.launchDeploymentWork(func() {
		defer func() { s.videoMu.Lock(); job.recovering = false; s.videoMu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		handle, err := s.adobeStore.LoadJob(route.JobKey)
		if err != nil {
			s.setVideoStatus(job, "failed", 0, map[string]any{"code": "handle_missing", "message": "upstream task handle unavailable; automatic resubmission disabled"})
			return
		}
		a, err := s.adobeStore.Resume(route.JobKey, route.AccountID)
		if err != nil {
			s.setVideoStatus(job, "failed", 0, map[string]any{"code": "account_unavailable", "message": adobe.SafeError(err)})
			return
		}
		s.finishAdobeVideo(ctx, job, a, handle)
	})
	return true
}

// A saved, encrypted handle closes the crash window between upstream acceptance
// and the main task checkpoint. A missing handle never authorizes resubmission.
func (s *Server) adobeRouteCanResume(route *mediaRoute, id, kind string) bool {
	if route == nil || route.Channel != "adobe" || route.JobKey == "" || s.adobeStore == nil {
		return false
	}
	if resolved, err := s.adobeStore.SubmissionResolved(route.JobKey); err != nil || resolved {
		return false
	}
	switch route.Stage {
	case "submit_started", "polling", "downloading", "completed":
	default:
		return false
	}
	job, err := s.adobeStore.LoadJob(route.JobKey)
	return err == nil && job.PollURL != "" && job.Model == id && job.Kind == kind
}

// Reconcile the terminal-checkpoint / reservation-release crash window using
// persisted task completion and the exact account binding, never a quota guess.
func (s *Server) reconcileAdobeMediaReservations() {
	if s.adobeStore == nil {
		return
	}
	type terminal struct {
		route       mediaRoute
		model, kind string
	}
	items := []terminal{}
	s.imageTaskMu.RLock()
	for _, task := range s.imageTasks {
		if task.Status == "success" && task.Route != nil && task.Route.Channel == "adobe" {
			items = append(items, terminal{*task.Route, task.Model, "image"})
		}
	}
	s.imageTaskMu.RUnlock()
	s.videoMu.RLock()
	for _, task := range s.videoJobs {
		if task.Status == "completed" && task.Route != nil && task.Route.Channel == "adobe" {
			items = append(items, terminal{*task.Route, task.Model, "video"})
		}
	}
	s.videoMu.RUnlock()
	for _, item := range items {
		a, err := s.adobeStore.Get(item.route.AccountID)
		if err != nil || a.InFlight != item.route.JobKey {
			continue
		}
		job, err := s.adobeStore.LoadJob(item.route.JobKey)
		if err != nil || job.Status != "COMPLETED" || job.Model != item.model || job.Kind != item.kind {
			continue
		}
		_ = s.adobeStore.Release(item.route.JobKey)
	}
}
