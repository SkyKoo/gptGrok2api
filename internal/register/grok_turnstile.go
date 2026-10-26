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

// GrokTurnstileSolver is intentionally independent from OpenAI Sentinel. It
// remains the compatibility seam for the raw protocol fallback; the built-in
// worker also implements GrokBrowserSignupFlow for the native page path.
type GrokTurnstileSolver interface {
	Solve(context.Context, string, string, string) (string, error)
}

// GrokBrowserContext is the short-lived browser context exported by the
// internal grok-browser worker. Cookies, user agent and SSO stay attached to
// the same Chromium session because xAI and Cloudflare bind challenge state to
// that context.
type GrokBrowserContext struct {
	SessionID   string
	Token       string
	SSO         string
	SSORW       string
	RedirectURL string
	Cookies     []browserprofile.Cookie
	UserAgent   string
	ExpiresAt   time.Time
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

// GrokBrowserSessionCloser releases the persistent Chromium page once the
// native registration flow has ended.
type GrokBrowserSessionCloser interface {
	CloseSession(context.Context, string) error
}

// GrokBrowserSignupFlow is the native xAI registration flow.  All four
// operations use one persistent browser page so the email form, Castle state,
// Turnstile result and final SSO cookies remain in the same context.
type GrokBrowserSignupFlow interface {
	StartSignup(context.Context, string, string) (GrokBrowserContext, error)
	SendEmailCode(context.Context, string, string) error
	VerifyEmailCode(context.Context, string, string, string) (GrokBrowserContext, error)
	CompleteSignup(context.Context, string, string, string, string, string, string, string) (GrokBrowserContext, error)
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

func (s *HTTPGrokBrowserSolver) workerPost(ctx context.Context, path string, payload any, stage string) ([]byte, error) {
	if s == nil || strings.TrimSpace(s.BaseURL) == "" {
		return nil, fail(stage, "browser_worker_not_configured")
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 240 * time.Second}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fail(stage, "browser_worker_request_failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return nil, fail(stage, "browser_worker_request_failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(s.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.APIKey))
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fail(stage, "browser_worker_unreachable")
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return nil, fail(stage, "browser_worker_read_failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &Failure{Stage: stage, Code: "browser_worker_rejected", HTTPStatus: resp.StatusCode}
	}
	return body, nil
}

func (s *HTTPGrokBrowserSolver) StartSignup(ctx context.Context, pageURL, proxy string) (GrokBrowserContext, error) {
	payload := map[string]any{"url": pageURL, "proxy": proxy}
	if s.Profile != nil {
		payload["timezone"] = strings.TrimSpace(s.Profile.TimezoneIANA)
		payload["locale"] = strings.TrimSpace(s.Profile.NavigatorLanguage)
	}
	body, err := s.workerPost(ctx, "/v1/session/start", payload, "bootstrap")
	if err != nil {
		return GrokBrowserContext{}, err
	}
	var result struct {
		OK        bool                    `json:"ok"`
		SessionID string                  `json:"session_id"`
		Cookies   []browserprofile.Cookie `json:"cookies"`
		UserAgent string                  `json:"user_agent"`
		URL       string                  `json:"url"`
	}
	if json.Unmarshal(body, &result) != nil || !result.OK || strings.TrimSpace(result.SessionID) == "" {
		return GrokBrowserContext{}, fail("bootstrap", "browser_worker_invalid_response")
	}
	return GrokBrowserContext{SessionID: strings.TrimSpace(result.SessionID), Cookies: result.Cookies, UserAgent: strings.TrimSpace(result.UserAgent), RedirectURL: strings.TrimSpace(result.URL)}, nil
}

func (s *HTTPGrokBrowserSolver) SendEmailCode(ctx context.Context, sessionID, email string) error {
	_, err := s.workerPost(ctx, "/v1/session/send-email", map[string]string{"session_id": sessionID, "email": email}, "send_code")
	return err
}

func (s *HTTPGrokBrowserSolver) VerifyEmailCode(ctx context.Context, sessionID, email, code string) (GrokBrowserContext, error) {
	body, err := s.workerPost(ctx, "/v1/session/verify-email", map[string]string{"session_id": sessionID, "email": email, "code": code}, "validate_code")
	if err != nil {
		return GrokBrowserContext{}, err
	}
	return parseNativeBrowserContext(body, "validate_code")
}

func (s *HTTPGrokBrowserSolver) CompleteSignup(ctx context.Context, sessionID, email, password, code, givenName, familyName, turnstileToken string) (GrokBrowserContext, error) {
	payload := map[string]string{"session_id": sessionID, "email": email, "password": password, "code": code, "given_name": givenName, "family_name": familyName, "turnstile_token": turnstileToken}
	body, err := s.workerPost(ctx, "/v1/session/signup", payload, "signup")
	if err != nil {
		return GrokBrowserContext{}, err
	}
	result, err := parseNativeBrowserContext(body, "signup")
	if err != nil {
		return GrokBrowserContext{}, err
	}
	if strings.TrimSpace(result.SSO) == "" && strings.TrimSpace(result.Token) == "" {
		return GrokBrowserContext{}, fail("sso", "missing_sso")
	}
	if result.Token == "" {
		result.Token = result.SSO
	}
	return result, nil
}

func parseNativeBrowserContext(body []byte, stage string) (GrokBrowserContext, error) {
	var result struct {
		OK          bool                    `json:"ok"`
		SessionID   string                  `json:"session_id"`
		Token       string                  `json:"token"`
		SSO         string                  `json:"sso"`
		SSORW       string                  `json:"sso_rw"`
		RedirectURL string                  `json:"redirect_url"`
		Cookies     []browserprofile.Cookie `json:"cookies"`
		UserAgent   string                  `json:"user_agent"`
	}
	if json.Unmarshal(body, &result) != nil || !result.OK {
		return GrokBrowserContext{}, fail(stage, "browser_worker_invalid_response")
	}
	return GrokBrowserContext{SessionID: strings.TrimSpace(result.SessionID), Token: strings.TrimSpace(result.Token), SSO: strings.TrimSpace(result.SSO), SSORW: strings.TrimSpace(result.SSORW), RedirectURL: strings.TrimSpace(result.RedirectURL), Cookies: result.Cookies, UserAgent: strings.TrimSpace(result.UserAgent)}, nil
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
