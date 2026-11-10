package httpapi

import (
	"context"
	"errors"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/tasks"
)

func (s *Server) beginDeploymentBackground() bool {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if s.deployPaused || s.deploySealed {
		return false
	}
	s.deployBackground++
	return true
}
func (s *Server) endDeploymentBackground() {
	s.deployMu.Lock()
	s.deployBackground--
	s.deployMu.Unlock()
}

// PauseDeployment changes admission only. It does not cancel admitted work or
// rewrite any user's scheduler switch/next-run time.
func (s *Server) PauseDeployment(paused bool) {
	s.deployMu.Lock()
	if s.deploySealed {
		s.deployMu.Unlock()
		return
	}
	s.deployPaused = paused
	s.deployMu.Unlock()
	if q, ok := s.taskQueue.(interface{ SetDeploymentPaused(bool) }); ok {
		q.SetDeploymentPaused(paused)
	}
}

func deploymentPending(status string) bool {
	switch status {
	case "", "queued", "pending", "running", "processing", "in_progress":
		return true
	}
	return false
}

// DeploymentStatus includes work that outlives its submitting HTTP request.
// Only counts leave this boundary; never expose account/session information.
func (s *Server) DeploymentStatus() map[string]int {
	s.deployMu.Lock()
	background := s.deployBackground
	detached := s.deployDetached
	s.deployMu.Unlock()
	counts := map[string]int{"background": background, "detached": detached}
	if s.freeRegister.Snapshot()["running"] == true {
		counts["openai_registration"]++
	}
	if s.adobeRegister != nil && s.adobeRegister.Snapshot()["running"] == true {
		counts["adobe_registration"]++
	}
	if s.grokRegister.Snapshot()["running"] == true {
		counts["grok_registration"]++
	}
	if s.registerRuntime.Status("openai")["running"] == true {
		counts["legacy_registration"]++
	}
	s.survivalMu.RLock()
	if s.survivalRunning {
		counts["survival"]++
	}
	s.survivalMu.RUnlock()
	s.imageTaskMu.RLock()
	for _, job := range s.imageTasks {
		if deploymentPending(job.Status) {
			counts["images"]++
		}
	}
	s.imageTaskMu.RUnlock()
	s.fileTaskMu.RLock()
	for _, job := range s.fileTasks {
		if deploymentPending(job.Status) {
			counts["files"]++
		}
	}
	s.fileTaskMu.RUnlock()
	s.videoMu.RLock()
	for _, job := range s.videoJobs {
		if deploymentPending(job.Status) {
			counts["videos"]++
		}
	}
	s.videoMu.RUnlock()
	s.refreshMu.RLock()
	for _, job := range s.refreshProgress {
		if !job.Done {
			counts["refresh"]++
		}
	}
	s.refreshMu.RUnlock()
	s.reloginMu.RLock()
	for _, job := range s.reloginProgress {
		if !job.Done {
			counts["relogin"]++
		}
	}
	s.reloginMu.RUnlock()
	s.twofaMu.Lock()
	for _, job := range s.twofaTasks {
		if job.Status == "running" {
			counts["twofa"]++
		}
	}
	s.twofaMu.Unlock()
	s.codexMu.RLock()
	for _, job := range s.codexProgress {
		if started, err := time.Parse(time.RFC3339Nano, job.StartedAt); job.Status == "running" && (err != nil || time.Since(started) < s.codexLogin.TTL) {
			counts["codex"]++
		}
	}
	s.codexMu.RUnlock()
	s.modelRefreshMu.Lock()
	counts["models"] = len(s.modelRefreshing)
	s.modelRefreshMu.Unlock()
	if q, ok := s.taskQueue.(interface{ DeploymentActive() int }); ok {
		counts["queue"] = q.DeploymentActive()
	} else {
		counts["unknown_queue"]++
	}
	s.grokOAuthActions.Range(func(_, _ any) bool { counts["oauth_actions"]++; return true })
	s.cpaSyncing.Range(func(_, _ any) bool { counts["cpa_sync"]++; return true })
	for _, item := range s.external.listSub() {
		if item.ImportJob != nil && deploymentPending(stringValue(item.ImportJob["status"])) {
			counts["imports"]++
		}
	}
	counts["device_sessions"] = s.deviceOAuth.ActiveSessions()
	counts["openai_sessions"] = s.openAILogin.ActiveSessions()
	counts["codex_sessions"] = s.codexLogin.ActiveSessions() + s.codexProtocolLogin.ActiveSessions()
	return counts
}

// Check before workers start, while the process holds the exclusive data lease.
func (s *Server) DeploymentReady() error {
	if s.mailboxInitErr != nil {
		return errors.New("mailbox database unavailable")
	}
	if s.cfg.MailboxDatabaseURL != "" && s.mailboxStore == nil {
		return errors.New("mailbox database unavailable")
	}
	if _, err := s.store.AccountList(); err != nil {
		return errors.New("account storage unavailable")
	}
	if _, err := s.grokRuntimeStore.AccountList(); err != nil {
		return errors.New("Grok runtime storage unavailable")
	}
	if s.adobeStore != nil {
		if _, err := s.adobeStore.List(); err != nil {
			return errors.New("Adobe account storage unavailable")
		}
	}
	if s.adobeRegister != nil && s.adobeRegister.Snapshot()["error"] != nil {
		return errors.New("Adobe registration journal unavailable")
	}
	if s.freeRegister.Snapshot()["error"] != nil || s.grokRegister.Snapshot()["error"] != nil {
		return errors.New("registration journal unavailable")
	}
	if s.imageTaskLoadErr != nil {
		return errors.New("image journal unavailable")
	}
	if s.videoLoadErr != nil {
		return errors.New("video journal unavailable")
	}
	if s.cfg.QueueBackend == "redis" {
		if _, ok := s.taskQueue.(*tasks.RedisQueue); !ok {
			return errors.New("Redis queue unavailable")
		}
	}
	return nil
}

// Count detached work before its submitting HTTP handler can return.
func (s *Server) launchDeploymentWork(run func()) {
	s.deployMu.Lock()
	s.deployDetached++
	s.deployMu.Unlock()
	go func() { defer func() { s.deployMu.Lock(); s.deployDetached--; s.deployMu.Unlock() }(); run() }()
}

// SealDeployment is irreversible in this process. All future request admission
// is fenced by Instance before this method can release the writer lease.
func (s *Server) SealDeployment(ctx context.Context) error {
	for _, count := range s.DeploymentStatus() {
		if count != 0 {
			return errors.New("work still active")
		}
	}
	s.deployMu.Lock()
	if !s.deployPaused || s.deployBackground != 0 || s.deployDetached != 0 {
		s.deployMu.Unlock()
		return errors.New("runtime not paused")
	}
	s.deploySealed = true
	s.deployMu.Unlock()
	if q, ok := s.taskQueue.(interface{ StopDeployment() }); ok {
		q.StopDeployment()
	}
	s.deployStopOnce.Do(func() { close(s.probeStop) })
	if err := s.store.SealAccounts(); err != nil {
		return err
	}
	if err := s.grokRuntimeStore.SealAccounts(); err != nil {
		return err
	}
	return s.ShutdownRegistration(ctx)
}
