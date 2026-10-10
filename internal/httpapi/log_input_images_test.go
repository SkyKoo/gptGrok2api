package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func assertLogInputBytes(t *testing.T, s *Server, detail map[string]any, inputs [][]byte) {
	t.Helper()
	raw, _ := json.Marshal(detail["input_images"])
	var previews []logInputImage
	_ = json.Unmarshal(raw, &previews)
	if len(previews) != len(inputs) {
		t.Fatalf("input previews: got %d, want %d", len(previews), len(inputs))
	}
	for i, preview := range previews {
		dimensions := decodeImagePixelSize(inputs[i], i+1)
		if preview.Index != i+1 || preview.Width != dimensions.Width || preview.Height != dimensions.Height || preview.Unavailable {
			t.Fatalf("wrong input order/dimensions: %#v", preview)
		}
		response := imageTaskCall(s, "GET", preview.URL, "admin-secret", "")
		if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), inputs[i]) {
			t.Fatal("preview must return the exact original bytes")
		}
		for _, key := range []string{"", "api-secret"} {
			if result := imageTaskCall(s, "GET", preview.URL, key, ""); result.Code != 401 {
				t.Fatal("input preview exposed without admin authentication")
			}
		}
	}
}

func TestLogInputImagesPersistOrderedPrivateReferences(t *testing.T) {
	s := imageTaskTestServer(t)
	callID := "test-input-preview"
	s.monitor.start(callID, "/v1/images/edits", "gpt-image-2", "edit")
	r := httptest.NewRequest("POST", "/v1/images/edits", nil)
	r = r.WithContext(context.WithValue(r.Context(), monitorCallIDKey{}, callID))
	inputs := []provider.OpenAIImageInput{
		{Name: "../../private.png?token=secret", Data: tinyPNG},
		{Name: "same-input.png", Data: tinyPNG},
	}
	s.recordInputImages(r, inputs)
	s.monitor.finish(callID, "failed", "", "", "upstream failed")
	record, ok := s.monitor.detail(callID)
	if !ok {
		t.Fatal("missing record")
	}
	s.appendCallLog(record, 502, nil, nil, "upstream failed")
	// No in-memory association is needed after restart, even for failed calls.
	reloaded := New(s.cfg)
	response := imageTaskCall(reloaded, "GET", "/api/logs", "admin-secret", "")
	var result struct{ Items []map[string]any }
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Items) != 1 {
		t.Fatal("missing log")
	}
	detail := mapValue(result.Items[0]["detail"])
	assertLogInputBytes(t, reloaded, detail, [][]byte{tinyPNG, tinyPNG})
	if strings.Contains(response.Body.String(), "token=secret") || strings.Contains(response.Body.String(), "base64") || len(anyList(detail["output_images"])) != 0 {
		t.Fatal("input information leaked into raw log or outputs")
	}
	previews := anyList(detail["input_images"])
	firstURL := stringValue(mapValue(previews[0])["url"])
	secondURL := stringValue(mapValue(previews[1])["url"])
	if firstURL == secondURL {
		t.Fatal("repeated input lost its position")
	}
	filename := filepath.Base(firstURL)
	public := imageTaskCall(s, "GET", "/images/"+filename, "", "")
	if public.Code != 404 {
		t.Fatal("input exposed through public output route")
	}
	if result := imageTaskCall(s, "POST", firstURL, "admin-secret", ""); result.Code != 405 {
		t.Fatal("accepted input write")
	}
	if result := imageTaskCall(s, "GET", "/api/logs/input-images/invalid.png", "admin-secret", ""); result.Code != 404 {
		t.Fatal("invalid filename accepted")
	}
	path := filepath.Join(s.logInputImagesDir(), filename)
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("input preview file permissions")
	}
	// Retention applies to private inputs, but does not remove fresh references.
	s.cfg.ImageRetentionDays = 1
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	preview := s.cleanupRetentionFiles(1, 1, true)
	if mapValue(preview["images"])["removed"] != 1 {
		t.Fatal("manual retention omitted input")
	}
	s.cleanupExpiredImages()
	if result := imageTaskCall(s, "GET", firstURL, "admin-secret", ""); result.Code != 404 {
		t.Fatal("expired input kept")
	}
	if result := imageTaskCall(s, "GET", secondURL, "admin-secret", ""); result.Code != 200 {
		t.Fatal("fresh input removed")
	}
	// Even an otherwise valid filename cannot be a link to another file.
	if err := os.Symlink(filepath.Join(s.logInputImagesDir(), filepath.Base(secondURL)), path); err != nil {
		t.Fatal(err)
	}
	if result := imageTaskCall(s, "GET", firstURL, "admin-secret", ""); result.Code != 404 {
		t.Fatal("symlink served")
	}
}

func TestLogInputStorageFailureKeepsUnavailablePosition(t *testing.T) {
	s := imageTaskTestServer(t)
	if err := os.WriteFile(s.logInputImagesDir(), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	s.monitor.start("call", "/v1/images/edits", "gpt-image-2", "edit")
	r := httptest.NewRequest("POST", "/v1/images/edits", nil)
	r = r.WithContext(context.WithValue(r.Context(), monitorCallIDKey{}, "call"))
	s.recordInputImages(r, []provider.OpenAIImageInput{{Data: tinyPNG}, {Data: []byte("invalid")}})
	record, _ := s.monitor.detail("call")
	if len(record.InputImages) != 2 {
		t.Fatal("missing positions")
	}
	for i, input := range record.InputImages {
		if input.Index != i+1 || !input.Unavailable || input.URL != "" {
			t.Fatal("storage failure hidden")
		}
	}
}
