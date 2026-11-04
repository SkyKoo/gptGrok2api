package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const VideoReviewFrameCount = 6

// Decode the actual video stream, never an attached poster. Both tools operate
// on a private local file with network protocols/playlists disabled.
func VideoReviewFrames(ctx context.Context, raw []byte, seconds int) ([]OpenAIImageInput, error) {
	if len(raw) == 0 || len(raw) > 128<<20 || seconds < 1 || seconds > 30 {
		return nil, fmt.Errorf("video review input exceeds limits")
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "cfm-video-review-*")
	if err != nil {
		return nil, fmt.Errorf("video review temporary storage unavailable")
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "input")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return nil, fmt.Errorf("video review temporary storage unavailable")
	}
	probe, err := videoToolOutput(ctx, "ffprobe", 64<<10,
		"-v", "error", "-protocol_whitelist", "file", "-format_whitelist", "mov,matroska,webm",
		"-select_streams", "V:0", "-show_entries", "stream=index,width,height,duration:format=duration", "-of", "json", path)
	if err != nil {
		return nil, fmt.Errorf("video review could not inspect video")
	}
	var info struct {
		Streams []struct {
			Index    int    `json:"index"`
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Duration string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(probe, &info) != nil || len(info.Streams) != 1 {
		return nil, fmt.Errorf("video review found no video stream")
	}
	stream := info.Streams[0]
	duration, _ := strconv.ParseFloat(stream.Duration, 64)
	if duration <= 0 {
		duration, _ = strconv.ParseFloat(info.Format.Duration, 64)
	}
	if math.IsNaN(duration) || math.IsInf(duration, 0) || duration < float64(seconds)-0.75 || duration > float64(seconds)+2 ||
		stream.Width < 1 || stream.Height < 1 || stream.Width > 4096 || stream.Height > 4096 {
		return nil, fmt.Errorf("video duration or dimensions could not be verified")
	}
	frames := make([]OpenAIImageInput, 0, VideoReviewFrameCount)
	for i := 0; i < VideoReviewFrameCount; i++ {
		at := 0.1 + (duration-0.25)*float64(i)/float64(VideoReviewFrameCount-1)
		frame, err := videoToolOutput(ctx, "ffmpeg", 2<<20,
			"-nostdin", "-v", "error", "-max_alloc", "67108864", "-threads", "1",
			"-protocol_whitelist", "file", "-format_whitelist", "mov,matroska,webm",
			"-ss", fmt.Sprintf("%.3f", at), "-i", path, "-map", fmt.Sprintf("0:%d", stream.Index),
			"-an", "-sn", "-dn", "-frames:v", "1", "-vf", "scale=640:640:force_original_aspect_ratio=decrease",
			"-filter_threads", "1", "-threads", "1", "-c:v", "mjpeg", "-q:v", "3", "-f", "image2pipe", "pipe:1")
		if err != nil {
			return nil, fmt.Errorf("video review frame extraction failed")
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(frame))
		if err != nil || format != "jpeg" || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 640 || cfg.Height > 640 {
			return nil, fmt.Errorf("video review frame is invalid")
		}
		frames = append(frames, OpenAIImageInput{Name: fmt.Sprintf("frame-%02d.jpg", i+1), MIME: "image/jpeg", Data: frame})
	}
	return frames, nil
}

type boundedVideoOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedVideoOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("video tool output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func videoToolOutput(ctx context.Context, tool string, limit int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	output := &boundedVideoOutput{limit: limit}
	cmd.Stdout, cmd.Stderr = output, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
