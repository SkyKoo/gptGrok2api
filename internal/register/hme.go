package register

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type HMEConfig struct{ BaseURL, Password, AccountID string }

func (c HMEConfig) Validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fail("config", "invalid_hme_base_url")
	}
	if c.Password == "" || c.AccountID == "" {
		return fail("config", "hme_password_and_account_id_required")
	}
	return nil
}

type Mailbox struct {
	Email     string `json:"email"`
	AccountID string `json:"account_id"`
}
type MailSource interface {
	Acquire(context.Context, string) (Mailbox, error)
	WaitCode(context.Context, Mailbox, time.Time) (string, error)
	Close()
}

// HME talks to the deployed icloud-hme API, not the legacy Privacy Mail API.
// It creates a labelled alias once and leaves it in place on failure so that a
// partially created account can be recovered without deleting its email address.
type HME struct {
	cfg      HMEConfig
	http     *http.Client
	csrf     string
	interval time.Duration
}

func NewHME(c HMEConfig, interval time.Duration) (*HME, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(nil)
	var sessionJar http.CookieJar = jar
	// Only the operator-pinned Docker endpoint may carry its Secure session over
	// internal HTTP. Public HTTP endpoints keep normal browser cookie semantics.
	if c.BaseURL == strings.TrimRight(os.Getenv("GO_HME_INTERNAL_BASE_URL"), "/") {
		origin, _ := url.Parse(c.BaseURL)
		if origin.Scheme == "http" {
			sessionJar = internalHMEJar{CookieJar: jar, origin: origin}
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Mail traffic never inherits the registration proxy.
	return &HME{cfg: c, interval: interval, http: &http.Client{Jar: sessionJar, Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Preserve normal domain, path and expiry checks; map only the pinned internal
// origin into its own secure jar namespace. Cookies never cross origins.
type internalHMEJar struct {
	http.CookieJar
	origin *url.URL
}

func (j internalHMEJar) cookieURL(u *url.URL) *url.URL {
	copy := *u
	if u.Scheme == j.origin.Scheme && u.Host == j.origin.Host {
		copy.Scheme = "https"
	}
	return &copy
}

func (j internalHMEJar) Cookies(u *url.URL) []*http.Cookie {
	if u.Scheme != j.origin.Scheme || u.Host != j.origin.Host {
		return nil
	}
	return j.CookieJar.Cookies(j.cookieURL(u))
}

func (j internalHMEJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if u.Scheme == j.origin.Scheme && u.Host == j.origin.Host {
		j.CookieJar.SetCookies(j.cookieURL(u), cookies)
	}
}
func (h *HME) Close() { h.http.CloseIdleConnections() }
func (h *HME) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fail("mail", "invalid_request")
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.cfg.BaseURL+path, reader)
	if err != nil {
		return fail("mail", "invalid_request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if h.csrf != "" && method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", h.csrf)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail("mail", "network_error")
	}
	defer resp.Body.Close()
	var envelope struct {
		Success bool            `json:"success"`
		Code    string          `json:"code"`
		Data    json.RawMessage `json:"data"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope)
	if err != nil {
		return &Failure{Stage: "mail", Code: "invalid_response", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !envelope.Success {
		code := "upstream_error"
		switch envelope.Code {
		case "AUTH_REQUIRED", "INVALID_CREDENTIALS", "RATE_LIMITED", "CSRF_INVALID", "ACCOUNT_NOT_FOUND", "UPSTREAM_UNAUTHORIZED", "ICLOUD_LOGIN_EXPIRED":
			code = strings.ToLower(envelope.Code)
		}
		return &Failure{Stage: "mail", Code: code, HTTPStatus: resp.StatusCode}
	}
	if out != nil && json.Unmarshal(envelope.Data, out) != nil {
		return fail("mail", "invalid_response")
	}
	return nil
}
func (h *HME) Acquire(ctx context.Context, jobID string) (Mailbox, error) {
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := h.request(ctx, http.MethodPost, "/api/auth/login", map[string]string{"password": h.cfg.Password}, &session); err != nil {
		return Mailbox{}, err
	}
	if session.CSRF == "" {
		return Mailbox{}, fail("mail", "missing_csrf")
	}
	h.csrf = session.CSRF
	var box Mailbox
	if err := h.request(ctx, http.MethodPost, "/api/create", map[string]string{"account_id": h.cfg.AccountID, "label": "CFM Free " + jobID}, &box); err != nil {
		return box, err
	}
	address, err := mail.ParseAddress(box.Email)
	if err != nil || address.Address != box.Email || box.AccountID != h.cfg.AccountID {
		return Mailbox{}, fail("mail", "invalid_alias_response")
	}
	return box, nil
}

type hmeMessage struct{ ID, Folder, From, To, Subject, Date, Preview, Body string }

var codePattern = regexp.MustCompile(`(?:^|[^0-9])([0-9]{6})(?:[^0-9]|$)`)

func mailCode(text string) string {
	matches := codePattern.FindAllStringSubmatch(text, -1)
	code := ""
	for _, match := range matches {
		if code != "" && code != match[1] {
			return ""
		}
		code = match[1]
	}
	return code
}
func suitableMessage(m hmeMessage, box Mailbox, after time.Time) bool {
	at, err := time.Parse(time.RFC3339, m.Date)
	if err != nil || at.Before(after) || at.After(time.Now().Add(time.Minute)) {
		return false
	}
	recipients, err := mail.ParseAddressList(m.To)
	if err != nil {
		return false
	}
	matched := false
	for _, recipient := range recipients {
		if strings.EqualFold(recipient.Address, box.Email) {
			matched = true
		}
	}
	if !matched {
		return false
	}
	sender, err := mail.ParseAddress(m.From)
	if err != nil {
		return false
	}
	return isOpenAISender(sender.Address)
}

// HME rewrites some forwarded senders into an iCloud address such as
// noreply_at_tm_openai_com_<opaque>@icloud.com. Keep the direct OpenAI sender
// allow-list, and accept only this recognizable HME encoding so unrelated
// iCloud messages cannot satisfy an OTP wait.
func isOpenAISender(raw string) bool {
	parts := strings.SplitN(strings.ToLower(raw), "@", 2)
	if len(parts) != 2 {
		return false
	}
	local, domain := parts[0], parts[1]
	if domain == "openai.com" || strings.HasSuffix(domain, ".openai.com") || domain == "chatgpt.com" || strings.HasSuffix(domain, ".chatgpt.com") {
		return true
	}
	return domain == "icloud.com" && strings.HasPrefix(local, "noreply_at_") &&
		(strings.Contains(local, "_openai_com") || strings.Contains(local, "_chatgpt_com"))
}
func (h *HME) WaitCode(ctx context.Context, box Mailbox, after time.Time) (string, error) {
	query := url.Values{"account_id": {box.AccountID}, "alias": {box.Email}, "folder": {"all"}, "limit": {"30"}, "days": {"1"}}
	for {
		var inbox struct {
			Method   string       `json:"method"`
			Messages []hmeMessage `json:"messages"`
		}
		if err := h.request(ctx, http.MethodGet, "/api/inbox?"+query.Encode(), nil, &inbox); err != nil {
			return "", err
		}
		sort.SliceStable(inbox.Messages, func(i, j int) bool {
			a, _ := time.Parse(time.RFC3339, inbox.Messages[i].Date)
			b, _ := time.Parse(time.RFC3339, inbox.Messages[j].Date)
			return a.After(b)
		})
		for _, m := range inbox.Messages {
			if !suitableMessage(m, box, after) {
				continue
			}
			if code := mailCode(m.Subject); code != "" {
				return code, nil
			}
			if code := mailCode(m.Preview); code != "" {
				return code, nil
			}
			if m.ID == "" || (m.Folder != "inbox" && m.Folder != "junk") || (inbox.Method != "imap" && inbox.Method != "web_api") {
				continue
			}
			q := url.Values{"account_id": {box.AccountID}, "folder": {m.Folder}, "method": {inbox.Method}}
			var full hmeMessage
			if err := h.request(ctx, http.MethodGet, "/api/inbox/"+url.PathEscape(m.ID)+"?"+q.Encode(), nil, &full); err != nil {
				return "", err
			}
			if suitableMessage(full, box, after) {
				if code := mailCode(full.Body); code != "" {
					return code, nil
				}
			}
		}
		if err := pause(ctx, h.interval); err != nil {
			return "", err
		}
	}
}
