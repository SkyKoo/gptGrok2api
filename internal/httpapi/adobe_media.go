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
	"image"
	"net/http"
	"strings"
	"time"
)

func (s *Server) generateAdobeImageData(r *http.Request, ctx context.Context, selected imageSelection, prompt, id, size, format, publicBase string, count int, inputs []provider.OpenAIImageInput, options provider.ImageOutputOptions) (result []map[string]string, resultErr error) {
	request, validationErr := adobeImageRequest(id, prompt, size, count, inputs)
	if validationErr != nil {
		s.adobeStore.Release(selected.Key)
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
		err := s.saveImageTaskLocked(task)
		if err == nil {
			s.imageTasks[imageTaskKey(owner, task.ID)] = task
		}
		s.imageTaskMu.Unlock()
		if err != nil {
			s.adobeStore.Release(selected.Key)
			return nil, err
		}
		ctx = context.WithValue(ctx, mediaTaskContextKey{}, task)
		defer func() {
			if resultErr != nil {
				s.finishImageTaskError(task, resultErr.Error())
				resultErr = &protocol.UpstreamError{Status: upstreamStatus(resultErr), Message: fmt.Sprintf("%v; task_id=%s (resume existing task via /api/image-tasks/%s/resume-poll)", resultErr, task.ID, task.ID)}
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
		s.adobeStore.Release(selected.Key)
		return nil, adobeAPIError(egressErr)
	}

	if format != "url" && format != "b64_json" {
		s.adobeStore.Release(selected.Key)
		return nil, &protocol.UpstreamError{Status: 400, Message: "response_format must be url or b64_json"}
	}
	m, _ := model.AdobeModel(id)
	s.enrichRequestMonitor(r, map[string]any{"media_channel": "adobe", "upstream_model": m.ModelID + "/" + m.ModelVersion})
	if _, err := s.adobeClient.PreflightImage(ctx, selected.Adobe, request); err != nil {
		s.adobeStore.Release(selected.Key)
		return nil, adobeAPIError(err)
	}
	if err := s.imageRouteStage(ctx, "submit_started"); err != nil {
		s.adobeStore.Release(selected.Key)
		return nil, err
	}
	job, err := s.adobeClient.SubmitImage(ctx, selected.Adobe, request)
	if err != nil {
		s.adobeSubmissionFailed(selected.Key, err)
		return nil, adobeAPIError(err)
	}
	if err = s.adobeStore.SaveJob(selected.Key, job); err != nil {
		s.adobeStore.MarkUnknown(selected.Key)
		return nil, fmt.Errorf("could not save upstream task handle; automatic resubmission disabled")
	}
	if err = s.imageRouteStage(ctx, "polling"); err != nil {
		return nil, err
	}
	return s.finishAdobeImage(ctx, selected.Adobe, selected.Key, job, format, publicBase)
}
func (s *Server) adobeSubmissionFailed(key string, err error) {
	var upstream *adobe.Error
	if errors.As(err, &upstream) && !upstream.SubmissionUnknown {
		s.adobeStore.Release(key)
	} else {
		s.adobeStore.MarkUnknown(key)
	}
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
	if a.ExpiresAt <= time.Now().Add(time.Minute).Unix() {
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
				s.adobeStore.Release(key)
				return job, &adobe.Error{Stage: "poll", Code: "generation_failed", Status: 422}
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
	s.ensureAdobeAccounts(ctx, job.Model)
	key := "video-" + job.ID
	a, err := s.adobeStore.ReserveForModel(key, job.Model)
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "no_available_account", "message": adobe.SafeError(err)})
		return
	}
	ctx, err = s.adobeAccountContext(ctx, a)
	if err != nil {
		s.adobeStore.Release(key)
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "account_proxy_unavailable", "message": adobe.SafeError(err)})
		return
	}
	m, _ := model.AdobeModel(job.Model)
	s.videoMu.Lock()
	job.Route = &mediaRoute{Channel: "adobe", AccountID: a.ID, UpstreamModel: m.ModelID + "/" + m.ModelVersion, Stage: "selected", JobKey: key}
	err = s.saveVideoCheckpointLocked(job)
	s.videoMu.Unlock()
	if err != nil {
		s.adobeStore.Release(key)
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "could not persist selected channel"})
		return
	}
	if _, err := s.adobeClient.PreflightVideo(ctx, a, request); err != nil {
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
		s.adobeSubmissionFailed(key, err)
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
	var egressErr error
	ctx, egressErr = s.adobeAccountContext(ctx, a)
	if egressErr != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "account_proxy_unavailable", "message": adobe.SafeError(egressErr)})
		return
	}

	key := job.Route.JobKey
	handle, err := s.pollAdobeJob(ctx, a, key, handle, func(progress int) { s.setVideoStatus(job, "in_progress", progress, nil) })
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "upstream_poll_failed", "message": err.Error()})
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
