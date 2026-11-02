package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type XAIQuotaResult struct {
	Quota                              map[string]any
	AccessToken, RefreshToken, IDToken string
	ExpiresAt                          int64
}

// QueryQuota only reads billing. It never issues a model response to obtain quota.
func (p *XAIProbe) QueryQuota(ctx context.Context, account map[string]any) XAIQuotaResult {
	result := XAIQuotaResult{AccessToken: stringValue(account["access_token"]), RefreshToken: stringValue(account["refresh_token"]), IDToken: stringValue(account["id_token"])}
	expiry, _ := numberValue(account["expires_at"])
	result.ExpiresAt = int64(expiry)
	if result.RefreshToken != "" && (tokenExpired(result.AccessToken) || result.AccessToken == "" || (result.ExpiresAt > 0 && result.ExpiresAt <= time.Now().Unix())) {
		rotated, err := p.refresh(ctx, result.RefreshToken)
		if err != nil {
			result.Quota = billingFailure("refresh_failed", "OAuth 凭据刷新失败，请检查授权状态", 0)
			return result
		}
		result.AccessToken = rotated.AccessToken
		if rotated.RefreshToken != "" {
			result.RefreshToken = rotated.RefreshToken
		}
		if rotated.IDToken != "" {
			result.IDToken = rotated.IDToken
		}
		result.ExpiresAt = rotated.ExpiresAt
	}
	if result.AccessToken == "" {
		result.Quota = billingFailure("missing_access_token", "缺少 OAuth Access Token", 0)
		return result
	}
	result.Quota = p.queryBilling(ctx, result.AccessToken)
	return result
}

func billingFailure(code, message string, status int) map[string]any {
	return map[string]any{"source": "billing", "status": "error", "code": code, "error": message, "http_status": status, "updated_at": time.Now().UTC().Format(time.RFC3339)}
}

func (p *XAIProbe) queryBilling(ctx context.Context, access string) map[string]any {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/billing", nil)
	if err != nil {
		return billingFailure("request_invalid", "OAuth 额度查询地址无效", 0)
	}
	setXAIHeaders(req, access)
	client := *p.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	resp, err := client.Do(req)
	if err != nil {
		return billingFailure("query_failed", "OAuth 额度查询失败或超时", 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return billingFailure(fmt.Sprintf("http_%d", resp.StatusCode), fmt.Sprintf("OAuth 额度查询返回 HTTP %d", resp.StatusCode), resp.StatusCode)
	}
	var data map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data) != nil {
		return billingFailure("invalid_response", "OAuth 额度响应格式无效", resp.StatusCode)
	}
	quota := parseXAIBilling(data)
	quota["http_status"] = resp.StatusCode
	quota["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	return quota
}

func parseXAIBilling(data map[string]any) map[string]any {
	out := map[string]any{"source": "billing", "status": "unavailable", "code": "quota_not_reported", "total_remaining_known": false}
	if cfg, ok := data["config"].(map[string]any); ok {
		period := map[string]any{}
		for key, destination := range map[string]string{"billingPeriodStart": "start", "billingPeriodEnd": "end"} {
			if value, ok := cfg[key].(string); ok {
				if _, err := time.Parse(time.RFC3339, value); err == nil {
					period[destination] = value
				}
			}
		}
		if len(period) > 0 {
			out["period"] = period
		}
		limit, hasLimit := billingNumber(cfg["monthlyLimit"])
		used, hasUsed := billingNumber(cfg["used"])
		cap, hasCap := billingNumber(cfg["onDemandCap"])
		if hasLimit {
			out["monthly_limit"] = limit
		}
		if hasUsed {
			out["used"] = used
		}
		if hasCap {
			out["on_demand_cap"] = cap
		}
		if hasLimit && hasUsed && limit >= 0 && used >= 0 {
			out["status"] = "success"
			delete(out, "code")
			if limit > 0 {
				percent := math.Min(100, used/limit*100)
				out["used_percent"] = percent
				out["remaining_percent"] = math.Max(0, 100-percent)
				out["monthly_exhausted"] = used >= limit
			} else if hasCap && cap == 0 {
				// Live verification: billing can be all zero while /responses
				// succeeds. This endpoint is not an account-wide allowance.
				out["monthly_zero"] = true
			} else if hasCap && cap > 0 {
				out["on_demand_only"] = true
			}
		}
		return out
	}
	if used, ok := billingNumber(data["creditUsagePercent"]); ok && used >= 0 && used <= 100 {
		out["status"] = "success"
		delete(out, "code")
		out["used_percent"] = used
		out["remaining_percent"] = 100 - used
	}
	return out
}

func billingNumber(value any) (float64, bool) {
	if object, ok := value.(map[string]any); ok {
		return billingNumber(object["val"])
	}
	if text, ok := value.(string); ok {
		n, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
	}
	n, ok := numberValue(value)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func xaiTokenExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	value, _ := numberValue(claims["exp"])
	return int64(value)
}
