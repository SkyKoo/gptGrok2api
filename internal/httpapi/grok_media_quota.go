package httpapi

import (
	"context"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// Refresh both observations without making media permissions a text-login test.
func (s *Server) refreshGrokAccountQuotas(ctx context.Context, token string, fields map[string]any) (map[string]provider.QuotaWindow, provider.GrokMediaQuotaSnapshot, error) {
	mediaResult := make(chan provider.GrokMediaQuotaSnapshot, 1)
	go func() { mediaResult <- s.grokQuota.RefreshMedia(ctx, token, fields) }()
	quotas, err := s.grokQuota.RefreshToken(ctx, token, fields)
	media := <-mediaResult
	if err == nil {
		s.persistRuntimeToken(token, quotas, "active", "")
	}
	// Replace the complete snapshot, including on failure, so stale positive
	// values cannot survive a denied query or missing categories.
	_, _ = s.grokRuntimeStore.UpdateAccountRuntime(token, map[string]any{"media_quota": media})
	items, loadErr := s.registerStore.ExportAccounts(nil)
	if loadErr == nil {
		for _, item := range items {
			saved := firstNonEmpty(stringValue(item["sso"]), stringValue(item["access_token"]), stringValue(item["token"]))
			if strings.TrimSpace(saved) == strings.TrimSpace(token) {
				_, _ = s.registerStore.UpdateAccountFields(stringValue(item["id"]), map[string]any{"media_quota": media})
			}
		}
	}
	return quotas, media, err
}
