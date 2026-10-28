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
	"strconv"
	"strings"
	"time"
)

type HMEConfig struct{ SourceID, BaseURL, Password, AccountID string }

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
	ID              string `json:"id,omitempty"`
	Email           string `json:"email"`
	AccountID       string `json:"account_id"`
	SourceID        string `json:"source_id,omitempty"`
	SourceType      string `json:"source_type,omitempty"`
	SourceMailboxID string `json:"source_mailbox_id,omitempty"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// hmeAlias is the alias shape returned by the deployed icloud-hme service.
// Its stable deletion identifier is anonymousId; older HME variants may use
// id, so the client accepts that field only as a compatibility fallback.
type hmeAlias struct {
	AnonymousID string `json:"anonymousId"`
	ID          string `json:"id"`
	Email       string `json:"email"`
}

// ListMailboxes returns the aliases currently known by the deployed HME
// account. It is used by the mailbox reconciliation job and never exposes the
// HME session or administrator credentials to callers.
func (h *HME) ListMailboxes(ctx context.Context) ([]Mailbox, error) {
	if h.csrf == "" {
		if err := h.login(ctx); err != nil {
			return nil, err
		}
	}
	var response struct {
		Aliases []hmeAlias `json:"aliases"`
	}
	query := url.Values{"account_id": {h.cfg.AccountID}}
	if err := h.request(ctx, http.MethodGet, "/api/aliases?"+query.Encode(), nil, &response); err != nil {
		return nil, err
	}
	result := make([]Mailbox, 0, len(response.Aliases))
	for _, alias := range response.Aliases {
		email := strings.TrimSpace(alias.Email)
		if email == "" {
			continue
		}
		id := strings.TrimSpace(alias.AnonymousID)
		if id == "" {
			id = strings.TrimSpace(alias.ID)
		}
		result = append(result, Mailbox{ID: id, Email: email, AccountID: h.cfg.AccountID, SourceID: h.cfg.SourceID, SourceType: "hme", SourceMailboxID: id})
	}
	return result, nil
}

type MailSource interface {
	Acquire(context.Context, string) (Mailbox, error)
	WaitCode(context.Context, Mailbox, time.Time) (string, error)
	Close()
}

// MailSessionPreparer re-authenticates an existing mailbox without creating a
// new alias. It is used by compensation registration after the first attempt
// failed after HME had already returned an address.
type MailSessionPreparer interface {
	Prepare(context.Context, Mailbox) error
}

// MailboxClaimCoordinator is the boundary between registration engines and
// the provider-neutral mailbox asset store. Implementations may reuse an
// existing mailbox, create a new provider mailbox, and persist the claim
// lifecycle without exposing provider details to GPT or Grok flows.
type MailboxClaimCoordinator interface {
	Reserve(context.Context, string, string, string, func(context.Context) (Mailbox, error), func(context.Context, Mailbox) error) (Mailbox, error)
	EnsureClaim(context.Context, string, string, Mailbox) error
	Commit(context.Context, string, string, string, Mailbox) error
	Release(context.Context, string, string, Mailbox, string) error
}

// CoordinatedMailSource keeps the existing HME client as the provider while
// routing mailbox allocation and claims through the central coordinator.
// Each registration worker receives its own wrapper, so the job ID is local
// and does not cross concurrent GPT/Grok registrations.
type CoordinatedMailSource struct {
	source      MailSource
	coordinator MailboxClaimCoordinator
	platform    string
	sourceID    string
	jobID       string
}

func NewCoordinatedMailSource(platform, sourceID string, source MailSource, coordinator MailboxClaimCoordinator) *CoordinatedMailSource {
	return &CoordinatedMailSource{source: source, coordinator: coordinator, platform: strings.ToLower(strings.TrimSpace(platform)), sourceID: strings.TrimSpace(sourceID)}
}

func (c *CoordinatedMailSource) Acquire(ctx context.Context, jobID string) (Mailbox, error) {
	c.jobID = strings.TrimSpace(jobID)
	if c.coordinator == nil {
		return c.source.Acquire(ctx, jobID)
	}
	prepare := func(ctx context.Context, box Mailbox) error {
		preparer, ok := c.source.(MailSessionPreparer)
		if !ok {
			return nil
		}
		return preparer.Prepare(ctx, box)
	}
	return c.coordinator.Reserve(ctx, c.platform, c.jobID, c.sourceID, func(ctx context.Context) (Mailbox, error) {
		return c.source.Acquire(ctx, c.jobID)
	}, prepare)
}

func (c *CoordinatedMailSource) Prepare(ctx context.Context, box Mailbox) error {
	return c.PrepareFor(ctx, c.jobID, box)
}

func (c *CoordinatedMailSource) PrepareFor(ctx context.Context, jobID string, box Mailbox) error {
	if strings.TrimSpace(jobID) != "" {
		c.jobID = strings.TrimSpace(jobID)
	}
	if c.coordinator != nil {
		if err := c.coordinator.EnsureClaim(ctx, c.platform, c.jobID, box); err != nil {
			return err
		}
	}
	preparer, ok := c.source.(MailSessionPreparer)
	if !ok {
		return fail("mail", "resume_not_supported")
	}
	return preparer.Prepare(ctx, box)
}

func (c *CoordinatedMailSource) Commit(ctx context.Context, accountID string, box Mailbox) error {
	if c.coordinator == nil {
		return nil
	}
	return c.coordinator.Commit(ctx, c.platform, c.jobID, accountID, box)
}

func (c *CoordinatedMailSource) Release(ctx context.Context, box Mailbox, reason string) error {
	if c.coordinator == nil {
		return nil
	}
	return c.coordinator.Release(ctx, c.platform, c.jobID, box, reason)
}

func (c *CoordinatedMailSource) WaitCode(ctx context.Context, box Mailbox, after time.Time) (string, error) {
	return c.source.WaitCode(ctx, box, after)
}

func (c *CoordinatedMailSource) WaitCodeFor(ctx context.Context, box Mailbox, after time.Time, matcher func(string) bool) (string, error) {
	if targetAware, ok := c.source.(interface {
		WaitCodeFor(context.Context, Mailbox, time.Time, func(string) bool) (string, error)
	}); ok {
		return targetAware.WaitCodeFor(ctx, box, after, matcher)
	}
	return c.source.WaitCode(ctx, box, after)
}

func (c *CoordinatedMailSource) Close() { c.source.Close() }

// HME talks to the deployed icloud-hme API, not the legacy Privacy Mail API.
// It creates a labelled alias once and leaves it in place on failure so that a
// partially created account can be recovered without deleting its email address.
type HME struct {
	cfg      HMEConfig
	http     *http.Client
	csrf     string
	interval time.Duration
	consumed map[string]bool
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

func (h *HME) login(ctx context.Context) error {
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := h.request(ctx, http.MethodPost, "/api/auth/login", map[string]string{"password": h.cfg.Password}, &session); err != nil {
		return err
	}
	if session.CSRF == "" {
		return fail("mail", "missing_csrf")
	}
	h.csrf = session.CSRF
	return nil
}

// Prepare resumes an already-created alias. It deliberately does not call
// /api/create, so a compensation attempt cannot consume another HME address.
func (h *HME) Prepare(ctx context.Context, box Mailbox) error {
	if box.AccountID == "" || box.AccountID != h.cfg.AccountID {
		return fail("mail", "mailbox_account_mismatch")
	}
	address, err := mail.ParseAddress(box.Email)
	if err != nil || address.Address != box.Email {
		return fail("mail", "invalid_saved_mailbox")
	}
	return h.login(ctx)
}

func (h *HME) request(ctx context.Context, method, path string, body any, out any) error {
	operation := hmeOperation(path)
	failure := func(code string, status int) *Failure {
		return &Failure{Stage: "mail", Code: code, HTTPStatus: status, Operation: operation}
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return failure("invalid_request", 0)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.cfg.BaseURL+path, reader)
	if err != nil {
		return failure("invalid_request", 0)
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
		return failure("network_error", 0)
	}
	defer resp.Body.Close()
	var envelope struct {
		Success        bool            `json:"success"`
		Code           string          `json:"code"`
		Data           json.RawMessage `json:"data"`
		Stage          string          `json:"stage"`
		UpstreamStatus int             `json:"upstream_status"`
		RetryAfter     string          `json:"retry_after"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope)
	if err != nil {
		result := failure("invalid_response", resp.StatusCode)
		result.RetryAfter = hmeRetryAfter(resp.Header.Get("Retry-After"))
		return result
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !envelope.Success {
		code := "upstream_error"
		switch envelope.Code {
		case "AUTH_REQUIRED", "INVALID_CREDENTIALS", "RATE_LIMITED", "CSRF_INVALID", "ACCOUNT_NOT_FOUND", "UPSTREAM_UNAUTHORIZED", "ICLOUD_LOGIN_EXPIRED",
			"UPSTREAM_RATE_LIMITED", "UPSTREAM_UNAVAILABLE", "UPSTREAM_NETWORK_ERROR", "UPSTREAM_TIMEOUT", "UPSTREAM_INVALID_RESPONSE", "UPSTREAM_REJECTED", "UPSTREAM_FAILURE", "ALIAS_CREATE_FAILED",
			"ICLOUD_LOGIN_RATE_LIMITED", "ICLOUD_LOGIN_REJECTED", "ICLOUD_LOGIN_FAILED", "ICLOUD_LOGIN_PROTOCOL_ERROR", "ICLOUD_LOGIN_ACTION_REQUIRED", "OTP_REQUIRED", "OTP_INVALID":
			code = strings.ToLower(envelope.Code)
		}
		result := failure(code, resp.StatusCode)
		result.RetryAfter = hmeRetryAfter(resp.Header.Get("Retry-After"))
		if result.RetryAfter == "" {
			result.RetryAfter = hmeRetryAfter(envelope.RetryAfter)
		}
		result.UpstreamStage = hmeUpstreamStage(envelope.Stage)
		if envelope.UpstreamStatus >= 100 && envelope.UpstreamStatus <= 599 {
			result.UpstreamStatus = envelope.UpstreamStatus
		}
		return result
	}
	if out != nil && json.Unmarshal(envelope.Data, out) != nil {
		return failure("invalid_response", resp.StatusCode)
	}
	return nil
}
func (h *HME) Acquire(ctx context.Context, jobID string) (Mailbox, error) {
	if err := h.login(ctx); err != nil {
		return Mailbox{}, err
	}
	var box Mailbox
	if err := h.request(ctx, http.MethodPost, "/api/create", map[string]string{"account_id": h.cfg.AccountID, "label": "CFM Free " + jobID}, &box); err != nil {
		return box, err
	}
	address, err := mail.ParseAddress(box.Email)
	if err != nil || address.Address != box.Email || box.AccountID != h.cfg.AccountID {
		return Mailbox{}, fail("mail", "invalid_alias_response")
	}
	box.SourceID = h.cfg.SourceID
	return box, nil
}

// DeleteMailbox removes the HME alias that belongs to the supplied CFM
// registration mailbox. The HME API scopes alias listing by account ID and
// requires the alias anonymousId plus account_id on DELETE. A missing alias is
// reported with a stable error code so callers can keep the CFM deletion
// successful while showing an actionable cleanup result.
func (h *HME) DeleteMailbox(ctx context.Context, box Mailbox) error {
	if box.AccountID == "" || box.AccountID != h.cfg.AccountID {
		return fail("mail", "mailbox_account_mismatch")
	}
	address, err := mail.ParseAddress(box.Email)
	if err != nil || address.Address != box.Email {
		return fail("mail", "invalid_saved_mailbox")
	}
	if h.csrf == "" {
		if err := h.login(ctx); err != nil {
			return err
		}
	}
	var response struct {
		Aliases []hmeAlias `json:"aliases"`
	}
	query := url.Values{"account_id": {box.AccountID}}
	if err := h.request(ctx, http.MethodGet, "/api/aliases?"+query.Encode(), nil, &response); err != nil {
		return err
	}
	for _, candidate := range response.Aliases {
		aliasID := strings.TrimSpace(candidate.AnonymousID)
		if aliasID == "" {
			aliasID = strings.TrimSpace(candidate.ID)
		}
		if aliasID == "" || !strings.EqualFold(candidate.Email, box.Email) {
			continue
		}
		return h.request(ctx, http.MethodDelete, "/api/aliases/"+url.PathEscape(aliasID), map[string]string{"account_id": box.AccountID}, nil)
	}
	return fail("mail", "mailbox_not_found")
}

type hmeMessage struct{ ID, Folder, From, To, Subject, Date, Preview, Body string }

// Grok formats its six-digit verification code as XXX-XXX while OpenAI
// commonly uses a contiguous value. Normalize both forms before the caller
// submits the code.
var codePattern = regexp.MustCompile(`(?:^|[^0-9])([0-9]{3})[-\s]?([0-9]{3})(?:[^0-9]|$)`)

func mailCode(text string) string {
	matches := codePattern.FindAllStringSubmatch(text, -1)
	code := ""
	for _, match := range matches {
		candidate := match[1] + match[2]
		if code != "" && code != candidate {
			return ""
		}
		code = candidate
	}
	return code
}
func suitableMessage(m hmeMessage, box Mailbox, after time.Time, senderMatcher func(string) bool) bool {
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
	return senderMatcher(sender.Address)
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

// isGrokSender is kept separate from the OpenAI allow-list because the two
// registration targets use different verification mail senders.
func isGrokSender(raw string) bool {
	parts := strings.SplitN(strings.ToLower(raw), "@", 2)
	if len(parts) != 2 {
		return false
	}
	local, domain := parts[0], parts[1]
	if domain == "x.ai" || strings.HasSuffix(domain, ".x.ai") || domain == "grok.com" || strings.HasSuffix(domain, ".grok.com") {
		return true
	}
	return domain == "icloud.com" && strings.HasPrefix(local, "noreply_at_") &&
		(strings.Contains(local, "_x_ai") || strings.Contains(local, "_grok_com"))
}

// WaitCodeFor is the target-aware variant used by parallel registration
// engines. WaitCode below preserves the original OpenAI-only behavior.
func (h *HME) WaitCodeFor(ctx context.Context, box Mailbox, after time.Time, senderMatcher func(string) bool) (string, error) {
	if senderMatcher == nil {
		senderMatcher = isOpenAISender
	}
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
			key := box.Email + "|" + m.Folder + "|" + m.ID
			if m.ID == "" {
				key += "|" + m.Date + "|" + m.From + "|" + m.Subject
			}
			if h.consumed[key] {
				continue
			}
			consume := func(code string) string {
				if h.consumed == nil {
					h.consumed = map[string]bool{}
				}
				h.consumed[key] = true
				return code
			}
			if !suitableMessage(m, box, after, senderMatcher) {
				continue
			}
			if code := mailCode(m.Subject); code != "" {
				return consume(code), nil
			}
			if code := mailCode(m.Preview); code != "" {
				return consume(code), nil
			}
			if m.ID == "" || (m.Folder != "inbox" && m.Folder != "junk") || (inbox.Method != "imap" && inbox.Method != "web_api") {
				continue
			}
			q := url.Values{"account_id": {box.AccountID}, "folder": {m.Folder}, "method": {inbox.Method}}
			var full hmeMessage
			if err := h.request(ctx, http.MethodGet, "/api/inbox/"+url.PathEscape(m.ID)+"?"+q.Encode(), nil, &full); err != nil {
				return "", err
			}
			if suitableMessage(full, box, after, senderMatcher) {
				if code := mailCode(full.Body); code != "" {
					return consume(code), nil
				}
			}
		}
		if err := pause(ctx, h.interval); err != nil {
			return "", err
		}
	}
}

func (h *HME) WaitCode(ctx context.Context, box Mailbox, after time.Time) (string, error) {
	return h.WaitCodeFor(ctx, box, after, isOpenAISender)
}

func hmeOperation(path string) string {
	switch {
	case path == "/api/auth/login":
		return "hme_login"
	case path == "/api/create":
		return "alias_create"
	case strings.HasPrefix(path, "/api/inbox?"):
		return "inbox_list"
	case strings.HasPrefix(path, "/api/inbox/"):
		return "message_read"
	default:
		return "hme_request"
	}
}

func hmeUpstreamStage(stage string) string {
	switch stage {
	case "session_validate", "alias_generate", "alias_reserve", "alias_list", "alias_deactivate", "alias_reactivate", "alias_delete",
		"start", "federate", "password_challenge", "password_verify", "otp_verify", "trust", "web_session", "session_save":
		return stage
	default:
		return ""
	}
}

func hmeRetryAfter(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 128 {
		return ""
	}
	if n, err := strconv.ParseUint(raw, 10, 31); err == nil {
		return strconv.FormatUint(n, 10)
	}
	if t, err := http.ParseTime(raw); err == nil {
		return t.UTC().Format(http.TimeFormat)
	}
	return ""
}
