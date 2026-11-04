package provider

import (
	"bytes"
	"context"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestVideoReviewFramesDecodeActualVideo(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required for video decoder regression", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "sample.mp4")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=8", "-t", "6", "-c:v", "mpeg4", "-threads", "1", file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v %s", err, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := VideoReviewFrames(ctx, raw, 6)
	if err != nil || len(frames) != VideoReviewFrameCount {
		t.Fatalf("frames=%d err=%v", len(frames), err)
	}
	for _, frame := range frames {
		if _, _, err := image.Decode(bytes.NewReader(frame.Data)); err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Equal(frames[0].Data, frames[len(frames)-1].Data) {
		t.Fatal("only a poster or repeated frame was sampled")
	}
	if _, err := VideoReviewFrames(ctx, raw, 15); err == nil {
		t.Fatal("shorter video accepted")
	}
	for _, invalid := range [][]byte{nil, []byte("not a video"), []byte("#EXTM3U\nhttp://127.0.0.1/private.mp4")} {
		if _, err := VideoReviewFrames(ctx, invalid, 6); err == nil {
			t.Fatal("invalid media accepted")
		}
	}
}
