package provider

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// VideoDiagnostics keeps fixed classifications and counters only. Response
// text, arbitrary object fields, URLs and credentials never leave the parser.
type VideoDiagnostics struct {
	core          *liteDiagnostics
	VideoEvents   int  `json:"video_events"`
	Moderated     bool `json:"moderated"`
	Rejected      bool `json:"rejected"`
	URLPresent    bool `json:"url_present"`
	PostIDPresent bool `json:"post_id_present"`
	MaxProgress   int  `json:"max_progress"`
	UserIDPresent bool `json:"user_id_present"`
}

func NewVideoDiagnostics(status int, contentType string, userIDPresent bool) *VideoDiagnostics {
	return &VideoDiagnostics{core: newLiteDiagnostics(status, contentType), UserIDPresent: userIDPresent}
}

func (d *VideoDiagnostics) Observe(line string) {
	d.core.observe(line)
	d.Moderated = d.Moderated || d.core.Moderated
	_, events := protocol.ParseSSELine(line)
	for _, event := range events {
		if event.Kind != "video" {
			continue
		}
		d.VideoEvents++
		d.Moderated = d.Moderated || event.Moderated
		switch strings.ToUpper(strings.TrimSpace(event.RejectionReason)) {
		case "", "NONE", "UNSPECIFIED", "REJECTION_REASON_UNSPECIFIED":
		default:
			d.Rejected = true
		}
		d.URLPresent = d.URLPresent || event.URL != ""
		d.PostIDPresent = d.PostIDPresent || event.PostID != "" || event.ImageID != ""
		if event.Progress > d.MaxProgress {
			d.MaxProgress = event.Progress
		}
	}
}

func (d *VideoDiagnostics) Failure() error {
	if d.core.Failure != "" || d.Moderated || d.Rejected {
		return d.Err("upstream_error")
	}
	return nil
}

func (d *VideoDiagnostics) Err(reason string) error {
	if d.core.Failure != "" {
		reason = d.core.Failure
	} else if d.Moderated {
		reason = "moderated"
	} else if d.Rejected {
		reason = "rejected"
	}
	status := 502
	label := "Grok 视频未返回可用结果"
	switch reason {
	case "rate_limited":
		status = 429
		label = "Grok 视频额度已耗尽或被上游限流"
	case "authentication_failed":
		status = 401
		label = "Grok 视频请求的登录凭据无效"
	case "permission_denied":
		status = 403
		label = "Grok 上游拒绝视频访问权限"
	case "moderated":
		status = 422
		label = "Grok 视频被上游内容审核拦截"
	case "rejected":
		status = 422
		label = "Grok 上游拒绝本次视频生成"
	case "stream_read_failed":
		label = "Grok 视频响应流中断，生成结果未知"
	case "user_id_missing":
		label = "Grok 返回了视频标识但未返回地址，账号缺少定位视频所需的用户标识"
	}
	summary := map[string]any{"reason": reason, "http_status": d.core.HTTPStatus, "format": d.core.Format, "lines": d.core.Lines, "json_events": d.core.JSONEvents, "invalid_json": d.core.InvalidJSON, "text_events": d.core.TextEvents, "done": d.core.Done, "video": d}
	raw, _ := json.Marshal(summary)
	return &protocol.UpstreamError{Status: status, Message: fmt.Sprintf("%s（video_gen）；diagnostics=%s", label, raw)}
}
