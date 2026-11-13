package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/tasks"
	"github.com/auucoder/gptgrok2api-go/internal/xaiquality"
)

// Caller must hold the per-account action and identity locks. This also covers
// scoring, persisting the block, and remote cleanup as one account operation.
func (s *Server) checkGrokQualityLocked(ctx context.Context, account oauth.Account) (oauth.Account, error) {
	ctx, cancel := context.WithTimeout(ctx, 7*time.Minute)
	defer cancel()
	report := xaiquality.Report{Version: xaiquality.Version, Model: xaiquality.Model, Status: "error", At: time.Now().Unix()}
	if account.Quality != nil {
		report.Blocked = account.Quality.Blocked
		report.Removal = account.Quality.Removal
		report.ConfirmedAt = account.Quality.ConfirmedAt
		report.RiskSamples = account.Quality.RiskSamples
	}
	var call xaiquality.Caller
	var target *xaiquality.Removal
	var err error
	if oauth.CPAManaged(account) {
		report.Source = "cpa"
		call, target, err = s.qualityCPACaller(ctx, account)
	} else {
		report.Source = "oauth"
		if account.AccessToken == "" || account.ExpiresAt > 0 && account.ExpiresAt <= time.Now().Unix() {
			err = errors.New("本地凭据已过期，请重新授权后检测")
		} else {
			ctx, err = s.oauthRequestContext(ctx, account)
			call = func(ctx context.Context, payload []byte) (xaiquality.Response, error) {
				return s.xaiProbe.QualityResponse(ctx, account.AccessToken, payload)
			}
		}
	}
	if err != nil {
		report.Summary = err.Error()
	} else {
		previous := report
		report = xaiquality.Run(ctx, call, account.Quality)
		report.Source = previous.Source
		report.Removal = previous.Removal
	}
	if report.Status == "normal" && (!oauth.CPAManaged(account) || report.Removal != nil && report.Removal.Status != "removed") {
		report.Removal = nil
	}
	// Only synthetic challenge answers are stored; never persist HTTP error
	// bodies, credentials, or the CPA management key as evidence.
	for i := range report.Samples {
		for _, secret := range []string{account.AccessToken, account.RefreshToken, account.IDToken} {
			if secret != "" {
				report.Samples[i].Answer = strings.ReplaceAll(report.Samples[i].Answer, secret, "[redacted]")
			}
		}
	}
	report.At = time.Now().Unix()
	if report.Status == "high_risk" {
		report.ConfirmedAt = report.At
		report.RiskSamples = report.Samples
		if target == nil && account.CPASync != nil && account.CPASync.ReplaceExisting {
			target = s.existingQualityCPATarget(ctx, account)
		}
	}
	if report.Status == "high_risk" && target != nil {
		target.Status = "pending"
		target.At = report.At
		report.Removal = target
	}
	updated, applied, saveErr := s.oauthStore.UpdateIfDelivery(account, func(current *oauth.Account) { current.Quality = &report })
	if saveErr != nil || !applied {
		return account, errors.New("检测结果未能保存或凭据已变化，未执行 CPA 删除")
	}
	account = updated
	// The block is durable before a destructive remote action. If the process
	// stops here, the pending cleanup is visible and can be retried safely.
	if report.Status == "high_risk" && target != nil && account.Status != "disabled" {
		return s.cleanupGrokQualityLocked(ctx, account)
	}
	return account, nil
}

func (s *Server) cleanupGrokQualityLocked(ctx context.Context, account oauth.Account) (oauth.Account, error) {
	latest, found, err := s.oauthStore.Get(account.ID)
	if err != nil || !found || latest.Status == "disabled" || !oauth.SameCredential(account, latest) {
		return account, errors.New("账号已移除、禁用或重新授权，未删除 CPA 凭据")
	}
	q := account.Quality
	if q == nil || !q.Blocked || q.ConfirmedAt == 0 || q.Removal == nil {
		return account, errors.New("没有已确认的降智风险及 CPA 删除目标")
	}
	if q.Removal.Status == "removed" {
		return account, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	report := *q
	removal := s.removeQualityCPA(ctx, account, *q.Removal)
	report.Removal = &removal
	updated, applied, err := s.oauthStore.UpdateIfDelivery(account, func(current *oauth.Account) {
		current.Quality = &report
		if current.CPASync != nil && removal.Status == "removed" {
			receipt := *current.CPASync
			receipt.Status = "quality_removed"
			receipt.Error = "降智风险凭据已从 CPA 移除"
			current.CPASync = &receipt
		}
	})
	if err != nil || !applied {
		return account, errors.New("CPA 删除状态未能保存，请人工核对")
	}
	return updated, nil
}

// Reauthorizing an account must not strand a known bad previous CPA copy.
// Its current token is neither downloaded nor replaced; only its identity is read.
func (s *Server) existingQualityCPATarget(ctx context.Context, account oauth.Account) *xaiquality.Removal {
	cp := account.CPASync
	if cp == nil || cp.PoolID == "" || !cpaFileNameValid(cp.FileName) {
		return nil
	}
	pool, ok := s.external.getCPA(cp.PoolID)
	target := &xaiquality.Removal{PoolID: cp.PoolID, PoolName: cp.PoolName, FileName: cp.FileName}
	if !ok {
		return target
	}
	target.PoolURL = strings.TrimRight(pool.BaseURL, "/")
	identity, exists, err := qualityCPAFile(ctx, qualityCPAClient(), pool, account)
	if err == nil && !exists {
		return nil
	}
	if err == nil {
		target.AuthIndex = identity.Index
	}
	return target
}

func (s *Server) checkGrokQuality(ctx context.Context, id string, cleanupOnly bool) (oauth.Account, error) {
	if _, busy := s.grokOAuthActions.LoadOrStore(id, true); busy {
		return oauth.Account{}, errors.New("该账号正在授权、检测或同步，请稍后重试")
	}
	defer s.grokOAuthActions.Delete(id)
	account, found, err := s.oauthStore.Get(id)
	if err != nil || !found {
		return account, errors.New("OAuth 账号不存在")
	}
	unlock := s.oauthStore.LockIdentity(account.Email)
	defer unlock()
	account, found, err = s.oauthStore.Get(id)
	if err != nil || !found {
		return account, errors.New("OAuth 账号不存在")
	}
	if account.Status == "disabled" {
		return account, errors.New("OAuth 账号已禁用")
	}
	if s.qualityAuthorizationBusy(account.Email) {
		return account, errors.New("该账号正在授权，请授权完成后再检测")
	}
	if cleanupOnly {
		return s.cleanupGrokQualityLocked(ctx, account)
	}
	return s.checkGrokQualityLocked(ctx, account)
}

func (s *Server) qualityAuthorizationBusy(email string) bool {
	if s.deviceOAuth.HasAccountSession(email) {
		return true
	}
	activeSources := map[string]bool{}
	for _, task := range s.taskQueue.List() {
		if task.Kind == "grok_oauth_authorize" && (task.Status == "queued" || task.Status == "running") {
			activeSources[stringValue(task.Payload["account_id"])] = true
		}
	}
	if len(activeSources) == 0 {
		return false
	}
	rows, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		return true
	}
	for _, row := range rows {
		if activeSources[stringValue(row["id"])] && strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(stringValue(row["email"]))) {
			return true
		}
	}
	return false
}

func (s *Server) runGrokQualityTask(task *tasks.Task) (map[string]any, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if current, found := s.taskQueue.Get(task.ID); !found || current.Status == "cancelled" {
		cancel()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			current, found := s.taskQueue.Get(task.ID)
			if !found || current.Status == "cancelled" {
				cancel()
				return
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	account, err := s.checkGrokQuality(ctx, stringValue(task.Payload["account_id"]), boolValue(task.Payload["cleanup_only"], false))
	return map[string]any{"account_id": account.ID}, err
}

func (s *Server) enqueueGrokQuality(ids []string, cleanupOnly bool) ([]string, error) {
	s.authStartMu.Lock()
	defer s.authStartMu.Unlock()
	accounts, err := s.oauthStore.List()
	if err != nil {
		return nil, errors.New("无法读取 OAuth 账号")
	}
	active := map[string]string{}
	for _, task := range s.taskQueue.List() {
		if task.Kind == "grok_oauth_quality" && (task.Status == "queued" || task.Status == "running") {
			active[stringValue(task.Payload["account_id"])] = task.ID
		}
	}
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	jobs := []string{}
	for _, account := range accounts {
		if !selected[account.ID] || account.Status == "disabled" {
			continue
		}
		if id := active[account.ID]; id != "" {
			jobs = append(jobs, id)
			continue
		}
		job := s.taskQueue.Submit("grok_oauth_quality", map[string]any{"account_id": account.ID, "cleanup_only": cleanupOnly})
		jobs = append(jobs, job.ID)
	}
	return jobs, nil
}

func (s *Server) grokQualityAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		items := []map[string]any{}
		for _, task := range s.taskQueue.List() {
			if task.Kind == "grok_oauth_quality" {
				items = append(items, map[string]any{"id": task.ID, "account_id": task.Payload["account_id"], "status": task.Status, "error": task.Error, "updated_at": task.UpdatedAt})
			}
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "不支持的请求方法", "invalid_request_error")
		return
	}
	var body struct {
		IDs         []string `json:"ids"`
		CleanupOnly bool     `json:"cleanup_only"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 || len(body.IDs) > 500 {
		writeError(w, 400, "请选择1至500个账号", "invalid_request_error")
		return
	}
	jobs, err := s.enqueueGrokQuality(body.IDs, body.CleanupOnly)
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	writeJSON(w, 202, map[string]any{"job_ids": jobs})
}
