package httpapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func videoTestMP4(media, metadata string) []byte {
	box := func(kind, data string) []byte {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, uint32(len(data)+8))
		b.WriteString(kind)
		b.WriteString(data)
		return b.Bytes()
	}
	return bytes.Join([][]byte{box("ftyp", "isom"), box("mdat", media), box("free", metadata)}, nil)
}

func TestVideoFingerprintExcludesChangingMetadata(t *testing.T) {
	a, b := videoTestMP4("same audio and video", "signature A"), videoTestMP4("same audio and video", "different signature B")
	ha, err := videoMediaFingerprint(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatal(err)
	}
	hb, err := videoMediaFingerprint(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatal("same encoded media not detected")
	}
	for _, broken := range [][]byte{a[:len(a)-1], []byte{0, 0, 0, 4, 'f', 't', 'y', 'p'}, []byte{0, 0, 0, 1, 'f', 't', 'y', 'p'}, videoTestMP4("", "")} {
		if _, err := videoMediaFingerprint(bytes.NewReader(broken), int64(len(broken))); err == nil {
			t.Fatal("broken MP4 accepted")
		}
	}
}

func TestVideoResultRejectsReplayAcrossRequestsAndRestart(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	s.cfg.VideoDataDir = filepath.Join(t.TempDir(), "videos")
	first := videoTestMP4("cat media", "signature A")
	replay := videoTestMP4("cat media", "signature B")
	if _, _, err := s.storeVideoResult("video_first", "request1", first, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.storeVideoResult("video_same", "request1", replay, "video/mp4"); err != nil {
		t.Fatal("same input was rejected", err)
	}
	if _, _, err := s.storeVideoResult("video_second", "request2", replay, "video/mp4"); !errors.Is(err, errRepeatedVideo) {
		t.Fatalf("replay accepted: %v", err)
	}
	s.videoFingerprints = nil // A restart must keep the request association.
	if _, _, err := s.storeVideoResult("video_after_restart", "request2", replay, "video/mp4"); !errors.Is(err, errRepeatedVideo) {
		t.Fatalf("restart lost guard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.VideoDataDir, "second.mp4")); !os.IsNotExist(err) {
		t.Fatal("rejected replay saved as a new video")
	}
}

func TestVideoResultDetectsLegacyAndConcurrentReplay(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		s := New(adminTestConfig(t.TempDir()))
		s.cfg.VideoDataDir = t.TempDir()
		if err := os.WriteFile(filepath.Join(s.cfg.VideoDataDir, "legacy.mp4"), videoTestMP4("cat", "old signature"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.storeVideoResult("video_new", "request", videoTestMP4("cat", "new signature"), "video/mp4"); !errors.Is(err, errRepeatedVideo) {
			t.Fatalf("legacy replay accepted: %v", err)
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		s := New(adminTestConfig(t.TempDir()))
		s.cfg.VideoDataDir = t.TempDir()
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, id := range []string{"a", "b"} {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				_, _, err := s.storeVideoResult("video_"+id, id, videoTestMP4("same", id), "video/mp4")
				results <- err
			}(id)
		}
		wg.Wait()
		close(results)
		success, duplicate := 0, 0
		for err := range results {
			if err == nil {
				success++
			} else if errors.Is(err, errRepeatedVideo) {
				duplicate++
			} else {
				t.Fatal(err)
			}
		}
		if success != 1 || duplicate != 1 {
			t.Fatalf("success=%d duplicate=%d", success, duplicate)
		}
	})
}

func TestVideoRequestFingerprintIncludesReferencesAndSettings(t *testing.T) {
	a := videoRequestFingerprint("model", "prompt", "9:16", "720p", 6, "custom", []provider.OpenAIImageInput{{Data: []byte("a")}})
	b := videoRequestFingerprint("model", "prompt", "9:16", "720p", 6, "custom", []provider.OpenAIImageInput{{Data: []byte("b")}})
	c := videoRequestFingerprint("model", "prompt", "9:16", "720p", 10, "custom", []provider.OpenAIImageInput{{Data: []byte("a")}})
	if a == b || a == c {
		t.Fatal("request identity omitted inputs")
	}
}
