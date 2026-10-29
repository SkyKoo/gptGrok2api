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
// the worker. The timezone follows the persisted registration profile; the
// language is normalized to English for all Grok browser sessions.
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
	copy := normalizeGrokBrowserProfile(profile)
	s.Profile = &copy
}

func (s *HTTPGrokBrowserSolver) browserProfilePayload() map[string]any {
	if s == nil || s.Profile == nil {
		return nil
	}
	raw, err := json.Marshal(s.Profile)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	return payload
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
		failure := &Failure{Stage: stage, Code: "browser_worker_rejected", HTTPStatus: resp.StatusCode}
		// The worker returns a structured FastAPI detail envelope. Preserve only
		// stable, non-sensitive reason codes in the task journal so a 504 can be
		// distinguished from rate limiting, duplicate email and page-state
		// failures without storing page text, cookies or challenge values.
		var envelope struct {
			Detail struct {
				Stage      string `json:"stage"`
				Error      string `json:"error"`
				Diagnostic struct {
					Reason                 string                  `json:"reason"`
					UpstreamCode           string                  `json:"upstream_code"`
					UpstreamStatus         int                     `json:"upstream_status"`
					NetworkFailures        []browserNetworkFailure `json:"network_failures"`
					TurnstileClicks        int                     `json:"turnstile_clicks"`
					TurnstileFrames        int                     `json:"turnstile_frames"`
					TurnstileVisibleFrames int                     `json:"turnstile_visible_frames"`
					TurnstileWidgets       int                     `json:"turnstile_widgets"`
					TurnstileReady         bool                    `json:"turnstile_ready"`
					TurnstileCallbackSeen  bool                    `json:"turnstile_callback_seen"`
					TurnstileTokenPresent  bool                    `json:"turnstile_token_present"`
				} `json:"diagnostic"`
			} `json:"detail"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			if workerStage := sanitizeBrowserWorkerStage(envelope.Detail.Stage); workerStage != "" {
				failure.UpstreamStage = workerStage
				if workerStage == "turnstile" {
					failure.Stage = workerStage
				}
			}
			if workerReason := sanitizeBrowserWorkerReason(envelope.Detail.Error); workerReason != "" {
				failure.Operation = "worker_error=" + workerReason
				if diagnostic := sanitizeBrowserWorkerDiagnostic(envelope.Detail.Diagnostic.Reason); diagnostic != "" && diagnostic != workerReason {
					failure.Operation += ";diagnostic=" + diagnostic
				}
				if code := sanitizeBrowserWorkerUpstreamCode(envelope.Detail.Diagnostic.UpstreamCode); code != "" {
					failure.Operation += ";upstream_code=" + code
				}
				if status := envelope.Detail.Diagnostic.UpstreamStatus; status >= 400 && status <= 599 {
					failure.Operation += fmt.Sprintf(";upstream_status=%d", status)
					failure.UpstreamStatus = status
				}
				if network := safeBrowserNetworkFailures(envelope.Detail.Diagnostic.NetworkFailures); network != "" {
					failure.Operation += ";network_failures=" + network
				}
				if workerReason == "turnstile_incomplete" {
					diagnostic := envelope.Detail.Diagnostic
					if diagnostic.TurnstileClicks >= 0 && diagnostic.TurnstileClicks <= 10 {
						failure.Operation += fmt.Sprintf(";turnstile_clicks=%d", diagnostic.TurnstileClicks)
					}
					if diagnostic.TurnstileFrames >= 0 && diagnostic.TurnstileFrames <= 20 {
						failure.Operation += fmt.Sprintf(";turnstile_frames=%d", diagnostic.TurnstileFrames)
					}
					if diagnostic.TurnstileVisibleFrames >= 0 && diagnostic.TurnstileVisibleFrames <= 20 {
						failure.Operation += fmt.Sprintf(";turnstile_visible_frames=%d", diagnostic.TurnstileVisibleFrames)
					}
					if diagnostic.TurnstileWidgets >= 0 && diagnostic.TurnstileWidgets <= 20 {
						failure.Operation += fmt.Sprintf(";turnstile_widgets=%d", diagnostic.TurnstileWidgets)
					}
					failure.Operation += fmt.Sprintf(";turnstile_ready=%t;turnstile_callback=%t;turnstile_token=%t",
						diagnostic.TurnstileReady, diagnostic.TurnstileCallbackSeen, diagnostic.TurnstileTokenPresent)
				}
			}
		}
		return nil, failure
	}
	return body, nil
}

type browserNetworkFailure struct {
	Host   string `json:"host"`
	Kind   string `json:"kind"`
	Status int    `json:"status"`
}

func safeBrowserNetworkFailures(events []browserNetworkFailure) string {
	var out []string
	for _, event := range events {
		switch event.Host {
		case "accounts.x.ai", "auth.x.ai", "challenges.cloudflare.com", "brunhild.challenges.cloudflare.com":
		default:
			continue
		}
		switch event.Kind {
		case "proxy_tunnel_failed", "proxy_connection_failed", "proxy_auth_failed", "network_timeout", "dns_failed", "connection_reset", "connection_closed", "tls_failed", "network_failed", "http_error":
		default:
			continue
		}
		if event.Status != 0 && (event.Status < 400 || event.Status > 599) {
			continue
		}
		out = append(out, fmt.Sprintf("%s|%s|%d", event.Host, event.Kind, event.Status))
		if len(out) == 8 {
			break
		}
	}
	return strings.Join(out, ",")
}

func sanitizeBrowserWorkerStage(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "browser_start", "browser_session", "send_code", "validate_code", "turnstile", "signup", "sso":
		return value
	default:
		return ""
	}
}

func sanitizeBrowserWorkerReason(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "signup_rate_limited", "email_already_registered", "email_invalid", "signup_unavailable", "verification_page_missing", "cloudflare_challenge", "proxy_connection_failed", "email_input_missing", "email_submit_button_missing", "verification_code_input_missing", "verification_submit_button_missing", "verification_code_invalid", "profile_form_missing", "turnstile_incomplete", "account_page_missing", "signup_profile_field_missing", "signup_submit_button_missing":
		return value
	default:
		return "unclassified"
	}
}

func sanitizeBrowserWorkerDiagnostic(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "cloudflare_challenge", "proxy_connection_failed", "signup_rate_limited", "email_already_registered", "signup_unavailable", "email_invalid":
		return value
	default:
		return ""
	}
}

func sanitizeBrowserWorkerUpstreamCode(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "email_signup_unavailable", "email_already_exists", "email_invalid", "rate_limited":
		return value
	default:
		return ""
	}
}

func (s *HTTPGrokBrowserSolver) StartSignup(ctx context.Context, pageURL, proxy string) (GrokBrowserContext, error) {
	payload := map[string]any{"url": pageURL, "proxy": proxy}
	if s.Profile != nil {
		payload["timezone"] = strings.TrimSpace(s.Profile.TimezoneIANA)
		payload["locale"] = strings.TrimSpace(s.Profile.NavigatorLanguage)
		if profile := s.browserProfilePayload(); profile != nil {
			payload["browser_profile"] = profile
		}
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
		if profile := s.browserProfilePayload(); profile != nil {
			payload["browser_profile"] = profile
		}
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
