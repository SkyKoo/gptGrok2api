package httpapi

import (
	"context"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"net/http"
	"strings"
	"time"
)

func (s *Server) refreshOAuthQuota(ctx context.Context, id string) (oauth.Account, error) {
	if _, busy := s.grokOAuthActions.LoadOrStore(id, true); busy {
		return oauth.Account{}, errors.New("该 OAuth 账号正在处理其他操作，请稍后刷新")
	}
	defer s.grokOAuthActions.Delete(id)
	account, found, err := s.oauthStore.Get(id)
	if err != nil {
		return account, err
	}
	if !found {
		return account, errors.New("OAuth 账号不存在")
	}
	if account.Status == "disabled" {
		return account, errors.New("OAuth 账号已禁用")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	probe := *s.xaiProbe
	if account.TokenEndpoint != "" {
		probe.TokenURL = account.TokenEndpoint
	}
	result := probe.QueryQuota(ctx, map[string]any{"access_token": account.AccessToken, "refresh_token": account.RefreshToken, "id_token": account.IDToken, "expires_at": account.ExpiresAt})
	updated, applied, err := s.oauthStore.UpdateIfCredential(account, func(current *oauth.Account) {
		current.Quota = result.Quota
		current.AccessToken = result.AccessToken
		current.RefreshToken = result.RefreshToken
		current.IDToken = result.IDToken
		current.ExpiresAt = result.ExpiresAt
		if current.CPASync != nil && !oauth.SameCredential(account, *current) {
			cp := *current.CPASync
			cp.Status = "pending"
			cp.Error = "凭据已刷新，请重新同步"
			current.CPASync = &cp
		}
	})
	if err != nil {
		return account, err
	}
	if !applied {
		return updated, errors.New("授权凭据已变化，请重新刷新")
	}
	return updated, nil
}

func (s *Server) finishOAuthQuota(ctx context.Context, result map[string]any) {
	if stringValue(result["status"]) != "authorized" {
		return
	}
	id := stringValue(mapValue(result["account"])["id"])
	if id == "" {
		return
	}
	if account, err := s.refreshOAuthQuota(ctx, id); err == nil {
		result["account"] = oauth.Public(account)
	}
}

func (s *Server) oauthQuotaAPI(w http.ResponseWriter, r *http.Request, id string) {
	item, err := s.refreshOAuthQuota(r.Context(), strings.TrimSpace(id))
	if err != nil {
		writeError(w, http.StatusConflict, err.Error(), "quota_refresh_failed")
		return
	}
	// Even a failed query returns its stored diagnostic; it must not erase authorization.
	writeJSON(w, http.StatusOK, map[string]any{"account": oauth.Public(item), "ok": stringValue(item.Quota["status"]) != "error"})
}
