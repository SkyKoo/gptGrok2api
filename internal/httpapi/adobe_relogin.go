package httpapi

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

var adobeReloginRequestID = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,80}$`)

// HTTP only admits or observes a task. Browser work outlives the request and is
// tracked by the same deployment drain used for existing account login jobs.
func (s *Server) adobeReloginAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		requestID := r.URL.Query().Get("request_id")
		if requestID != "" && !adobeReloginRequestID.MatchString(requestID) {
			writeError(w, 400, "重新登录请求标识无效", "invalid_request_error")
			return
		}
		items, err := s.adobeReloginJobs.List(requestID)
		if err != nil {
			writeError(w, 503, "重新登录进度暂不可读取，请稍后核对；不会自动重试登录", "storage_error")
			return
		}
		writeJSON(w, 200, map[string]any{"request_id": requestID, "items": items})
		return
	}
	var body struct {
		RequestID string   `json:"request_id"`
		IDs       []string `json:"ids"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ids := uniqueStrings(body.IDs)
	if len(ids) == 0 || len(ids) > 100 || !adobeReloginRequestID.MatchString(body.RequestID) {
		writeError(w, 400, "请选择 1–100 个 Adobe 账号并提供唯一 request_id", "invalid_request_error")
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		writeError(w, 503, "服务维护中，未启动重新登录", "adobe_relogin_not_started")
		return
	}
	if !s.beginDeploymentBackground() {
		writeError(w, 503, "服务维护中，未启动重新登录", "adobe_relogin_not_started")
		return
	}
	defer s.endDeploymentBackground()
	s.adobeReloginMu.Lock()
	items, err := s.adobeReloginJobs.Start(body.RequestID, ids, s.adobeStore)
	if err == nil && !s.adobeReloginRunning {
		s.adobeReloginRunning = true
		s.launchDeploymentWork(s.runAdobeRelogins)
	}
	s.adobeReloginMu.Unlock()
	if err != nil {
		writeError(w, 409, adobe.SafeError(err), "adobe_relogin_start_failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"request_id": body.RequestID, "items": items})
}
func (s *Server) runAdobeRelogins() {
	for {
		// Admission and the empty-queue decision share this lock; a late POST
		// cannot leave a persisted queued job without a worker.
		s.adobeReloginMu.Lock()
		task, ok, err := s.adobeReloginJobs.Next()
		if err != nil || !ok {
			s.adobeReloginRunning = false
			s.adobeReloginMu.Unlock()
			return
		}
		s.adobeReloginMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		account, failure := s.adobeStore.Get(task.AccountRef)
		if failure == nil {
			ctx, failure = s.adobeAccountContext(ctx, account)
		}
		if failure == nil {
			// The Store and browser must observe the same pinned egress so a
			// successful login can become reusable media-session evidence.
			account, failure = s.adobeStore.ReloginRevision(ctx, task.AccountRef, task.CredentialRevision)
		}
		cancel()
		if err = s.adobeReloginJobs.Finish(task.ProgressID, account, failure); err != nil {
			s.adobeReloginMu.Lock()
			s.adobeReloginRunning = false
			s.adobeReloginMu.Unlock()
			return
		}
	}
}
