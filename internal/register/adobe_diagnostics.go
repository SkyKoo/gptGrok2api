package register

import "strings"

// Adobe diagnostics are an allowlisted projection; never log raw Worker JSON,
// challenge content, URLs, credentials or upstream response text.
type adobeRegistrationDiagnostics struct {
	SubmissionStarted         bool   `json:"submission_started"`
	HumanVerificationRequired bool   `json:"human_verification_required"`
	VerificationSource        string `json:"verification_source"`
	VerificationNetworkCode   string `json:"verification_network_code"`
}

func adobeVerificationProgress(result adobeBrowserResult, progress Progress) error {
	if progress == nil || result.Stage == "completed" {
		return nil
	}
	stages := []string{}
	switch result.Diagnostics.VerificationSource {
	case "visible_challenge_frame", "visible_instruction", "upstream_captcha_response":
		stages = append(stages, "adobe_verification_"+result.Diagnostics.VerificationSource)
	}
	switch result.Diagnostics.VerificationNetworkCode {
	case "verification_dns_failed", "verification_request_timeout", "verification_request_aborted", "captcha_required":
		stages = append(stages, "adobe_"+result.Diagnostics.VerificationNetworkCode)
	}
	if result.SubmissionStarted || result.Diagnostics.SubmissionStarted {
		stages = append(stages, "adobe_creation_submitted")
	}
	if adobeRegistrationReviewHint("adobe: "+result.Code) != "" {
		stages = append(stages, "adobe_registration_needs_review")
	}
	for _, stage := range stages {
		if err := progress(stage); err != nil {
			return err
		}
	}
	return nil
}

func adobeRegistrationStageText(stage string) string {
	switch stage {
	case "adobe_verification_visible_challenge_frame":
		return "检测到可见的 Adobe 人机验证组件"
	case "adobe_verification_visible_instruction":
		return "检测到可见的人工验证提示"
	case "adobe_verification_upstream_captcha_response", "adobe_captcha_required":
		return "Adobe 注册接口明确要求人机验证"
	case "adobe_verification_dns_failed":
		return "人工验证组件域名解析失败"
	case "adobe_verification_request_timeout":
		return "人工验证组件请求超时"
	case "adobe_verification_request_aborted":
		return "验证组件请求被取消，保留当前提交记录等待恢复"
	case "adobe_creation_submitted":
		return "已发起创建账户操作，尚未确认注册结果；恢复时不会再次创建"
	case "adobe_registration_needs_review":
		return "本次浏览器流程已结束，邮箱与原提交记录保留，按任务状态安排恢复"
	case "adobe_recovery_started":
		return "正在恢复原浏览器会话并核对注册结果"
	default:
		return stage
	}
}

func adobeRegistrationReviewHint(failure string) string {
	// Exact fixed codes keep unrelated or untrusted error text out of the hint.
	code := strings.TrimPrefix(failure, "adobe: ")
	switch code {
	case "human_verification_required", "captcha_required", "verification_dns_failed", "verification_request_timeout", "verification_request_aborted":
		return "Adobe 验证尚未完成，邮箱与原提交记录已保留。当前浏览器已结束，不是在等待邮件验证码；恢复会复用原会话，通过正常登录和接码核对结果。持续的人机验证仍需通过 Adobe 验证，系统不会重复创建账号。"
	default:
		return ""
	}
}
