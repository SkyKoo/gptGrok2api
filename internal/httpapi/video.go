package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

type videoJob struct {
	Route          *mediaRoute `json:"-"`
	OwnerID        string      `json:"-"`
	RequestHash    string      `json:"-"`
	ClientKey      string      `json:"-"`
	recovering     bool
	ID             string             `json:"id"`
	Object         string             `json:"object"`
	CreatedAt      int64              `json:"created_at"`
	Status         string             `json:"status"`
	Model          string             `json:"model"`
	Progress       int                `json:"progress"`
	Prompt         string             `json:"prompt"`
	Seconds        string             `json:"seconds"`
	Size           string             `json:"size"`
	Quality        string             `json:"quality"`
	ResolutionName string             `json:"resolution_name"`
	GenerateAudio  bool               `json:"generate_audio,omitempty"`
	ReferenceCount int                `json:"reference_count,omitempty"`
	AspectRatio    string             `json:"aspect_ratio"`
	Preset         string             `json:"preset"`
	CompletedAt    int64              `json:"completed_at,omitempty"`
	Error          map[string]any     `json:"error,omitempty"`
	ContentPath    string             `json:"-"`
	VideoURL       string             `json:"-"`
	Verification   *videoVerification `json:"verification,omitempty"`
	logWritten     bool
	savedRecord    *monitorRecord
}

func (s *Server) videosCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVideoReferences*maxVideoReferenceBytes+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form", "invalid_request_error")
		return
	}
	defer r.MultipartForm.RemoveAll()
	modelName := strings.TrimSpace(r.FormValue("model"))
	if modelName == "" {
		modelName = "grok-imagine-video"
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt cannot be empty", "invalid_request_error")
		return
	}
	options, err := parseVideoOptions(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if spec, ok := model.Find(s.catalog, modelName); !ok || spec.Capability&model.Video == 0 {
		writeError(w, http.StatusBadRequest, "model is not a video model", "invalid_request_error")
		return
	}
	inputs, err := videoReferenceInputs(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if m, ok := model.AdobeModel(modelName); ok && m.Kind == "video" {
		if _, err := adobeVideoRequest(modelName, prompt, options, inputs); err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
	}
	owner := s.authIdentity(r)
	clientKey := strings.TrimSpace(firstNonEmpty(r.Header.Get("Idempotency-Key"), r.FormValue("client_task_id")))
	if len(clientKey) > 128 {
		writeError(w, 400, "idempotency key too long", "invalid_request_error")
		return
	}
	requestHash := videoRequestFingerprint(modelName, prompt, options.Aspect, options.Resolution, options.Seconds, options.Preset, inputs, options.GenerateAudio)
	job := &videoJob{ID: "video_" + randomID(), Object: "video", CreatedAt: time.Now().Unix(), Status: "queued", Model: modelName, Progress: 0, Prompt: prompt, Seconds: fmt.Sprint(options.Seconds), Size: options.Size, Quality: "standard", ResolutionName: options.Resolution, AspectRatio: options.Aspect, Preset: options.Preset, GenerateAudio: options.GenerateAudio, ReferenceCount: len(inputs)}
	job.OwnerID = owner
	job.RequestHash = requestHash
	job.ClientKey = clientKey
	if clientKey != "" {
		job.ID = "video_" + imageTaskKey(owner, clientKey)
	}
	s.videoMu.Lock()
	if previous := s.videoJobs[job.ID]; previous != nil {
		if previous.OwnerID != owner || previous.RequestHash != requestHash {
			s.videoMu.Unlock()
			writeError(w, 409, "idempotency key belongs to a different request", "invalid_request_error")
			return
		}
		copy := *previous
		s.videoMu.Unlock()
		writeJSON(w, 200, copy)
		return
	}
	if s.videoLoadErr != nil {
		s.videoMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "video task storage requires recovery", "storage_error")
		return
	}
	s.monitor.start(job.ID, "/v1/videos", modelName, "视频生成")
	if err := s.saveVideoCheckpointLocked(job); err != nil {
		s.videoMu.Unlock()
		s.monitor.finish(job.ID, "failed", modelName, "视频生成", "无法保存视频任务")
		writeError(w, http.StatusServiceUnavailable, "could not persist video task", "storage_error")
		return
	}
	s.videoJobs[job.ID] = job
	s.videoMu.Unlock()
	// Serialize a submission snapshot; the worker immediately mutates job.
	s.videoMu.RLock()
	initial := *job
	s.videoMu.RUnlock()
	s.launchDeploymentWork(func() {
		if m, ok := model.AdobeModel(job.Model); ok && m.Kind == "video" {
			s.runAdobeVideoJob(job, prompt, options, inputs)
		} else {
			s.runVideoJob(job, prompt, options.Aspect, options.Resolution, options.Seconds, options.Preset, inputs)
		}
	})
	writeJSON(w, http.StatusOK, initial)
}

func (s *Server) runVideoJob(job *videoJob, prompt, aspect, resolution string, seconds int, preset string, inputs []provider.OpenAIImageInput) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	stage := func(name string, metrics map[string]any) bool {
		if s.videoStage(job, name, metrics) {
			return true
		}
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "无法保存视频诊断，已停止处理；未自动重试"})
		return false
	}
	if _, ok := s.monitor.detail(job.ID); !ok {
		s.monitor.start(job.ID, "/v1/videos", job.Model, "视频生成")
	}
	requestHash := videoRequestFingerprint(job.Model, prompt, aspect, resolution, seconds, preset, inputs)
	referenceHashes := make([]string, len(inputs))
	for i, input := range inputs {
		referenceHashes[i] = fmt.Sprintf("%x", sha256.Sum256(input.Data))
	}
	s.monitor.mu.Lock()
	if record := s.monitor.active[job.ID]; record != nil {
		record.RequestMeta = map[string]any{"model": job.Model, "seconds": seconds, "aspect_ratio": aspect, "resolution_name": resolution, "preset": preset, "reference_count": len(inputs)}
	}
	s.monitor.mu.Unlock()
	if !s.videoStage(job, "video_validated", map[string]any{"video_request_sha256": requestHash, "video_prompt_sha256": videoDigest(prompt), "video_reference_count": len(inputs), "video_reference_sha256": referenceHashes}) {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "无法保存视频诊断，已停止生成"})
		return
	}
	s.setVideoStatus(job, "in_progress", 1, nil)
	settings, err := s.store.Config()
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "video_settings_unavailable", "message": "无法读取视频核对设置，本次未向 Grok 提交生成"})
		return
	}
	// Capture the setting once so saving settings cannot change an active job.
	reviewEnabled, _ := settings["video_review_enabled"].(bool)
	s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"video_review_enabled": reviewEnabled}})
	for attempt := 1; attempt <= grokMediaGenerationAttempts; attempt++ {
		if !s.runVideoAttempt(ctx, job, prompt, aspect, resolution, seconds, preset, inputs, requestHash, reviewEnabled, attempt) {
			return
		}
		s.videoMu.Lock()
		job.Verification = nil
		s.videoMu.Unlock()
		s.setVideoStatus(job, "in_progress", 1, nil)
		if !stage("grok_media_regenerating", map[string]any{"grok_generation_retry": 1}) {
			return
		}
	}
}

func (s *Server) runVideoAttempt(ctx context.Context, job *videoJob, prompt, aspect, resolution string, seconds int, preset string, inputs []provider.OpenAIImageInput, requestHash string, reviewEnabled bool, attempt int) bool {
	stage := func(name string, metrics map[string]any) bool {
		if s.videoStage(job, name, metrics) {
			return true
		}
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "无法保存视频诊断，已停止处理"})
		return false
	}
	if !stage("video_generation_attempt", map[string]any{"grok_generation_attempt": attempt}) {
		return false
	}
	segments, _ := protocol.VideoSegmentLengths(seconds)
	units := 0
	for _, length := range segments {
		units += (length + 5) / 6
	}
	intent := accounts.Intent{Kind: "video", Model: job.Model, VideoSegments: len(segments), VideoUnits: units, Uploads: len(inputs)}
	s.ensureGrokMediaQuotas(ctx, intent)
	lease, err := s.grokAccountPool.ReserveIntent(ctx, []string{"basic", "super", "heavy"}, nil, nil, 0, intent)
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "no_available_account", "message": err.Error()})
		return false
	}
	defer s.grokAccountPool.Release(lease)
	ctx = s.proxyManager.WithAccountEgress(ctx, lease.Account.Fields)
	s.videoMu.Lock()
	job.Route = &mediaRoute{Channel: "grok", AccountID: accounts.Identity(lease.Account), UpstreamModel: job.Model, Stage: "selected"}
	saveErr := s.saveVideoCheckpointLocked(job)
	s.videoMu.Unlock()
	if saveErr != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": "could not persist selected route"})
		return false
	}
	s.monitor.enrich(job.ID, map[string]any{
		"account_id": "sha256:" + videoDigest(accounts.Identity(lease.Account)),
	})
	refs := make([]string, 0, len(inputs))
	for index, input := range inputs {
		if !stage("video_reference_upload", map[string]any{"video_upload_index": index + 1, "video_upload_completed": index}) {
			return false
		}
		s.grokAccountPool.MarkSent(lease, "file_upload")
		fileID, fileURI, uploadErr := s.mediaProvider.Upload(ctx, lease.Account, input.Name, input.MIME, base64.StdEncoding.EncodeToString(input.Data))
		if uploadErr != nil {
			// The provider exposes only safe diagnostics, preserving challenge vs quota errors.
			code := "reference_upload_failed"
			if diagnostics := provider.MediaUploadDiagnostics(uploadErr); diagnostics != nil {
				s.monitor.enrich(job.ID, map[string]any{"metrics": diagnostics})
				if diagnostics["upload_challenge"] == true {
					code = "reference_verification_required"
				}
			}
			s.failVideoUpstream(job, lease, code, fmt.Errorf("第 %d 张视频参考图：%w", index+1, uploadErr))
			return false
		}
		ref := videoReferenceURL(fileID, fileURI, cookieUserID(lease.Account))
		if ref == "" {
			s.failVideoUpstream(job, lease, "invalid_reference", &protocol.UpstreamError{Status: 502, Message: fmt.Sprintf("第 %d 张视频参考图未返回有效资源标识；已停止生成", index+1)})
			return false
		}
		refs = append(refs, ref)
		if !stage("video_reference_uploaded", map[string]any{"video_upload_completed": index + 1, "video_upload_ref_sha256": videoDigest(ref)}) {
			return false
		}
	}
	if !stage("video_create_post", nil) {
		return false
	}
	post, err := s.mediaProvider.CreatePost(ctx, lease.Account, protocol.VideoPostMediaType, "", prompt)
	if err != nil {
		s.failVideoUpstream(job, lease, "create_post_failed", err)
		return false
	}
	postObject, _ := post["post"].(map[string]any)
	parentID := stringValue(postObject["id"])
	if parentID == "" {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "invalid_post", "message": "video create-post returned no post id"})
		return false
	}
	if !stage("video_post_created", map[string]any{"video_parent_sha256": videoDigest(parentID)}) {
		return false
	}
	diagnostics := provider.NewVideoDiagnostics(0, "", cookieUserID(lease.Account) != "")
	videoURL := ""
	assetID := ""
	extendPostID := parentID
	elapsedSeconds := 0
	for index, segmentLength := range segments {
		if !stage("video_generation", map[string]any{"video_segment": index + 1}) {
			return false
		}
		payload := protocol.BuildVideoPayload(prompt, parentID, aspect, resolution, segmentLength, preset, refs)
		if index > 0 {
			if extendPostID == "" {
				s.failVideoUpstream(job, lease, "missing_extension_source", diagnostics.Err("missing_extension_source"))
				return false
			}
			payload = protocol.BuildVideoExtendPayload(prompt, parentID, extendPostID, aspect, resolution, segmentLength, preset, float64(elapsedSeconds)+1.0/24.0)
		}
		s.grokAccountPool.MarkSent(lease, "video_gen")
		streamStarted := time.Now()
		response, requestErr := s.mediaProvider.StreamChat(ctx, lease.Account, payload)
		if requestErr != nil {
			s.failVideoUpstream(job, lease, "upstream_failed", requestErr)
			return false
		}
		diagnostics = provider.NewVideoDiagnostics(response.StatusCode, response.Header.Get("Content-Type"), cookieUserID(lease.Account) != "")
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		result := provider.NewVideoResult(parentID)
		lastProgress := -1
		for scanner.Scan() {
			line := scanner.Text()
			diagnostics.Observe(line)
			s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"video_stream": diagnostics.Snapshot(), "conversation_stream_ms": time.Since(streamStarted).Milliseconds()}})
			if streamErr := diagnostics.Failure(); streamErr != nil {
				_ = response.Body.Close()
				s.failVideoUpstream(job, lease, "upstream_stream_error", streamErr)
				return false
			}
			kind, events := protocol.ParseSSELine(line)
			if kind == "done" {
				break
			}
			for _, event := range events {
				if err := result.Observe(event); err != nil {
					_ = response.Body.Close()
					s.failVideoUpstream(job, lease, "video_result_mismatch", err)
					return false
				}
				if result.Progress > lastProgress {
					lastProgress = result.Progress
					scaled := int((float64(index) + float64(result.Progress)/100.0) / float64(len(segments)) * 100.0)
					s.setVideoStatus(job, "in_progress", min(scaled, 99), nil)
					if !stage("video_progress", map[string]any{"video_upstream_progress": result.Progress}) {
						_ = response.Body.Close()
						return false
					}
				}
			}
		}
		_ = response.Body.Close()
		if scanner.Err() != nil {
			s.failVideoUpstream(job, lease, "stream_failed", diagnostics.Err("stream_read_failed"))
			return false
		}
		if !result.Complete() {
			s.failVideoUpstream(job, lease, "incomplete_video", diagnostics.Err("incomplete_video"))
			return false
		}
		// The final segment must supply its own result. Never report an earlier
		// shorter segment as a successful extension of the requested duration.
		videoURL = result.URL
		assetID = result.AssetID
		extendPostID = result.ResultPostID()
		s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{
			"video_parent_sha256":          fmt.Sprintf("%x", sha256.Sum256([]byte(parentID))),
			"video_result_sha256":          fmt.Sprintf("%x", sha256.Sum256([]byte(result.PostID+"/"+result.VideoID))),
			"video_parent_verified":        result.ParentVerified,
			"video_upstream_prompt_sha256": result.UpstreamPromptHash,
			"video_asset_sha256":           videoDigest(result.AssetID),
			"video_url_sha256":             videoDigest(result.URL),
		}})
		elapsedSeconds += segmentLength
	}
	if videoURL == "" && assetID != "" {
		videoURL = protocol.ResolveAssetReference(assetID, "", "")
	}
	if videoURL == "" && extendPostID != "" {
		videoURL = protocol.ResolveGeneratedVideoReference(cookieUserID(lease.Account), extendPostID)
	}
	if videoURL == "" {
		reason := "no_video_result"
		if diagnostics != nil && diagnostics.PostIDPresent && !diagnostics.UserIDPresent {
			reason = "user_id_missing"
		}
		s.failVideoUpstream(job, lease, "no_video", diagnostics.Err(reason))
		return false
	}
	if !stage("video_download", nil) {
		return false
	}
	downloadStarted := time.Now()
	raw, mimeType, err := s.mediaProvider.Fetch(ctx, lease.Account, videoURL)
	if err != nil {
		s.failVideoUpstream(job, lease, "download_failed", err)
		return false
	}
	s.grokAccountPool.FeedbackIntent(lease, http.StatusOK, nil)
	fingerprint, integrityErr := videoMediaFingerprint(bytes.NewReader(raw), int64(len(raw)))
	s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"video_media_sha256": fingerprint}})
	if integrityErr != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "video_integrity_failed", "message": "无法确认视频文件完整性，已停止交付"})
		return false
	}
	if !stage("video_downloaded", map[string]any{"download_ms": time.Since(downloadStarted).Milliseconds(), "video_bytes": len(raw)}) {
		return false
	}
	verification := videoVerification{Verdict: "skipped", Reason: "disabled", References: len(inputs)}
	reviewMetrics := map[string]any{}
	if reviewEnabled {
		if !stage("video_content_review", nil) {
			return false
		}
		reviewStarted := time.Now()
		verification = s.reviewGrokMedia(ctx, job.ID, "video", requestHash, fingerprint, prompt, inputs, raw, seconds, attempt, 1)
		reviewMetrics["video_review_ms"] = time.Since(reviewStarted).Milliseconds()
	}
	s.videoMu.Lock()
	job.Verification = &verification
	s.videoMu.Unlock()
	reviewMetrics["video_verification"] = verification
	if verification.parseError != "" {
		reviewMetrics["video_review_parse_error"] = verification.parseError
	}
	reviewStage := "video_content_review_skipped"
	if reviewEnabled {
		reviewStage = "video_content_reviewed"
	}
	if !stage(reviewStage, reviewMetrics) {
		return false
	}
	if reviewEnabled && verification.Verdict != "matched" {
		if verification.Verdict == "mismatch" && attempt < grokMediaGenerationAttempts {
			return true
		}
		code, message := "video_content_unverified", "视频内容核对未能完成，已保存核对故障；未判定为占位视频"
		if verification.Verdict == "mismatch" {
			code, message = "video_content_mismatch", "视频明显无关，已停止交付并保存核对证据；未重新生成"
		}
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": code, "message": message, "verification": verification})
		return false
	}
	path, fingerprint, err := s.storeVideoResultWithPolicy(job.ID, requestHash, raw, mimeType, false, verification)
	if fingerprint != "" {
		s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"video_media_sha256": fingerprint}})
	}
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": err.Error()})
		return false
	}
	s.videoMu.Lock()
	job.CompletedAt = time.Now().Unix()
	job.ContentPath = path
	job.VideoURL = videoURL
	s.videoMu.Unlock()
	s.setVideoStatus(job, "completed", 100, nil)
	return false
}

// Feed body-level SSE failures back through the same status classifier used
// for HTTP failures; video quota limits must not invalidate the whole account.
func (s *Server) failVideoUpstream(job *videoJob, lease *accounts.Lease, code string, err error) {
	status := upstreamStatus(err)
	s.grokAccountPool.FeedbackIntent(lease, status, err)
	message := "Grok 视频上游处理失败，已停止生成；请查看任务诊断"
	if status == 422 {
		message = "Grok 视频被上游审核拦截或拒绝，未返回视频"
	}
	if status == 429 {
		message = "Grok 视频额度不足或上游限流，未自动重试"
	}
	if code == "reference_verification_required" {
		message = "参考图上传遇到 Cloudflare 人机验证，已停止生成；未自动重试"
	}
	if code == "repeated_video_result" {
		message = errRepeatedVideo.Error()
	}
	failure := map[string]any{"code": code, "message": message, "upstream_status": status}
	if diagnostics := provider.MediaUploadDiagnostics(err); diagnostics != nil {
		failure["upload_diagnostics"] = diagnostics
	}
	s.setVideoStatus(job, "failed", 0, failure)
}

func (s *Server) videoByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
	if strings.HasSuffix(path, "/content") {
		id := strings.TrimSuffix(path, "/content")
		s.videoContent(w, r, id)
		return
	}
	resume := strings.HasSuffix(path, "/resume-poll")
	id := strings.TrimSuffix(path, "/resume-poll")
	if r.Method != http.MethodGet && !(r.Method == http.MethodPost && resume) {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	s.videoMu.RLock()
	job, ok := s.videoJobs[id]
	var copy videoJob
	if ok {
		copy = *job
	}
	s.videoMu.RUnlock()
	if !ok || (copy.OwnerID != "" && copy.OwnerID != s.authIdentity(r)) {
		writeError(w, http.StatusNotFound, "video not found", "not_found")
		return
	}
	if resume && r.Method == http.MethodPost && s.resumeAdobeVideoJob(job) {
		writeJSON(w, 202, map[string]any{"id": job.ID, "status": "in_progress"})
		return
	}
	writeJSON(w, http.StatusOK, &copy)
}

func (s *Server) videoContent(w http.ResponseWriter, r *http.Request, id string) {
	s.videoMu.RLock()
	job, ok := s.videoJobs[id]
	var path string
	if ok && job.Status == "completed" {
		path = job.ContentPath
	}
	s.videoMu.RUnlock()
	if !ok || (job.OwnerID != "" && job.OwnerID != s.authIdentity(r)) {
		writeError(w, http.StatusNotFound, "video not found", "not_found")
		return
	}
	if path == "" {
		writeError(w, http.StatusConflict, "video content is not ready yet", "video_not_ready")
		return
	}
	if !isWithin(s.cfg.VideoDataDir, path) {
		writeError(w, http.StatusNotFound, "video not found", "not_found")
		return
	}
	http.ServeFile(w, r, path)
}

func (s *Server) setVideoStatus(job *videoJob, status string, progress int, failure map[string]any) {
	s.videoMu.Lock()
	defer s.videoMu.Unlock()
	if job.Status == "completed" || job.Status == "failed" {
		return
	}
	job.Status = status
	if progress >= 0 {
		job.Progress = progress
	}
	if failure != nil {
		job.Error = failure
		s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"video_error_code": failure["code"], "video_upstream_status": failure["upstream_status"]}})
	}
	terminal := status == "completed" || status == "failed"
	if terminal {
		job.CompletedAt = time.Now().Unix()
	}
	if status == "failed" {
		job.ContentPath, job.VideoURL = "", ""
	}
	if s.monitor != nil {
		if status == "completed" {
			s.monitor.mu.Lock()
			if record := s.monitor.active[job.ID]; record != nil {
				record.OutputVideos = []map[string]string{{"url": "/images/video/" + filepath.Base(job.ContentPath), "filename": filepath.Base(job.ContentPath)}}
			}
			s.monitor.mu.Unlock()
			s.monitor.finish(job.ID, "success", job.Model, "视频生成", "")
		} else if status == "failed" {
			message := "video job failed"
			if value, ok := failure["message"].(string); ok && value != "" {
				message = value
			}
			s.monitor.finish(job.ID, "failed", job.Model, "视频生成", message)
		} else {
			s.monitor.update(job.ID, status, progress, "")
		}
	}
	if terminal {
		record, _ := s.monitor.detail(job.ID)
		job.savedRecord = &record
		if err := s.saveVideoCheckpointLocked(job); err != nil {
			log.Printf("video terminal persistence failed: task=%s", job.ID)
			if job.Status == "completed" {
				path := job.ContentPath
				job.Status, job.Progress, job.ContentPath, job.VideoURL = "failed", 0, "", ""
				job.Error = map[string]any{"code": "video_storage_failed", "message": "无法持久保存视频任务终态，已停止交付；未自动重试"}
				record.Status, record.Stage, record.Progress, record.Error, record.OutputVideos = "failed", "failed", 0, stringValue(job.Error["message"]), nil
				record.Metrics = cloneMap(record.Metrics)
				record.Metrics["video_error_code"] = "video_storage_failed"
				record.Events = append([]map[string]any(nil), record.Events...)
				if len(record.Events) > 0 {
					record.Events[len(record.Events)-1] = map[string]any{"event": "failed", "stage": "failed", "status": "failed", "label": "终态保存失败", "time": time.Now().UTC()}
				}
				s.monitor.mu.Lock()
				for i := range s.monitor.completed {
					if s.monitor.completed[i].CallID == job.ID {
						s.monitor.completed[i] = record
						break
					}
				}
				s.monitor.mu.Unlock()
				if isWithin(s.cfg.VideoDataDir, path) {
					_ = os.Remove(path)
					_ = os.Remove(path + ".meta.json")
				}
				_ = s.saveVideoCheckpointLocked(job)
			}
		}
		s.writeVideoLogLocked(job, record)
		log.Printf("video task terminal: task=%s status=%s code=%s", job.ID, job.Status, stringValue(job.Error["code"]))
	}
}

func videoDigest(value string) string {
	if value == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func multipartFiles(r *http.Request, keys ...string) []*multipart.FileHeader {
	if r.MultipartForm == nil {
		return nil
	}
	for _, key := range keys {
		if files := r.MultipartForm.File[key]; len(files) > 0 {
			return files
		}
	}
	return nil
}

func randomID() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }
