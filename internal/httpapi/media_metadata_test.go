package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLogsRestoreHistoricalImagePreviewsWithoutRewritingHistory(t *testing.T) {
	s := imageTaskTestServer(t)
	if err := os.MkdirAll(s.cfg.ImageDataDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture := func(name, callID, source, role string, exists bool) {
		t.Helper()
		path := filepath.Join(s.cfg.ImageDataDir, name)
		if exists {
			if err := os.WriteFile(path, tinyPNG, 0600); err != nil {
				t.Fatal(err)
			}
		}
		meta, _ := json.Marshal(map[string]any{"call_id": callID, "source_type": source, "role": role})
		if err := os.WriteFile(path+".meta.json", meta, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture("a.png", "call-missing", "generated_output", "output", true)
	writeFixture("b.png", "call-missing", "generated_output", "output", true)
	writeFixture("deleted.png", "call-missing", "generated_output", "output", false)
	writeFixture("reference.png", "call-missing", "uploaded_reference", "input", true)
	writeFixture("other.png", "unrelated-call", "generated_output", "output", true)
	writeFixture("legacy.png", "call-existing", "generated_output", "output", true)
	writeFixture("fallback name.png", "fallback-id", "generated_output", "output", true)
	writeFixture("invalid.png", "call-missing", "generated_output", "output", true)
	if err := os.WriteFile(filepath.Join(s.cfg.ImageDataDir, "invalid.png.meta.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	writeFixture("linked.png", "call-missing", "generated_output", "output", false)
	if err := os.Symlink(filepath.Join(s.cfg.ImageDataDir, "a.png"), filepath.Join(s.cfg.ImageDataDir, "linked.png")); err != nil {
		t.Fatal(err)
	}
	logs := []map[string]any{
		{"id": "entry-1", "type": "call", "time": "2026-10-09T03:49:00Z", "detail": map[string]any{"call_id": "call-missing", "status": "success"}},
		{"id": "entry-2", "type": "call", "time": "2026-10-09T03:48:00Z", "detail": map[string]any{"call_id": "call-existing", "status": "success", "output_images": []map[string]string{{"url": "https://legacy.example/images/kept.png"}}}},
		{"id": "fallback-id", "type": "call", "time": "2026-10-09T03:47:00Z", "detail": map[string]any{"status": "success"}},
		{"id": "entry-4", "type": "call", "time": "2026-10-09T03:46:00Z", "detail": map[string]any{"call_id": "call-no-image", "status": "failed"}},
		{"id": "entry-5", "type": "system", "time": "2026-10-09T03:45:00Z", "detail": map[string]any{"call_id": "call-missing"}},
	}
	var original bytes.Buffer
	for _, item := range logs {
		if err := json.NewEncoder(&original).Encode(item); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(s.cfg.DataDir, "logs.jsonl")
	if err := os.WriteFile(logPath, original.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	// A fresh server has no in-memory request records; restoration uses only
	// existing files and their exact call IDs, including multiple outputs.
	s = New(s.cfg)
	get := func(path string) []map[string]any {
		t.Helper()
		response := adminRequest(s.Handler(), "GET", path, nil)
		if response.Code != 200 {
			t.Fatalf("logs request failed: %d", response.Code)
		}
		var body struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Total != len(logs) {
			t.Fatalf("log count changed: %d", body.Total)
		}
		return body.Items
	}
	for pass := 0; pass < 2; pass++ {
		items := get("/api/logs?limit=5")
		images := anyList(mapValue(items[0]["detail"])["output_images"])
		if len(images) != 2 || stringValue(mapValue(images[0])["url"]) != "/images/a.png" || stringValue(mapValue(images[1])["url"]) != "/images/b.png" {
			t.Fatalf("wrong historical association: %#v", images)
		}
		aliases := anyList(mapValue(items[0]["detail"])["image_urls"])
		if len(aliases) != 2 {
			t.Fatal("missing legacy field used by log viewers")
		}
		legacy := anyList(mapValue(items[1]["detail"])["output_images"])
		if len(legacy) != 1 || stringValue(mapValue(legacy[0])["url"]) != "https://legacy.example/images/kept.png" {
			t.Fatal("existing output links were changed")
		}
		fallback := anyList(mapValue(items[2]["detail"])["output_images"])
		if len(fallback) != 1 || stringValue(mapValue(fallback[0])["url"]) != "/images/fallback%20name.png" {
			t.Fatal("entry ID fallback or filename escaping failed")
		}
		preview := adminRequest(s.Handler(), "GET", stringValue(mapValue(fallback[0])["url"]), nil)
		if preview.Code != 200 || !bytes.Equal(preview.Body.Bytes(), tinyPNG) {
			t.Fatal("restored image URL is not readable")
		}
		for _, item := range items[3:] {
			if len(anyList(mapValue(item["detail"])["output_images"])) > 0 {
				t.Fatal("unrelated or non-call entry received an image")
			}
		}
	}
	page := get("/api/logs?offset=2&limit=1")
	if len(page) != 1 || stringValue(page[0]["id"]) != "fallback-id" || len(anyList(mapValue(page[0]["detail"])["output_images"])) != 1 {
		t.Fatal("paginated history was not restored")
	}
	if err := os.Remove(filepath.Join(s.cfg.ImageDataDir, "a.png")); err != nil {
		t.Fatal(err)
	}
	images := anyList(mapValue(get("/api/logs?limit=1")[0]["detail"])["output_images"])
	if len(images) != 1 || stringValue(mapValue(images[0])["url"]) != "/images/b.png" {
		t.Fatal("deleted image was restored as a broken preview")
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/logs", nil))
	if response.Code != 401 {
		t.Fatal("log authorization changed")
	}
	persisted, err := os.ReadFile(logPath)
	if err != nil || !bytes.Equal(persisted, original.Bytes()) {
		t.Fatal("reading historical previews rewrote the original logs")
	}
}
