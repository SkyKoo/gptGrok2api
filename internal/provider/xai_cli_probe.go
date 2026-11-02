package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	XAICLIModel   = "grok-4.5"
	XAIClientID   = "b1a00492-073a-47ea-816f-4c329264a828"
	XAIOAuthScope = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write"
)

type XAIProbe struct {
	BaseURL  string
	TokenURL string
	HTTP     *http.Client
}

func NewXAIProbe(baseURL, tokenURL string, client *http.Client) *XAIProbe {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &XAIProbe{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), TokenURL: strings.TrimSpace(tokenURL), HTTP: client}
}

type XAIProbeResult struct {
	Status       string
	HTTPStatus   int
	Code         string
	Error        string
	Quota        map[string]any
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    int64
}

func (p *XAIProbe) Test(ctx context.Context, account map[string]any, model, prompt string) (result XAIProbeResult) {
	expiry, _ := numberValue(account["expires_at"])
	expiresAt := int64(expiry)
	defer func() { result.ExpiresAt = expiresAt }()
	access, refresh, id := stringValue(account["access_token"]), stringValue(account["refresh_token"]), stringValue(account["id_token"])
	if access == "" {
		return XAIProbeResult{Status: "invalid", Code: "invalid_credentials", Error: "access_token is missing"}
	}
	if strings.TrimSpace(model) == "" {
		model = XAICLIModel
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Reply only OK."
	}
	payload := map[string]any{"model": model, "input": []any{map[string]any{"role": "user", "content": prompt}}, "stream": false, "max_output_tokens": 64}
	if (tokenExpired(access) || (expiresAt > 0 && expiresAt <= time.Now().Unix())) && refresh != "" {
		if rotated, err := p.refresh(ctx, refresh); err == nil {
			access = rotated.AccessToken
			expiresAt = rotated.ExpiresAt
			if rotated.RefreshToken != "" {
				refresh = rotated.RefreshToken
			}
			if rotated.IDToken != "" {
				id = rotated.IDToken
			}
		}
	}
	return p.probeWithToken(ctx, access, refresh, id, payload)
}

func (p *XAIProbe) Probe(ctx context.Context, account map[string]any) (result XAIProbeResult) {
	expiry, _ := numberValue(account["expires_at"])
	expiresAt := int64(expiry)
	defer func() { result.ExpiresAt = expiresAt }()
	access, refresh, id := stringValue(account["access_token"]), stringValue(account["refresh_token"]), stringValue(account["id_token"])
	if access == "" {
		return XAIProbeResult{Status: "invalid", Code: "invalid_credentials", Error: "access_token is missing"}
	}
	if (tokenExpired(access) || (expiresAt > 0 && expiresAt <= time.Now().Unix())) && refresh != "" {
		if rotated, err := p.refresh(ctx, refresh); err == nil {
			access = rotated.AccessToken
			expiresAt = rotated.ExpiresAt
			if rotated.RefreshToken != "" {
				refresh = rotated.RefreshToken
			}
			if rotated.IDToken != "" {
				id = rotated.IDToken
			}
		}
	}
	payload := map[string]any{"model": XAICLIModel, "input": []any{map[string]any{"role": "user", "content": "Reply only OK."}}, "stream": false, "max_output_tokens": 8}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", strings.NewReader(string(raw)))
	if err != nil {
		return XAIProbeResult{Status: "unknown", Error: err.Error(), AccessToken: access, RefreshToken: refresh, IDToken: id}
	}
	setXAIHeaders(req, access)
	client := *p.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	resp, err := client.Do(req)
	if err != nil {
		return XAIProbeResult{Status: "unknown", Error: err.Error(), AccessToken: access, RefreshToken: refresh, IDToken: id}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	code, message := xaiError(body)
	if resp.StatusCode == http.StatusUnauthorized && refresh != "" {
		if rotated, refreshErr := p.refresh(ctx, refresh); refreshErr == nil {
			access = rotated.AccessToken
			expiresAt = rotated.ExpiresAt
			if rotated.RefreshToken != "" {
				refresh = rotated.RefreshToken
			}
			if rotated.IDToken != "" {
				id = rotated.IDToken
			}
			_ = resp.Body.Close()
			return p.probeWithToken(ctx, access, refresh, id, payload)
		}
	}
	result = XAIProbeResult{HTTPStatus: resp.StatusCode, Code: code, Error: message, AccessToken: access, RefreshToken: refresh, IDToken: id}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.Status = "valid"
	case resp.StatusCode == http.StatusTooManyRequests:
		result.Status = "limited"
	case resp.StatusCode == http.StatusForbidden:
		result.Status = "denied"
	case resp.StatusCode == http.StatusUnauthorized:
		result.Status = "invalid"
	default:
		result.Status = "unknown"
	}
	if result.Status == "valid" || result.Status == "limited" {
		result.Quota = p.billing(ctx, access)
	}
	return result
}

func (p *XAIProbe) probeWithToken(ctx context.Context, access, refresh, id string, payload map[string]any) XAIProbeResult {
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", strings.NewReader(string(raw)))
	if err != nil {
		return XAIProbeResult{Status: "unknown", Error: err.Error(), AccessToken: access, RefreshToken: refresh, IDToken: id}
	}
	setXAIHeaders(req, access)
	client := *p.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	resp, err := client.Do(req)
	if err != nil {
		return XAIProbeResult{Status: "unknown", Error: err.Error(), AccessToken: access, RefreshToken: refresh, IDToken: id}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	code, message := xaiError(body)
	result := XAIProbeResult{HTTPStatus: resp.StatusCode, Code: code, Error: message, AccessToken: access, RefreshToken: refresh, IDToken: id}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.Status = "valid"
	} else if resp.StatusCode == 429 {
		result.Status = "limited"
	} else if resp.StatusCode == 403 {
		result.Status = "denied"
	} else if resp.StatusCode == 401 {
		result.Status = "invalid"
	} else {
		result.Status = "unknown"
	}
	if result.Status == "valid" || result.Status == "limited" {
		result.Quota = p.billing(ctx, access)
	}
	return result
}

func (p *XAIProbe) refresh(ctx context.Context, refreshToken string) (XAITokens, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {XAIClientID}, "scope": {XAIOAuthScope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return XAITokens{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := *p.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	resp, err := client.Do(req)
	if err != nil {
		return XAITokens{}, err
	}
	defer resp.Body.Close()
	var data map[string]any
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data)
	access := stringValue(data["access_token"])
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || access == "" {
		return XAITokens{}, fmt.Errorf("xAI OAuth refresh HTTP %d: %s", resp.StatusCode, stringValue(data["error_description"]))
	}
	expiry := xaiTokenExpiry(access)
	if seconds, ok := numberValue(data["expires_in"]); ok && seconds > 0 {
		expiry = time.Now().Unix() + int64(seconds)
	}
	return XAITokens{AccessToken: access, RefreshToken: stringValue(data["refresh_token"]), IDToken: stringValue(data["id_token"]), ExpiresAt: expiry}, nil
}

type XAITokens struct {
	AccessToken, RefreshToken, IDToken string
	ExpiresAt                          int64
}

func (p *XAIProbe) billing(ctx context.Context, access string) map[string]any {
	return p.queryBilling(ctx, access)
}

func setXAIHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	version := strings.TrimSpace(os.Getenv("CFM_XAI_CLI_CLIENT_VERSION"))
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		version = "1.0.70"
	}
	req.Header.Set("User-Agent", "xai-grok-workspace/"+version)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", version)
	req.Header.Set("x-grok-client-identifier", "grok-shell")
	req.Header.Set("x-authenticateresponse", "authenticate-response")
}
func xaiError(raw []byte) (string, string) {
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	if nested, ok := data["error"].(map[string]any); ok {
		return stringValue(nested["code"]), stringValue(nested["message"])
	}
	return stringValue(data["code"]), xaiFirstNonEmpty(stringValue(data["error"]), stringValue(data["message"]), string(raw))
}
func tokenExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	exp, ok := numberValue(claims["exp"])
	return ok && exp > 0 && time.Until(time.Unix(int64(exp), 0)) <= time.Minute
}
func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		v, err := typed.Float64()
		return v, err == nil
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}

func xaiFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
