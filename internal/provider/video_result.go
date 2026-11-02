package provider

import (
	"crypto/sha256"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// VideoResult keeps progress, IDs and the final URL on one candidate. The
// single-result API disables side-by-side but still ignores unsolicited slots.
type VideoResult struct {
	parentID           string
	VideoID            string
	PostID             string
	AssetID            string
	URL                string
	Progress           int
	ParentVerified     bool
	UpstreamPromptHash string
}

func NewVideoResult(parentID string) *VideoResult { return &VideoResult{parentID: parentID} }

func (v *VideoResult) Observe(e protocol.MediaEvent) error {
	if e.Kind != "video" || e.SideBySideIndex != 0 {
		return nil
	}
	mismatch := func() error {
		return &protocol.UpstreamError{Status: 502, Message: "Grok 视频结果与当前任务关联冲突，已停止返回结果；请检查任务日志"}
	}
	if e.ParentPostID != "" {
		if e.ParentPostID != v.parentID {
			return mismatch()
		}
		v.ParentVerified = true
	}
	if e.VideoID != "" && v.VideoID != "" && e.VideoID != v.VideoID {
		return mismatch()
	}
	if e.PostID != "" && v.PostID != "" && e.PostID != v.PostID {
		return mismatch()
	}
	if e.AssetID != "" && v.AssetID != "" && e.AssetID != v.AssetID {
		return mismatch()
	}
	if e.VideoPrompt != "" {
		v.UpstreamPromptHash = fmt.Sprintf("%x", sha256.Sum256([]byte(e.VideoPrompt)))
	}
	if e.VideoID != "" {
		v.VideoID = e.VideoID
	}
	if e.PostID != "" {
		v.PostID = e.PostID
	}
	if e.AssetID != "" {
		v.AssetID = e.AssetID
	}
	if e.Progress > v.Progress {
		v.Progress = e.Progress
	}
	// Preview URLs never become final merely because a later event completes.
	if e.Progress >= 100 && e.URL != "" {
		v.URL = e.URL
	}
	return nil
}

func (v *VideoResult) Complete() bool {
	return v.Progress >= 100 && (v.URL != "" || v.AssetID != "" || v.PostID != "" || v.VideoID != "")
}

func (v *VideoResult) ResultPostID() string {
	if v.PostID != "" {
		return v.PostID
	}
	return v.VideoID
}
