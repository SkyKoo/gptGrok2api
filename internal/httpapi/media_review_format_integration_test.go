package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

const noisyMatchedReview = "Here is the relevance check:\n```JSON\n" + `{"Verdict":" MATCHED ","reason":"The main subject is related.","framesReviewed":"99","references_reviewed":null,"expected":"test pattern","observed":"test pattern","basis":"主要画面相关","confidence":0.99,}` + "\n```\nReview complete."

func formattingReviewServer(t *testing.T) (*Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	s, uploads := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Error("harmless formatting triggered another review")
		}
		event, _ := json.Marshal(map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "content": map[string]any{"parts": []string{noisyMatchedReview}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\ndata: [DONE]\n", event)
	})
	return s, calls, uploads
}

func TestGrokImageFormattingToleranceDeliversWithoutRetry(t *testing.T) {
	s, calls, uploads := formattingReviewServer(t)
	enableGrokReview(t, s)
	grokTestAccounts(t, s)
	callID := "image-format-tolerance"
	s.monitor.start(callID, "/v1/images/generations", "grok-imagine-image", "fixture")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil).WithContext(context.WithValue(context.Background(), monitorCallIDKey{}, callID))
	generations := 0
	got, err := s.generateReviewedGrokImages(r, "test pattern", "grok-imagine-image", "1024x1024", "b64_json", 1, nil, func(_ context.Context, lease *accounts.Lease, n int) ([]provider.ImageResult, error) {
		generations++
		s.grokAccountPool.MarkSent(lease, "image_gen")
		return []provider.ImageResult{{Base64: base64.StdEncoding.EncodeToString(tinyPNG), MIME: "image/png"}}, nil
	})
	if err != nil || len(got) != 1 || got[0]["b64_json"] != base64.StdEncoding.EncodeToString(tinyPNG) || generations != 1 || calls.Load() != 1 || uploads.Load() != 1 || len(s.videoReviewSlots) != 0 {
		t.Fatalf("correct image lost or retried: err=%v outputs=%d generations=%d reviews=%d uploads=%d", err, len(got), generations, calls.Load(), uploads.Load())
	}
	assertReviewFormattingEvidence(t, s, callID, 1, 0)
}

func TestGrokVideoFormattingToleranceUsesOneReview(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "sample.mp4")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=8", "-t", "6", "-c:v", "mpeg4", "-threads", "1", file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	s, calls, uploads := formattingReviewServer(t)
	callID := "video-format-tolerance"
	s.monitor.start(callID, "/v1/videos", "grok-imagine-video", "fixture")
	refs := []provider.OpenAIImageInput{{Name: "ref.png", MIME: "image/png", Data: tinyPNG}}
	got := s.reviewGrokMedia(ctx, callID, "video", "test-intent", "test-media", "test pattern", refs, raw, 6, 1, 1)
	if got.Verdict != "matched" || got.parseError != "" || got.Frames != 6 || got.References != 1 || calls.Load() != 1 || uploads.Load() != 7 || len(s.videoReviewSlots) != 0 {
		t.Fatalf("correct video rejected or reviewed again: %+v reviews=%d uploads=%d", got, calls.Load(), uploads.Load())
	}
	assertReviewFormattingEvidence(t, s, callID, 6, 1)
}

func assertReviewFormattingEvidence(t *testing.T, s *Server, callID string, images, references int) {
	t.Helper()
	record, ok := s.monitor.detail(callID)
	if !ok || len(record.GrokMediaReviews) != 1 {
		t.Fatal("review evidence not retained")
	}
	e := record.GrokMediaReviews[0]
	if len(e.Checks) != 1 || e.Checks[0].Response != noisyMatchedReview || e.Verdict != "matched" || len(e.Images) != images || len(e.References) != references {
		t.Fatalf("raw review or attachments lost: %+v", e)
	}
	for _, note := range []string{"json_envelope_removed", "trailing_commas_removed", "normalized_verdict", "frames_reviewed_uses_actual_count", "references_reviewed_uses_actual_count"} {
		if !reviewHasNote(e.Checks[0].ParseNotes, note) {
			t.Fatalf("missing saved normalization %q: %+v", note, e.Checks[0])
		}
	}
}
