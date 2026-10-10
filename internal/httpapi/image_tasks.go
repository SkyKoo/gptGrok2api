package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// This context is set only by the background worker, never by client headers.
// A stable, owner-scoped ID links the execution log to its durable task.
type imageTaskLogContextKey struct{}

type imageTaskLogContext struct {
	CallID string
	TaskID string
}

// Include the owner in both the memory key and the filename: callers may use
// identical client_task_id values without replacing another caller's task.
func imageTaskKey(owner, id string) string {
	sum := sha256.Sum256([]byte(owner + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

func imageTaskHash(task *imageTaskState) string {
	fields := []any{task.Mode, task.Model, task.N, task.Size, task.Quality, task.Prompt, task.Images}
	// Preserve persisted hashes for requests created before output options existed.
	if task.Background != "" || task.OutputFormat != "" {
		fields = append(fields, task.ImageOutputOptions)
	}
	// Unmasked requests keep their historical hashes. A different mask must
	// conflict even after restart, when the original input bytes are absent.
	if len(task.Mask) > 0 {
		fields = append(fields, struct {
			Mask []byte `json:"mask"`
		}{task.Mask})
	}
	raw, _ := json.Marshal(fields)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Server) submitImageTask(w http.ResponseWriter, r *http.Request, task *imageTaskState) {
	inputs := make([]provider.OpenAIImageInput, len(task.Images))
	for i, raw := range task.Images {
		inputs[i].Data = raw
	}
	if _, err := prepareImageMask(task.Model, task.Mask, inputs); err != nil {
		writeImageOptionError(w, err)
		return
	}
	task.ID = strings.TrimSpace(task.ID)
	if len(task.ID) > 128 || strings.ContainsAny(task.ID, "/\\?#") || task.N < 1 || task.N > 10 {
		writeError(w, http.StatusBadRequest, "invalid client_task_id or n (expected 1 to 10)", "invalid_request_error")
		return
	}
	// Old edit tasks defaulted to square, while generations stored the raw size.
	// Compare that exact legacy hash only against legacy records. Never equate a
	// new explicit square request with auto, and never rewrite or restart a match.
	legacy := *task
	if task.Mode == "edit" {
		legacy.Size = firstNonEmpty(task.Size, "1024x1024")
	}
	legacyHash := imageTaskHash(&legacy)
	if isOpenAIImageModel(task.Model) {
		task.Size = openAIImageRequestedSize(task.Size)
		task.SizeVersion = 1
		if !validOpenAIImageSize(task.Size) {
			writeError(w, http.StatusBadRequest, "invalid image size", "invalid_request_error")
			return
		}
	} else {
		task.Size = legacy.Size
	}
	task.RequestHash = imageTaskHash(task)
	key := imageTaskKey(task.OwnerID, task.ID)
	s.imageTaskMu.Lock()
	if s.imageTaskLoadErr != nil {
		s.imageTaskMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "image task storage requires recovery", "storage_error")
		return
	}
	start := false
	if previous := s.imageTasks[key]; previous != nil {
		if previous.RequestHash != task.RequestHash && !(previous.SizeVersion == 0 && previous.RequestHash == legacyHash) {
			s.imageTaskMu.Unlock()
			writeError(w, http.StatusConflict, "client_task_id already belongs to a different request", "invalid_request_error")
			return
		}
		task = previous
	} else {
		// The durable acceptance record is written before any upstream work.
		if err := s.saveImageTaskLocked(task); err != nil {
			s.imageTaskMu.Unlock()
			log.Printf("persist image task submission: %v", err)
			writeError(w, http.StatusServiceUnavailable, "could not persist image task", "storage_error")
			return
		}
		s.imageTasks[key] = task
		start = true
	}
	response := imageTaskPublic(task)
	s.imageTaskMu.Unlock()
	if start {
		ctx, cancel := s.imageTaskContext(context.Background())
		authHeader, apiKey := r.Header.Get("Authorization"), r.Header.Get("X-API-Key")
		go func() {
			defer cancel()
			s.runImageTask(ctx, task, authHeader, apiKey)
		}()
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (s *Server) imageTasksDir() string {
	return filepath.Join(s.cfg.DataDir, "image_tasks")
}

// Called while holding imageTaskMu. Credentials, prompts and input image bytes
// are intentionally excluded by imageTaskState's JSON tags.
func (s *Server) saveImageTaskLocked(task *imageTaskState) error {
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	dir := s.imageTasksDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".task-*")
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
	return os.Rename(f.Name(), filepath.Join(dir, imageTaskKey(task.OwnerID, task.ID)+".json"))
}

func (s *Server) persistFinishedImageTaskLocked(task *imageTaskState) {
	// Inputs are no longer required once a task reaches its terminal state.
	task.Prompt, task.Images, task.ImageNames, task.Mask = "", nil, nil, nil
	if err := s.saveImageTaskLocked(task); err != nil {
		log.Printf("persist image task completion: %v", err)
	}
}

func (s *Server) loadImageTasks() {
	entries, err := os.ReadDir(s.imageTasksDir())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.imageTaskLoadErr = err
		log.Printf("load image tasks: %v", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var task imageTaskState
		raw, err := os.ReadFile(filepath.Join(s.imageTasksDir(), entry.Name()))
		if err == nil {
			err = json.Unmarshal(raw, &task)
		}
		if err == nil && (task.ID == "" || task.OwnerID == "" || entry.Name() != imageTaskKey(task.OwnerID, task.ID)+".json") {
			err = fmt.Errorf("invalid image task record")
		}
		if err != nil {
			s.imageTaskLoadErr = err
			log.Printf("load image task record: %v", err)
			continue
		}
		if task.Status == "queued" || task.Status == "running" {
			// The provider has no resumable execution handle. Replaying a prompt
			// after restart could generate and charge twice; report interruption.
			task.Status = "error"
			task.Error = "service restarted before image task completed; automatic resubmission disabled"
			task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := s.saveImageTaskLocked(&task); err != nil {
				s.imageTaskLoadErr = err
				log.Printf("persist interrupted image task: %v", err)
			}
		}
		s.imageTasks[imageTaskKey(task.OwnerID, task.ID)] = &task
	}
}

func (s *Server) cleanupExpiredImageTasks() {
	cutoff := time.Now().Add(-time.Duration(max(s.cfg.ImageRetentionDays, 1)) * 24 * time.Hour)
	s.imageTaskMu.Lock()
	defer s.imageTaskMu.Unlock()
	for key, task := range s.imageTasks {
		if task.Status != "success" && task.Status != "error" {
			continue
		}
		updated, err := time.Parse(time.RFC3339, task.UpdatedAt)
		if err != nil || !updated.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.imageTasksDir(), key+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("remove expired image task: %v", err)
			continue
		}
		delete(s.imageTasks, key)
	}
}

// JSON references are decoded before durable acceptance. The worker retains
// only bytes in memory; reference URLs/base64 and credentials are not persisted.
func (s *Server) imageTaskJSONEdits(w http.ResponseWriter, r *http.Request) {
	var body struct {
		provider.ImageOutputOptions
		ClientTaskID string   `json:"client_task_id"`
		Prompt       string   `json:"prompt"`
		Model        string   `json:"model"`
		N            *int     `json:"n"`
		Size         string   `json:"size"`
		Quality      string   `json:"quality"`
		Images       []string `json:"images"`
		ImageURL     string   `json:"image_url"`
		Mask         any      `json:"mask"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ClientTaskID) == "" || strings.TrimSpace(body.Prompt) == "" {
		writeError(w, 400, "client_task_id and prompt are required", "invalid_request_error")
		return
	}
	if !validateImageOutput(w, firstNonEmpty(body.Model, "gpt-image-2"), body.ImageOutputOptions) {
		return
	}
	n := 1
	if body.N != nil {
		n = *body.N
	}
	if n < 1 || n > 2 {
		writeError(w, 400, "n must be between 1 and 2", "invalid_request_error")
		return
	}
	if body.ImageURL != "" {
		body.Images = append(body.Images, body.ImageURL)
	}
	if len(body.Images) < 1 || len(body.Images) > 7 {
		writeError(w, 400, "between 1 and 7 reference images are required", "invalid_request_error")
		return
	}
	// Bound URL downloads independently of the asynchronous generation deadline.
	// The existing downloader validates destinations and every redirect, and
	// never sends the API authorization header to a reference-image host.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	images := make([][]byte, 0, len(body.Images))
	names := make([]string, 0, len(body.Images))
	for _, ref := range body.Images {
		input, err := s.imageInputFromString(ctx, strings.TrimSpace(ref))
		if err != nil {
			// Do not expose a signed reference URL in a validation error.
			writeError(w, 400, "invalid or inaccessible reference image", "invalid_request_error")
			return
		}
		if len(input.Data) == 0 || len(input.Data) > 16<<20 {
			writeError(w, 400, "each reference image must contain 1 byte to 16 MiB", "invalid_request_error")
			return
		}
		images = append(images, input.Data)
		names = append(names, input.Name)
	}
	mask, err := s.imageMaskFromValue(ctx, body.Mask)
	if err != nil {
		writeImageOptionError(w, err)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	task := &imageTaskState{ImageOutputOptions: body.ImageOutputOptions, ID: body.ClientTaskID, OwnerID: s.authIdentity(r), Status: "queued", Mode: "edit",
		Model: firstNonEmpty(body.Model, "gpt-image-2"), N: n, Size: body.Size,
		Quality: firstNonEmpty(body.Quality, "auto"), Prompt: strings.TrimSpace(body.Prompt), Images: images,
		ImageNames: names, Mask: mask, CreatedAt: now, UpdatedAt: now}
	s.submitImageTask(w, r, task)
}
