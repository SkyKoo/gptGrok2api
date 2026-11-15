package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/xaiquality"
)

// Removal changes only the delivery receipt. Authorization and quality evidence
// remain intact, including when CPA refuses deletion or the response is lost.
func (s *Server) removeGrokOAuthCPA(ctx context.Context, id, poolID, fileName string) (oauth.Account, error) {
	if _, busy := s.grokOAuthActions.LoadOrStore(id, true); busy {
		return oauth.Account{}, errors.New("该账号正在授权、检测或同步，请稍后重试")
	}
	defer s.grokOAuthActions.Delete(id)
	account, found, err := s.oauthStore.Get(id)
	if err != nil || !found {
		return account, errors.New("无法读取 OAuth 记录")
	}
	unlock := s.oauthStore.LockIdentity(account.Email)
	defer unlock()
	account, found, err = s.oauthStore.Get(id)
	if err != nil || !found {
		return account, errors.New("无法读取 OAuth 记录")
	}
	if s.qualityAuthorizationBusy(account.Email) {
		return account, errors.New("该账号正在授权，请完成后再移除 CPA 凭据")
	}
	cp := account.CPASync
	if !oauth.CPAManaged(account) || cp == nil || cp.PoolID == "" || !cpaFileNameValid(cp.FileName) {
		return account, errors.New("没有可核对的 CPA 交付记录，不能删除远端凭据")
	}
	if poolID != cp.PoolID || fileName != cp.FileName {
		return account, errors.New("CPA 目标已变化，请刷新账号后重新确认")
	}
	if cp.ManualRemoval != nil && cp.ManualRemoval.Status == "removed" {
		return account, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var target xaiquality.Removal
	if cp.ManualRemoval != nil {
		// Retry the original pinned target; never adopt a replacement file.
		target = *cp.ManualRemoval
	} else {
		pool, ok := s.external.getCPA(cp.PoolID)
		if !ok {
			return account, errors.New("原 CPA 连接不存在，请核对远端凭据")
		}
		identity, exists, lookupErr := qualityCPAFile(ctx, qualityCPAClient(), pool, account)
		if lookupErr != nil {
			return account, lookupErr
		}
		if exists && identity.Index == "" {
			return account, errors.New("CPA 凭据缺少 auth_index，无法安全指定删除目标，请人工核对")
		}
		target = xaiquality.Removal{PoolID: cp.PoolID, PoolName: pool.Name, PoolURL: strings.TrimRight(pool.BaseURL, "/"), FileName: cp.FileName, AuthIndex: identity.Index}
	}
	save := func(status string) error {
		updated, applied, saveErr := s.oauthStore.UpdateIfDelivery(account, func(current *oauth.Account) {
			receipt := *current.CPASync
			removal := target
			receipt.Status, receipt.Error, receipt.ManualRemoval = status, target.Error, &removal
			current.CPASync = &receipt
		})
		if saveErr != nil || !applied {
			return errors.New("CPA 移除状态未能保存或账号已变化，请核对远端文件")
		}
		account = updated
		return nil
	}
	target.Status, target.Error, target.At = "pending", "", time.Now().Unix()
	// Fence uploads and queued probes durably before touching the remote file.
	if err := save("remove_pending"); err != nil {
		return account, err
	}
	target = s.removeQualityCPA(ctx, account, target)
	status := "remove_failed"
	if target.Status == "removed" {
		status = "removed"
	}
	if err := save(status); err != nil {
		return account, err
	}
	if target.Status != "removed" {
		return account, errors.New(target.Error)
	}
	return account, nil
}

func (s *Server) grokOAuthCPARemove(w http.ResponseWriter, r *http.Request, id string) {
	var request struct {
		PoolID   string `json:"cpa_pool_id"`
		FileName string `json:"file_name"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.PoolID == "" || request.FileName == "" {
		writeError(w, http.StatusBadRequest, "请指定账号已记录的 CPA 连接和文件名", "invalid_request_error")
		return
	}
	account, err := s.removeGrokOAuthCPA(r.Context(), strings.TrimSpace(id), request.PoolID, request.FileName)
	code, message := http.StatusOK, ""
	if err != nil {
		code, message = http.StatusConflict, err.Error()
	}
	writeJSON(w, code, map[string]any{"ok": err == nil, "account": oauth.Public(account), "error": message})
}
