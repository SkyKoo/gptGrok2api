package httpapi

import (
	"context"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
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
	unlock := s.oauthStore.LockIdentity(account.Email)
	defer unlock()
	account, found, err = s.oauthStore.Get(id)
	if err != nil || !found {
		return account, errors.New("无法读取 OAuth 凭据")
	}
	if oauth.CPAManaged(account) {
		return account, errors.New("该 OAuth 已交给 CPA 续期，请在 CPA 查看额度和状态")
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
	ctx, err = s.oauthRequestContext(ctx, account)
	if err != nil {
		return account, err
	}
	result := probe.QueryQuota(ctx, map[string]any{"access_token": account.AccessToken, "refresh_token": account.RefreshToken, "id_token": account.IDToken, "expires_at": account.ExpiresAt})
	updated, applied, err := s.oauthStore.UpdateIfCredential(account, func(current *oauth.Account) {
		current.Quota = result.Quota
		// Billing validates credentials, not model permissions or media quotas.
		if current.Status != "disabled" && current.Status == account.Status {
			switch intValue(result.Quota["http_status"]) {
			case 200:
				if stringValue(result.Quota["status"]) == "error" {
					result.Quota["credential_status"] = "unknown"
					break
				}
				result.Quota["credential_status"] = "valid"
				if current.Status == "invalid" || current.Status == "expired" {
					current.Status = "active"
				}
			case 401:
				result.Quota["credential_status"] = "invalid"
				current.Status = "invalid"
			default:
				result.Quota["credential_status"] = "unknown"
			}
		}
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

// OAuth upkeep follows the same daily route as the source Grok account. Imported
// OAuth identities without a local SSO account inherit the current global exit.
func (s *Server) oauthRequestContext(ctx context.Context, account oauth.Account) (context.Context, error) {
	var source map[string]any
	rows, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		return ctx, errors.New("无法读取账号日常出口")
	}
	for _, row := range rows {
		if strings.EqualFold(stringValue(row["email"]), account.Email) && account.Email != "" {
			source = row
			break
		}
	}
	route := ""
	if s.proxyManager != nil {
		value, bound, err := s.proxyManager.ResolveBound(source)
		if err != nil {
			return ctx, errors.New("账号日常出口不可用")
		}
		if bound {
			route = value
		} else {
			route = s.proxyManager.Resolve(source, false)
		}
	}
	return proxyruntime.WithURL(ctx, route), nil
}
