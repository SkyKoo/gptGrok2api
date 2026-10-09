package httpapi

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var generatedMediaMetaMu sync.Mutex

func (s *Server) recordGeneratedMedia(ctx context.Context, result map[string]string) {
	rawURL := strings.TrimSpace(result["url"])
	if rawURL == "" {
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	id := strings.TrimSpace(parsed.Query().Get("id"))
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(parsed.Path), filepath.Ext(parsed.Path))
	}
	entries, _ := os.ReadDir(s.cfg.ImageDataDir)
	filename := ""
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), id+".") && !strings.HasSuffix(entry.Name(), ".meta.json") {
			filename = entry.Name()
			break
		}
	}
	if filename == "" {
		return
	}
	callID, _ := ctx.Value(monitorCallIDKey{}).(string)
	endpoint, model := "", ""
	if callID != "" {
		s.monitor.addOutputImage(callID, localImageLogOutput(filename))
		if record, ok := s.monitor.detail(callID); ok {
			endpoint = record.Endpoint
			model = record.Model
		}
	}
	meta := map[string]any{"call_id": callID, "endpoint": endpoint, "model": model, "generated_at": time.Now().UTC().Format(time.RFC3339), "source_type": "generated_output", "role": "output"}
	b, _ := json.Marshal(meta)
	generatedMediaMetaMu.Lock()
	defer generatedMediaMetaMu.Unlock()
	_ = os.WriteFile(filepath.Join(s.cfg.ImageDataDir, filename+".meta.json"), b, 0o600)
}

func mediaMetadata(path string) map[string]any {
	b, err := os.ReadFile(path + ".meta.json")
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func localImageLogOutput(filename string) map[string]string {
	return map[string]string{"url": "/images/" + url.PathEscape(filename), "filename": filename}
}

// Restore previews for older calls whose Base64 response could not supply URLs.
// This is a read-time join: it never rewrites logs or recreates missing images.
func (s *Server) restoreLogImageOutputs(items []map[string]any) {
	missing := map[string][]map[string]any{}
	for _, item := range items {
		if stringValue(item["type"]) != "call" {
			continue
		}
		detail := mapValue(item["detail"])
		if len(detail) == 0 || len(anyList(detail["output_images"])) > 0 || len(anyList(detail["image_urls"])) > 0 {
			continue
		}
		callID := firstNonEmpty(stringValue(detail["call_id"]), stringValue(item["id"]))
		if callID != "" {
			missing[callID] = append(missing[callID], detail)
		}
	}
	if len(missing) == 0 {
		return
	}
	outputs := map[string][]map[string]string{}
	entries, _ := os.ReadDir(s.cfg.ImageDataDir)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !isImageStorageFile(entry.Name()) {
			continue
		}
		meta := mediaMetadata(filepath.Join(s.cfg.ImageDataDir, entry.Name()))
		callID := stringValue(meta["call_id"])
		if len(missing[callID]) == 0 || stringValue(meta["source_type"]) != "generated_output" || stringValue(meta["role"]) != "output" {
			continue
		}
		outputs[callID] = append(outputs[callID], localImageLogOutput(entry.Name()))
	}
	for callID, images := range outputs {
		for _, detail := range missing[callID] {
			detail["output_images"] = images
			detail["image_urls"] = images
		}
	}
}
