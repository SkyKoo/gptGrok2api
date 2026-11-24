package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"github.com/auucoder/gptgrok2api-go/internal/tasks"
)

const grokReloginKind = "grok_sso_relogin"
const grokReloginCheckpoint = "sso_relogin_checkpoint"

func (s *Server) grokReloginAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		id := r.URL.Query().Get("account_id")
		type progress struct {
			tasks.Task
			Email string `json:"email,omitempty"`
		}
		items := []progress{}
		var ids []string
		var jobs []tasks.Task
		if queue, ok := s.taskQueue.(interface{ ListWithError() ([]tasks.Task, error) }); ok {
			var err error
			jobs, err = queue.ListWithError()
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "暂时无法读取重新登录进度", "grok_relogin_progress_unavailable")
				return
			}
		} else {
			jobs = s.taskQueue.List()
		}
		for _, task := range jobs {
			if task.Kind == grokReloginKind && (id == "" || task.Payload["account_id"] == id) {
				items = append(items, progress{Task: task})
				ids = append(ids, stringValue(task.Payload["account_id"]))
			}
		}
		if len(items) > 0 {
			accounts, err := s.registerStore.GetAccounts(ids)
			if err == nil {
				emails := make(map[string]string, len(accounts))
				for _, account := range accounts {
					emails[stringValue(account["id"])] = stringValue(account["email"])
				}
				for i := range items {
					items[i].Email = emails[stringValue(items[i].Payload["account_id"])]
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	var body struct {
		AccountID string `json:"account_id"`
		RequestID string `json:"request_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	id := strings.TrimSpace(body.AccountID)
	if id == "" || len(id) > 128 || !adobeReloginRequestID.MatchString(body.RequestID) {
		writeError(w, http.StatusBadRequest, "请选择 Grok 账号并提供有效的请求标识", "invalid_request_error")
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) || !s.beginDeploymentBackground() {
		writeError(w, http.StatusServiceUnavailable, "服务维护中，未启动重新登录", "grok_relogin_unavailable")
		return
	}
	defer s.endDeploymentBackground()
	s.authStartMu.Lock()
	defer s.authStartMu.Unlock()
	var active *tasks.Task
	for _, task := range s.taskQueue.List() {
		if task.Kind != grokReloginKind {
			continue
		}
		if task.Payload["request_id"] == body.RequestID {
			if task.Payload["account_id"] != id {
				writeError(w, http.StatusConflict, "请求标识已用于另一个账号", "grok_relogin_request_conflict")
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"job": task})
			return
		}
		if task.Payload["account_id"] == id && (task.Status == "queued" || task.Status == "running") {
			active = &task
		}
	}
	if active != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"job": active})
		return
	}
	if _, err := s.grokReloginSource(id); err != nil {
		writeError(w, http.StatusConflict, "账号不可重新登录，请确认已启用且保存了邮箱和密码", err.Error())
		return
	}
	job := s.taskQueue.Submit(grokReloginKind, map[string]any{"account_id": id, "request_id": body.RequestID})
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) grokReloginSource(id string) (map[string]any, error) {
	rows, err := s.registerStore.GetAccounts([]string{id})
	if err != nil {
		return nil, errors.New("grok_relogin_storage_failed")
	}
	if len(rows) != 1 {
		return nil, errors.New("grok_relogin_account_unavailable")
	}
	password, _ := rows[0]["password"].(string)
	if grokAccountDisabled(rows[0]) || stringValue(rows[0]["email"]) == "" || strings.TrimSpace(password) == "" {
		return nil, errors.New("grok_relogin_account_unavailable")
	}
	return rows[0], nil
}

func (s *Server) runGrokRelogin(task *tasks.Task) (map[string]any, error) {
	id := stringValue(task.Payload["account_id"])
	account, err := s.grokReloginSource(id)
	if err != nil {
		return nil, err
	}
	checkpoint := mapValue(account[grokReloginCheckpoint])
	if checkpoint["stage"] == "running" && checkpoint["task_id"] == task.ID {
		return nil, errors.New("grok_relogin_interrupted")
	}
	setStage := func(stage string) error {
		current, present := s.taskQueue.Get(task.ID)
		if !present || current.Status != "running" {
			return errors.New("grok_relogin_cancelled")
		}
		if queue, ok := s.taskQueue.(interface{ SetStage(string, string) }); ok {
			queue.SetStage(task.ID, stage)
		}
		return nil
	}
	var session *grokBrowserSession
	if checkpoint["stage"] != "saved" && !(checkpoint["stage"] == "synced" && checkpoint["task_id"] == task.ID) {
		running := map[string]any{"task_id": task.ID, "stage": "running"}
		if err := s.writeGrokReloginAccount(account, map[string]any{grokReloginCheckpoint: running}); err != nil {
			return nil, err
		}
		account[grokReloginCheckpoint] = running
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		newSession, err := s.fetchGrokBrowserSession(ctx, task.ID, account, setStage)
		cancel()
		if err != nil {
			return nil, err
		}
		if !sameGrokReloginIdentity(account, newSession) {
			return nil, errors.New("grok_relogin_identity_mismatch")
		}
		session = &newSession
	}
	if err := setStage("syncing"); err != nil {
		return nil, err
	}
	saved := account
	if checkpoint["stage"] != "synced" || session != nil {
		saved, err = s.syncGrokRelogin(task.ID, account, session)
		if err != nil {
			return nil, err
		}
	}
	if err := setStage("refreshing_quota"); err != nil {
		return nil, err
	}
	quotaCtx, cancelQuota := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelQuota()
	token := stringValue(saved["sso"])
	quotas, media, refreshErr := s.refreshGrokAccountQuotas(quotaCtx, token, saved)
	if refreshErr != nil {
		s.recordGrokCredentialFailure(token, refreshErr)
	}
	_, image := media.Quotas["imagePro"]
	_, video := media.Quotas["video720p"]
	return map[string]any{
		"session_updated": true, "text_quota": len(quotas) > 0,
		"image_quota": image && media.Status == "success", "video_quota": video && media.Status == "success",
		"stage": "completed",
	}, nil
}

type grokBrowserSession struct {
	SSO   string `json:"sso"`
	SSORW string `json:"sso_rw"`
	Code  string `json:"code"`
}

func (s *Server) fetchGrokBrowserSession(ctx context.Context, taskID string, account map[string]any, stage func(string) error) (grokBrowserSession, error) {
	var result grokBrowserSession
	proxyURL := ""
	if s.proxyManager != nil {
		var bound bool
		var err error
		proxyURL, bound, err = s.proxyManager.ResolveBound(account)
		if err != nil {
			return result, errors.New("grok_relogin_proxy_unavailable")
		}
		if !bound {
			proxyURL = s.proxyManager.Resolve(account, false)
		}
	}
	config := mapValue(s.registerStore.Get()["grok"])
	base := strings.TrimRight(firstNonEmpty(stringValue(config["api_base"]), os.Getenv("CFM_BROWSER_WORKER_URL"), os.Getenv("CFM_GROK_BROWSER_URL"), "http://cfm-browser-worker:8899"), "/")
	key := stringValue(config["api_key"])
	lease, err := browserqueue.Acquire(ctx, s.requestClient, base, key, map[string]any{
		"provider": "grok", "proxy": proxyURL, "daily_direct": proxyURL == "", "browser_profile": account["browser_profile"],
	}, stage)
	if err != nil {
		return result, errors.New("grok_relogin_browser_unavailable")
	}
	defer lease.Release()
	if err = stage("signing_in"); err != nil {
		return result, err
	}
	current, err := s.grokReloginSource(stringValue(account["id"]))
	if err != nil || !sameGrokReloginAccount(account, current) {
		return result, errors.New("grok_relogin_account_changed")
	}
	password, _ := current["password"].(string)
	payload, _ := json.Marshal(map[string]string{
		"task_id": taskID, "browser_lease": lease.ID, "email": stringValue(current["email"]), "password": password,
	})
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, ""), http.MethodPost, base+"/v1/grok/relogin", bytes.NewReader(payload))
	if err != nil {
		return result, errors.New("grok_relogin_browser_unavailable")
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := *s.requestClient
	client.Timeout = 75 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	resp, err := client.Do(req)
	if err != nil {
		return result, errors.New("grok_relogin_result_unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 24<<10)).Decode(&result) != nil {
		return result, errors.New("grok_relogin_browser_failed")
	}
	switch result.Code {
	case "verification_required", "age_confirmation_required", "login_timeout", "session_wait_timeout", "password_request_unobserved", "login_entry_forbidden", "login_page_unrecognized", "login_action_ambiguous", "login_failed", "credentials_rejected", "upstream_rate_limited", "password_input_unconfirmed":
		return result, errors.New("grok_relogin_" + result.Code)
	case "":
		if validGrokSessionCookie(result.SSO) && (result.SSORW == "" || validGrokSessionCookie(result.SSORW)) {
			return result, nil
		}
	}
	return result, errors.New("grok_relogin_session_missing")
}

func validGrokSessionCookie(value string) bool {
	if len(value) == 0 || len(value) > 8192 {
		return false
	}
	for _, c := range value {
		if c < 0x21 || c > 0x7e || c == ',' || c == ';' || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

func grokTokenClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	_ = json.Unmarshal(raw, &claims)
	return claims
}

func sameGrokReloginIdentity(account map[string]any, session grokBrowserSession) bool {
	previous := grokTokenClaims(grokArchivedSSO(account))
	next := grokTokenClaims(session.SSO)
	if email := stringValue(next["email"]); email != "" && !strings.EqualFold(email, stringValue(account["email"])) {
		return false
	}
	for _, key := range []string{"sub", "user_id"} {
		if old, new := stringValue(previous[key]), stringValue(next[key]); old != "" && new != old {
			return false
		}
	}
	return true
}

func grokArchivedSSO(account map[string]any) string {
	return firstNonEmpty(stringValue(account["sso"]), stringValue(account["access_token"]), stringValue(account["token"]))
}

func sameGrokReloginAccount(expected, current map[string]any) bool {
	if grokAccountDisabled(current) {
		return false
	}
	for _, key := range []string{"sso", "access_token", "token", "email", "password", "proxy", "proxy_url", "proxyUrl", "proxy_binding", "proxy_policy", grokReloginCheckpoint} {
		if !reflect.DeepEqual(expected[key], current[key]) {
			return false
		}
	}
	return true
}

func (s *Server) writeGrokReloginAccount(expected, fields map[string]any) error {
	ok, err := s.registerStore.UpdateAccountFieldsIf(stringValue(expected["id"]), fields, func(current map[string]any) bool {
		return sameGrokReloginAccount(expected, current)
	})
	if err != nil {
		return errors.New("grok_relogin_storage_failed")
	}
	if !ok {
		return errors.New("grok_relogin_account_changed")
	}
	return nil
}

func grokTokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func (s *Server) syncGrokRelogin(taskID string, account map[string]any, fresh *grokBrowserSession) (map[string]any, error) {
	s.proxyBindingMu.Lock()
	defer s.proxyBindingMu.Unlock()
	id := stringValue(account["id"])
	token := grokArchivedSSO(account)
	checkpoint := mapValue(account[grokReloginCheckpoint])
	oldDigest := stringValue(checkpoint["previous"])
	if fresh != nil {
		oldDigest, token = grokTokenDigest(token), fresh.SSO
	}
	rows, _, err := s.grokRuntimeStore.AccountSnapshot()
	if err != nil {
		return nil, errors.New("grok_relogin_sync_failed")
	}
	var runtime map[string]any
	for _, row := range rows {
		rowToken := accountToken(row)
		isPrevious := grokTokenDigest(rowToken) == oldDigest
		if rowToken == token || isPrevious || row["grok_account_id"] == id {
			if runtime != nil || (rowToken != token && !isPrevious) ||
				(stringValue(row["grok_account_id"]) != "" && row["grok_account_id"] != id) {
				return nil, errors.New("grok_relogin_runtime_conflict")
			}
			runtime = row
		}
	}
	if runtime != nil && grokAccountDisabled(runtime) {
		return nil, errors.New("grok_relogin_account_changed")
	}
	if fresh != nil {
		archive, err := s.registerStore.ExportAccounts(nil)
		if err != nil {
			return nil, errors.New("grok_relogin_storage_failed")
		}
		for _, row := range archive {
			if row["id"] != id && grokArchivedSSO(row) == token {
				return nil, errors.New("grok_relogin_runtime_conflict")
			}
		}
	}
	runToken := accountToken(runtime)
	err = s.grokAccountPool.WithGrokSessionWrite(runtime, runToken, func() error {
		if fresh != nil {
			header := "sso=" + token
			if fresh.SSORW != "" {
				header += "; sso-rw=" + fresh.SSORW
			}
			fields := map[string]any{
				"sso": token, "sso_rw": fresh.SSORW, "cookie_header": header, "web_cookies": nil,
				"access_token": nil, "token": nil,
				"status": "active", "last_runtime_error": nil, "runtime": nil, "probe": nil, "media_quota": nil,
				grokReloginCheckpoint: map[string]any{"task_id": taskID, "stage": "saved", "previous": oldDigest},
			}
			if err := s.writeGrokReloginAccount(account, fields); err != nil {
				return err
			}
			for key, value := range fields {
				account[key] = value
			}
		} else {
			current, err := s.grokReloginSource(id)
			if err != nil || current["sso"] != token || !reflect.DeepEqual(current[grokReloginCheckpoint], checkpoint) {
				return errors.New("grok_relogin_account_changed")
			}
			account = current
		}
		if runtime == nil {
			if _, _, err := s.syncRegisteredGrokRuntimeAccountLocked(account, token); err != nil {
				return errors.New("grok_relogin_sync_failed")
			}
		} else {
			updates := map[string]any{
				"email": account["email"], "grok_account_id": id, "status": "正常",
				"accessToken": nil,
				"sso_rw":      account["sso_rw"], "cookie_header": account["cookie_header"], "web_cookies": nil,
				"media_quota": nil, "media_quota_refresh_needed": true, "grok_credential_check": nil,
				"auto_quota": nil, "fast_quota": nil, "expert_quota": nil, "heavy_quota": nil, "console_quota": nil,
				"media_quota_last_attempt_at": nil, "media_quota_failures": nil,
			}
			if _, _, err := s.grokRuntimeStore.RotateAccountTokensIf(runToken, token, "", "", updates, func(current map[string]any) bool {
				return !grokAccountDisabled(current)
			}); err != nil {
				return errors.New("grok_relogin_sync_failed")
			}
		}
		return s.writeGrokReloginAccount(account, map[string]any{
			grokReloginCheckpoint: map[string]any{"task_id": taskID, "stage": "synced"},
		})
	})
	if errors.Is(err, accounts.ErrRefreshDeferred) {
		return nil, errors.New("grok_relogin_account_busy")
	}
	return account, err
}
