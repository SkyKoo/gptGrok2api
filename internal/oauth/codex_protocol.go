package oauth

// This file contains the protocol controller used by the standalone Codex
// page.  It deliberately keeps phone numbers, SMS codes and TOTP values in
// the request stack only; none of them are persisted in a session or account
// credential.  The controller is intentionally conservative: it follows the
// normal auth endpoints and reports an upstream challenge when the server
// asks for a step that CFM does not understand.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserprofile"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

var (
	phonePattern = regexp.MustCompile(`^\+?[0-9][0-9 .()-]{6,20}$`)
	codePattern  = regexp.MustCompile(`^[0-9]{4,8}$`)
)

type CodexProtocolLogin struct {
	standard *CodexLogin
	mu       sync.Mutex
	sessions map[string]*codexProtocolSession
}

type codexProtocolSession struct {
	ID                string
	Email             string
	Password          string
	State             string
	Verifier          string
	AuthBase          string
	Client            tlsclient.HttpClient
	Profile           browserprofile.Profile
	DeviceID          string
	AuthSessionID     string
	FactorID          string
	WorkspaceSelected bool
	Stage             string
	CreatedAt         time.Time
}

// NewCodexProtocolLogin creates a protocol controller sharing the configured
// transport with the regular PKCE implementation.  Each login gets a private
// cookie jar so concurrent accounts never share auth state.
func NewCodexProtocolLogin(standard *CodexLogin) *CodexProtocolLogin {
	return &CodexProtocolLogin{standard: standard, sessions: map[string]*codexProtocolSession{}}
}

// Start creates a protocol session, opens the authorization redirect chain,
// submits the account email and returns the first upstream-required step.
func (s *CodexProtocolLogin) Start(ctx context.Context, emailHint, password string) (map[string]string, error) {
	if s == nil || s.standard == nil {
		return nil, fmt.Errorf("%w: protocol login is not configured", ErrCodexOAuth)
	}
	started, err := s.standard.Start(emailHint)
	if err != nil {
		return nil, err
	}
	sessionID := started["session_id"]
	state := started["state"]
	verifier := ""
	s.standard.mu.Lock()
	if item := s.standard.sessions[sessionID]; item != (codexLoginSession{}) {
		verifier = item.Verifier
	}
	s.standard.mu.Unlock()
	if verifier == "" {
		return nil, fmt.Errorf("%w: protocol PKCE session is unavailable", ErrCodexOAuth)
	}
	authURL, err := url.Parse(started["authorize_url"])
	if err != nil || authURL.Scheme == "" || authURL.Host == "" {
		return nil, fmt.Errorf("%w: invalid authorize URL", ErrCodexOAuth)
	}
	profile := browserprofile.New()
	if location := provider.ResolveOpenAITimezone(ctx, "https://chat.openai.com", "direct"); location != nil {
		profile.SetTimezone(location, time.Now())
	}
	deviceID, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("%w: generate device id: %v", ErrCodexOAuth, err)
	}
	authSessionID, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("%w: generate auth session id: %v", ErrCodexOAuth, err)
	}
	query := authURL.Query()
	query.Set("ext-oai-did", deviceID)
	query.Set("auth_session_logging_id", authSessionID)
	query.Set("screen_hint", "login_or_signup")
	authURL.RawQuery = query.Encode()
	client, err := newProtocolClient(profile)
	if err != nil {
		return nil, fmt.Errorf("%w: create auth client: %v", ErrCodexOAuth, err)
	}
	client.SetCookies(authURL, []*fhttp.Cookie{{Name: "oai-did", Value: deviceID, Path: "/", Secure: authURL.Scheme == "https"}})
	session := &codexProtocolSession{ID: sessionID, Email: strings.TrimSpace(emailHint), Password: strings.TrimSpace(password), State: state, Verifier: verifier, AuthBase: authURL.Scheme + "://" + authURL.Host, Client: client, Profile: profile, DeviceID: deviceID, AuthSessionID: authSessionID, Stage: "starting", CreatedAt: time.Now().UTC()}
	s.mu.Lock()
	s.purgeLocked(time.Now().UTC())
	s.sessions[sessionID] = session
	s.mu.Unlock()
	result, err := s.bootstrap(ctx, session, authURL.String())
	if err != nil {
		s.remove(sessionID)
		return nil, err
	}
	result["session_id"] = sessionID
	result["authorize_url"] = started["authorize_url"]
	result["state"] = state
	result["redirect_uri_prefix"] = s.standard.RedirectURI
	result["expires_in"] = started["expires_in"]
	return result, nil
}

func randomHex(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", buf), nil
}

func newProtocolClient(profile browserprofile.Profile) (tlsclient.HttpClient, error) {
	return tlsclient.NewHttpClient(tlsclient.NewNoopLogger(),
		tlsclient.WithClientProfile(profile.TLSClientProfile()),
		tlsclient.WithTimeoutSeconds(60),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
	)
}

func (s *CodexProtocolLogin) bootstrap(ctx context.Context, session *codexProtocolSession, authorizeURL string) (map[string]string, error) {
	// Warm the same Auth document used by the browser flow before requesting the
	// OAuth authorize URL. This establishes the auth cookies and request context;
	// it does not solve or bypass a challenge returned by the upstream.
	preflight, err := fhttp.NewRequestWithContext(ctx, http.MethodGet, session.AuthBase+"/log-in", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create auth preflight: %v", ErrCodexOAuth, err)
	}
	setProtocolHeaders(preflight, session.Profile, session.DeviceID, session.AuthSessionID, "")
	if _, err = session.Client.Do(preflight); err != nil {
		return nil, fmt.Errorf("%w: auth preflight: %v", ErrCodexOAuth, err)
	}
	req, err := fhttp.NewRequestWithContext(ctx, http.MethodGet, authorizeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create authorize request: %v", ErrCodexOAuth, err)
	}
	setProtocolHeaders(req, session.Profile, session.DeviceID, session.AuthSessionID, session.AuthBase+"/log-in")
	resp, err := session.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: authorize request: %v", ErrCodexOAuth, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, protocolHTTPError("authorize", resp)
	}
	result, err := s.postJSON(ctx, session, "/api/accounts/authorize/continue", map[string]any{"username": map[string]string{"kind": "email", "value": session.Email}}, "https://auth.openai.com/log-in")
	if err != nil {
		return nil, err
	}
	return s.advanceResult(ctx, session, result)
}

func (s *CodexProtocolLogin) SubmitPhone(ctx context.Context, id, phone string) (map[string]string, error) {
	session, err := s.session(id)
	if err != nil {
		return nil, err
	}
	phone = strings.TrimSpace(phone)
	if !phonePattern.MatchString(phone) {
		return nil, fmt.Errorf("%w: invalid phone number", ErrCodexOAuth)
	}
	phone = strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '(' || r == ')' {
			return -1
		}
		return r
	}, phone)
	if !strings.HasPrefix(phone, "+") {
		phone = "+" + phone
	}
	_, err = s.postJSON(ctx, session, "/api/accounts/add-phone/send", map[string]string{"phone_number": phone, "channel": "sms"}, session.AuthBase+"/add-phone")
	if err != nil {
		return nil, err
	}
	s.setStage(session, "waiting_sms_code")
	return map[string]string{"session_id": id, "stage": "waiting_sms_code", "message": "验证码已发送，请输入短信验证码"}, nil
}

func (s *CodexProtocolLogin) SubmitSMSCode(ctx context.Context, id, code string) (map[string]string, error) {
	session, err := s.session(id)
	if err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)
	if !codePattern.MatchString(code) {
		return nil, fmt.Errorf("%w: invalid SMS code", ErrCodexOAuth)
	}
	result, err := s.postJSON(ctx, session, "/api/accounts/phone-otp/validate", map[string]string{"code": code}, session.AuthBase+"/phone-verification")
	if err != nil {
		return nil, err
	}
	return s.advanceResult(ctx, session, result)
}

// SubmitEmailCode continues a login when OpenAI asks for the mailbox OTP.
// Email verification is a separate challenge from the optional phone/SMS
// challenge, so keep it as its own state and endpoint in the UI as well.
func (s *CodexProtocolLogin) SubmitEmailCode(ctx context.Context, id, code string) (map[string]string, error) {
	session, err := s.session(id)
	if err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)
	if !codePattern.MatchString(code) {
		return nil, fmt.Errorf("%w: invalid email verification code", ErrCodexOAuth)
	}
	result, err := s.postJSON(ctx, session, "/api/accounts/email-otp/validate", map[string]string{"code": code}, session.AuthBase+"/email-verification")
	if err != nil {
		return nil, err
	}
	return s.advanceResult(ctx, session, result)
}

func (s *CodexProtocolLogin) Submit2FA(ctx context.Context, id, code string) (map[string]string, error) {
	session, err := s.session(id)
	if err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)
	if !codePattern.MatchString(code) {
		return nil, fmt.Errorf("%w: invalid 2FA code", ErrCodexOAuth)
	}
	if session.FactorID == "" {
		return nil, fmt.Errorf("%w: MFA factor is missing", ErrCodexOAuth)
	}
	if _, err = s.postJSON(ctx, session, "/api/accounts/mfa/issue_challenge", map[string]any{"id": session.FactorID, "type": "totp", "force_fresh_challenge": false}, session.AuthBase+"/mfa-challenge"); err != nil {
		return nil, err
	}
	result, err := s.postJSON(ctx, session, "/api/accounts/mfa/verify", map[string]string{"id": session.FactorID, "type": "totp", "code": code}, session.AuthBase+"/mfa-challenge/"+url.PathEscape(session.FactorID))
	if err != nil {
		return nil, err
	}
	return s.advanceResult(ctx, session, result)
}

func (s *CodexProtocolLogin) FinishCallback(ctx context.Context, id, callback string) (map[string]string, error) {
	session, err := s.session(id)
	if err != nil {
		return nil, err
	}
	code, state, err := codexExtractCallback(callback)
	if err != nil {
		return nil, err
	}
	if state != "" && state != session.State {
		return nil, fmt.Errorf("%w: state mismatch", ErrCodexOAuth)
	}
	tokens, err := s.standard.exchange(ctx, code, session.Verifier)
	if err != nil {
		return nil, err
	}
	s.remove(id)
	return tokens, nil
}

func (s *CodexProtocolLogin) bootstrapResult(id string) (map[string]string, error) {
	session, err := s.session(id)
	if err != nil {
		return nil, err
	}
	return map[string]string{"session_id": id, "stage": session.Stage}, nil
}

func (s *CodexProtocolLogin) advanceResult(ctx context.Context, session *codexProtocolSession, result map[string]any) (map[string]string, error) {
	if callback := findCallback(result); callback != "" {
		code, state, err := codexExtractCallback(callback)
		if err != nil {
			return nil, err
		}
		if state != "" && state != session.State {
			return nil, fmt.Errorf("%w: state mismatch", ErrCodexOAuth)
		}
		tokens, err := s.standard.exchange(ctx, code, session.Verifier)
		if err != nil {
			return nil, err
		}
		s.remove(session.ID)
		return tokens, nil
	}
	text := strings.ToLower(jsonText(result))
	if strings.Contains(text, "mfa-challenge") || strings.Contains(text, "mfa_challenge") || strings.Contains(text, "totp") {
		session.FactorID = factorID(result)
		if session.FactorID == "" {
			return nil, fmt.Errorf("%w: MFA factor is missing", ErrCodexOAuth)
		}
		s.setStage(session, "waiting_2fa")
		return map[string]string{"session_id": session.ID, "stage": "waiting_2fa", "message": "上游要求 2FA，请输入一次性验证码"}, nil
	}
	page := pageType(result)
	if page == "password" || page == "password_challenge" || page == "password_verification" || page == "login_password" || strings.Contains(text, "password_verify") || strings.Contains(text, "password-verification") || strings.Contains(text, "login-password") || strings.Contains(text, "password_challenge") {
		if session.Password == "" {
			s.setStage(session, "waiting_password")
			return map[string]string{"session_id": session.ID, "stage": "waiting_password", "message": "上游要求登录密码；该账号没有保存注册密码"}, nil
		}
		verified, err := s.postJSON(ctx, session, "/api/accounts/password/verify", map[string]string{"password": session.Password}, session.AuthBase+"/log-in/password")
		if err != nil {
			return nil, err
		}
		return s.advanceResult(ctx, session, verified)
	}
	if strings.Contains(text, "add-phone") || strings.Contains(text, "add_phone") || strings.Contains(text, "phone_verification") || strings.Contains(text, "phone_otp") {
		s.setStage(session, "waiting_phone")
		return map[string]string{"session_id": session.ID, "stage": "waiting_phone", "message": "上游要求手机号，请输入手机号"}, nil
	}
	if strings.Contains(text, "email-verification") || strings.Contains(text, "email_verification") || strings.Contains(text, "email_otp") || strings.Contains(text, "email-otp") {
		s.setStage(session, "waiting_email_code")
		return map[string]string{"session_id": session.ID, "stage": "waiting_email_code", "message": "上游要求邮箱验证码，请输入邮箱验证码"}, nil
	}
	if !session.WorkspaceSelected {
		if workspaceID := findWorkspaceID(result); workspaceID != "" {
			session.WorkspaceSelected = true
			selected, err := s.postJSON(ctx, session, "/api/accounts/workspace/select", map[string]string{"workspace_id": workspaceID}, session.AuthBase+"/sign-in-with-chatgpt/codex/consent")
			if err != nil {
				return nil, err
			}
			if callback := findCallback(selected); callback != "" {
				return s.advanceResult(ctx, session, map[string]any{"callback_url": callback})
			}
			if next := nextURL(selected); next != "" {
				callback, followErr := s.follow(ctx, session, next)
				if followErr != nil {
					return nil, followErr
				}
				if callback != "" {
					return s.advanceResult(ctx, session, map[string]any{"callback_url": callback})
				}
			}
		}
	}
	if next := nextURL(result); next != "" {
		callback, err := s.follow(ctx, session, next)
		if err == nil && callback != "" {
			return s.advanceResult(ctx, session, map[string]any{"callback_url": callback})
		}
	}
	s.setStage(session, "waiting_authorization")
	return map[string]string{"session_id": session.ID, "stage": "waiting_authorization", "message": "授权会话已建立，等待上游继续"}, nil
}

func (s *CodexProtocolLogin) follow(ctx context.Context, session *codexProtocolSession, next string) (string, error) {
	if strings.HasPrefix(next, "/") {
		next = session.AuthBase + next
	}
	for i := 0; i < 15 && next != ""; i++ {
		if strings.Contains(next, s.standard.RedirectURI) {
			return next, nil
		}
		req, err := fhttp.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return "", err
		}
		setProtocolHeaders(req, session.Profile, session.DeviceID, session.AuthSessionID, session.AuthBase+"/")
		resp, err := session.Client.Do(req)
		if err != nil {
			return "", err
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			return "", nil
		}
		if strings.HasPrefix(loc, "/") {
			next = session.AuthBase + loc
		} else {
			next = loc
		}
	}
	return "", fmt.Errorf("%w: redirect limit", ErrCodexOAuth)
}

func (s *CodexProtocolLogin) postJSON(ctx context.Context, session *codexProtocolSession, path string, body any, referer string) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := fhttp.NewRequestWithContext(ctx, http.MethodPost, session.AuthBase+path, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	setProtocolHeaders(req, session.Profile, session.DeviceID, session.AuthSessionID, referer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := session.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: protocol request: %v", ErrCodexOAuth, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var payload map[string]any
	_ = json.Unmarshal(data, &payload)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, protocolBodyError(resp.StatusCode, payload, data)
	}
	return payload, nil
}

func setProtocolHeaders(req *fhttp.Request, profile browserprofile.Profile, deviceID, authSessionID, referer string) {
	// Keep this aligned with the registration browser profile. The headers are
	// identity metadata, not a challenge solver.
	for key, value := range profile.Headers() {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if deviceID != "" {
		req.Header.Set("Oai-Device-Id", deviceID)
	}
	if authSessionID != "" {
		req.Header.Set("X-Auth-Session-Logging-Id", authSessionID)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

func protocolHTTPError(name string, resp *fhttp.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return protocolBodyError(resp.StatusCode, nil, data)
}

func protocolBodyError(status int, payload map[string]any, raw []byte) error {
	detail := codexString(payload, "error_description", "error", "message", "detail")
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}
	if len(detail) > 300 {
		detail = detail[:300]
	}
	return fmt.Errorf("%w: protocol HTTP %d: %s", ErrCodexOAuth, status, detail)
}

func (s *CodexProtocolLogin) session(id string) (*codexProtocolSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(time.Now().UTC())
	item := s.sessions[strings.TrimSpace(id)]
	if item == nil {
		return nil, fmt.Errorf("%w: protocol session expired or not found", ErrCodexOAuth)
	}
	return item, nil
}

func (s *CodexProtocolLogin) remove(id string) { s.mu.Lock(); delete(s.sessions, id); s.mu.Unlock() }
func (s *CodexProtocolLogin) setStage(session *codexProtocolSession, stage string) {
	s.mu.Lock()
	session.Stage = stage
	s.mu.Unlock()
}
func (s *CodexProtocolLogin) purgeLocked(now time.Time) {
	for id, item := range s.sessions {
		if now.Sub(item.CreatedAt) > s.standard.TTL {
			delete(s.sessions, id)
		}
	}
}

func jsonText(value any) string { raw, _ := json.Marshal(value); return string(raw) }
func pageType(value map[string]any) string {
	if page, ok := value["page"].(map[string]any); ok {
		return strings.ToLower(stringValueAny(page["type"]))
	}
	return strings.ToLower(stringValueAny(value["page_type"]))
}
func nextURL(value map[string]any) string {
	for _, key := range []string{"continue_url", "redirect_url", "url", "next", "location"} {
		if v := stringValueAny(value[key]); v != "" {
			return v
		}
	}
	return ""
}
func stringValueAny(value any) string {
	if value, ok := value.(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}
func factorID(value map[string]any) string {
	var found string
	var walk func(any)
	walk = func(item any) {
		if found != "" {
			return
		}
		switch value := item.(type) {
		case map[string]any:
			if v := stringValueAny(value["factor_id"]); v != "" {
				found = v
				return
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		case string:
			if strings.Contains(value, "/mfa-challenge/") {
				trimmed := strings.TrimSuffix(value, "/")
				found = trimmed[strings.LastIndex(trimmed, "/")+1:]
			}
		}
	}
	walk(value)
	return found
}

func findWorkspaceID(value map[string]any) string {
	var found string
	var walk func(any)
	walk = func(item any) {
		if found != "" {
			return
		}
		switch value := item.(type) {
		case map[string]any:
			if id := stringValueAny(value["workspace_id"]); id != "" {
				found = id
				return
			}
			if id := stringValueAny(value["workspaceId"]); id != "" {
				found = id
				return
			}
			if id := stringValueAny(value["id"]); id != "" {
				if _, hasName := value["name"]; hasName {
					found = id
					return
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(value)
	return found
}
func findCallback(value any) string {
	var found string
	var walk func(any)
	walk = func(v any) {
		if found != "" {
			return
		}
		switch item := v.(type) {
		case string:
			if strings.Contains(item, "localhost:1455/auth/callback") {
				found = item
			}
		case map[string]any:
			for _, child := range item {
				walk(child)
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(value)
	return found
}
