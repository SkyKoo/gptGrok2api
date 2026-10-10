package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// Input previews are private local copies, never signed source URLs or Base64
// embedded in the log. Keep one entry per position, including repeated inputs.
type logInputImage struct {
	Index       int    `json:"index"`
	URL         string `json:"url,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
}

func (s *Server) logInputImagesDir() string {
	return filepath.Join(s.cfg.DataDir, "log_input_images")
}

func (s *Server) recordInputImages(r *http.Request, inputs []provider.OpenAIImageInput) {
	callID, _ := r.Context().Value(monitorCallIDKey{}).(string)
	if callID == "" || len(inputs) == 0 || s.monitor == nil {
		return
	}
	previews := make([]logInputImage, len(inputs))
	dir := s.logInputImagesDir()
	dirErr := os.MkdirAll(dir, 0700)
	for i, input := range inputs {
		preview := logInputImage{Index: i + 1, Unavailable: true}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(input.Data))
		if err == nil && (format == "png" || format == "jpeg" || format == "webp" || format == "gif") {
			preview.Width, preview.Height = cfg.Width, cfg.Height
			// Names are scoped to this call and position; user-controlled file
			// names and source URLs are not used as filesystem paths.
			hash := sha256.New()
			_, _ = fmt.Fprintf(hash, "%s:%d:", callID, i)
			_, _ = hash.Write(input.Data)
			filename := hex.EncodeToString(hash.Sum(nil)) + "." + format
			if dirErr == nil && os.WriteFile(filepath.Join(dir, filename), input.Data, 0600) == nil {
				preview.URL = "/api/logs/input-images/" + filename
				preview.Filename = fmt.Sprintf("reference-%d.%s", i+1, format)
				preview.Unavailable = false
			}
		}
		previews[i] = preview
	}
	s.monitor.mu.Lock()
	defer s.monitor.mu.Unlock()
	if item := s.monitor.active[callID]; item != nil {
		item.InputImages = previews
	}
}

func (s *Server) logInputImage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/logs/input-images/")
	ext := filepath.Ext(name)
	id := strings.TrimSuffix(name, ext)
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != sha256.Size || (ext != ".png" && ext != ".jpeg" && ext != ".webp" && ext != ".gif") {
		writeError(w, http.StatusNotFound, "reference image not found", "not_found")
		return
	}
	path := filepath.Join(s.logInputImagesDir(), name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "reference image expired or unavailable", "not_found")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, path)
}
