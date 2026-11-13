package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/tasks"
)

var grokOAuthMessages = map[string]string{
	"oauth_cpa_required":             "请先选择已配置的 CPA，同步目标会随本次授权固定",
	"oauth_cpa_conflict":             "该账号已有发往其他 CPA 的授权任务，请等待完成",
	"oauth_browser_unavailable":      "浏览器准备失败或等待超时，请检查后台浏览器服务",
	"browser_wait_timeout":           "等待浏览器会话释放超时，本次尚未申请设备授权码",
	"oauth_in_progress":              "该账号的后台授权仍在收尾，请稍后重试",
	"oauth_email_required":           "请先补充账号邮箱",
	"oauth_session_required":         "账号缺少 SSO 登录态，请先恢复登录",
	"oauth_account_unavailable":      "账号不存在、已禁用或登录态失效，请先处理账号状态",
	"oauth_account_changed":          "授权期间账号发生变化，未保存本次凭据",
	"oauth_identity_missing":         "xAI 未返回可核对的账号邮箱，未保存凭据",
	"oauth_identity_mismatch":        "返回凭据的邮箱与所选账号不一致，未保存凭据",
	"oauth_interrupted":              "上次授权执行被中断，未自动重复授权；请确认后重新发起",
	"oauth_cancelled":                "授权任务已取消，未保存凭据",
	"oauth_proxy_unavailable":        "账号绑定的调用出口不可用，请检查账号代理设置",
	"oauth_worker_config_invalid":    "后台浏览器服务配置无效",
	"oauth_worker_rejected":          "后台浏览器服务拒绝请求，请检查服务版本及认证配置",
	"oauth_worker_result_unknown":    "后台确认结果暂不明确，未重复提交授权",
	"oauth_verification_url_invalid": "xAI 返回的设备授权地址不符合预期，已停止",
	"oauth_login_required":           "SSO 未能恢复 xAI 登录，请先恢复账号登录后重试",
	"oauth_verification_required":    "xAI 要求额外验证，后台授权已停止，请处理后重试",
	"oauth_consent_unrecognized":     "xAI 授权确认页面发生变化，已停止自动操作",
	"oauth_consent_rejected":         "xAI 拒绝本次授权",
	"oauth_deadline":                 "未在时限内取得 OAuth 凭据，未自动重复授权",
	"oauth_upstream_failed":          "xAI 授权请求失败，请稍后检查账号状态",
	"oauth_storage_failed":           "授权状态保存失败，请检查服务存储",
}

func grokOAuthErrorCode(err error) string {
	if err != nil {
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
	if _, active := s.grokOAuthRuns.LoadOrStore(id, true); active {
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
			_, _ = s.registerStore.SetOAuthAuthorization(id, "failed", code)
			finalErr = errors.New(code)
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
	setStage := func(stage string) error {
		if err := guard(); err != nil {
			return err
		}
		if queue, ok := s.taskQueue.(interface{ SetStage(string, string) }); ok {
			queue.SetStage(task.ID, stage)
		}
		return nil
	}
	lease, err := browserqueue.Acquire(ctx, s.requestClient, base, stringValue(config["api_key"]), map[string]any{"proxy": proxyURL, "daily_direct": proxyURL == ""}, setStage)
	if err != nil {
		if guardErr := guard(); guardErr != nil {
			return nil, guardErr
		}
		if err.Error() == "browser_wait_timeout" {
			return nil, err
		}
		return nil, errors.New("oauth_browser_unavailable")
	}
	defer lease.Release()
	// Browser is ready now; the device code's validity is never spent in queue.
	grantCtx, grantCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer grantCancel()
	ctx = grantCtx
	if err := setStage("authorize"); err != nil {
		return nil, err
	}
	started, err := s.deviceOAuth.StartForAccount(ctx, proxyURL, email, guard)
	if err != nil {
		return nil, err
	}
	sessionID := stringValue(started["id"])
	defer s.deviceOAuth.Discard(sessionID)
	if err := s.deviceOAuth.SetCPATarget(sessionID, oauth.CPASync{Status: "pending", PoolID: pool.ID, PoolName: pool.Name, ReplaceExisting: true, AuthorizationTaskID: task.ID}); err != nil {
		return nil, errors.New("oauth_storage_failed")
	}
	outcome, err := oauth.ConfirmDeviceInBrowser(ctx, s.requestClient, base, stringValue(config["api_key"]), oauth.BrowserDeviceRequest{
		BrowserLease: lease.ID, VerificationURL: stringValue(started["verification_uri_complete"]), UserCode: stringValue(started["user_code"]), SSO: sso, SSORW: stringValue(account["sso_rw"]), Email: email, Proxy: proxyURL, Timeout: 120,
	})
	if err != nil && err.Error() != "oauth_worker_result_unknown" {
		return nil, err
	}
	if outcome.Status == "manual_action_required" {
		return finish("manual_action_required", grokOAuthErrorCode(errors.New(outcome.Code)))
	}
	if outcome.Status == "failed" {
		return nil, errors.New(grokOAuthErrorCode(errors.New(outcome.Code)))
	}
	// A lost worker response is not permission to repeat consent. Query the same
	// device grant until it succeeds or expires, without issuing a new grant.
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
			return nil, err
		}
		if stringValue(value["status"]) == "authorized" {
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
		case "syncing_cpa":
			stage, message = "syncing_cpa", "OAuth 已授权，正在同步 CPA"
		case "waiting_browser":
			stage, message = "waiting_browser", "等待浏览器会话释放"
		case "starting_browser":
			stage, message = "starting_browser", "正在启动浏览器"
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
