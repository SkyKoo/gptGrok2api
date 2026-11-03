package httpapi

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

type videoJob struct {
	ID             string         `json:"id"`
	Object         string         `json:"object"`
	CreatedAt      int64          `json:"created_at"`
	Status         string         `json:"status"`
	Model          string         `json:"model"`
	Progress       int            `json:"progress"`
	Prompt         string         `json:"prompt"`
	Seconds        string         `json:"seconds"`
	Size           string         `json:"size"`
	Quality        string         `json:"quality"`
	ResolutionName string         `json:"resolution_name"`
	AspectRatio    string         `json:"aspect_ratio"`
	Preset         string         `json:"preset"`
	CompletedAt    int64          `json:"completed_at,omitempty"`
	Error          map[string]any `json:"error,omitempty"`
	ContentPath    string         `json:"-"`
	VideoURL       string         `json:"-"`
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
	if _, ok := model.Find(s.catalog, modelName); !ok || modelName != "grok-imagine-video" {
		writeError(w, http.StatusBadRequest, "model is not a video model", "invalid_request_error")
		return
	}
	inputs, err := videoReferenceInputs(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	job := &videoJob{ID: "video_" + randomID(), Object: "video", CreatedAt: time.Now().Unix(), Status: "queued", Model: modelName, Progress: 0, Prompt: prompt, Seconds: fmt.Sprint(options.Seconds), Size: options.Size, Quality: "standard", ResolutionName: options.Resolution, AspectRatio: options.Aspect, Preset: options.Preset}
	s.videoMu.Lock()
	s.videoJobs[job.ID] = job
	s.videoMu.Unlock()
	s.monitor.start(job.ID, "/v1/videos", modelName, prompt)
	// Serialize a submission snapshot; the worker immediately mutates job.
	s.videoMu.RLock()
	initial := *job
	s.videoMu.RUnlock()
	go s.runVideoJob(job, prompt, options.Aspect, options.Resolution, options.Seconds, options.Preset, inputs)
	writeJSON(w, http.StatusOK, initial)
}

func (s *Server) runVideoJob(job *videoJob, prompt, aspect, resolution string, seconds int, preset string, inputs []provider.OpenAIImageInput) {
	s.setVideoStatus(job, "in_progress", 1, nil)
	s.ensureGrokMediaQuotas(context.Background())
	segments, _ := protocol.VideoSegmentLengths(seconds)
	units := 0
	for _, length := range segments {
		units += (length + 5) / 6
	}
	lease, err := s.grokAccountPool.ReserveIntent(context.Background(), []string{"basic", "super", "heavy"}, nil, nil, 0, accounts.Intent{Kind: "video", Model: job.Model, VideoSegments: len(segments), VideoUnits: units, Uploads: len(inputs)})
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "no_available_account", "message": err.Error()})
		return
	}
	defer s.grokAccountPool.Release(lease)
	requestHash := videoRequestFingerprint(job.Model, prompt, aspect, resolution, seconds, preset, inputs)
	s.monitor.enrich(job.ID, map[string]any{
		"account_email": stringValue(lease.Account.Fields["email"]),
		"account_id":    stringValue(lease.Account.Fields["account_id"]),
		"metrics":       map[string]any{"video_request_sha256": requestHash, "video_reference_count": len(inputs)},
	})
	refs := make([]string, 0, len(inputs))
	for index, input := range inputs {
		s.grokAccountPool.MarkSent(lease, "file_upload")
		fileID, fileURI, uploadErr := s.mediaProvider.Upload(context.Background(), lease.Account, input.Name, input.MIME, base64.StdEncoding.EncodeToString(input.Data))
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
			return
		}
		ref := videoReferenceURL(fileID, fileURI, cookieUserID(lease.Account))
		if ref == "" {
			s.failVideoUpstream(job, lease, "invalid_reference", &protocol.UpstreamError{Status: 502, Message: fmt.Sprintf("第 %d 张视频参考图未返回有效资源标识；已停止生成", index+1)})
			return
		}
		refs = append(refs, ref)
	}
	post, err := s.mediaProvider.CreatePost(context.Background(), lease.Account, protocol.VideoPostMediaType, "", prompt)
	if err != nil {
		s.grokAccountPool.FeedbackIntent(lease, upstreamStatus(err), err)
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "create_post_failed", "message": err.Error()})
		return
	}
	postObject, _ := post["post"].(map[string]any)
	parentID := stringValue(postObject["id"])
	if parentID == "" {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "invalid_post", "message": "video create-post returned no post id"})
		return
	}
	diagnostics := provider.NewVideoDiagnostics(0, "", cookieUserID(lease.Account) != "")
	videoURL := ""
	assetID := ""
	extendPostID := parentID
	elapsedSeconds := 0
	for index, segmentLength := range segments {
		payload := protocol.BuildVideoPayload(prompt, parentID, aspect, resolution, segmentLength, preset, refs)
		if index > 0 {
			if extendPostID == "" {
				s.failVideoUpstream(job, lease, "missing_extension_source", diagnostics.Err("missing_extension_source"))
				return
			}
			payload = protocol.BuildVideoExtendPayload(prompt, parentID, extendPostID, aspect, resolution, segmentLength, preset, float64(elapsedSeconds)+1.0/24.0)
		}
		s.grokAccountPool.MarkSent(lease, "video_gen")
		response, requestErr := s.mediaProvider.StreamChat(context.Background(), lease.Account, payload)
		if requestErr != nil {
			s.grokAccountPool.FeedbackIntent(lease, upstreamStatus(requestErr), requestErr)
			s.setVideoStatus(job, "failed", 0, map[string]any{"code": "upstream_failed", "message": requestErr.Error()})
			return
		}
		diagnostics = provider.NewVideoDiagnostics(response.StatusCode, response.Header.Get("Content-Type"), cookieUserID(lease.Account) != "")
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		result := provider.NewVideoResult(parentID)
		for scanner.Scan() {
			line := scanner.Text()
			diagnostics.Observe(line)
			if streamErr := diagnostics.Failure(); streamErr != nil {
				_ = response.Body.Close()
				s.failVideoUpstream(job, lease, "upstream_stream_error", streamErr)
				return
			}
			kind, events := protocol.ParseSSELine(line)
			if kind == "done" {
				break
			}
			for _, event := range events {
				if err := result.Observe(event); err != nil {
					_ = response.Body.Close()
					s.failVideoUpstream(job, lease, "video_result_mismatch", err)
					return
				}
				if result.Progress > 0 {
					scaled := int((float64(index) + float64(result.Progress)/100.0) / float64(len(segments)) * 100.0)
					s.setVideoStatus(job, "in_progress", scaled, nil)
				}
			}
		}
		_ = response.Body.Close()
		if scanner.Err() != nil {
			s.failVideoUpstream(job, lease, "stream_failed", diagnostics.Err("stream_read_failed"))
			return
		}
		if !result.Complete() {
			s.failVideoUpstream(job, lease, "incomplete_video", diagnostics.Err("incomplete_video"))
			return
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
		return
	}
	raw, mimeType, err := s.mediaProvider.Fetch(context.Background(), lease.Account, videoURL)
	if err != nil {
		s.failVideoUpstream(job, lease, "download_failed", err)
		return
	}
	path, fingerprint, err := s.storeVideoResult(job.ID, requestHash, raw, mimeType)
	if fingerprint != "" {
		s.monitor.enrich(job.ID, map[string]any{"metrics": map[string]any{"video_media_sha256": fingerprint}})
	}
	if err != nil {
		code := "storage_failed"
		if errors.Is(err, errRepeatedVideo) {
			s.failVideoUpstream(job, lease, "repeated_video_result", &protocol.UpstreamError{Status: 502, Message: err.Error()})
			return
		}
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": code, "message": err.Error()})
		return
	}
	s.videoMu.Lock()
	job.CompletedAt = time.Now().Unix()
	job.ContentPath = path
	job.VideoURL = videoURL
	s.videoMu.Unlock()
	s.setVideoStatus(job, "completed", 100, nil)
	s.grokAccountPool.FeedbackIntent(lease, http.StatusOK, nil)
}

// Feed body-level SSE failures back through the same status classifier used
// for HTTP failures; video quota limits must not invalidate the whole account.
func (s *Server) failVideoUpstream(job *videoJob, lease *accounts.Lease, code string, err error) {
	status := upstreamStatus(err)
	s.grokAccountPool.FeedbackIntent(lease, status, err)
	failure := map[string]any{"code": code, "message": err.Error(), "upstream_status": status}
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
	id := path
	s.videoMu.RLock()
	job, ok := s.videoJobs[id]
	var copy videoJob
	if ok {
		copy = *job
	}
	s.videoMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "video not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, &copy)
}

func (s *Server) videoContent(w http.ResponseWriter, r *http.Request, id string) {
	s.videoMu.RLock()
	job, ok := s.videoJobs[id]
	var path string
	if ok {
		path = job.ContentPath
	}
	s.videoMu.RUnlock()
	if !ok {
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
	job.Status = status
	if progress >= 0 {
		job.Progress = progress
	}
	if failure != nil {
		job.Error = failure
	}
	if s.monitor != nil {
		if status == "completed" {
			s.monitor.finish(job.ID, "success", job.Model, job.Prompt, "")
		} else if status == "failed" {
			message := "video job failed"
			if value, ok := failure["message"].(string); ok && value != "" {
				message = value
			}
			s.monitor.finish(job.ID, "failed", job.Model, job.Prompt, message)
		} else {
			s.monitor.update(job.ID, status, progress, "")
		}
	}
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
