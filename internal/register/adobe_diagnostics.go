package register

import "strings"

// Adobe diagnostics are an allowlisted projection; never log raw Worker JSON,
// challenge content, URLs, credentials or upstream response text.
type adobeRegistrationDiagnostics struct {
	SubmissionStarted           bool     `json:"submission_started"`
	HumanVerificationRequired   bool     `json:"human_verification_required"`
	VerificationSource          string   `json:"verification_source"`
	VerificationNetworkCode     string   `json:"verification_network_code"`
	VerificationInstructionKind string   `json:"verification_instruction_kind"`
	VerificationFrameKind       string   `json:"verification_frame_kind"`
	PuzzleEntryVisible          *bool    `json:"puzzle_entry_visible"`
	FormError                   string   `json:"form_error"`
	InvalidFields               []string `json:"invalid_fields"`
	FailureAction               string   `json:"failure_action"`
	FailurePhase                string   `json:"failure_phase"`
	ExceptionType               string   `json:"exception_type"`
}

// SubmissionStarted describes the original journal, not a new action performed
// by this recovery request. Both stages retain the same no-repeat fence.
func adobeSubmissionProgressStage(result adobeBrowserResult, recoveryMode string) string {
	if !result.SubmissionStarted && !result.Diagnostics.SubmissionStarted {
		return ""
	}
	if recoveryMode == "resume_session" {
		return "adobe_original_submission_recorded"
	}
	return "adobe_creation_submitted"
}

func adobeVerificationProgress(result adobeBrowserResult, recoveryMode string, progress Progress) error {
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
	if stage := adobeSubmissionProgressStage(result, recoveryMode); stage != "" {
		stages = append(stages, stage)
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
	case "adobe_original_submission_recorded":
		return "原注册流程已有创建提交记录，恢复沿用该记录，不代表本次重新创建"
	case "adobe_registration_needs_review":
		return "本次浏览器流程已结束，邮箱与原提交记录保留，按任务状态安排恢复"
	case "adobe_recovery_started":
		return "正在恢复原浏览器会话并核对注册结果"
	default:
		return stage
	}
}

// Only fixed labels leave the private Worker response. Unknown values, page
// text and unbounded lists cannot become log content or change retry policy.
func adobeFailureDiagnosticText(d adobeRegistrationDiagnostics) string {
	parts := []string{}
	if label := map[string]string{
		"start_puzzle": "开始拼图", "complete_puzzle": "完成拼图", "prove_human": "证明是人类",
		"not_robot": "确认不是机器人", "identity_instruction": "身份验证提示，尚未确认具体拼图",
	}[d.VerificationInstructionKind]; label != "" {
		parts = append(parts, "验证提示类型："+label)
	}
	if label := map[string]string{
		"adobe_verification": "Adobe 验证组件", "challenge_provider": "已知验证服务组件",
		"adobe_page": "Adobe 页面", "other_frame": "其他页面组件",
	}[d.VerificationFrameKind]; label != "" {
		parts = append(parts, "验证提示来源："+label)
	}
	if d.PuzzleEntryVisible != nil {
		label := "已检查，当前未见"
		if *d.PuzzleEntryVisible {
			label = "可见"
		}
		parts = append(parts, "开始拼图按钮："+label)
	}
	if label := map[string]string{
		"account_not_found": "账号不存在", "invalid_email": "邮箱被拒绝",
		"invalid_password": "密码被拒绝", "invalid_verification_code": "验证码被拒绝",
		"invalid_field": "字段校验未通过", "generic_notice": "页面显示通用错误提示",
	}[d.FormError]; label != "" {
		parts = append(parts, "表单反馈："+label)
	}
	fields := []string{}
	seen := map[string]bool{}
	for _, field := range d.InvalidFields[:min(len(d.InvalidFields), 5)] {
		label := map[string]string{"email": "邮箱", "password": "密码", "verification_code": "验证码", "profile": "个人资料", "other": "其他字段"}[field]
		if label != "" && !seen[field] {
			seen[field] = true
			fields = append(fields, label)
		}
	}
	if len(fields) > 0 {
		parts = append(parts, "校验未通过字段："+strings.Join(fields, "、"))
	}
	if label := map[string]string{
		"inspect_form": "检查表单", "open_login": "打开登录页", "click_login_entry": "进入登录",
		"fill_email": "填写邮箱", "fill_password": "填写密码", "submit_email": "提交邮箱", "submit_password": "提交密码",
		"wait_login_result": "等待登录结果", "request_code": "请求邮件验证码", "verify_code": "提交验证码",
		"export_session": "导出登录会话", "open_firefly": "打开 Firefly",
	}[d.FailureAction]; label != "" {
		parts = append(parts, "出错步骤："+label)
	}
	if label := map[string]string{"before_submission": "动作提交前", "submission_started": "动作已开始提交，结果仍需核对"}[d.FailurePhase]; label != "" {
		parts = append(parts, "步骤进度："+label)
	}
	if label := map[string]string{"registration": "流程校验", "timeout": "等待超时", "navigation": "页面跳转", "page_closed": "页面已关闭", "unknown": "未分类异常"}[d.ExceptionType]; label != "" {
		parts = append(parts, "异常类别："+label)
	}
	return strings.Join(parts, "；")
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
