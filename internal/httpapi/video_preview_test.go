package httpapi

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGalleryVideoPreviewUsesVideoDirectoryAndRange(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	if err := os.MkdirAll(cfg.VideoDataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.VideoDataDir, "clip.mp4"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.VideoDataDir, "clip.mp4.meta.json"), []byte(`{"secret":"PRIVATE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(cfg)
	for _, tc := range []struct {
		method, path, byteRange string
		code                    int
		body                    string
	}{
		{"GET", "/images/video/clip.mp4", "", 200, "0123456789"},
		{"HEAD", "/images/video/clip.mp4", "", 200, ""},
		{"GET", "/images/video/clip.mp4", "bytes=2-5", 206, "2345"},
		{"GET", "/images/video/clip.mp4", "bytes=20-30", 416, ""},
		{"GET", "/images/video/clip.mp4.meta.json", "", 404, ""},
		{"GET", "/images/video/../clip.mp4", "", 404, ""},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.byteRange != "" {
			r.Header.Set("Range", tc.byteRange)
		}
		w := httptest.NewRecorder()
		s.publicImage(w, r)
		if w.Code != tc.code || (tc.code < 400 && w.Body.String() != tc.body) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if tc.code < 400 && !strings.HasPrefix(w.Header().Get("Content-Type"), "video/mp4") {
			t.Fatalf("MIME=%s", w.Header().Get("Content-Type"))
		}
		if strings.Contains(w.Body.String(), "PRIVATE") {
			t.Fatal("metadata leaked")
		}
	}
}
