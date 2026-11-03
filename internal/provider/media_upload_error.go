package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// MediaUploadError keeps only bounded, non-secret classification evidence.
// Never retain the upstream body: it may contain cookies, HTML or signed URLs.
type MediaUploadError struct {
	Stage       string
	Status      int
	Kind        string
	ContentType string
	Challenge   bool
}

func (e *MediaUploadError) Error() string {
	reason := map[string]string{
		"verification_required":    "上传被 Cloudflare 验证拦截，当前调用会话未通过上游网页验证",
		"authentication_required":  "上传凭据被上游拒绝，需要重新验证账号登录态",
		"upload_rate_limited":      "文件上传被上游限流，请等待上传额度或冷却恢复",
		"region_restricted":        "上传接口返回地区访问限制",
		"upload_forbidden":         "上传被上游拒绝，未返回明确的验证或权限原因",
		"invalid_upload":           "上游拒绝图片上传参数或格式",
		"upload_too_large":         "图片超过上游上传大小限制",
		"upstream_unavailable":     "上游上传服务暂时不可用",
		"upload_timeout":           "上传超时，结果未确认",
		"upload_network_error":     "上传连接失败，结果未确认",
		"invalid_upload_response":  "上传接口未返回有效的文件信息",
		"invalid_upload_target":    "上游返回的文件上传地址不受支持",
		"upload_processing_failed": "上游文件处理失败，未获得可用参考图",
		"storage_upload_rejected":  "预签名文件上传被存储服务拒绝",
	}[e.Kind]
	if reason == "" {
		reason = "上游图片上传失败"
	}
	return fmt.Sprintf("Grok %s（HTTP %d，file_upload）；已停止生成", reason, e.Status)
}

func (e *MediaUploadError) Unwrap() error {
	status := e.Status
	// A storage signature refusal is not an account SSO authentication failure.
	if e.Challenge {
		status = http.StatusForbidden
	} else if e.Stage == "storage_put" && status == http.StatusUnauthorized {
		status = http.StatusBadGateway
	}
	return &protocol.UpstreamError{Endpoint: "grok_file_upload", Status: status, Message: e.Error()}
}

func MediaUploadDiagnostics(err error) map[string]any {
	var upload *MediaUploadError
	if !errors.As(err, &upload) {
		return nil
	}
	return map[string]any{"upload_stage": upload.Stage, "upload_error_kind": upload.Kind, "upload_http_status": upload.Status, "upload_content_type": upload.ContentType, "upload_challenge": upload.Challenge}
}

func readMediaUploadError(response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	text := strings.ToLower(string(raw))
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	switch contentType {
	case "application/json", "text/html", "text/plain":
	default:
		contentType = "other"
	}
	challenge := strings.EqualFold(response.Header.Get("Cf-Mitigated"), "challenge") || contentType == "text/html" && (strings.Contains(text, "challenge-platform") || strings.Contains(text, "<title>just a moment"))
	kind := "upload_failed"
	switch {
	case challenge:
		kind = "verification_required"
	case response.StatusCode == http.StatusUnauthorized:
		kind = "authentication_required"
	case response.StatusCode == http.StatusTooManyRequests:
		kind = "upload_rate_limited"
	case response.StatusCode == http.StatusForbidden && strings.Contains(text, "not available in your region"):
		kind = "region_restricted"
	case response.StatusCode == http.StatusForbidden:
		kind = "upload_forbidden"
	case response.StatusCode == http.StatusRequestEntityTooLarge:
		kind = "upload_too_large"
	case response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnsupportedMediaType || response.StatusCode == http.StatusUnprocessableEntity:
		kind = "invalid_upload"
	case response.StatusCode >= 500:
		kind = "upstream_unavailable"
	}
	return &MediaUploadError{Status: response.StatusCode, Kind: kind, ContentType: contentType, Challenge: challenge}
}

func safeMediaUploadError(err error) error {
	var upload *MediaUploadError
	if errors.As(err, &upload) {
		return upload
	}
	var upstream *protocol.UpstreamError
	if errors.As(err, &upstream) {
		return &MediaUploadError{Status: upstream.Status, Kind: "upload_failed", ContentType: "other"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &MediaUploadError{Status: 504, Kind: "upload_timeout", ContentType: "none"}
	}
	return &MediaUploadError{Status: 502, Kind: "upload_network_error", ContentType: "none"}
}
