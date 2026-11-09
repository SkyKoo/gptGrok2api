package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

var errRepeatedVideo = errors.New("Grok 返回了与其他输入相同的视频，无法确认结果与本次提示词和参考图一致；已停止返回该视频，未自动重试")
var errRejectedVideo = errors.New("Grok 返回了曾被判定为内容不符的视频，已停止交付；未自动重试")

type videoResultRecord struct {
	RequestHash  string             `json:"request_sha256"`
	MediaHash    string             `json:"media_sha256"`
	CallID       string             `json:"call_id"`
	Model        string             `json:"model,omitempty"`
	Endpoint     string             `json:"endpoint,omitempty"`
	SourceType   string             `json:"source_type,omitempty"`
	GeneratedAt  string             `json:"generated_at,omitempty"`
	Verification *videoVerification `json:"verification,omitempty"`
	Rejected     bool               `json:"rejected,omitempty"`
}

func videoRequestFingerprint(model, prompt, aspect, resolution string, seconds int, preset string, inputs []provider.OpenAIImageInput) string {
	refs := make([]string, len(inputs))
	for i, input := range inputs {
		refs[i] = fmt.Sprintf("%x", sha256.Sum256(input.Data))
	}
	raw, _ := json.Marshal(struct {
		Model, Prompt, Aspect, Resolution, Preset string
		Seconds                                   int
		References                                []string
	}{model, prompt, aspect, resolution, preset, seconds, refs})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// Hash encoded audio/video only for MP4: signed metadata can vary while the
// returned media is byte-identical. Legacy files are streamed with bounded RAM.
func videoMediaFingerprint(reader io.ReaderAt, size int64) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("empty video")
	}
	h := sha256.New()
	var header [16]byte
	var offset int64
	found, mp4 := false, false
	if size >= 8 {
		if _, err := reader.ReadAt(header[:8], 0); err != nil {
			return "", err
		}
		mp4 = string(header[4:8]) == "ftyp"
	}
	if mp4 {
		for offset < size {
			if size-offset < 8 {
				return "", fmt.Errorf("invalid MP4 box header")
			}
			if _, err := reader.ReadAt(header[:8], offset); err != nil {
				return "", err
			}
			length, headerSize := int64(binary.BigEndian.Uint32(header[:4])), int64(8)
			kind := string(header[4:8])
			if length == 1 {
				if size-offset < 16 {
					return "", fmt.Errorf("invalid extended MP4 box")
				}
				if _, err := reader.ReadAt(header[8:16], offset+8); err != nil {
					return "", err
				}
				unsigned := binary.BigEndian.Uint64(header[8:16])
				if unsigned > uint64(size-offset) {
					return "", fmt.Errorf("invalid MP4 box length")
				}
				length, headerSize = int64(unsigned), 16
			}
			if length == 0 {
				length = size - offset
			}
			if length < headerSize || length > size-offset {
				return "", fmt.Errorf("invalid MP4 box length")
			}
			if kind == "mdat" && length > headerSize {
				if _, err := io.Copy(h, io.NewSectionReader(reader, offset+headerSize, length-headerSize)); err != nil {
					return "", err
				}
				found = true
			}
			offset += length
		}
		if !found {
			return "", fmt.Errorf("MP4 contains no media data")
		}
	} else {
		if _, err := io.Copy(h, io.NewSectionReader(reader, 0, size)); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Called with videoResultMu held. Historical content has no trusted request
// identity, so a repeat cannot be attributed to the same input.
func (s *Server) loadVideoFingerprints() error {
	if s.videoFingerprints != nil {
		return nil
	}
	entries, err := os.ReadDir(s.cfg.VideoDataDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	records := make(map[string]videoResultRecord)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || (filepath.Ext(entry.Name()) != ".mp4" && filepath.Ext(entry.Name()) != ".webm") {
			continue
		}
		path := filepath.Join(s.cfg.VideoDataDir, entry.Name())
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return statErr
		}
		hash, hashErr := videoMediaFingerprint(file, info.Size())
		_ = file.Close()
		// Incomplete legacy files are not successful results.
		if hashErr != nil {
			continue
		}
		record := videoResultRecord{MediaHash: hash}
		if raw, err := os.ReadFile(path + ".meta.json"); err == nil {
			var meta videoResultRecord
			if json.Unmarshal(raw, &meta) == nil && meta.MediaHash == hash {
				record = meta
			}
		}
		if previous, ok := records[hash]; ok && previous.RequestHash != record.RequestHash {
			record.RequestHash = ""
		}
		records[hash] = record
	}
	rejections, err := os.ReadDir(filepath.Join(s.cfg.DataDir, "video_rejections"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range rejections {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "video_rejections", entry.Name()))
		var record videoResultRecord
		if err != nil || json.Unmarshal(raw, &record) != nil || len(record.MediaHash) != 64 || entry.Name() != record.MediaHash+".json" || !record.Rejected {
			return fmt.Errorf("video rejection history unavailable")
		}
		records[record.MediaHash] = record
	}
	s.videoFingerprints = records
	return nil
}

func (s *Server) checkVideoResult(requestHash string, raw []byte) (string, error) {
	hash, err := videoMediaFingerprint(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("Grok 返回的视频文件不完整，已停止返回结果")
	}
	s.videoResultMu.Lock()
	defer s.videoResultMu.Unlock()
	if err := s.loadVideoFingerprints(); err != nil {
		return hash, err
	}
	return hash, s.checkVideoFingerprintLocked(hash, requestHash)
}

func (s *Server) checkVideoFingerprintLocked(hash, requestHash string) error {
	if previous, exists := s.videoFingerprints[hash]; exists {
		if previous.Rejected {
			return errRejectedVideo
		}
		if previous.RequestHash == "" || previous.RequestHash != requestHash {
			return errRepeatedVideo
		}
	}
	return nil
}

// Keep only the encoded-media hash of a definite mismatch, never rejected media
// bytes. Unlike same-input duplicates, a known bad result is never deliverable.
func (s *Server) rejectVideoResult(callID, requestHash, hash string, verification videoVerification) error {
	s.videoResultMu.Lock()
	defer s.videoResultMu.Unlock()
	if err := s.loadVideoFingerprints(); err != nil {
		return err
	}
	record := videoResultRecord{CallID: callID, RequestHash: requestHash, MediaHash: hash, Rejected: true, Verification: &verification}
	s.videoFingerprints[hash] = record
	return writeVideoJSON(filepath.Join(s.cfg.DataDir, "video_rejections", hash+".json"), record)
}

func (s *Server) storeVideoResult(callID, requestHash string, raw []byte, mime string, verification ...videoVerification) (string, string, error) {
	return s.storeVideoResultWithPolicy(callID, requestHash, raw, mime, true, verification...)
}

// Live Grok tasks use the shared review/cache decision. Disabling review also
// disables historical content judgements; structural media validation remains.
func (s *Server) storeVideoResultWithPolicy(callID, requestHash string, raw []byte, mime string, enforceHistory bool, verification ...videoVerification) (string, string, error) {
	hash, err := videoMediaFingerprint(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", "", fmt.Errorf("Grok 返回的视频文件不完整，已停止返回结果")
	}
	s.videoResultMu.Lock()
	defer s.videoResultMu.Unlock()
	if enforceHistory {
		if err := s.loadVideoFingerprints(); err != nil {
			return "", "", err
		}
	} else if s.videoFingerprints == nil {
		s.videoFingerprints = make(map[string]videoResultRecord)
	}
	if err := s.checkVideoFingerprintLocked(hash, requestHash); enforceHistory && err != nil {
		return "", hash, err
	}
	if err := os.MkdirAll(s.cfg.VideoDataDir, 0o755); err != nil {
		return "", "", err
	}
	ext := ".mp4"
	if strings.Contains(mime, "webm") {
		ext = ".webm"
	}
	path := filepath.Join(s.cfg.VideoDataDir, strings.TrimPrefix(callID, "video_")+ext)
	record := videoResultRecord{RequestHash: requestHash, MediaHash: hash, CallID: callID, Model: "grok-imagine-video", Endpoint: "/v1/videos", SourceType: "generated_output", GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	if len(verification) > 0 {
		record.Verification = &verification[0]
	}
	f, err := os.CreateTemp(s.cfg.VideoDataDir, ".video-*")
	if err != nil {
		return "", hash, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", hash, err
	}
	if closeErr != nil {
		return "", hash, closeErr
	}
	if err := writeVideoJSON(path+".meta.json", record); err != nil {
		return "", "", err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return "", hash, err
	}
	s.videoFingerprints[hash] = record
	return path, hash, nil
}
