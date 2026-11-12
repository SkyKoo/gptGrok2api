package register

import (
	"fmt"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
)

// Adobe diagnostics are an allowlisted projection; never log raw Worker JSON,
// challenge content, URLs, credentials or upstream response text.
type adobeRegistrationDiagnostics struct {
	SubmissionStarted           bool                      `json:"submission_started"`
	HumanVerificationRequired   bool                      `json:"human_verification_required"`
	VerificationSource          string                    `json:"verification_source"`
	VerificationNetworkCode     string                    `json:"verification_network_code"`
	VerificationInstructionKind string                    `json:"verification_instruction_kind"`
	VerificationFrameKind       string                    `json:"verification_frame_kind"`
	PuzzleEntryVisible          *bool                     `json:"puzzle_entry_visible"`
	FormError                   string                    `json:"form_error"`
	InvalidFields               []string                  `json:"invalid_fields"`
	InvalidFieldReasons         []adobeInvalidFieldReason `json:"invalid_field_reasons"`
	ProfileField                string                    `json:"profile_field"`
	ProfileOptionReason         string                    `json:"profile_option_reason"`
	OptionCount                 *int                      `json:"option_count"`
	FailureAction               string                    `json:"failure_action"`
	FailurePhase                string                    `json:"failure_phase"`
	ExceptionType               string                    `json:"exception_type"`
	ElapsedMS                   *int64                    `json:"elapsed_ms"`
	DurationMS                  *int64                    `json:"duration_ms"`
	VerificationWaitElapsedMS   *int64                    `json:"verification_wait_elapsed_ms"`
	VerificationWaitOutcome     string                    `json:"verification_wait_outcome"`
}

type adobeInvalidFieldReason struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func adobeInvalidFieldLabel(field string) string {
	return map[string]string{
		"email": "邮箱", "password": "密码", "verification_code": "验证码", "profile": "个人资料",
		"first_name": "名", "last_name": "姓", "birth_day": "出生日期（日）", "birth_month": "出生月份",
		"birth_year": "出生年份", "country": "国家/地区", "terms": "条款同意项", "phone": "手机号", "other": "其他字段",
	}[field]
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
	if label := adobeStartupStageText(stage); label != "" {
		return label
	}
	switch stage {
	case "waiting_browser":
		return "等待浏览器会话释放"
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
		return "观察到验证组件请求被取消；单个请求取消不代表整个验证失败"
	case "adobe_creation_submitted":
		return "已发起创建账户操作；是否重新注册由持久保存的创建结果决定"
	case "adobe_original_submission_recorded":
		return "原注册流程已有创建提交记录，恢复沿用该记录，不代表本次重新创建"
	case "adobe_registration_needs_review":
		return "本次注册流程失败；后续重试安排见任务状态"
	case "adobe_recovery_started":
		return "创建已获确认，正在继续原账号的验证与会话采集"
	default:
		return stage
	}
}

func adobeStartupStageText(stage string) string {
	return map[string]string{
		"starting_browser":                  "启动浏览器",
		"adobe_loading_account_page":        "加载 Adobe 账户页",
		"adobe_waiting_create_entry":        "等待创建账户入口",
		"adobe_reloading_account_page":      "重新加载 Adobe 账户页",
		"adobe_opening_registration":        "打开 Adobe 注册页",
		"adobe_waiting_registration_form":   "等待 Adobe 注册表单",
		"adobe_checking_registration_form":  "识别 Adobe 注册表单",
		"adobe_restoring_registration_page": "恢复原 Adobe 注册页面",
	}[stage]
}

func adobeStartupEventText(event browserqueue.StartupEvent) string {
	if !event.Valid() {
		return ""
	}
	label := adobeStartupStageText(event.Stage)
	if event.Status == "started" {
		return fmt.Sprintf("%s（准备累计 %.1f 秒）", label, float64(event.ElapsedMS)/1000)
	}
	result := "完成"
	if event.Status == "failed" {
		result = "失败"
	}
	text := fmt.Sprintf("%s%s（本步 %.1f 秒，准备累计 %.1f 秒）", label, result, float64(event.DurationMS)/1000, float64(event.ElapsedMS)/1000)
	if event.Status == "failed" && event.ExceptionType != "" {
		text += "；" + adobeFailureDiagnosticText(adobeRegistrationDiagnostics{ExceptionType: event.ExceptionType})
	}
	return text
}

// Only fixed labels leave the private Worker response. Unknown values, page
// text and unbounded lists cannot become log content or change retry policy.
func adobeFailureDiagnosticText(d adobeRegistrationDiagnostics) string {
	parts := []string{}
	if field := map[string]string{"country": "国家/地区", "birth_month": "出生月份", "birth_year": "出生年份", "birth_day": "出生日期（日）"}[d.ProfileField]; field != "" {
		if reason := map[string]string{"missing": "找不到目标选项", "incomplete": "选项尚未完整加载", "ambiguous": "选项不唯一", "disabled": "目标选项不可用", "invalid": "目标值不符合要求"}[d.ProfileOptionReason]; reason != "" {
			text := "下拉选项：" + field + "，" + reason
			if d.OptionCount != nil && *d.OptionCount >= 0 && *d.OptionCount <= 400 {
				text += fmt.Sprintf("（已读取 %d 项）", *d.OptionCount)
			}
			parts = append(parts, text)
		}
	}
	if d.VerificationWaitElapsedMS != nil && *d.VerificationWaitElapsedMS >= 0 && *d.VerificationWaitElapsedMS <= 150000 {
		if result := map[string]string{
			"still_required": "页面仍要求人机验证", "resolved": "页面已离开验证阶段",
			"network_error": "验证请求发生网络错误", "interrupted": "等待已取消",
			"observation_error": "无法继续读取验证页面",
		}[d.VerificationWaitOutcome]; result != "" {
			parts = append(parts, fmt.Sprintf("已等待验证 %.1f 秒，%s", float64(*d.VerificationWaitElapsedMS)/1000, result))
		}
	}
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
	for _, field := range d.InvalidFields[:min(len(d.InvalidFields), 20)] {
		label := adobeInvalidFieldLabel(field)
		if label != "" && !seen[field] {
			seen[field] = true
			fields = append(fields, label)
		}
	}
	if len(fields) > 0 {
		parts = append(parts, "校验未通过字段："+strings.Join(fields, "、"))
	}
	fieldReasons := []string{}
	reasonFields := map[string]bool{}
	for _, item := range d.InvalidFieldReasons[:min(len(d.InvalidFieldReasons), 20)] {
		if !seen[item.Field] || reasonFields[item.Field] {
			continue
		}
		reason := map[string]string{
			"required": "必填项未填写", "format": "格式不符合要求", "range": "超出允许范围",
			"unchecked": "必选项未勾选", "invalid": "字段校验未通过",
		}[item.Reason]
		if reason == "" {
			continue
		}
		reasonFields[item.Field] = true
		fieldReasons = append(fieldReasons, adobeInvalidFieldLabel(item.Field)+"："+reason)
	}
	if len(fieldReasons) > 0 {
		parts = append(parts, "字段原因："+strings.Join(fieldReasons, "、"))
	}
	if label := map[string]string{
		"inspect_form": "检查表单", "open_login": "打开登录页", "click_login_entry": "进入登录",
		"fill_email": "填写邮箱", "fill_password": "填写密码", "submit_email": "提交邮箱", "submit_password": "提交密码",
		"wait_login_result": "等待登录结果", "request_code": "请求邮件验证码", "verify_code": "提交验证码",
		"export_session": "导出登录会话", "open_firefly": "打开 Firefly",
	}[d.FailureAction]; label != "" {
		parts = append(parts, "出错步骤："+label)
	} else if label := adobeStartupStageText(d.FailureAction); label != "" {
		parts = append(parts, "出错步骤："+label)
	}
	if adobeStartupStageText(d.FailureAction) != "" && d.ElapsedMS != nil && d.DurationMS != nil &&
		*d.DurationMS >= 0 && *d.DurationMS <= *d.ElapsedMS && *d.ElapsedMS <= 150000 {
		parts = append(parts, fmt.Sprintf("本步 %.1f 秒，准备累计 %.1f 秒", float64(*d.DurationMS)/1000, float64(*d.ElapsedMS)/1000))
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
	case "human_verification_required", "captcha_required":
		return "注册失败：Adobe 人机验证未通过。"
	case "verification_dns_failed":
		return "注册失败：验证服务域名解析失败。"
	case "verification_request_timeout":
		return "注册失败：等待验证服务超时。"
	case "verification_request_aborted":
		return "注册失败：验证流程未完成，观察到验证组件请求取消。"
	default:
		return ""
	}
}
