package register

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

const sentinelOrigin = "https://sentinel.openai.com"
const sentinelVersion = "20260810913b"

type Progress func(stage string) error
type RegistrationFlow interface {
	Register(context.Context, Mailbox, FreeConfig, func(context.Context, time.Time) (string, error), Progress) (map[string]any, error)
	Close()
}

// RegistrationProfileProvider exposes the protocol profile selected for a
// flow so the durable task journal can reuse it during compensation retry.
type RegistrationProfileProvider interface {
	BrowserProfile() BrowserProfile
}

// WebRegistrar ports the old ChatGPTWebRegistrar's HTTP flow. Endpoints are
// constructor arguments for isolated contract tests; the UI cannot redirect auth.
type WebRegistrar struct {
	chat, auth, sentinel string
	http                 tlsclient.HttpClient
	profile              BrowserProfile
	device, state        string
	authSessionID        string
}

func NewWebRegistrar(chat, auth, proxy string, requestedProfile ...BrowserProfile) (*WebRegistrar, error) {
	for _, base := range []string{chat, auth} {
		u, err := url.Parse(base)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fail("config", "invalid_auth_origin")
		}
	}
	profile := NewBrowserProfile()
	if len(requestedProfile) > 0 && requestedProfile[0].valid() {
		profile = requestedProfile[0]
		profile.NavigatorLanguages = append([]string(nil), profile.NavigatorLanguages...)
	}
	options := []tlsclient.HttpClientOption{tlsclient.WithClientProfile(profile.tlsClientProfile()), tlsclient.WithTimeoutSeconds(30), tlsclient.WithNotFollowRedirects(), tlsclient.WithCookieJar(tlsclient.NewCookieJar())}
	if proxy != "" && proxy != "direct" {
		options = append(options, tlsclient.WithProxyUrl(proxy))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, fail("config", "http_client_failed")
	}
	return &WebRegistrar{chat: strings.TrimRight(chat, "/"), auth: strings.TrimRight(auth, "/"), sentinel: sentinelOrigin, http: client, profile: profile}, nil
}
func (r *WebRegistrar) Close()                         { r.http.CloseIdleConnections() }
func (r *WebRegistrar) BrowserProfile() BrowserProfile { return r.profile }
func sameOrigin(raw, base string) bool {
	a, e := url.Parse(raw)
	b, f := url.Parse(base)
	return e == nil && f == nil && a.User == nil && a.Scheme == b.Scheme && a.Host == b.Host
}
func (r *WebRegistrar) trusted(raw string) bool {
	return sameOrigin(raw, r.chat) || sameOrigin(raw, r.auth) || sameOrigin(raw, r.sentinel)
}

type webReply struct {
	data    map[string]any
	headers fhttp.Header
	status  int
}

func (r *WebRegistrar) request(ctx context.Context, stage, method, rawURL, body, contentType string, headers map[string]string) (webReply, error) {
	var result webReply
	if !r.trusted(rawURL) {
		return result, fail(stage, "untrusted_redirect")
	}
	req, err := fhttp.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(body))
	if err != nil {
		return result, fail(stage, "invalid_request")
	}
	req.Header = fhttp.Header{"user-agent": {r.profile.UserAgent}, "accept": {"application/json"}, "accept-language": {r.profile.AcceptLanguage}, "sec-ch-ua": {r.profile.SecCHUA}, "sec-ch-ua-mobile": {r.profile.SecCHUAMobile}, "sec-ch-ua-platform": {r.profile.SecCHUAPlatform}}
	if strings.HasPrefix(req.URL.Path, "/api/auth/") && sameOrigin(rawURL, r.chat) {
		req.Header.Set("origin", r.chat)
		req.Header.Set("referer", r.chat+"/")
		req.Header.Set("x-openai-target-path", req.URL.Path)
		route := req.URL.Path
		if route == "/api/auth/signin/openai" {
			route = "/api/auth/signin/{provider}"
		}
		req.Header.Set("x-openai-target-route", route)
	} else if strings.HasPrefix(req.URL.Path, "/api/accounts/") {
		req.Header.Set("origin", r.auth)
	}
	if contentType != "" {
		req.Header.Set("content-type", contentType)
	}
	if r.device != "" {
		req.Header.Set("oai-device-id", r.device)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fail(stage, "network_error")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return result, fail(stage, "read_error")
	}
	result = webReply{data: map[string]any{}, headers: resp.Header, status: resp.StatusCode}
	_ = json.Unmarshal(raw, &result.data)
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		code := "upstream_rejected"
		if resp.StatusCode == 403 || strings.Contains(string(raw), "cf-chl-") {
			code = "verification_required"
		}
		if resp.StatusCode == 429 {
			code = "rate_limited"
		}
		return result, &Failure{Stage: stage, Code: code, HTTPStatus: resp.StatusCode}
	}
	if errorValue := result.data["error"]; errorValue != nil && errorValue != "" {
		return result, fail(stage, "upstream_rejected")
	}
	if success, ok := result.data["success"].(bool); ok && !success {
		return result, fail(stage, "upstream_rejected")
	}
	return result, nil
}
func (r *WebRegistrar) json(ctx context.Context, stage, path string, body any, headers map[string]string) (webReply, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return webReply{}, fail(stage, "invalid_request")
	}
	return r.request(ctx, stage, "POST", r.auth+path, string(raw), "application/json", headers)
}
func (r *WebRegistrar) navigate(ctx context.Context, stage, rawURL string) (string, error) {
	for hop := 0; hop < 10; hop++ {
		referer := r.auth + "/email-verification"
		if stage == "authorize" {
			referer = r.chat + "/"
		}
		if stage == "session" {
			referer = r.auth + "/about-you"
		}
		res, err := r.request(ctx, stage, "GET", rawURL, "", "", map[string]string{"accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", "referer": referer})
		if err != nil {
			return "", err
		}
		if res.status == 200 {
			return rawURL, nil
		}
		if res.status < 300 || res.status >= 400 {
			return "", fail(stage, "unexpected_status")
		}
		location := res.headers.Get("Location")
		if location == "" {
			return "", fail(stage, "missing_redirect")
		}
		base, _ := url.Parse(rawURL)
		next, err := base.Parse(location)
		if err != nil {
			return "", fail(stage, "invalid_redirect")
		}
		rawURL = next.String()
	}
	return "", fail(stage, "redirect_limit")
}
func (r *WebRegistrar) cookie(rawURL, name, value string) {
	u, _ := url.Parse(rawURL)
	r.http.SetCookies(u, []*fhttp.Cookie{{Name: name, Value: value, Path: "/", Secure: u.Scheme == "https", HttpOnly: true}})
}
func (r *WebRegistrar) begin(ctx context.Context, email string) (time.Time, error) {
	// Match Turb's protocol flow: establish the anonymous ChatGPT context, then
	// put login_hint on the NextAuth signin request. The authorize redirect is
	// what enters the email-verification page and triggers the first OTP; do not
	// submit authorize/continue or call email-otp/send a second time here.
	for _, path := range []string{"/api/auth/providers", "/api/auth/session"} {
		if _, err := r.request(ctx, "authorize", http.MethodGet, r.chat+path, "", "", nil); err != nil {
			return time.Time{}, err
		}
	}
	csrf, err := r.request(ctx, "authorize", http.MethodGet, r.chat+"/api/auth/csrf", "", "", nil)
	if err != nil {
		return time.Time{}, err
	}
	token := stringValue(csrf.data["csrfToken"])
	if csrf.status != 200 || token == "" {
		return time.Time{}, fail("authorize", "missing_csrf")
	}
	deviceBytes := make([]byte, 16)
	if _, err := rand.Read(deviceBytes); err != nil {
		return time.Time{}, fail("authorize", "random_failed")
	}
	loggingBytes := make([]byte, 16)
	if _, err := rand.Read(loggingBytes); err != nil {
		return time.Time{}, fail("authorize", "random_failed")
	}
	r.device = hex.EncodeToString(deviceBytes)
	r.authSessionID = hex.EncodeToString(loggingBytes)
	query := url.Values{
		"prompt":                  {"login"},
		"ext-oai-did":             {r.device},
		"auth_session_logging_id": {r.authSessionID},
		"screen_hint":             {"login_or_signup"},
		"login_hint":              {email},
	}
	body := url.Values{"csrfToken": {token}, "callbackUrl": {"/"}, "json": {"true"}}
	signin, err := r.request(ctx, "authorize", http.MethodPost, r.chat+"/api/auth/signin/openai?"+query.Encode(), body.Encode(), "application/x-www-form-urlencoded", nil)
	if err != nil {
		return time.Time{}, err
	}
	authorize := stringValue(signin.data["url"])
	u, err := url.Parse(authorize)
	if err != nil || !sameOrigin(authorize, r.auth) || u.Path != "/api/accounts/authorize" {
		return time.Time{}, fail("authorize", "invalid_authorize_url")
	}
	if returnedDevice := u.Query().Get("device_id"); returnedDevice != "" {
		r.device = returnedDevice
	}
	r.state = u.Query().Get("state")
	r.cookie(r.auth, "oai-did", r.device)
	r.cookie(r.sentinel, "oai-did", r.device)
	started := time.Now().UTC().Add(-10 * time.Second)
	landing, err := r.navigate(ctx, "authorize", authorize)
	if err != nil {
		return time.Time{}, err
	}
	if !sameOrigin(landing, r.auth) {
		return time.Time{}, fail("authorize", "invalid_landing")
	}
	return started, nil
}

func (r *WebRegistrar) resendCode(ctx context.Context) error {
	_, err := r.navigate(ctx, "send_code", r.auth+"/api/accounts/email-otp/send")
	return err
}

func profilePage(data map[string]any) bool {
	switch stringValue(object(data["page"])["type"]) {
	case "about_you", "about-you", "user_profile", "create_profile":
		return true
	default:
		return false
	}
}

// continueAfterOTP follows the upstream result. New accounts can require the
// Turb-compatible about-you step before OAuth can be completed.
func (r *WebRegistrar) continueAfterOTP(ctx context.Context, reply webReply, email string) error {
	next := continuationURL(reply.data)
	if next == "" {
		next = reply.headers.Get("Location")
	}
	if profilePage(reply.data) {
		return r.completeProfile(ctx, email, next)
	}
	return r.followContinuation(ctx, next, email, false)
}

// followContinuation validates every redirect before requesting it. When the
// upstream exposes the profile page, profileDone determines whether this is
// the first profile completion or a rejection after submission.
func (r *WebRegistrar) followContinuation(ctx context.Context, next, email string, profileDone bool) error {
	base, _ := url.Parse(r.auth + "/")
	for hop := 0; next != ""; hop++ {
		if hop >= 10 {
			return fail("session", "redirect_limit")
		}
		u, err := base.Parse(next)
		if err != nil || !r.trusted(u.String()) {
			return fail("session", "untrusted_continuation")
		}
		path := strings.TrimRight(u.Path, "/")
		if sameOrigin(u.String(), r.auth) && (path == "/about-you" || path == "/api/accounts/user/profile") {
			if profileDone {
				return fail("session", "profile_submission_rejected")
			}
			return r.completeProfile(ctx, email, u.String())
		}
		if u.Path == "/api/auth/callback/openai" {
			if !sameOrigin(u.String(), r.chat) || (r.state != "" && u.Query().Get("state") != r.state) {
				return fail("session", "callback_state_mismatch")
			}
		}
		res, err := r.request(ctx, "session", "GET", u.String(), "", "", map[string]string{"referer": r.auth + "/email-verification"})
		if err != nil {
			return err
		}
		base = u
		if res.status >= 300 && res.status < 400 {
			next = res.headers.Get("Location")
			if next == "" {
				return fail("session", "missing_redirect")
			}
			continue
		}
		if res.status != 200 {
			return fail("session", "unexpected_status")
		}
		if profilePage(res.data) {
			if profileDone {
				return fail("session", "profile_submission_rejected")
			}
			return r.completeProfile(ctx, email, continuationURL(res.data))
		}
		next = continuationURL(res.data)
		if next == "" && !sameOrigin(u.String(), r.chat) {
			return fail("session", "unexpected_post_verification_page")
		}
	}
	return nil
}

// completeProfile mirrors Turb's pure-protocol branch: load about-you, obtain
// the oauth_create_account Sentinel token, submit generated profile data, and
// then follow the returned OAuth continuation.
func (r *WebRegistrar) completeProfile(ctx context.Context, email, aboutURL string) error {
	target := aboutURL
	parsed, err := url.Parse(target)
	if target == "" || err != nil || !sameOrigin(target, r.auth) || strings.TrimRight(parsed.Path, "/") != "/about-you" {
		target = r.auth + "/about-you"
	}
	if _, err := r.navigate(ctx, "session", target); err != nil {
		return err
	}
	headers, err := r.challenge(ctx, "oauth_create_account")
	if err != nil {
		return err
	}
	name, birthday := profileForEmail(email)
	headers["referer"] = r.auth + "/about-you"
	created, err := r.json(ctx, "session", "/api/accounts/create_account", map[string]string{
		"name":      name,
		"birthdate": birthday,
	}, headers)
	if err != nil {
		return err
	}
	next := continuationURL(created.data)
	if next == "" {
		next = created.headers.Get("Location")
	}
	if next == "" {
		return fail("session", "profile_continuation_missing")
	}
	return r.followContinuation(ctx, next, email, true)
}

func (r *WebRegistrar) Register(ctx context.Context, box Mailbox, cfg FreeConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	if err := progress("authorize"); err != nil {
		return nil, err
	}
	after, err := r.begin(ctx, box.Email)
	if err != nil {
		return nil, err
	}
	if err := progress("wait_code"); err != nil {
		return nil, err
	}
	var code string
	var waitErr error
	for attempt := 0; attempt < 3; attempt++ {
		mailCtx, cancel := context.WithTimeout(ctx, cfg.MailTimeout)
		code, waitErr = wait(mailCtx, after)
		cancel()
		if waitErr == nil {
			break
		}
		if ctx.Err() != nil || attempt == 2 {
			return nil, waitErr
		}
		if err := progress("send_code"); err != nil {
			return nil, err
		}
		if err := r.resendCode(ctx); err != nil {
			return nil, err
		}
		after = time.Now().UTC().Add(-10 * time.Second)
		if err := progress("wait_code"); err != nil {
			return nil, err
		}
	}
	if len(code) != 6 || mailCode(code) != code {
		return nil, fail("wait_code", "invalid_code")
	}
	if err := progress("validate_code"); err != nil {
		return nil, err
	}
	res, err := r.json(ctx, "validate_code", "/api/accounts/email-otp/validate", map[string]string{"code": code}, map[string]string{"referer": r.auth + "/email-verification"})
	if err != nil {
		return nil, err
	}
	if res.status != 200 {
		return nil, fail("validate_code", "unexpected_status")
	}
	if err := progress("session"); err != nil {
		return nil, err
	}
	if err := r.continueAfterOTP(ctx, res, box.Email); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		res, err = r.request(ctx, "session", "GET", r.chat+"/api/auth/session", "", "", nil)
		if err != nil {
			return nil, err
		}
		token := stringValue(res.data["accessToken"])
		if res.status == 200 && token != "" {
			user := object(res.data["user"])
			if email := stringValue(user["email"]); !strings.EqualFold(email, box.Email) {
				return nil, fail("session", "email_mismatch")
			}
			fingerprint := r.profile.fingerprintMetadata()
			fingerprint["oai-device-id"] = r.device
			return map[string]any{"email": box.Email, "access_token": token, "source_type": "chatgpt_web", "enabled": false, "status": "待验证", "created_at": time.Now().UTC().Format(time.RFC3339), "user_id": stringValue(user["id"]), "expired": stringValue(res.data["expires"]), "fp": fingerprint}, nil
		}
		if attempt < 3 {
			if err = pause(ctx, time.Duration(attempt+1)*time.Second); err != nil {
				return nil, err
			}
		}
	}
	return nil, fail("session", "missing_access_token")
}

// challenge follows Turb's Sentinel protocol for the oauth_create_account
// profile step. The generated header is only used for the immediate request.
func (r *WebRegistrar) challenge(ctx context.Context, flow string) (map[string]string, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, fail("challenge", "random_failed")
	}
	sentinelSID := hex.EncodeToString(id)
	configuration := []any{
		3000,
		time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)"),
		r.profile.JSHeapSizeLimit,
		1,
		r.profile.UserAgent,
		r.sentinel + "/sentinel/" + sentinelVersion + "/sdk.js",
		nil,
		r.profile.NavigatorLanguage,
		r.profile.navigatorLanguagesValue(),
		1,
		"hardwareConcurrency",
		"location",
		"Object",
		1000,
		sentinelSID,
		"",
		r.profile.DeviceMemory,
		time.Now().UnixMilli() - 1000,
		0, 0, 0, 0, 0, 0, 0,
	}
	encode := func() string { raw, _ := json.Marshal(configuration); return base64.StdEncoding.EncodeToString(raw) }
	requirements := "gAAAAAC" + encode()
	frameURL := r.sentinel + "/backend-api/sentinel/frame.html?sv=" + sentinelVersion
	if _, err := r.request(ctx, "challenge", "GET", frameURL, "", "", map[string]string{
		"accept":         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"referer":        r.auth,
		"sec-fetch-site": "same-site",
		"sec-fetch-mode": "navigate",
		"sec-fetch-dest": "iframe",
	}); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"p": requirements, "id": r.device, "flow": flow})
	res, err := r.request(ctx, "challenge", "POST", r.sentinel+"/backend-api/sentinel/req", string(body), "text/plain;charset=UTF-8", map[string]string{
		"accept":         "*/*",
		"origin":         r.sentinel,
		"referer":        frameURL,
		"sec-fetch-site": "same-origin",
		"sec-fetch-mode": "cors",
		"sec-fetch-dest": "empty",
	})
	if err != nil {
		return nil, err
	}
	token := stringValue(res.data["token"])
	if res.status != 200 || token == "" {
		return nil, fail("challenge", "missing_token")
	}
	soRequired := boolValue(object(res.data["so"])["required"], false)
	pow := object(res.data["proofofwork"])
	proof := requirements
	if boolValue(pow["required"], false) {
		seed, difficulty := stringValue(pow["seed"]), strings.ToLower(stringValue(pow["difficulty"]))
		if seed == "" || len(difficulty) < 1 || len(difficulty) > 8 || strings.Trim(difficulty, "0123456789abcdef") != "" {
			return nil, fail("challenge", "unsupported_difficulty")
		}
		proof = ""
		start := time.Now()
		for i := 0; i < 500000; i++ {
			if i%256 == 0 && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			configuration[3], configuration[9] = i, time.Since(start).Milliseconds()
			encoded := encode()
			if legacyRegistrationHash(seed + encoded)[:len(difficulty)] <= difficulty {
				proof = "gAAAAAB" + encoded + "~S"
				break
			}
		}
		if proof == "" {
			return nil, fail("challenge", "work_limit")
		}
	}
	turnstile := ""
	ts := object(res.data["turnstile"])
	if boolValue(ts["required"], false) {
		turnstile, err = provider.RegistrationSentinelToken(stringValue(ts["dx"]), requirements)
		if err != nil || turnstile == "" {
			return nil, fail("challenge", "unsupported_challenge")
		}
	}
	var cookie string
	for _, line := range res.headers.Values("Set-Cookie") {
		h := fhttp.Header{"Set-Cookie": {line}}
		response := fhttp.Response{Header: h}
		for _, c := range response.Cookies() {
			if c.Name == "oai-sc" {
				cookie = c.Value
			}
		}
	}
	if cookie == "" {
		origin, _ := url.Parse(r.chat)
		for _, existing := range r.http.GetCookies(origin) {
			if existing.Name == "oai-sc" {
				cookie = existing.Value
				break
			}
		}
	}
	if cookie == "" {
		return nil, fail("challenge", "missing_session_cookie")
	}
	r.cookie(r.sentinel, "oai-sc", cookie)
	r.cookie(r.auth, "oai-sc", cookie)
	if soRequired {
		headers, err := r.sentinelHeadersFromRunner(ctx, flow, requirements, sentinelSID, cookie, res.data)
		if err != nil {
			return nil, fail("challenge", "unsupported_so")
		}
		return headers, nil
	}
	value, _ := json.Marshal(map[string]string{"p": proof, "t": turnstile, "c": token, "id": r.device, "flow": flow})
	return map[string]string{"openai-sentinel-token": string(value)}, nil
}
func legacyRegistrationHash(text string) string {
	h := uint32(2166136261)
	for _, ch := range text {
		h ^= uint32(ch)
		h *= 16777619
	}
	h ^= h >> 16
	h *= 2246822507
	h ^= h >> 13
	h *= 3266489909
	h ^= h >> 16
	return fmt.Sprintf("%08x", h)
}

func continuationURL(data map[string]any) string {
	for _, key := range []string{"continue_url", "continueUrl"} {
		if value := stringValue(data[key]); value != "" {
			return value
		}
	}
	for _, source := range []map[string]any{object(object(data["page"])["payload"]), object(data["oai-client-auth-session"])} {
		for _, key := range []string{"continue_url", "continueUrl", "next_url", "nextUrl"} {
			if value := stringValue(source[key]); value != "" {
				return value
			}
		}
	}
	return ""
}
