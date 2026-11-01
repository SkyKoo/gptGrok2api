package httpapi

import (
	"context"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// Text authentication and media balances remain independent. The pool gates all
// media observations, including manual refreshes, against in-flight generation.
func (s *Server) refreshGrokAccountQuotas(ctx context.Context, token string, fields map[string]any) (map[string]provider.QuotaWindow, provider.GrokMediaQuotaSnapshot, error) {
	// Ensure newly imported accounts exist before the media pool locks the identity.
	// This does not re-enable an existing invalid/disabled account.
	s.ensureGrokRuntimeAccountForQuota(token, fields)
	mediaResult := make(chan provider.GrokMediaQuotaSnapshot, 1)
	go func() { mediaResult <- s.refreshGrokMediaQuota(ctx, token, fields) }()
	quotas, err := s.grokQuota.RefreshToken(ctx, token, fields)
	media := <-mediaResult
	if err == nil {
		s.persistRuntimeToken(token, quotas, "active", "")
	}
	return quotas, media, err
}

func (s *Server) ensureGrokRuntimeAccountForQuota(token string, fields map[string]any) {
	rows, _ := s.grokRuntimeStore.AccountList()
	for _, row := range rows {
		if accountToken(row) == token {
			return
		}
	}
	_, _, _ = s.syncRegisteredGrokRuntimeAccount(fields, token)
}
