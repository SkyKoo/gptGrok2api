package register

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserprofile"
)

// GrokTurnstileSolver is intentionally independent from OpenAI Sentinel. A
// solver receives the live Grok site key and page URL and returns a provider
// issued Turnstile token for the same proxy used by the protocol session.
type GrokTurnstileSolver interface {
	Solve(context.Context, string, string, string) (string, error)
}

// GrokBrowserContext is the short-lived browser context exported by the
// internal grok-browser worker. The token is single-use and the cookies and
// user agent are returned with it because Cloudflare may bind the token to the
// browser session that produced it.
type GrokBrowserContext struct {
	SessionID string
	Token     string
	Cookies   []browserprofile.Cookie
	UserAgent string
	ExpiresAt time.Time
}

// GrokBrowserContextSolver returns the worker session context together with
// the token so the protocol request can reuse the same browser identity.
type GrokBrowserContextSolver interface {
	SolveContext(context.Context, string, string, string) (GrokBrowserContext, error)
}

// GrokBrowserProfileSetter supplies the account-scoped browser environment to
// the worker. Locale and timezone must follow the persisted registration
// profile instead of being global worker defaults.
type GrokBrowserProfileSetter interface {
	SetBrowserProfile(BrowserProfile)
}

// GrokBrowserSessionCloser releases the Chromium page that issued a
// single-use Turnstile token once the protocol flow has ended.
type GrokBrowserSessionCloser interface {
	CloseSession(context.Context, string) error
}

// GrokSSOMaterializer lets the same worker turn a signup wrapper cookie into
// the final session cookie using a browser navigation.
type GrokSSOMaterializer interface {
	MaterializeSSO(context.Context, string, string, []browserprofile.Cookie) (GrokBrowserContext, error)
}

// HTTPGrokBrowserSolver calls the CFM-owned grok-browser worker. The worker
// runs Chromium/CDP in its own container and never exposes browser control to
// the public API.
type HTTPGrokBrowserSolver struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
	Timeout time.Duration
	Profile *BrowserProfile
}

func (s *HTTPGrokBrowserSolver) SetBrowserProfile(profile BrowserProfile) {
	if s == nil {
		return
	}
	copy := profile
	s.Profile = &copy
}

func (s *HTTPGrokBrowserSolver) Solve(ctx context.Context, siteKey, pageURL, proxy string) (string, error) {
	result, err := s.SolveContext(ctx, siteKey, pageURL, proxy)
	if err != nil {
		return "", err
	}
	return result.Token, nil
}

func (s *HTTPGrokBrowserSolver) SolveContext(ctx context.Context, siteKey, pageURL, proxy string) (GrokBrowserContext, error) {
	if s == nil || strings.TrimSpace(s.BaseURL) == "" {
		return GrokBrowserContext{}, fail("turnstile", "browser_worker_not_configured")
	}
	if strings.TrimSpace(siteKey) == "" {
		return GrokBrowserContext{}, fail("turnstile", "missing_site_key")
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	payload := map[string]any{
		"sitekey":   siteKey,
		"url":       pageURL,
		"proxy":     proxy,
		"timeout_s": int(timeout / time.Second),
	}
	if s.Profile != nil {
		payload["timezone"] = strings.TrimSpace(s.Profile.TimezoneIANA)
		payload["locale"] = strings.TrimSpace(s.Profile.NavigatorLanguage)
	}
	endpoint := strings.TrimRight(s.BaseURL, "/") + "/v1/turnstile/session"
	raw, err := json.Marshal(payload)
	if err != nil {
		return GrokBrowserContext{}, fail("turnstile", "browser_worker_request_failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return GrokBrowserContext{}, fail("turnstile", "browser_worker_request_failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(s.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.APIKey))
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return GrokBrowserContext{}, ctx.Err()
		}
		return GrokBrowserContext{}, fail("turnstile", "browser_worker_unreachable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return GrokBrowserContext{}, fail("turnstile", "browser_worker_read_failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return GrokBrowserContext{}, fail("turnstile", "browser_worker_rejected")
	}
	var result struct {
		OK        bool                    `json:"ok"`
		Token     string                  `json:"token"`
		SessionID string                  `json:"session_id"`
		Cookies   []browserprofile.Cookie `json:"cookies"`
		UserAgent string                  `json:"user_agent"`
		ExpiresAt string                  `json:"expires_at"`
		Error     string                  `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return GrokBrowserContext{}, fail("turnstile", "browser_worker_invalid_response")
	}
	if !result.OK || strings.TrimSpace(result.Token) == "" {
		if result.Error != "" {
			return GrokBrowserContext{}, &Failure{Stage: "turnstile", Code: "browser_worker_failed"}
		}
		return GrokBrowserContext{}, fail("turnstile", "browser_worker_missing_token")
	}
	var expiresAt time.Time
	if result.ExpiresAt != "" {
		expiresAt, _ = time.Parse(time.RFC3339, result.ExpiresAt)
	}
	return GrokBrowserContext{
		SessionID: result.SessionID,
		Token:     result.Token,
		Cookies:   result.Cookies,
		UserAgent: strings.TrimSpace(result.UserAgent),
		ExpiresAt: expiresAt,
	}, nil
}

func (s *HTTPGrokBrowserSolver) CloseSession(ctx context.Context, sessionID string) error {
	if s == nil || strings.TrimSpace(s.BaseURL) == "" || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	payload, err := json.Marshal(map[string]string{"session_id": strings.TrimSpace(sessionID)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/v1/session/close", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(s.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.APIKey))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("browser worker close rejected: %s", resp.Status)
	}
	return nil
}

func (s *HTTPGrokBrowserSolver) MaterializeSSO(ctx context.Context, wrapper, proxy string, cookies []browserprofile.Cookie) (GrokBrowserContext, error) {
	if s == nil || strings.TrimSpace(s.BaseURL) == "" {
		return GrokBrowserContext{}, fail("sso", "browser_worker_not_configured")
	}
	if strings.TrimSpace(wrapper) == "" {
		return GrokBrowserContext{}, fail("sso", "missing_sso_wrapper")
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	payload := map[string]any{"wrapper": wrapper, "proxy": proxy, "cookies": cookies}
	raw, err := json.Marshal(payload)
	if err != nil {
		return GrokBrowserContext{}, fail("sso", "browser_worker_request_failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/v1/sso/materialize", bytes.NewReader(raw))
	if err != nil {
		return GrokBrowserContext{}, fail("sso", "browser_worker_request_failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(s.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.APIKey))
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return GrokBrowserContext{}, ctx.Err()
		}
		return GrokBrowserContext{}, fail("sso", "browser_worker_unreachable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return GrokBrowserContext{}, fail("sso", "browser_worker_rejected")
	}
	var result struct {
		OK        bool                    `json:"ok"`
		Token     string                  `json:"token"`
		SessionID string                  `json:"session_id"`
		Cookies   []browserprofile.Cookie `json:"cookies"`
		UserAgent string                  `json:"user_agent"`
		ExpiresAt string                  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil || !result.OK || strings.TrimSpace(result.Token) == "" {
		return GrokBrowserContext{}, fail("sso", "browser_worker_missing_sso")
	}
	var expiresAt time.Time
	if result.ExpiresAt != "" {
		expiresAt, _ = time.Parse(time.RFC3339, result.ExpiresAt)
	}
	return GrokBrowserContext{SessionID: result.SessionID, Token: result.Token, Cookies: result.Cookies, UserAgent: strings.TrimSpace(result.UserAgent), ExpiresAt: expiresAt}, nil
}
