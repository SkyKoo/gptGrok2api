package httpapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type videoCheckpoint struct {
	Job         videoJob      `json:"job"`
	ContentFile string        `json:"content_file,omitempty"`
	Record      monitorRecord `json:"record"`
	LogWritten  bool          `json:"log_written"`
}

func videoJournalName(id string) string { return videoDigest(id) + ".json" }

func writeVideoJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".video-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// Called under videoMu. Prompts, input bytes, account credentials, upstream
// URLs and raw stream bodies never enter the journal or its diagnostic log.
func (s *Server) saveVideoCheckpointLocked(job *videoJob) error {
	copy := *job
	copy.Prompt, copy.VideoURL = "", ""
	record, ok := s.monitor.detail(job.ID)
	if job.savedRecord != nil && (!ok || job.Status == "completed" || job.Status == "failed") {
		record = *job.savedRecord
	}
	checkpoint := videoCheckpoint{Job: copy, Record: record, LogWritten: job.logWritten}
	if job.ContentPath != "" {
		checkpoint.ContentFile = filepath.Base(job.ContentPath)
	}
	return writeVideoJSON(filepath.Join(s.cfg.DataDir, "video_tasks", videoJournalName(job.ID)), checkpoint)
}

func (s *Server) loadVideoTasks() {
	entries, err := os.ReadDir(filepath.Join(s.cfg.DataDir, "video_tasks"))
	if err != nil && !os.IsNotExist(err) {
		s.videoLoadErr = err
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var saved videoCheckpoint
		raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "video_tasks", entry.Name()))
		if err == nil {
			err = json.Unmarshal(raw, &saved)
		}
		if err != nil || saved.Job.ID == "" || entry.Name() != videoJournalName(saved.Job.ID) || saved.Record.CallID != saved.Job.ID {
			s.videoLoadErr = fmt.Errorf("invalid video checkpoint")
			continue
		}
		job := saved.Job
		job.logWritten = saved.LogWritten
		if saved.ContentFile != "" && filepath.Base(saved.ContentFile) == saved.ContentFile {
			job.ContentPath = filepath.Join(s.cfg.VideoDataDir, saved.ContentFile)
		}
		s.videoJobs[job.ID] = &job
		if job.Status == "completed" || job.Status == "failed" {
			// Uncommitted terminal logs are recovered by the active writer only.
			job.savedRecord = &saved.Record
		} else {
			s.monitor.active[job.ID] = &saved.Record
		}
	}
	s.loadLegacyVideoTasks()
}

// Older releases kept job state only in memory. Rebuild a download index from
// their saved media metadata without inventing logs or modifying business data.
func (s *Server) loadLegacyVideoTasks() {
	entries, err := os.ReadDir(s.cfg.VideoDataDir)
	if err != nil && !os.IsNotExist(err) {
		s.videoLoadErr = err
		return
	}
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if !entry.Type().IsRegular() || (ext != ".mp4" && ext != ".webm") {
			continue
		}
		id := "video_" + strings.TrimSuffix(entry.Name(), ext)
		if _, exists := s.videoJobs[id]; exists {
			continue
		}
		created, err := strconv.ParseInt(strings.TrimPrefix(id, "video_"), 10, 64)
		if err != nil || created <= 0 {
			continue
		}
		path := filepath.Join(s.cfg.VideoDataDir, entry.Name())
		raw, err := os.ReadFile(path + ".meta.json")
		var meta videoResultRecord
		if err != nil || json.Unmarshal(raw, &meta) != nil || meta.CallID != id || meta.Rejected {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			continue
		}
		hash, hashErr := videoMediaFingerprint(file, info.Size())
		_ = file.Close()
		if hashErr != nil || hash != meta.MediaHash {
			continue
		}
		s.videoJobs[id] = &videoJob{ID: id, Object: "video", Model: "grok-imagine-video",
			CreatedAt: created / 1_000_000_000, CompletedAt: info.ModTime().Unix(),
			Status: "completed", Progress: 100, ContentPath: path, logWritten: true,
			Verification: meta.Verification}
	}
}

func (s *Server) recoverVideoTasks() {
	s.videoMu.Lock()
	pending := []*videoJob{}
	for _, job := range s.videoJobs {
		if job.Status != "completed" && job.Status != "failed" {
			pending = append(pending, job)
		} else if !job.logWritten && job.savedRecord != nil {
			s.writeVideoLogLocked(job, *job.savedRecord)
		}
	}
	s.videoMu.Unlock()
	for _, job := range pending {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "video_interrupted", "message": "服务重启前视频任务未完成，结果未知；未自动重发生成请求"})
	}
}

// One atomic file per task makes terminal log recovery idempotent. It is read
// alongside logs.jsonl by the existing log API and can be deleted independently.
func (s *Server) writeVideoLogLocked(job *videoJob, record monitorRecord) {
	status := http.StatusOK
	if job.Status == "failed" {
		status = http.StatusBadGateway
	}
	entry := callLogEntry(record, status, record.RequestMeta, nil, record.Error)
	path := filepath.Join(s.cfg.DataDir, "video_logs", videoJournalName(job.ID))
	s.logMu.Lock()
	err := writeVideoJSON(path, entry)
	s.logMu.Unlock()
	if err != nil {
		log.Printf("video terminal log persistence failed: task=%s", job.ID)
		return
	}
	job.logWritten = true
	if err := s.saveVideoCheckpointLocked(job); err != nil {
		log.Printf("video terminal checkpoint persistence failed: task=%s", job.ID)
	}
}

func (s *Server) loadVideoCallLogs() []map[string]any {
	entries, _ := os.ReadDir(filepath.Join(s.cfg.DataDir, "video_logs"))
	items := []map[string]any{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "video_logs", entry.Name()))
		var item map[string]any
		if err == nil && json.Unmarshal(raw, &item) == nil && videoJournalName(stringValue(item["id"])) == entry.Name() {
			items = append(items, item)
		}
	}
	return items
}

func (s *Server) videoStage(job *videoJob, stage string, metrics map[string]any) bool {
	s.videoMu.Lock()
	defer s.videoMu.Unlock()
	s.monitor.update(job.ID, stage, -1, "")
	s.monitor.enrich(job.ID, map[string]any{"metrics": metrics})
	if err := s.saveVideoCheckpointLocked(job); err != nil {
		log.Printf("video stage persistence failed: task=%s stage=%s", job.ID, stage)
		return false
	}
	return true
}
