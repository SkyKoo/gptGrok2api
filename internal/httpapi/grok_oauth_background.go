package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/tasks"
)

var grokOAuthMessages = map[string]string{
	"oauth_cpa_required":                 "请先选择已配置的 CPA，同步目标会随本次授权固定",
	"oauth_cpa_conflict":                 "该账号已有发往其他 CPA 的授权任务，请等待完成",
	"oauth_browser_unavailable":          "浏览器准备失败或等待超时，请检查后台浏览器服务",
	"browser_wait_timeout":               "等待浏览器会话释放超时，本次尚未申请设备授权码",
	"oauth_in_progress":                  "该账号的后台授权仍在收尾，请稍后重试",
	"oauth_email_required":               "请先补充账号邮箱",
	"oauth_session_required":             "账号缺少 SSO 登录态，请先恢复登录",
	"oauth_account_unavailable":          "账号不存在、已禁用或登录态失效，请先处理账号状态",
	"oauth_account_changed":              "授权期间账号发生变化，未保存本次凭据",
	"oauth_identity_missing":             "xAI 未返回可核对的账号邮箱，未保存凭据",
	"oauth_identity_mismatch":            "返回凭据的邮箱与所选账号不一致，未保存凭据",
	"oauth_interrupted":                  "上次授权执行被中断，未自动重复授权；请确认后重新发起",
	"oauth_cancelled":                    "授权任务已取消，未保存凭据",
	"oauth_proxy_unavailable":            "账号绑定的调用出口不可用，请检查账号代理设置",
	"oauth_worker_config_invalid":        "后台浏览器服务配置无效",
	"oauth_worker_rejected":              "后台浏览器服务拒绝请求，请检查服务版本及认证配置",
	"oauth_worker_result_unknown":        "后台确认结果暂不明确，未重复提交授权",
	"oauth_verification_url_invalid":     "xAI 返回的设备授权地址不符合预期，已停止",
	"oauth_login_required":               "SSO 未能恢复 xAI 登录，请先恢复账号登录后重试",
	"oauth_verification_required":        "xAI 要求额外验证，后台授权已停止，请处理后重试",
	"oauth_consent_unrecognized":         "xAI 授权确认页面发生变化，已停止自动操作",
	"oauth_consent_rejected":             "xAI 拒绝本次授权",
	"oauth_deadline":                     "未在时限内取得 OAuth 凭据，未自动重复授权",
	"oauth_page_navigation_timeout":      "xAI 授权页面加载超时",
	"oauth_page_navigation_failed":       "xAI 授权页面加载失败",
	"oauth_page_observation_timeout":     "xAI 授权页面读取超时",
	"oauth_page_observation_failed":      "xAI 授权页面读取失败",
	"oauth_device_code_click_timeout":    "xAI 设备码页面确认超时",
	"oauth_device_code_click_failed":     "xAI 设备码页面确认失败",
	"oauth_consent_click_timeout":        "xAI 授权确认点击超时，结果待核对",
	"oauth_consent_click_failed":         "xAI 授权确认点击异常，结果待核对",
	"oauth_callback_timeout":             "已提交 xAI 授权但未收到完成回调",
	"oauth_authorization_timeout":        "浏览器授权执行超时",
	"browser_start_failed":               "浏览器启动失败",
	"browser_launch_timeout":             "浏览器启动超时",
	"browser_launch_proxy_error":         "浏览器代理初始化失败",
	"browser_launch_profile_locked":      "浏览器资料被占用",
	"browser_launch_process_exited":      "浏览器进程提前退出",
	"browser_launch_profile_unavailable": "浏览器资料不可用",
	"browser_launch_license_denied":      "浏览器许可不可用",
	"oauth_upstream_failed":              "xAI 授权请求失败，请稍后检查账号状态",
	"oauth_storage_failed":               "授权状态保存失败，请检查服务存储",
}

var grokOAuthDiagnosticCodes = map[string]struct{}{
	"oauth_cpa_required": {}, "oauth_cpa_conflict": {}, "oauth_browser_unavailable": {},
	"browser_wait_timeout": {}, "oauth_in_progress": {}, "oauth_email_required": {},
	"oauth_session_required": {}, "oauth_account_unavailable": {}, "oauth_account_changed": {},
	"oauth_identity_missing": {}, "oauth_identity_mismatch": {}, "oauth_interrupted": {},
	"oauth_cancelled": {}, "oauth_proxy_unavailable": {}, "oauth_worker_config_invalid": {},
	"oauth_worker_rejected": {}, "oauth_worker_result_unknown": {}, "oauth_verification_url_invalid": {},
	"oauth_login_required": {}, "oauth_verification_required": {}, "oauth_consent_unrecognized": {},
	"oauth_consent_rejected": {}, "oauth_deadline": {}, "oauth_upstream_failed": {},
	"oauth_storage_failed":          {},
	"oauth_page_navigation_timeout": {}, "oauth_page_navigation_failed": {},
	"oauth_page_observation_timeout": {}, "oauth_page_observation_failed": {},
	"oauth_device_code_click_timeout": {}, "oauth_device_code_click_failed": {},
	"oauth_consent_click_timeout": {}, "oauth_consent_click_failed": {},
	"oauth_callback_timeout": {}, "oauth_authorization_timeout": {},
	"browser_start_failed": {}, "browser_launch_timeout": {}, "browser_launch_proxy_error": {},
	"browser_launch_profile_locked": {}, "browser_launch_process_exited": {},
	"browser_launch_profile_unavailable": {}, "browser_launch_license_denied": {},
}

func safeGrokOAuthDiagnostic(value string, allowed map[string]struct{}, fallback string) string {
	if _, ok := allowed[value]; ok {
		return value
	}
	return fallback
}

var grokOAuthNetworkCode = regexp.MustCompile(`^(ERR_[A-Z0-9_]+|TIMEOUT|TIMED_OUT|CANCELLED|unknown)$`)

func safeGrokOAuthNetworkCodes(value string) string {
	if value == "" || len(value) > 256 {
		return ""
	}
	parts := strings.Split(value, ",")
	if len(parts) > 8 {
		return ""
	}
	for _, part := range parts {
		if !grokOAuthNetworkCode.MatchString(part) {
			return ""
		}
	}
	return value
}

func logGrokOAuthDiagnostic(taskID, stage, status, code, detail, pagePath string, elapsedMS int64, consentSubmitted bool) {
	logGrokOAuthDiagnosticWithNetwork(taskID, stage, status, code, detail, pagePath, elapsedMS, consentSubmitted, 0, "")
}

func logGrokOAuthDiagnosticWithNetwork(taskID, stage, status, code, detail, pagePath string, elapsedMS int64, consentSubmitted bool, networkFailures int, networkErrorCodes string) {
	stage = safeGrokOAuthDiagnostic(stage, map[string]struct{}{
		"queued": {}, "waiting_browser": {}, "starting_browser": {}, "browser_ready": {},
		"authorize": {}, "device_code": {}, "consent_page": {}, "consent_submit": {},
		"consent_callback": {}, "polling": {}, "syncing_cpa": {}, "browser_release": {},
		"failed": {}, "completed": {}, "page_navigation": {}, "page_observation": {},
		"browser_start": {}, "browser_context": {}, "browser_cookies": {}, "browser_routing": {},
		"browser_claim": {}, "worker_timeout": {}, "browser_exception": {},
		"device_code_form": {}, "device_code_submit": {}, "adobe_loading_account_page": {},
		"adobe_waiting_create_entry": {}, "adobe_reloading_account_page": {},
		"adobe_opening_registration": {}, "adobe_waiting_registration_form": {},
		"adobe_checking_registration_form": {}, "adobe_restoring_registration_page": {},
	}, "other")
	status = safeGrokOAuthDiagnostic(status, map[string]struct{}{
		"queued": {}, "running": {}, "approved": {}, "authorized": {}, "uncertain": {},
		"failed": {}, "manual_action_required": {}, "completed": {},
	}, "other")
	code = safeGrokOAuthDiagnostic(code, grokOAuthDiagnosticCodes, "")
	detail = safeGrokOAuthDiagnostic(detail, map[string]struct{}{
		"task_started": {}, "task_failed": {}, "task_completed": {}, "queue_claim_failed": {},
		"grant_created": {}, "worker_result": {}, "poll_authorized": {}, "poll_pending": {},
		"poll_failed": {}, "browser_startup": {}, "browser_startup_failed": {},
		"browser_release_start": {}, "browser_release_done": {}, "worker_timeout": {},
		"untrusted_destination": {}, "login_required": {}, "verification_required": {},
		"callback_received": {}, "body_or_title_timeout": {}, "body_or_title_failed": {},
		"challenge_detected": {}, "invalid_form_method": {}, "user_code_mismatch": {},
		"continue_click_timeout": {}, "continue_click_failed": {}, "identity_mismatch": {},
		"product_marker_missing": {}, "allow_click_timeout": {}, "allow_click_failed": {},
		"callback_wait_timeout": {}, "consent_button_missing": {}, "navigation_timeout": {},
		"navigation_failed": {}, "browser_exception": {}, "worker_deadline": {},
	}, "other")
	pagePath = safeGrokOAuthDiagnostic(pagePath, map[string]struct{}{
		"/oauth2/device": {}, "/oauth2/device/consent": {}, "/oauth2/device/done": {},
		"/sign-in": {}, "/sign-up": {}, "/sso-select-account": {}, "/mfa/verify": {},
		"/one-time-code": {}, "/verify-email": {}, "<other>": {}, "<invalid>": {},
	}, "<other>")
	if elapsedMS < 0 {
		elapsedMS = 0
	} else if elapsedMS > 900000 {
		elapsedMS = 900000
	}
	if networkFailures < 0 {
		networkFailures = 0
	} else if networkFailures > 32 {
		networkFailures = 32
	}
	networkErrorCodes = safeGrokOAuthNetworkCodes(networkErrorCodes)
	// Task IDs are already public queue handles. All other fields are fixed
	// allowlists so worker exceptions, URLs, and credentials cannot leak here.
	log.Printf("grok oauth diagnostic task=%s stage=%s status=%s code=%s detail=%s elapsed_ms=%d consent_submitted=%t page_path=%s network_failures=%d network_error_codes=%s", taskID, stage, status, code, detail, elapsedMS, consentSubmitted, pagePath, networkFailures, networkErrorCodes)
}

func grokOAuthErrorCode(err error) string {
	if err != nil {
		var startupErr *browserqueue.StartupError
		if errors.As(err, &startupErr) {
			if _, ok := grokOAuthDiagnosticCodes[startupErr.Code]; ok {
				return startupErr.Code
			}
		}
		if _, ok := grokOAuthMessages[err.Error()]; ok {
			return err.Error()
		}
	}
	return "oauth_upstream_failed"
}

func (s *Server) grokOAuthSource(id string) (map[string]any, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("oauth_account_unavailable")
	}
	items, err := s.registerStore.ExportAccounts([]string{id})
	if err != nil || len(items) != 1 {
		return nil, errors.New("oauth_account_unavailable")
	}
	item := items[0]
	if stringValue(item["email"]) == "" {
		return nil, errors.New("oauth_email_required")
	}
	if stringValue(item["sso"]) == "" {
		return nil, errors.New("oauth_session_required")
	}
	for _, value := range []string{stringValue(item["status"]), stringValue(item["validation_status"])} {
		switch value {
		case "disabled", "invalid", "expired", "异常":
			return nil, errors.New("oauth_account_unavailable")
		}
	}
	if !boolValue(item["enabled"], true) {
		return nil, errors.New("oauth_account_unavailable")
	}
	runtime, err := s.grokRuntimeStore.AccountList()
	if err != nil {
		return nil, errors.New("oauth_account_unavailable")
	}
	for _, row := range runtime {
		if accountToken(row) != stringValue(item["sso"]) {
			continue
		}
		if !boolValue(row["enabled"], true) {
			return nil, errors.New("oauth_account_unavailable")
		}
		switch stringValue(row["status"]) {
		case "disabled", "invalid", "expired", "异常":
			return nil, errors.New("oauth_account_unavailable")
		}
	}
	return item, nil
}

func (s *Server) startGrokOAuth(id string, poolIDs ...string) (*tasks.Task, bool, error) {
	s.authStartMu.Lock()
	defer s.authStartMu.Unlock()
	source, sourceErr := s.grokOAuthSource(id)
	if sourceErr != nil {
		return nil, false, sourceErr
	}
	if s.deviceOAuth.HasAccountSession(stringValue(source["email"])) {
		return nil, false, errors.New("oauth_in_progress")
	}
	target := ""
	if len(poolIDs) > 0 {
		target = poolIDs[0]
	}
	pool, err := s.xaiCPATarget(target)
	if err != nil {
		return nil, false, err
	}
	for _, task := range s.taskQueue.List() {
		if task.Kind == "grok_oauth_authorize" && stringValue(task.Payload["account_id"]) == id && (task.Status == "queued" || task.Status == "running") {
			if stringValue(task.Payload["cpa_pool_id"]) != pool.ID {
				return nil, false, errors.New("oauth_cpa_conflict")
			}
			copy := task
			return &copy, true, nil
		}
	}
	if _, active := s.grokOAuthRuns.Load(id); active {
		return nil, false, errors.New("oauth_in_progress")
	}
	if found, err := s.registerStore.SetOAuthAuthorization(id, "queued", ""); err != nil || !found {
		return nil, false, errors.New("oauth_storage_failed")
	}
	return s.taskQueue.Submit("grok_oauth_authorize", map[string]any{"account_id": id, "cpa_pool_id": pool.ID, "cpa_pool_name": pool.Name}), false, nil
}

func (s *Server) runGrokOAuthTask(task *tasks.Task) (result map[string]any, finalErr error) {
	id := stringValue(task.Payload["account_id"])
	startedAt := time.Now()
	logGrokOAuthDiagnostic(task.ID, "queued", "running", "", "task_started", "", 0, false)
	if _, active := s.grokOAuthRuns.LoadOrStore(id, true); active {
		logGrokOAuthDiagnostic(task.ID, "failed", "failed", "oauth_in_progress", "task_failed", "", time.Since(startedAt).Milliseconds(), false)
		return nil, errors.New("oauth_in_progress")
	}
	defer s.grokOAuthRuns.Delete(id)
	finish := func(status, code string) (map[string]any, error) {
		if found, err := s.registerStore.SetOAuthAuthorization(id, status, code); err != nil || !found {
			return nil, errors.New("oauth_storage_failed")
		}
		return map[string]any{"status": status, "code": code}, nil
	}
	defer func() {
		if finalErr != nil {
			code := grokOAuthErrorCode(finalErr)
			logGrokOAuthDiagnostic(task.ID, "failed", "failed", code, "task_failed", "", time.Since(startedAt).Milliseconds(), false)
			_, _ = s.registerStore.SetOAuthAuthorization(id, "failed", code)
			finalErr = errors.New(code)
		} else {
			logGrokOAuthDiagnostic(task.ID, "completed", "completed", "", "task_completed", "", time.Since(startedAt).Milliseconds(), false)
		}
	}()
	account, err := s.grokOAuthSource(id)
	if err != nil {
		return nil, err
	}
	// A saved token is a durable checkpoint. A recovered task may complete its
	// delivery/readback, but must never issue a second device authorization.
	credentials, loadErr := s.oauthStore.List()
	if loadErr != nil {
		return nil, errors.New("oauth_storage_failed")
	}
	for _, credential := range credentials {
		if credential.CPASync != nil && credential.CPASync.AuthorizationTaskID == task.ID {
			current, present := s.taskQueue.Get(task.ID)
			if !present || current.Status == "cancelled" {
				return nil, errors.New("oauth_cancelled")
			}
			if !strings.EqualFold(credential.Email, stringValue(account["email"])) || credential.CPASync.PoolID != stringValue(task.Payload["cpa_pool_id"]) {
				return nil, errors.New("oauth_account_changed")
			}
			value := map[string]any{"status": "authorized", "account": oauth.Public(credential)}
			recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer recoveryCancel()
			s.finishOAuthCPADelivery(recoveryCtx, value)
			if _, err := finish("authorized", ""); err != nil {
				return nil, err
			}
			return value, nil
		}
	}
	// Queue recovery must not replay an authorization whose confirmation may
	// already have reached xAI before the process stopped.
	if stringValue(mapValue(account["oauth_authorization"])["status"]) == "running" {
		return finish("manual_action_required", "oauth_interrupted")
	}
	pool, poolErr := s.xaiCPATarget(stringValue(task.Payload["cpa_pool_id"]))
	if poolErr != nil || stringValue(task.Payload["cpa_pool_id"]) == "" {
		return finish("manual_action_required", "oauth_cpa_required")
	}
	if found, err := s.registerStore.SetOAuthAuthorization(id, "running", ""); err != nil || !found {
		return nil, errors.New("oauth_storage_failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()
	overallCtx := ctx
	email, sso := stringValue(account["email"]), stringValue(account["sso"])
	guard := func() error {
		if ctx.Err() != nil {
			return errors.New("oauth_deadline")
		}
		current, found := s.taskQueue.Get(task.ID)
		if !found || current.Status == "cancelled" {
			return errors.New("oauth_cancelled")
		}
		latest, err := s.grokOAuthSource(id)
		if err != nil || stringValue(latest["sso"]) != sso || !strings.EqualFold(stringValue(latest["email"]), email) {
			return errors.New("oauth_account_changed")
		}
		return nil
	}
	if err := guard(); err != nil {
		return nil, err
	}
	proxyURL := ""
	if s.proxyManager != nil {
		raw, bound, err := s.proxyManager.ResolveBound(account)
		if err != nil {
			return nil, errors.New("oauth_proxy_unavailable")
		}
		if bound {
			proxyURL = raw
		} else {
			proxyURL = s.proxyManager.Resolve(account, false)
		}
	}
	config := mapValue(s.registerStore.Get()["grok"])
	base := firstNonEmpty(stringValue(config["api_base"]), os.Getenv("CFM_BROWSER_WORKER_URL"), os.Getenv("CFM_GROK_BROWSER_URL"), "http://cfm-browser-worker:8899")
	lastLoggedStage := ""
	setStage := func(stage string) error {
		if err := guard(); err != nil {
			return err
		}
		if stage != lastLoggedStage {
			logGrokOAuthDiagnostic(task.ID, stage, "running", "", "", "", time.Since(startedAt).Milliseconds(), false)
			lastLoggedStage = stage
		}
		if queue, ok := s.taskQueue.(interface{ SetStage(string, string) }); ok {
			queue.SetStage(task.ID, stage)
		}
		return nil
	}
	startupProgress := func(event browserqueue.StartupEvent) error {
		logGrokOAuthDiagnostic(task.ID, event.Stage, "running", "", "browser_startup", "", event.ElapsedMS, false)
		return nil
	}
	lease, err := browserqueue.AcquireWithStartupProgress(ctx, s.requestClient, base, stringValue(config["api_key"]), map[string]any{"proxy": proxyURL, "daily_direct": proxyURL == ""}, setStage, startupProgress)
	if err != nil {
		logGrokOAuthDiagnostic(task.ID, "failed", "failed", grokOAuthErrorCode(err), "queue_claim_failed", "", time.Since(startedAt).Milliseconds(), false)
		if guardErr := guard(); guardErr != nil {
			return nil, guardErr
		}
		if err.Error() == "browser_wait_timeout" {
			return nil, err
		}
		return nil, errors.New("oauth_browser_unavailable")
	}
	defer func() {
		logGrokOAuthDiagnostic(task.ID, "browser_release", "running", "", "browser_release_start", "", time.Since(startedAt).Milliseconds(), false)
		lease.Release()
		logGrokOAuthDiagnostic(task.ID, "browser_release", "running", "", "browser_release_done", "", time.Since(startedAt).Milliseconds(), false)
	}()
	if err := setStage("browser_ready"); err != nil {
		return nil, err
	}
	// Browser is ready now; the device code's validity is never spent in queue.
	grantCtx, grantCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer grantCancel()
	ctx = grantCtx
	if err := setStage("authorize"); err != nil {
		return nil, err
	}
	if err := setStage("device_code"); err != nil {
		return nil, err
	}
	started, err := s.deviceOAuth.StartForAccount(ctx, proxyURL, email, guard)
	if err != nil {
		logGrokOAuthDiagnostic(task.ID, "device_code", "failed", grokOAuthErrorCode(err), "poll_failed", "", time.Since(startedAt).Milliseconds(), false)
		return nil, err
	}
	logGrokOAuthDiagnostic(task.ID, "device_code", "running", "", "grant_created", "", time.Since(startedAt).Milliseconds(), false)
	sessionID := stringValue(started["id"])
	defer s.deviceOAuth.Discard(sessionID)
	if err := s.deviceOAuth.SetCPATarget(sessionID, oauth.CPASync{Status: "pending", PoolID: pool.ID, PoolName: pool.Name, ReplaceExisting: true, AuthorizationTaskID: task.ID}); err != nil {
		return nil, errors.New("oauth_storage_failed")
	}
	if err := setStage("consent_page"); err != nil {
		return nil, err
	}
	outcome, err := oauth.ConfirmDeviceInBrowser(ctx, s.requestClient, base, stringValue(config["api_key"]), oauth.BrowserDeviceRequest{
		BrowserLease: lease.ID, VerificationURL: stringValue(started["verification_uri_complete"]), UserCode: stringValue(started["user_code"]), SSO: sso, SSORW: stringValue(account["sso_rw"]), Email: email, Proxy: proxyURL, Timeout: 120,
	})
	logGrokOAuthDiagnosticWithNetwork(task.ID, outcome.Stage, outcome.Status, outcome.Code, "worker_result", outcome.PagePath, outcome.ElapsedMS, outcome.ConsentSubmitted, outcome.NetworkFailures, outcome.NetworkErrorCodes)
	if err != nil && err.Error() != "oauth_worker_result_unknown" {
		return nil, err
	}
	if outcome.Status == "manual_action_required" {
		return finish("manual_action_required", grokOAuthErrorCode(errors.New(outcome.Code)))
	}
	if outcome.Status == "failed" {
		return nil, errors.New(grokOAuthErrorCode(errors.New(outcome.Code)))
	}
	if outcome.Status == "uncertain" && outcome.Code != "" && outcome.Code != "oauth_worker_result_unknown" && !outcome.ConsentSubmitted {
		return nil, errors.New(grokOAuthErrorCode(errors.New(outcome.Code)))
	}
	// A lost worker response is not permission to repeat consent. Query the same
	// device grant until it succeeds or expires, without issuing a new grant.
	if err := setStage("polling"); err != nil {
		return nil, err
	}
	interval := time.Duration(intValue(started["interval"])) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	for {
		if err := guard(); err != nil {
			return nil, err
		}
		value, err := s.deviceOAuth.Poll(ctx, sessionID)
		if err != nil {
			logGrokOAuthDiagnostic(task.ID, "polling", "failed", grokOAuthErrorCode(err), "poll_failed", "", time.Since(startedAt).Milliseconds(), outcome.ConsentSubmitted)
			return nil, err
		}
		if stringValue(value["status"]) == "authorized" {
			logGrokOAuthDiagnostic(task.ID, "polling", "authorized", "", "poll_authorized", "", time.Since(startedAt).Milliseconds(), outcome.ConsentSubmitted)
			if _, err := finish("authorized", ""); err != nil {
				return nil, err
			}
			if queue, ok := s.taskQueue.(interface{ SetStage(string, string) }); ok {
				queue.SetStage(task.ID, "syncing_cpa")
			}
			// The grant deadline no longer applies after its tokens are saved.
			s.finishOAuthCPADelivery(overallCtx, value)
			return value, nil
		}
		if seconds := intValue(value["interval"]); seconds > 0 {
			interval = time.Duration(seconds) * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			logGrokOAuthDiagnostic(task.ID, "polling", "failed", "oauth_deadline", "worker_timeout", "", time.Since(startedAt).Milliseconds(), outcome.ConsentSubmitted)
			return nil, errors.New("oauth_deadline")
		case <-timer.C:
		}
	}
}

func grokOAuthPublicJob(task tasks.Task) map[string]any {
	status, stage, message := "pending", "queued", "后台授权任务已排队"
	switch task.Status {
	case "running":
		status, stage, message = "running", "authorize", "正在使用所选账号完成授权"
		switch stringValue(task.Result["stage"]) {
		case "checking_quality":
			stage, message = "checking_quality", "OAuth 已授权，正在检测 grok-4.7"
		case "browser_ready":
			stage, message = "browser_ready", "浏览器已就绪，正在创建 xAI 设备授权"
		case "syncing_cpa":
			stage, message = "syncing_cpa", "OAuth 已授权，正在同步 CPA"
		case "waiting_browser":
			stage, message = "waiting_browser", "等待浏览器会话释放"
		case "starting_browser":
			stage, message = "starting_browser", "正在启动浏览器"
		case "device_code":
			stage, message = "device_code", "正在创建 xAI 设备授权"
		case "consent_page":
			stage, message = "consent_page", "正在打开 xAI 授权确认页"
		case "polling":
			stage, message = "polling", "等待 xAI 授权结果"
		}
	case "completed":
		status = stringValue(task.Result["status"])
		if status == "authorized" {
			stage, message = "completed", "OAuth 已授权并同步 CPA，由 CPA 自动续期"
			if stringValue(task.Result["cpa_error"]) != "" {
				stage, message = "cpa_sync_failed", "OAuth 已授权，CPA 同步未完成，可单独处理"
			}
		} else {
			status, stage = "manual_action_required", "attention"
			message = grokOAuthMessages[stringValue(task.Result["code"])]
		}
	case "cancelled":
		status, stage, message = "failed", "failed", grokOAuthMessages["oauth_cancelled"]
	case "failed":
		status, stage = "failed", "failed"
		message = grokOAuthMessages[grokOAuthErrorCode(errors.New(task.Error))]
	}
	item := map[string]any{"id": task.ID, "status": status, "stage": stage, "message": message, "error": "", "source_account_id": task.Payload["account_id"], "cpa_pool_id": task.Payload["cpa_pool_id"], "cpa_pool_name": task.Payload["cpa_pool_name"], "created_at": task.CreatedAt, "updated_at": task.UpdatedAt, "models": []string{}}
	if status == "failed" || status == "manual_action_required" {
		item["error"] = message
	}
	if message := stringValue(task.Result["cpa_error"]); message != "" {
		item["cpa_error"] = message
	}
	if account, ok := task.Result["account"]; ok {
		item["account"] = account
	}
	return item
}

func (s *Server) grokOAuthBackgroundAPI(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case path == "start" && r.Method == http.MethodPost:
		var body struct {
			AccountID string `json:"account_id"`
			PoolID    string `json:"cpa_pool_id"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		job, reused, err := s.startGrokOAuth(strings.TrimSpace(body.AccountID), strings.TrimSpace(body.PoolID))
		if err != nil {
			writeError(w, http.StatusBadRequest, grokOAuthMessages[grokOAuthErrorCode(err)], "invalid_request_error")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"job": grokOAuthPublicJob(*job), "job_id": job.ID, "reused": reused})
	case strings.HasPrefix(path, "jobs/") && r.Method == http.MethodGet:
		task, ok := s.taskQueue.Get(strings.TrimPrefix(path, "jobs/"))
		if !ok || task.Kind != "grok_oauth_authorize" {
			writeError(w, 404, "OAuth 任务不存在", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": grokOAuthPublicJob(task)})
	case path == "status" && r.Method == http.MethodGet:
		items := []map[string]any{}
		for _, task := range s.taskQueue.List() {
			if task.Kind == "grok_oauth_authorize" {
				items = append(items, grokOAuthPublicJob(task))
			}
		}
		writeJSON(w, 200, map[string]any{"queue": map[string]any{"items": items, "runtime": "go"}})
	default:
		writeError(w, 404, "OAuth endpoint not found", "not_found")
	}
}
