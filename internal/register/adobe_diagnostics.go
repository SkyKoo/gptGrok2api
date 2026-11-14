package register

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
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
	// Decode optional observations independently: malformed diagnostics must not
	// discard an otherwise valid creation result or alter compensation policy.
	MediaRuntime json.RawMessage `json:"media_runtime"`
	MediaPage    json.RawMessage `json:"media_page"`
	MediaTiming  json.RawMessage `json:"media_timing"`
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
	case "adobe_media_pending":
		return "账号已创建并验证邮箱，正在按账号日常出口准备媒体会话"
	case "adobe_media_preparing":
		return "正在使用原账号浏览器资料采集媒体会话，不重新注册"
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
	parts := adobeMediaDiagnosticText(d)
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
		*d.DurationMS >= 0 && *d.DurationMS <= *d.ElapsedMS && *d.ElapsedMS <= 300000 {
		parts = append(parts, fmt.Sprintf("本步 %.1f 秒，准备累计 %.1f 秒", float64(*d.DurationMS)/1000, float64(*d.ElapsedMS)/1000))
	}
	if label := map[string]string{"before_submission": "动作提交前", "submission_started": "动作已开始提交，结果仍需核对"}[d.FailurePhase]; label != "" {
		parts = append(parts, "步骤进度："+label)
	}
	if label := map[string]string{
		"registration": "流程校验", "timeout": "等待超时", "navigation": "页面跳转", "page_closed": "页面已关闭", "unknown": "未分类异常",
		"proxy_tunnel": "代理隧道失败", "proxy_connection": "代理连接失败", "proxy_auth": "代理认证失败",
		"dns": "域名解析失败", "network_timeout": "网络连接超时", "tls": "TLS 握手失败", "network_connection": "网络连接失败",
	}[d.ExceptionType]; label != "" {
		parts = append(parts, "异常类别："+label)
	}
	return strings.Join(parts, "；")
}

func adobeMediaDiagnosticText(d adobeRegistrationDiagnostics) []string {
	runtime := adobe.SanitizeMediaPreparationDiagnostic(d.MediaRuntime)
	if runtime == nil {
		return nil
	}
	parts := []string{}
	label := map[string]string{
		"performance_unavailable": "无法读取初始化阶段记录", "instance_missing": "尚未观察到初始化实例记录",
		"instance_ambiguous": "初始化实例记录不唯一", "vendor_unrecognized": "出现未识别的初始化组件",
		"vendors_pending": "初始化组件尚未全部完成", "token_pending": "初始化组件已完成，尚未收到媒体凭据", "token_settling": "媒体凭据尚未稳定",
		"editor_not_ready": "媒体编辑页尚未就绪", "observer_unavailable": "媒体观察接口不可用",
		"context_changed": "媒体页面上下文已变化", "identity_pending": "最终身份同步尚未完成",
		"identity_changed": "最终身份与采集记录不一致", "runtime_changed": "媒体运行状态在收尾校验时变化",
		"verification_timeout": "最终媒体会话校验超时", "authentication_missing": "媒体认证组件尚未出现",
		"arp_observer_missing": "媒体凭据观察接口尚未出现", "editor_route_changed": "页面已离开媒体编辑路径",
		"page_hidden": "媒体观察期间页面已离开", "observation_cancelled": "媒体观察已取消",
		"evaluation_failed": "无法读取媒体页面运行状态", "navigation_budget_exhausted": "媒体编辑页导航已耗尽等待时间",
	}[runtime.ReadinessReason]
	if label == "" {
		label = map[string]string{
			"unavailable": "暂不可用", "pending": "仍在等待就绪", "invalid": "媒体凭据格式无效",
			"invalid_contract": "媒体观察接口不符合预期", "changed": "收尾状态已变化",
			"observation_timeout": "媒体运行状态观察超时", "ready": "观测状态已就绪",
			"editor_not_ready": "媒体编辑页尚未就绪", "observer_unavailable": "媒体观察接口不可用",
			"context_changed": "媒体页面上下文已变化",
		}[runtime.State]
	}
	if label != "" {
		parts = append(parts, "媒体会话："+label)
	}
	if value := map[string]string{"unobserved": "尚未收到初值", "null": "当前观测为空", "undefined": "当前观测未定义", "empty_string": "当前观测为空字符串"}[runtime.ValueKind]; value != "" {
		parts = append(parts, "媒体凭据观察："+value)
	}
	if runtime.VendorsStarted != nil {
		parts = append(parts, fmt.Sprintf("初始化组件已启动 %d 个", *runtime.VendorsStarted))
	}
	if runtime.VendorsComplete != nil {
		parts = append(parts, fmt.Sprintf("初始化组件已完成 %d 个", *runtime.VendorsComplete))
	}
	page := adobeMediaObservationFields(d.MediaPage)
	if route, ok := page["route"].(string); ok {
		if label := map[string]string{"editor": "图片编辑页", "other": "其他页面", "unavailable": "无法确认"}[route]; label != "" {
			parts = append(parts, "媒体页面："+label)
		}
	}
	counts := make(map[string]int)
	for _, key := range []string{"matching_host_count", "authentication_count", "arp_observer_count"} {
		if count, ok := adobeMediaObservationNumber(page[key], 32); ok {
			counts[key] = count
		}
	}
	hosts, hasHosts := counts["matching_host_count"]
	authentications, hasAuthentications := counts["authentication_count"]
	observers, hasObservers := counts["arp_observer_count"]
	if (!hasHosts || !hasAuthentications || authentications <= hosts) && (!hasAuthentications || !hasObservers || observers <= authentications) {
		for _, field := range [][2]string{{"matching_host_count", "媒体组件"}, {"authentication_count", "认证实例"}, {"arp_observer_count", "凭据观察器"}} {
			if count, ok := counts[field[0]]; ok {
				parts = append(parts, fmt.Sprintf("已观察到%s %d 个", field[1], count))
			}
		}
	}
	if complete, ok := page["scan_complete"].(bool); ok && !complete {
		parts = append(parts, "媒体页面扫描未完成")
	}
	timing := adobeMediaObservationFields(d.MediaTiming)
	for _, field := range [][2]string{
		{"candidate_changes", "候选会话变化"}, {"source_changes", "观察接口变化"},
		{"authentication_changes", "认证实例变化"}, {"emissions", "凭据观察回调"},
		{"value_changes", "观察值变化"}, {"proof_changes", "初始化记录变化"},
	} {
		if count, ok := adobeMediaObservationNumber(timing[field[0]], 65535); ok {
			parts = append(parts, fmt.Sprintf("%s %d 次", field[1], count))
		}
	}
	for _, field := range [][2]string{
		{"first_source_ms", "首次观察接口"}, {"first_nonempty_ms", "首次非空凭据"},
		{"vendors_ready_ms", "组件首次就绪"}, {"last_candidate_ms", "最近候选会话"},
	} {
		if elapsed, ok := adobeMediaObservationNumber(timing[field[0]], 60000); ok {
			parts = append(parts, fmt.Sprintf("%s在观察开始后 %.1f 秒", field[1], float64(elapsed)/1000))
		}
	}
	return parts
}

func adobeMediaObservationFields(raw json.RawMessage) map[string]any {
	var fields map[string]any
	if len(raw) > 8192 || json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

func adobeMediaObservationNumber(value any, maximum int) (int, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > float64(maximum) || math.Trunc(number) != number {
		return 0, false
	}
	return int(number), true
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
