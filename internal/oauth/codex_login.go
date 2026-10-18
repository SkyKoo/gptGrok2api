package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	CodexOAuthClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexOAuthAuthURL     = "https://auth.openai.com/oauth/authorize"
	CodexOAuthTokenURL    = "https://auth.openai.com/oauth/token"
	CodexOAuthRedirectURI = "http://localhost:1455/auth/callback"
	CodexOAuthScope       = "openid email profile offline_access"
)

var ErrCodexOAuth = errors.New("codex oauth login failed")

// CodexLogin implements the standard Codex OAuth PKCE flow. Authentication
// challenges (including phone verification and optional 2FA) are completed by
// the user in the normal authorization page; this service only creates and
// validates the OAuth session and exchanges the callback code.
type CodexLogin struct {
	AuthURL     string
	TokenURL    string
	ClientID    string
	RedirectURI string
	Scope       string
	HTTP        *http.Client
	TTL         time.Duration
	mu          sync.Mutex
	sessions    map[string]codexLoginSession
}

type codexLoginSession struct {
	Verifier  string
	State     string
	CreatedAt time.Time
}

func NewCodexLogin(authURL, tokenURL, clientID, redirectURI, scope string, client *http.Client) *CodexLogin {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	if strings.TrimSpace(authURL) == "" {
		authURL = CodexOAuthAuthURL
	}
	if strings.TrimSpace(tokenURL) == "" {
		tokenURL = CodexOAuthTokenURL
	}
	if strings.TrimSpace(clientID) == "" {
		clientID = CodexOAuthClientID
	}
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = CodexOAuthRedirectURI
	}
	if strings.TrimSpace(scope) == "" {
		scope = CodexOAuthScope
	}
	return &CodexLogin{
		AuthURL:  strings.TrimRight(strings.TrimSpace(authURL), "/"),
		TokenURL: strings.TrimSpace(tokenURL), ClientID: strings.TrimSpace(clientID),
		RedirectURI: strings.TrimSpace(redirectURI), Scope: strings.TrimSpace(scope),
		HTTP: client, TTL: 10 * time.Minute, sessions: map[string]codexLoginSession{},
	}
}

func (s *CodexLogin) Start(emailHint string) (map[string]string, error) {
	if s.AuthURL == "" || s.TokenURL == "" || s.ClientID == "" || s.RedirectURI == "" {
		return nil, fmt.Errorf("%w: oauth endpoints are not configured", ErrCodexOAuth)
	}
	verifier, challenge, err := codexPKCE()
	if err != nil {
		return nil, fmt.Errorf("%w: generate PKCE: %v", ErrCodexOAuth, err)
	}
	sessionID, err := codexRandomURL(24)
	if err != nil {
		return nil, fmt.Errorf("%w: generate session: %v", ErrCodexOAuth, err)
	}
	state, err := codexRandomURL(32)
	if err != nil {
		return nil, fmt.Errorf("%w: generate state: %v", ErrCodexOAuth, err)
	}
	params := url.Values{
		"client_id": {s.ClientID}, "response_type": {"code"},
		"redirect_uri": {s.RedirectURI}, "scope": {s.Scope}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"prompt": {"login"}, "id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow": {"true"},
	}
	if hint := strings.TrimSpace(emailHint); hint != "" {
		params.Set("login_hint", hint)
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.purgeLocked(now)
	s.sessions[sessionID] = codexLoginSession{Verifier: verifier, State: state, CreatedAt: now}
	s.mu.Unlock()
	return map[string]string{
		"session_id": sessionID, "authorize_url": s.AuthURL + "?" + params.Encode(),
		"state": state, "expires_in": strconv.Itoa(int(s.TTL / time.Second)),
		"redirect_uri_prefix": s.RedirectURI,
	}, nil
}

func (s *CodexLogin) Finish(ctx context.Context, sessionID, callback string) (map[string]string, error) {
	code, state, err := codexExtractCallback(callback)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.purgeLocked(time.Now().UTC())
	session, ok := s.sessions[strings.TrimSpace(sessionID)]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: oauth session expired or not found", ErrCodexOAuth)
	}
	if state != "" && state != session.State {
		return nil, fmt.Errorf("%w: state mismatch", ErrCodexOAuth)
	}
	tokens, err := s.exchange(ctx, code, session.Verifier)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.sessions, strings.TrimSpace(sessionID))
	s.mu.Unlock()
	return tokens, nil
}

func (s *CodexLogin) exchange(ctx context.Context, code, verifier string) (map[string]string, error) {
	form := url.Values{"client_id": {s.ClientID}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.RedirectURI}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: create token request: %v", ErrCodexOAuth, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://auth.openai.com")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: token request: %v", ErrCodexOAuth, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	access := codexString(payload, "access_token")
	refresh := codexString(payload, "refresh_token")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || access == "" || refresh == "" {
		detail := codexString(payload, "error_description", "error", "message")
		if detail == "" {
			detail = strings.TrimSpace(string(raw))
		}
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return nil, fmt.Errorf("%w: token endpoint HTTP %d: %s", ErrCodexOAuth, resp.StatusCode, detail)
	}
	return map[string]string{
		"access_token": access, "refresh_token": refresh,
		"id_token":   codexString(payload, "id_token"),
		"token_type": codexString(payload, "token_type"),
		"expires_in": codexString(payload, "expires_in"),
	}, nil
}

func (s *CodexLogin) purgeLocked(now time.Time) {
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	for id, item := range s.sessions {
		if now.Sub(item.CreatedAt) > ttl {
			delete(s.sessions, id)
		}
	}
	for len(s.sessions) > 64 {
		var oldestID string
		var oldest time.Time
		for id, item := range s.sessions {
			if oldestID == "" || item.CreatedAt.Before(oldest) {
				oldestID, oldest = id, item.CreatedAt
			}
		}
		if oldestID == "" {
			break
		}
		delete(s.sessions, oldestID)
	}
}

func codexPKCE() (string, string, error) {
	verifier, err := codexRandomURL(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func codexRandomURL(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func codexExtractCallback(value string) (string, string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", "", fmt.Errorf("%w: callback or code is required", ErrCodexOAuth)
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("%w: invalid callback URL", ErrCodexOAuth)
		}
		query := parsed.Query()
		if detail := query.Get("error_description"); detail != "" {
			return "", "", fmt.Errorf("%w: %s", ErrCodexOAuth, detail)
		}
		if detail := query.Get("error"); detail != "" {
			return "", "", fmt.Errorf("%w: %s", ErrCodexOAuth, detail)
		}
		code := strings.TrimSpace(query.Get("code"))
		if code == "" {
			return "", "", fmt.Errorf("%w: callback URL has no code", ErrCodexOAuth)
		}
		return code, strings.TrimSpace(query.Get("state")), nil
	}
	return raw, "", nil
}

func codexString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		if value, ok := values[key].(float64); ok {
			return strconv.FormatInt(int64(value), 10)
		}
	}
	return ""
}
