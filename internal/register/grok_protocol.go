package register

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserprofile"
	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

const (
	grokAccountsURL = "https://accounts.x.ai"
	grokSignupURL   = grokAccountsURL + "/sign-up?redirect=grok-com"
	grokSignupPage  = grokAccountsURL + "/sign-up"
	grokCreateCode  = grokAccountsURL + "/auth_mgmt.AuthManagement/CreateEmailValidationCode"
	grokVerifyCode  = grokAccountsURL + "/auth_mgmt.AuthManagement/VerifyEmailValidationCode"
)

var (
	grokSiteKeyRE = regexp.MustCompile(`0x4AAAAAAA[a-zA-Z0-9_-]+`)
	// Current xAI deployments append a cache/deployment query string to
	// Next.js chunks (for example `foo.js?dpl=...`). Keep the query out of
	// the request path while still accepting both old and new markup.
	grokScriptRE          = regexp.MustCompile(`src=["'](/_next/static/[^"']+\.js)(?:\?[^"']*)?["']`)
	grokChunkRE           = regexp.MustCompile(`["']((?:/_next/)?static/chunks/[^"'?]+\.js)(?:\?[^"']*)?["']`)
	grokActionRE          = regexp.MustCompile(`(?i)[a-f0-9]{40,50}`)
	grokStateRE           = regexp.MustCompile(`self\.__next_f\.push\(\[1,"(.*?)"\]\)`)
	grokJWTRE             = regexp.MustCompile(`eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+`)
	grokSSOCookieRE       = regexp.MustCompile(`(?i)(?:^|[;,\s])(?:sso|sso-rw|sso_jwt)=([^;\s]+)`)
	grokCookieURL         = regexp.MustCompile(`https?://[^\s"'<>\\]+set-cookie/?\?q=eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+`)
	grokCookieRelativeURL = regexp.MustCompile(`(/[A-Za-z0-9_./-]*set-cookie/?\?q=eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)`)
)

type GrokSignupConfig struct {
	SiteKey   string
	ActionID  string
	StateTree string
}

type grokResponse struct {
	Status  int
	Headers fhttp.Header
	Body    []byte
}

// GrokProtocolClient implements the xAI signup transport without opening the
// signup page in a persistent browser. The only browser-like component is the
// configured Turnstile provider owned by GrokProtocolFlow.
type GrokProtocolClient struct {
	http                  tlsclient.HttpClient
	cookies               *browserprofile.CookieJar
	profile               BrowserProfile
	proxy                 string
	requestTimeout        time.Duration
	config                GrokSignupConfig
	browserUserAgent      string
	ssoMaterializer       GrokSSOMaterializer
	lastGRPCStatus        string
	lastGRPCStatusPresent bool
}

func NewGrokProtocolClient(proxy string, profile BrowserProfile, timeout time.Duration) (*GrokProtocolClient, error) {
	if !profile.valid() {
		profile = NewBrowserProfile()
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	jar := browserprofile.NewCookieJar(grokAccountsURL)
	options := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(profile.tlsClientProfile()),
		tlsclient.WithTimeoutSeconds(int(timeout / time.Second)),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithCookieJar(jar),
	}
	if proxy != "" && proxy != "direct" {
		options = append(options, tlsclient.WithProxyUrl(proxy))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, fail("bootstrap", "http_client_failed")
	}
	return &GrokProtocolClient{http: client, cookies: jar, profile: profile, proxy: proxy, requestTimeout: timeout}, nil
}

func (c *GrokProtocolClient) Close() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

func (c *GrokProtocolClient) BrowserProfile() BrowserProfile { return c.profile }

func (c *GrokProtocolClient) SetSSOMaterializer(materializer GrokSSOMaterializer) {
	if c != nil {
		c.ssoMaterializer = materializer
	}
}

// RestoreBrowserContext carries the worker's short-lived cookies into the
// protocol jar and aligns the request User-Agent with the browser that solved
// Turnstile. The worker never receives account credentials.
func (c *GrokProtocolClient) RestoreBrowserContext(context GrokBrowserContext) {
	if c == nil {
		return
	}
	if len(context.Cookies) > 0 && c.cookies != nil {
		// The protocol jar is intentionally scoped to accounts.x.ai. Browser
		// snapshots may contain a parent-domain (.x.ai) cookie, so make those
		// host-only before restoring instead of silently dropping them.
		restored := make([]browserprofile.Cookie, 0, len(context.Cookies))
		for _, cookie := range context.Cookies {
			cookie.Domain = ""
			restored = append(restored, cookie)
		}
		c.cookies.Restore(restored)
	}
	if strings.TrimSpace(context.UserAgent) != "" {
		c.browserUserAgent = strings.TrimSpace(context.UserAgent)
	}
}

func (c *GrokProtocolClient) request(ctx context.Context, method, rawURL string, body []byte, contentType string, headers map[string]string) (grokResponse, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || !grokTrustedHost(u.Hostname()) {
		return grokResponse{}, fail("protocol", "untrusted_redirect")
	}
	req, err := fhttp.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return grokResponse{}, fail("protocol", "invalid_request")
	}
	for key, value := range browserprofile.Profile(c.profile).Headers() {
		req.Header.Set(key, value)
	}
	if strings.TrimSpace(c.browserUserAgent) != "" {
		req.Header.Set("User-Agent", c.browserUserAgent)
	}
	req.Header.Set("Accept", "*/*")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	browserprofile.SetHeaderOrder(req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return grokResponse{}, ctx.Err()
		}
		return grokResponse{}, fail("protocol", "network_error")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return grokResponse{}, fail("protocol", "read_error")
	}
	result := grokResponse{Status: resp.StatusCode, Headers: resp.Header, Body: raw}
	if resp.StatusCode >= 400 {
		code := "upstream_rejected"
		if resp.StatusCode == 403 {
			code = "verification_required"
		}
		if resp.StatusCode == 429 {
			code = "rate_limited"
		}
		failure := &Failure{Stage: "protocol", Code: code, HTTPStatus: resp.StatusCode}
		failure.UpstreamStage = grokUpstreamCode(raw)
		return result, failure
	}
	return result, nil
}

func grokUpstreamCode(body []byte) string {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return ""
	}
	for _, key := range []string{"code", "error_code", "errorCode"} {
		candidate := strings.TrimSpace(stringValue(value[key]))
		if candidate != "" && regexp.MustCompile(`^[A-Za-z0-9:_-]{2,128}$`).MatchString(candidate) {
			return candidate
		}
	}
	if nested, ok := value["error"].(map[string]any); ok {
		candidate := strings.TrimSpace(stringValue(nested["code"]))
		if candidate != "" && regexp.MustCompile(`^[A-Za-z0-9:_-]{2,128}$`).MatchString(candidate) {
			return candidate
		}
	}
	return ""
}

func grokTrustedHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "accounts.x.ai" || host == "auth.grokusercontent.com" || host == "auth.x.ai" || host == "grok.com" || host == "www.grok.com"
}

func (c *GrokProtocolClient) FetchConfig(ctx context.Context) (GrokSignupConfig, error) {
	response, err := c.request(ctx, "GET", grokSignupURL, nil, "", map[string]string{
		"Accept":  "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Referer": "https://grok.com/",
	})
	if err != nil {
		return GrokSignupConfig{}, err
	}
	if response.Status != 200 || strings.Contains(strings.ToLower(string(response.Body[:minInt(len(response.Body), 500)])), "just a moment") {
		return GrokSignupConfig{}, &Failure{Stage: "bootstrap", Code: "signup_page_blocked", HTTPStatus: response.Status}
	}
	html := string(response.Body)
	config := GrokSignupConfig{}
	if match := grokSiteKeyRE.FindString(html); match != "" {
		config.SiteKey = match
	}
	config.StateTree = scrapeGrokStateTree(html)
	queue := make([]string, 0, 32)
	seen := make(map[string]bool)
	for _, match := range grokScriptRE.FindAllStringSubmatch(html, -1) {
		if len(match) == 2 {
			queue = append(queue, grokAccountsURL+match[1])
		}
	}
	for index := 0; index < len(queue) && index < 120; index++ {
		scriptURL := queue[index]
		if seen[scriptURL] {
			continue
		}
		seen[scriptURL] = true
		script, fetchErr := c.request(ctx, "GET", scriptURL, nil, "", map[string]string{"Referer": grokSignupURL})
		if fetchErr != nil || script.Status != 200 {
			continue
		}
		source := string(script.Body)
		if config.ActionID == "" {
			config.ActionID = discoverGrokActionID(source)
		}
		for _, chunk := range grokChunkRE.FindAllStringSubmatch(source, -1) {
			if len(chunk) != 2 {
				continue
			}
			path := chunk[1]
			if !strings.HasPrefix(path, "/") {
				path = "/_next/" + path
			}
			queue = append(queue, grokAccountsURL+path)
		}
	}
	if config.SiteKey == "" || config.ActionID == "" || config.StateTree == "" {
		return GrokSignupConfig{}, fail("bootstrap", "signup_config_incomplete")
	}
	c.config = config
	return config, nil
}

func discoverGrokActionID(source string) string {
	if source == "" {
		return ""
	}
	// Prefer a named Server Action reference bound to the signup function.
	named := regexp.MustCompile(`(?i)createServerReference\)\("([a-f0-9]{40,64})"[^)]{0,260},"([A-Za-z0-9_]+)"\)`)
	for _, match := range named.FindAllStringSubmatch(source, -1) {
		if len(match) == 3 && (strings.Contains(strings.ToLower(match[2]), "createuser") || strings.Contains(strings.ToLower(match[2]), "signup") || strings.Contains(strings.ToLower(match[2]), "emailvalidation")) {
			return match[1]
		}
	}
	// Next's generated code often leaves the function name out. In that case,
	// only accept hashes co-located with the signup payload fields.
	for _, key := range []string{"createUserAndSessionRequest", "emailValidationCode"} {
		for start := 0; ; {
			index := strings.Index(source[start:], key)
			if index < 0 {
				break
			}
			index += start
			lo, hi := index-700, index+700
			if lo < 0 {
				lo = 0
			}
			if hi > len(source) {
				hi = len(source)
			}
			window := source[lo:hi]
			if match := regexp.MustCompile(`(?i)createServerReference\)?\(["']([a-f0-9]{40,64})["']`).FindStringSubmatch(window); len(match) == 2 {
				return match[1]
			}
			if match := regexp.MustCompile(`["']([a-f0-9]{40,64})["']`).FindStringSubmatch(window); len(match) == 2 {
				return match[1]
			}
			start = index + len(key)
		}
	}
	if match := regexp.MustCompile(`(?i)next-action["'\s:=]+([a-f0-9]{40,64})`).FindStringSubmatch(source); len(match) == 2 {
		return match[1]
	}
	if strings.Contains(source, "createUser") || strings.Contains(source, "registerUser") || strings.Contains(source, "emailValidation") {
		return grokActionRE.FindString(source)
	}
	return ""
}

func scrapeGrokStateTree(html string) string {
	for _, match := range grokStateRE.FindAllStringSubmatch(html, -1) {
		if len(match) != 2 {
			continue
		}
		decoded := strings.ReplaceAll(match[1], `\"`, `"`)
		if !strings.Contains(decoded, "sign-up") {
			continue
		}
		startMarker := `"f":[[[`
		start := strings.Index(decoded, startMarker)
		if start < 0 {
			continue
		}
		// Keep the same slice boundary as the reference implementation: the
		// state tree begins at the second opening bracket of `"f":[[[`.
		start += 5
		end := strings.Index(decoded[start:], `"$undefined"`)
		if end < 0 {
			continue
		}
		raw := decoded[start : start+end]
		raw = strings.ReplaceAll(strings.ReplaceAll(raw, `\\"`, `"`), `\\`, "")
		encoded := url.QueryEscape(raw)
		return strings.ReplaceAll(encoded, "+", "%20")
	}
	return ""
}

func pbVarint(value int) []byte {
	result := make([]byte, 0, 5)
	for value > 0x7f {
		result = append(result, byte(value)|0x80)
		value >>= 7
	}
	return append(result, byte(value))
}

func pbString(field int, value string) []byte {
	raw := []byte(value)
	result := []byte{byte((field << 3) | 2)}
	result = append(result, pbVarint(len(raw))...)
	return append(result, raw...)
}

func grpcWebFrame(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = 0
	frame[1] = byte(len(payload) >> 24)
	frame[2] = byte(len(payload) >> 16)
	frame[3] = byte(len(payload) >> 8)
	frame[4] = byte(len(payload))
	copy(frame[5:], payload)
	return frame
}

func (c *GrokProtocolClient) grpc(ctx context.Context, endpoint string, payload []byte) error {
	response, err := c.request(ctx, "POST", endpoint, grpcWebFrame(payload), "application/grpc-web+proto", map[string]string{
		"X-Grpc-Web":   "1",
		"X-User-Agent": "connect-es/2.1.1",
		"Origin":       grokAccountsURL,
		"Referer":      grokSignupURL,
	})
	if err != nil {
		return err
	}
	status, message, hasStatus := grpcResponseStatus(response.Body, response.Headers)
	c.lastGRPCStatus = status
	c.lastGRPCStatusPresent = hasStatus
	if response.Status != 200 || (hasStatus && status != "0") {
		failure := &Failure{Stage: "grpc", Code: "upstream_rejected", HTTPStatus: response.Status}
		if hasStatus {
			failure.UpstreamStage = "grpc_status_" + status
		}
		if message != "" {
			failure.Operation = "grpc_message=" + sanitizeGRPCMessage(message)
		}
		return failure
	}
	return nil
}

// GRPCStatusStage exposes only the diagnostic result needed by the
// registration log. It never includes an upstream message or response body.
func (c *GrokProtocolClient) GRPCStatusStage() string {
	if c != nil && c.lastGRPCStatusPresent && c.lastGRPCStatus == "0" {
		return "send_code_status_0"
	}
	return "send_code_status_unknown"
}

func grpcOK(body []byte, headers fhttp.Header) bool {
	status, _, hasStatus := grpcResponseStatus(body, headers)
	return !hasStatus || status == "0" // xAI may return an empty 200 data frame.
}

// grpcResponseStatus reads both the normal response header and the binary
// trailer frame used by grpc-web. Some xAI responses omit the header and put
// grpc-status only in the trailer payload, so checking headers alone can turn
// an upstream rejection into a false success.
func grpcResponseStatus(body []byte, headers fhttp.Header) (status, message string, found bool) {
	if value := strings.TrimSpace(headers.Get("grpc-status")); value != "" {
		return value, strings.TrimSpace(headers.Get("grpc-message")), true
	}
	for _, trailer := range grpcWebTrailers(body) {
		if value, text, ok := grpcTrailerFields(trailer); ok {
			return value, text, true
		}
	}
	// Keep compatibility with intermediaries that strip the frame marker but
	// leave the ASCII trailer payload in the response body.
	if value, text, ok := grpcTrailerFields(body); ok {
		return value, text, true
	}
	return "", "", false
}

func grpcWebTrailers(body []byte) [][]byte {
	trailers := make([][]byte, 0, 1)
	for offset := 0; offset+5 <= len(body); {
		flags := body[offset]
		size := int(binary.BigEndian.Uint32(body[offset+1 : offset+5]))
		end := offset + 5 + size
		if size < 0 || end > len(body) {
			break
		}
		if flags&0x80 != 0 {
			trailers = append(trailers, body[offset+5:end])
		}
		offset = end
	}
	return trailers
}

func grpcTrailerFields(payload []byte) (status, message string, found bool) {
	text := string(payload)
	statusMatch := regexp.MustCompile(`(?im)(?:^|[\r\n])grpc-status:\s*([^\r\n]+)`).FindStringSubmatch(text)
	if len(statusMatch) != 2 {
		return "", "", false
	}
	status = strings.TrimSpace(statusMatch[1])
	messageMatch := regexp.MustCompile(`(?im)(?:^|[\r\n])grpc-message:\s*([^\r\n]*)`).FindStringSubmatch(text)
	if len(messageMatch) == 2 {
		message = strings.TrimSpace(messageMatch[1])
	}
	return status, message, true
}

func sanitizeGRPCMessage(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 96 {
		value = value[:96]
	}
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	return strings.TrimSpace(value)
}

func (c *GrokProtocolClient) CreateEmailValidationCode(ctx context.Context, email string) error {
	if strings.TrimSpace(email) == "" {
		return fail("send_code", "missing_email")
	}
	return c.grpc(ctx, grokCreateCode, pbString(1, email))
}

func (c *GrokProtocolClient) VerifyEmailValidationCode(ctx context.Context, email, code string) error {
	code = strings.ReplaceAll(strings.TrimSpace(code), "-", "")
	if email == "" || code == "" {
		return fail("validate_code", "missing_email_or_code")
	}
	return c.grpc(ctx, grokVerifyCode, append(pbString(1, email), pbString(2, code)...))
}

func (c *GrokProtocolClient) Signup(ctx context.Context, email, password, code, given, family, turnstile string) (string, error) {
	config := c.config
	if config.ActionID == "" || config.StateTree == "" {
		return "", fail("signup", "signup_config_missing")
	}
	conversionID := randomUUID()
	body := []any{map[string]any{
		"emailValidationCode": code,
		"createUserAndSessionRequest": map[string]any{
			"email":              email,
			"givenName":          given,
			"familyName":         family,
			"clearTextPassword":  password,
			"tosAcceptedVersion": 1,
		},
		"turnstileToken":     turnstile,
		"conversionId":       conversionID,
		"castleRequestToken": "",
	}, map[string]any{
		"client":      "$T",
		"meta":        "$undefined",
		"mutationKey": "$undefined",
	}}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fail("signup", "invalid_request")
	}
	response, err := c.request(ctx, "POST", grokSignupURL, raw, "text/plain;charset=UTF-8", map[string]string{
		"Accept":                 "text/x-component",
		"Next-Action":            config.ActionID,
		"next-action":            config.ActionID,
		"Next-Router-State-Tree": config.StateTree,
		"next-router-state-tree": config.StateTree,
		"Origin":                 grokAccountsURL,
		"Referer":                grokSignupURL,
		"Sec-Fetch-Site":         "same-origin",
		"Sec-Fetch-Mode":         "cors",
		"Sec-Fetch-Dest":         "empty",
	})
	if token := c.cookieValue("sso"); token != "" {
		return token, nil
	}
	if token := extractGrokSSOCookieHeaders(response.Headers); token != "" {
		return token, nil
	}
	if token := c.materializeGrokSSOHeaders(ctx, response.Headers); token != "" {
		return token, nil
	}
	for _, candidate := range extractGrokSetCookieURLs(string(response.Body)) {
		if token := c.followSSO(ctx, candidate); token != "" {
			return token, nil
		}
	}
	if token := extractGrokJWT(string(response.Body)); token != "" {
		return token, nil
	}
	if token := c.materializeGrokSSO(ctx, string(response.Body)); token != "" {
		return token, nil
	}
	return "", fail("sso", "missing_sso")
}

func (c *GrokProtocolClient) materializeGrokSSO(ctx context.Context, body string) string {
	seen := map[string]bool{}
	candidates := append([]string{}, grokJWTRE.FindAllString(body, -1)...)
	for _, rawURL := range []string{grokAccountsURL, "https://auth.grokusercontent.com", "https://auth.x.ai", "https://grok.com", "https://x.ai"} {
		u, _ := url.Parse(rawURL)
		for _, cookie := range c.http.GetCookies(u) {
			if cookie.Name != "sso" && cookie.Name != "sso-rw" && cookie.Name != "sso_jwt" {
				continue
			}
			candidates = append(candidates, grokJWTRE.FindAllString(cookie.Value, -1)...)
		}
	}
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if isSessionGrokSSO(candidate) {
			return candidate
		}
		if c.ssoMaterializer != nil {
			if result, err := c.ssoMaterializer.MaterializeSSO(ctx, candidate, c.proxy, c.cookies.Snapshot()); err == nil {
				c.RestoreBrowserContext(result)
				if strings.TrimSpace(result.Token) != "" {
					return result.Token
				}
			}
		}
		for _, hop := range expandGrokSSOHops("https://auth.grokusercontent.com/set-cookie?q=" + candidate) {
			if token := c.followSSO(ctx, hop); token != "" {
				return token
			}
		}
	}
	return ""
}

func (c *GrokProtocolClient) materializeGrokSSOHeaders(ctx context.Context, headers fhttp.Header) string {
	values := extractGrokSSOCookieCandidates(headers)
	if len(values) == 0 {
		return ""
	}
	return c.materializeGrokSSO(ctx, strings.Join(values, "\n"))
}

func extractGrokSSOCookieHeaders(headers fhttp.Header) string {
	for _, candidate := range extractGrokSSOCookieCandidates(headers) {
		if isSessionGrokSSO(candidate) {
			return candidate
		}
	}
	return ""
}

func extractGrokSSOCookieCandidates(headers fhttp.Header) []string {
	seen := map[string]bool{}
	result := make([]string, 0, 2)
	for _, value := range headers.Values("Set-Cookie") {
		for _, match := range grokSSOCookieRE.FindAllStringSubmatch(value, -1) {
			if len(match) != 2 {
				continue
			}
			candidate := strings.TrimSpace(match[1])
			if candidate == "" || seen[candidate] || !strings.HasPrefix(candidate, "eyJ") {
				continue
			}
			seen[candidate] = true
			result = append(result, candidate)
		}
	}
	return result
}

func extractGrokRedirectURLs(value any) []string {
	seen := map[string]bool{}
	result := make([]string, 0, 4)
	var visit func(any)
	visit = func(current any) {
		switch item := current.(type) {
		case map[string]any:
			for key, nested := range item {
				lower := strings.ToLower(strings.TrimSpace(key))
				if strings.Contains(lower, "redirect") || strings.Contains(lower, "cookie") || strings.Contains(lower, "hop") || strings.Contains(lower, "success") {
					if text := strings.TrimSpace(stringValue(nested)); text != "" {
						parsed, err := url.Parse(text)
						if err == nil && parsed.Scheme == "" {
							parsed, err = url.Parse(grokAccountsURL + "/" + strings.TrimPrefix(text, "/"))
						}
						if err == nil && grokTrustedHost(parsed.Hostname()) && parsed.Scheme == "https" && !seen[parsed.String()] {
							seen[parsed.String()] = true
							result = append(result, parsed.String())
						}
					}
				}
				visit(nested)
			}
		case []any:
			for _, nested := range item {
				visit(nested)
			}
		}
	}
	visit(value)
	return result
}

func extractGrokSessionToken(value any) string {
	var visit func(any, int) string
	visit = func(current any, depth int) string {
		if depth > 8 {
			return ""
		}
		switch item := current.(type) {
		case map[string]any:
			for key, nested := range item {
				lower := strings.ToLower(strings.TrimSpace(key))
				if lower == "sso" || lower == "sso-rw" || lower == "sso_jwt" || lower == "ssotoken" || lower == "sessioncookie" || lower == "session_cookie" || lower == "accesstoken" || lower == "access_token" || lower == "token" {
					if candidate := strings.TrimSpace(stringValue(nested)); isSessionGrokSSO(candidate) {
						return candidate
					}
				}
				if candidate := visit(nested, depth+1); candidate != "" {
					return candidate
				}
			}
		case []any:
			for _, nested := range item {
				if candidate := visit(nested, depth+1); candidate != "" {
					return candidate
				}
			}
		case string:
			if candidate := extractGrokJWT(item); candidate != "" {
				return candidate
			}
		}
		return ""
	}
	return visit(value, 0)
}

func (c *GrokProtocolClient) cookieValue(name string) string {
	for _, rawURL := range []string{grokAccountsURL, "https://auth.grokusercontent.com", "https://auth.x.ai", "https://grok.com", "https://x.ai"} {
		u, _ := url.Parse(rawURL)
		for _, cookie := range c.http.GetCookies(u) {
			if (cookie.Name == name || (name == "sso" && (cookie.Name == "sso-rw" || cookie.Name == "sso_jwt"))) && isSessionGrokSSO(strings.TrimSpace(cookie.Value)) {
				return strings.TrimSpace(cookie.Value)
			}
		}
	}
	return ""
}

func (c *GrokProtocolClient) followSSO(ctx context.Context, start string) string {
	queue := []string{start}
	seen := map[string]bool{}
	for len(queue) > 0 && len(seen) < 10 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		response, err := c.request(ctx, "GET", current, nil, "", map[string]string{"Accept": "text/html,application/xhtml+xml", "Referer": grokSignupURL})
		if err != nil {
			continue
		}
		if token := c.cookieValue("sso"); token != "" {
			return token
		}
		if token := extractGrokSSOCookieHeaders(response.Headers); token != "" {
			return token
		}
		if token := c.materializeGrokSSOHeaders(ctx, response.Headers); token != "" {
			return token
		}
		if token := extractGrokJWT(string(response.Body)); token != "" {
			return token
		}
		for _, extra := range expandGrokSSOHops(current) {
			if !seen[extra] {
				queue = append(queue, extra)
			}
		}
		location := response.Headers.Get("Location")
		if location != "" {
			base, _ := url.Parse(current)
			if next, parseErr := base.Parse(location); parseErr == nil && grokTrustedHost(next.Hostname()) {
				queue = append(queue, next.String())
			}
		}
	}
	return ""
}

func extractGrokSetCookieURLs(text string) []string {
	body := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(text, `\u0026`, "&"), `\u003d`, "="), `\/`, "/")
	seen := map[string]bool{}
	result := make([]string, 0, 4)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		result = append(result, value)
	}
	for _, value := range grokCookieURL.FindAllString(body, -1) {
		add(value)
	}
	for _, value := range grokCookieRelativeURL.FindAllString(body, -1) {
		add(grokAccountsURL + value)
	}
	if len(result) == 0 {
		lower := strings.ToLower(body)
		if index := strings.Index(lower, "set-cookie"); index >= 0 {
			if token := extractGrokJWT(body[index:minInt(len(body), index+400)]); token != "" {
				add("https://auth.grokusercontent.com/set-cookie?q=" + token)
			}
		}
	}
	return result
}

func expandGrokSSOHops(start string) []string {
	result := []string{start}
	if token := extractGrokJWTAny(start); token != "" {
		result = append(result, "https://auth.grokusercontent.com/set-cookie?q="+token)
		if successURL := grokJWTSuccessURL(token); successURL != "" {
			result = append(result, successURL)
			if strings.Contains(successURL, "set-cookie") && !strings.Contains(successURL, "q=") {
				result = append(result, strings.TrimRight(successURL, "/")+"?q="+token)
			}
		}
	}
	return result
}

func extractGrokJWTAny(text string) string {
	return grokJWTRE.FindString(text)
}

func grokJWTSuccessURL(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4))
	}
	if err != nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(decoded, &payload) != nil {
		return ""
	}
	for _, value := range []any{payload["success_url"], nestedValue(payload, "config", "success_url")} {
		candidate := strings.TrimSpace(stringValue(value))
		if parsed, parseErr := url.Parse(candidate); parseErr == nil && parsed.Scheme == "https" && grokTrustedHost(parsed.Hostname()) {
			return parsed.String()
		}
	}
	return ""
}

func nestedValue(value map[string]any, path ...string) any {
	var current any = value
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func extractGrokJWT(text string) string {
	for _, candidate := range grokJWTRE.FindAllString(text, -1) {
		if isSessionGrokSSO(candidate) {
			return candidate
		}
	}
	return ""
}

func isSessionGrokSSO(token string) bool {
	if !strings.HasPrefix(token, "eyJ") || strings.Count(token, ".") != 2 || len(token) <= 40 {
		return false
	}
	parts := strings.Split(token, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4))
	}
	if err != nil {
		return true
	}
	var payload map[string]any
	if json.Unmarshal(decoded, &payload) != nil {
		return true
	}
	if _, exists := payload["success_url"]; exists {
		return false
	}
	if config, ok := payload["config"].(map[string]any); ok {
		if _, exists := config["success_url"]; exists {
			return false
		}
		if _, exists := config["token"]; exists {
			return false
		}
	}
	return true
}

func randomUUID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return hex.EncodeToString(raw)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(raw[:4]), hex.EncodeToString(raw[4:6]), hex.EncodeToString(raw[6:8]), hex.EncodeToString(raw[8:10]), hex.EncodeToString(raw[10:]))
}

type GrokProtocolFlow struct {
	Proxy            string
	Profile          BrowserProfile
	Timeout          time.Duration
	Solver           GrokTurnstileSolver
	Client           *GrokProtocolClient
	browserCloser    GrokBrowserSessionCloser
	browserSessionID string
}

func NewGrokProtocolFlow(cfg GrokConfig, solver GrokTurnstileSolver) (*GrokProtocolFlow, error) {
	profile := NewBrowserProfile()
	if cfg.BrowserProfile != nil {
		if parsed, ok := ParseBrowserProfile(cfg.BrowserProfile); ok {
			profile = parsed
		}
	}
	client, err := NewGrokProtocolClient(cfg.Proxy, profile, cfg.RequestTimeout)
	if err != nil {
		return nil, err
	}
	if materializer, ok := solver.(GrokSSOMaterializer); ok {
		client.SetSSOMaterializer(materializer)
	}
	if setter, ok := solver.(GrokBrowserProfileSetter); ok {
		setter.SetBrowserProfile(profile)
	}
	flow := &GrokProtocolFlow{Proxy: cfg.Proxy, Profile: profile, Timeout: cfg.RequestTimeout, Solver: solver, Client: client}
	if closer, ok := solver.(GrokBrowserSessionCloser); ok {
		flow.browserCloser = closer
	}
	return flow, nil
}

func (f *GrokProtocolFlow) Close() {
	if f != nil && f.browserCloser != nil && strings.TrimSpace(f.browserSessionID) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = f.browserCloser.CloseSession(ctx, f.browserSessionID)
		cancel()
		f.browserSessionID = ""
	}
	if f != nil && f.Client != nil {
		f.Client.Close()
	}
}

func (f *GrokProtocolFlow) BrowserProfile() BrowserProfile { return f.Profile }

func (f *GrokProtocolFlow) Register(ctx context.Context, box Mailbox, cfg GrokConfig, wait func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	if f.Solver == nil {
		return nil, fail("turnstile", "solver_not_configured")
	}
	if err := progress("bootstrap"); err != nil {
		return nil, err
	}
	metadata, err := f.Client.FetchConfig(ctx)
	if err != nil {
		return nil, err
	}
	if err := progress("turnstile"); err != nil {
		return nil, err
	}
	pageURL := grokSignupURL
	var turnstile string
	if contextual, ok := f.Solver.(GrokBrowserContextSolver); ok {
		browserContext, solveErr := contextual.SolveContext(ctx, metadata.SiteKey, pageURL, cfg.Proxy)
		if solveErr != nil {
			return nil, solveErr
		}
		turnstile = browserContext.Token
		f.browserSessionID = strings.TrimSpace(browserContext.SessionID)
		f.Client.RestoreBrowserContext(browserContext)
	} else {
		turnstile, err = f.Solver.Solve(ctx, metadata.SiteKey, pageURL, cfg.Proxy)
		if err != nil {
			return nil, err
		}
	}
	if err := progress("send_code"); err != nil {
		return nil, err
	}
	sendCode := func() error {
		// The browser session is used to mint Turnstile and to preserve the
		// browser identity for the later signup action. Email-code delivery must
		// use the standalone AuthManagement gRPC endpoint: the page JSON endpoint
		// first mints a Castle step token and can return the misleading
		// account:email-signup-unavailable error when that token is not bound to
		// the live page. The gRPC endpoint is the protocol path used by the
		// reference implementations and returns an explicit grpc-status.
		if err := f.Client.CreateEmailValidationCode(ctx, box.Email); err != nil {
			return err
		}
		return progress(f.Client.GRPCStatusStage())
	}
	if err := sendCode(); err != nil {
		return nil, err
	}
	if err := progress("wait_code"); err != nil {
		return nil, err
	}
	var code string
	var waitErr error
	after := time.Now().UTC().Add(-10 * time.Second)
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
		if err := sendCode(); err != nil {
			return nil, err
		}
		if err := progress("wait_code"); err != nil {
			return nil, err
		}
		after = time.Now().UTC().Add(-10 * time.Second)
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), "-", "")
	if len(code) != 6 {
		return nil, fail("wait_code", "invalid_code")
	}
	if err := progress("validate_code"); err != nil {
		return nil, err
	}
	if err := f.Client.VerifyEmailValidationCode(ctx, box.Email, code); err != nil {
		return nil, err
	}
	if err := progress("signup"); err != nil {
		return nil, err
	}
	password := generateGrokPassword()
	given, family := grokProfileName(box.Email)
	sso, err := f.Client.Signup(ctx, box.Email, password, code, given, family, turnstile)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sso) == "" {
		return nil, fail("sso", "missing_sso")
	}
	return map[string]any{
		"email":           box.Email,
		"password":        password,
		"sso":             sso,
		"source_type":     "grok_protocol",
		"platform":        "grok",
		"status":          "待验证",
		"enabled":         false,
		"created_at":      time.Now().UTC().Format(time.RFC3339),
		"cookie_header":   "sso=" + sso + "; sso-rw=" + sso,
		"browser_profile": browserprofile.Profile(f.Profile).FingerprintMetadata(),
	}, nil
}

func generateGrokPassword() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "Grok-Cfm-Registration-9a7b!"
	}
	return fmt.Sprintf("Grok-%x-Aa1!", raw)
}

func grokProfileName(email string) (string, string) {
	local := email
	if index := strings.LastIndex(local, "@"); index >= 0 {
		local = local[:index]
	}
	parts := regexp.MustCompile(`[A-Za-z]+`).FindAllString(local, 2)
	if len(parts) == 0 {
		return "Grok", "User"
	}
	if len(parts) == 1 {
		return titleWord(parts[0]), "User"
	}
	return titleWord(parts[0]), titleWord(parts[1])
}

func titleWord(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "User"
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
