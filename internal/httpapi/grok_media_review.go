package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// A failed content review is terminal. Generation is never replayed.
const grokMediaGenerationAttempts = 1
const grokReviewPolicyVersion = 2

// Evidence belongs to the administrator log, never a public task/result response.
type grokMediaReview struct {
	Kind             string               `json:"kind"`
	Attempt          int                  `json:"attempt"`
	Output           int                  `json:"output"`
	Verdict          string               `json:"verdict"`
	Reason           string               `json:"reason"`
	Expected         string               `json:"expected,omitempty"`
	Observed         string               `json:"observed,omitempty"`
	Basis            string               `json:"basis,omitempty"`
	Model            string               `json:"model,omitempty"`
	MediaHash        string               `json:"media_sha256"`
	FileHash         string               `json:"file_sha256"`
	RequestHash      string               `json:"request_sha256"`
	CacheHit         bool                 `json:"cache_hit"`
	SourceCallID     string               `json:"source_call_id,omitempty"`
	Confirmed        bool                 `json:"confirmed"`
	Checks           []grokReviewResponse `json:"checks,omitempty"`
	Images           []logInputImage      `json:"images,omitempty"`
	References       []logInputImage      `json:"references,omitempty"`
	PersistenceError string               `json:"persistence_error,omitempty"`
	At               string               `json:"at"`
}

type grokReviewCache struct {
	Version     int             `json:"version"`
	Kind        string          `json:"kind"`
	MediaHash   string          `json:"media_sha256"`
	RequestHash string          `json:"request_sha256"`
	Evidence    grokMediaReview `json:"evidence"`
}

func grokImageFingerprint(raw []byte) (string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 25_000_000 {
		return "", fmt.Errorf("invalid or oversized Grok image")
	}
	// Do not collapse an animation onto its first frame.
	if format == "gif" {
		return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("invalid Grok image")
	}
	h := sha256.New()
	fmt.Fprintf(h, "grok-pixels-v1:%d:%d:", cfg.Width, cfg.Height)
	var pixel [8]byte
	for y := decoded.Bounds().Min.Y; y < decoded.Bounds().Max.Y; y++ {
		for x := decoded.Bounds().Min.X; x < decoded.Bounds().Max.X; x++ {
			r, g, b, a := decoded.At(x, y).RGBA()
			binary.BigEndian.PutUint16(pixel[0:2], uint16(r))
			binary.BigEndian.PutUint16(pixel[2:4], uint16(g))
			binary.BigEndian.PutUint16(pixel[4:6], uint16(b))
			binary.BigEndian.PutUint16(pixel[6:8], uint16(a))
			h.Write(pixel[:])
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (s *Server) grokReviewCachePath(kind, hash, requestHash string) string {
	// All path components are fixed or digested, never upstream paths.
	key := videoDigest(fmt.Sprintf("%d:%s:%s:%s:%s", grokReviewPolicyVersion, s.cfg.VideoReviewModel, kind, hash, requestHash))
	return filepath.Join(s.cfg.DataDir, "grok_media_reviews", key+".json")
}

func (s *Server) cachedGrokReview(kind, hash, requestHash string) (grokMediaReview, bool) {
	raw, err := os.ReadFile(s.grokReviewCachePath(kind, hash, requestHash))
	var entry grokReviewCache
	if err != nil || json.Unmarshal(raw, &entry) != nil || entry.Version != grokReviewPolicyVersion || entry.Kind != kind || entry.MediaHash != hash || entry.RequestHash != requestHash {
		return grokMediaReview{}, false
	}
	e := entry.Evidence
	// An unrelated image can be correct for a different prompt. Cache only an
	// exact content + intent pair, including reference bytes and model settings.
	if e.Verdict != "matched" && !(e.Verdict == "mismatch" && e.Confirmed) {
		return grokMediaReview{}, false
	}
	return e, true
}

func (s *Server) recordGrokReview(callID string, evidence grokMediaReview) {
	s.monitor.mu.Lock()
	defer s.monitor.mu.Unlock()
	if item := s.monitor.active[callID]; item != nil {
		item.GrokMediaReviews = append(append([]grokMediaReview{}, item.GrokMediaReviews...), evidence)
	}
}

// Hash lookup happens before reserving a reviewer, uploading, or extracting frames.
func (s *Server) reviewGrokMedia(ctx context.Context, callID, kind, requestHash, hash, prompt string, references []provider.OpenAIImageInput, raw []byte, seconds, attempt, output int) videoVerification {
	e := grokMediaReview{Kind: kind, Attempt: attempt, Output: output, MediaHash: hash, FileHash: fmt.Sprintf("%x", sha256.Sum256(raw)), RequestHash: requestHash, At: time.Now().UTC().Format(time.RFC3339Nano)}
	result := videoVerification{Verdict: "uncertain", Reason: "review_unavailable", References: len(references)}
	if cached, ok := s.cachedGrokReview(kind, hash, requestHash); ok {
		e.Verdict, e.Reason, e.Expected, e.Observed, e.Basis, e.Model = cached.Verdict, cached.Reason, cached.Expected, cached.Observed, cached.Basis, cached.Model
		e.Confirmed, e.CacheHit, e.SourceCallID = cached.Confirmed, true, cached.SourceCallID
		// Existing private previews may have expired; UI explicitly reports that.
		e.Images, e.References = cached.Images, cached.References
		result.Verdict, result.Reason, result.Model, result.confirmed = e.Verdict, e.Reason, e.Model, e.Confirmed
		result.expected, result.observed, result.basis = e.Expected, e.Observed, e.Basis
		result.Frames = 1
		if kind == "video" {
			result.Frames = provider.VideoReviewFrameCount
		}
		s.recordGrokReview(callID, e)
		return result
	}
	if kind == "video" {
		verifier, err := s.newVideoVerifier(ctx, len(references))
		if err == nil {
			result = func() videoVerification {
				defer verifier.Close()
				return verifier.Verify(ctx, prompt, references, raw, seconds)
			}()
		} else {
			result.checks = []grokReviewResponse{{Verdict: "uncertain", Reason: "review_unavailable", Error: err.Error()}}
		}
	} else {
		verifier, err := s.newImageVerifier(ctx, len(references))
		if err == nil {
			result = func() videoVerification {
				defer verifier.Close()
				return verifier.VerifyImage(ctx, prompt, references, provider.OpenAIImageInput{Name: "generated-image", Data: raw, MIME: imageMIME(raw)})
			}()
		} else {
			result.checks = []grokReviewResponse{{Verdict: "uncertain", Reason: "review_unavailable", Error: err.Error()}}
		}
	}
	// Only a confirmed mismatch can regenerate or poison the hash cache.
	if result.Verdict == "mismatch" && !result.confirmed {
		result.Verdict, result.Reason = "uncertain", "mismatch_not_confirmed"
	}
	e.Verdict, e.Reason, e.Expected, e.Observed, e.Basis, e.Model = result.Verdict, result.Reason, result.expected, result.observed, result.basis, result.Model
	e.Confirmed, e.Checks, e.SourceCallID = result.confirmed, result.checks, callID
	e.Images = s.storeLogImages(fmt.Sprintf("%s:%s:%d:%d", callID, kind, attempt, output), result.samples)
	e.References = s.storeLogImages(callID+":review-references", references)
	if result.Verdict == "matched" || result.Verdict == "mismatch" && result.confirmed {
		cached := e
		cached.Checks = nil
		entry := grokReviewCache{Version: grokReviewPolicyVersion, Kind: kind, MediaHash: hash, RequestHash: requestHash, Evidence: cached}
		if err := writeVideoJSON(s.grokReviewCachePath(kind, hash, requestHash), entry); err != nil {
			e.PersistenceError = "hash_cache_write_failed"
		}
	}
	s.recordGrokReview(callID, e)
	return result
}

func imageMIME(raw []byte) string {
	_, format, _ := image.DecodeConfig(bytes.NewReader(raw))
	if format == "jpeg" {
		return "image/jpeg"
	}
	return "image/" + format
}
