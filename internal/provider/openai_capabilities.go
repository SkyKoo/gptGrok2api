package provider

import (
	"context"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"time"
)

type quotaObserverKey struct{}

func WithQuotaObserver(ctx context.Context, observer func(string)) context.Context {
	return context.WithValue(ctx, quotaObserverKey{}, observer)
}
func notifyQuotaSent(ctx context.Context, feature string) {
	if f, ok := ctx.Value(quotaObserverKey{}).(func(string)); ok && f != nil {
		f(feature)
	}
}

// FetchQuotas never rotates credentials or triggers login. A failed observation
// must leave the last known quota and authentication status untouched.
func (c *OpenAIAccountClient) FetchQuotas(ctx context.Context, a accounts.Account) (map[string]any, error) {
	init, err := c.getConversationInit(ctx, a.Token, a.Fields)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	limits, ok := init["limits_progress"].([]any)
	if !ok {
		return nil, fmt.Errorf("upstream did not return a limits_progress array")
	}
	quota, reset, unknown := extractImageQuota(limits)
	return map[string]any{"limits_progress": limits, "capability_quotas": accounts.Quotas(limits, now), "quota_observed_at": now.UTC().Format(time.RFC3339), "quota": quota, "image_quota_unknown": unknown, "restore_at": reset, "default_model_slug": init["default_model_slug"]}, nil
}
