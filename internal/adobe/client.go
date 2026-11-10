package adobe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
const refreshURL = "https://adobeid-na1.services.adobe.com/ims/check/v6/token?jslVersion=v2-v0.48.0-1-g1e322cb"
const refreshScope = "AdobeID,firefly_api,openid,pps.read,pps.write,additional_info.projectedProductContext,additional_info.ownerOrg,uds_read,uds_write,ab.manage,read_organizations,additional_info.roles,account_cluster.read,creative_production,profile"

var pollHost = regexp.MustCompile(`^bks-epo[0-9]+\.adobe\.io$`)

type Client struct{ http *http.Client }

func NewClient(client *http.Client) *Client {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.Jar = nil
	return &Client{http: &c}
}
func trustedURL(raw, kind string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || u.Fragment != "" {
		return false
	}
	switch kind {
	case "submit":
		return u.Hostname() == "firefly-3p.ff.adobe.io" && (u.Path == "/v2/3p-images/generate-async" || u.Path == "/v2/3p-videos/generate-async")
	case "balance":
		return u.Hostname() == "firefly.adobe.io" && u.Path == "/v1/credits/balance"
	case "refresh":
		return u.Hostname() == "adobeid-na1.services.adobe.com" && u.Path == "/ims/check/v6/token"
	case "poll":
		return (pollHost.MatchString(u.Hostname()) || u.Hostname() == "firefly-3p.ff.adobe.io") && strings.HasPrefix(u.Path, "/v2/jobs/result/")
	case "download":
		return u.Hostname() == "pre-signed-firefly-prod.s3-accelerate.amazonaws.com"
	}
	return false
}
func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b[:])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}
func (c *Client) request(ctx context.Context, stage, method, endpoint string, a Account, body []byte, headers map[string]string) (map[string]any, http.Header, error) {
	if !trustedURL(endpoint, stage) {
		return nil, nil, &Error{Stage: stage, Code: "unexpected_host"}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, &Error{Stage: stage, Code: "invalid_request"}
	}
	req.GetBody = nil
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", "https://firefly.adobe.com")
	req.Header.Set("Referer", "https://firefly.adobe.com/")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	ua := a.UserAgent
	if ua == "" {
		ua = defaultUA
	}
	req.Header.Set("User-Agent", ua)
	version := "145"
	if m := regexp.MustCompile(`Chrome/([0-9]+)`).FindStringSubmatch(ua); len(m) > 1 {
		version = m[1]
	}
	req.Header.Set("sec-ch-ua", `"Chromium";v="`+version+`", "Google Chrome";v="`+version+`", "Not/A)Brand";v="99"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	platform := `"Windows"`
	if strings.Contains(ua, "Macintosh") {
		platform = `"macOS"`
	} else if strings.Contains(ua, "Linux") {
		platform = `"Linux"`
	}
	req.Header.Set("sec-ch-ua-platform", platform)
	req.Header.Set("sec-fetch-site", "cross-site")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("Content-Type", "application/json")
	if stage != "refresh" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
		req.Header.Set("x-api-key", "clio-playground-web")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, &Error{Stage: stage, Code: "transport_error", SubmissionUnknown: stage == "submit"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if err != nil || len(raw) > 4*1024*1024 {
		return nil, resp.Header, &Error{Stage: stage, Status: resp.StatusCode, Code: "response_unreadable", SubmissionUnknown: stage == "submit"}
	}
	var data map[string]any
	decodeErr := json.Unmarshal(raw, &data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, &Error{Stage: stage, Status: resp.StatusCode, Code: responseCode(resp.StatusCode, data), SubmissionUnknown: stage == "submit" && resp.StatusCode >= 500}
	}
	if decodeErr != nil || data == nil {
		return nil, resp.Header, &Error{Stage: stage, Status: resp.StatusCode, Code: "invalid_response", SubmissionUnknown: stage == "submit"}
	}
	return data, resp.Header, nil
}
func responseCode(status int, data map[string]any) string {
	code, _ := data["error_code"].(string)
	if code == "" {
		code, _ = data["code"].(string)
	}
	switch code {
	case "access_error", "rate_limit_exceeded", "insufficient_credits", "content_policy_violation", "authentication_error", "invalid_request":
		return code
	}
	switch status {
	case 401:
		return "unauthorized"
	case 403:
		return "access_denied"
	case 429:
		return "rate_limited"
	case 400, 422:
		return "request_rejected"
	}
	if status >= 500 {
		return "upstream_error"
	}
	return "http_error"
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string            { s, _ := v.(string); return s }
func number(v any) *float64 {
	n, ok := v.(float64)
	if !ok {
		return nil
	}
	return &n
}
func (c *Client) Balance(ctx context.Context, a Account) (Quota, error) {
	d, _, err := c.request(ctx, "balance", http.MethodGet, "https://firefly.adobe.io/v1/credits/balance", a, nil, map[string]string{"x-account-id": a.AccountID})
	if err != nil {
		return Quota{Status: "error", Error: SafeError(err)}, err
	}
	total := object(d["total"])
	q := object(total["quota"])
	out := Quota{Status: "unknown", Total: number(q["total"]), Used: number(q["used"]), Available: number(q["available"]), AvailableUntil: str(total["availableUntil"]), CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if out.Available != nil && *out.Available >= 0 {
		out.Status = "known"
		out.LastSuccessAt = out.CheckedAt
	}
	return out, nil
}
func (c *Client) Refresh(ctx context.Context, a Account) (Account, error) {
	if a.RefreshCookie == "" {
		return Account{}, &Error{Stage: "refresh", Code: "scoped_cookie_missing"}
	}
	body := url.Values{"client_id": {"clio-playground-web"}, "guest_allowed": {"false"}, "scope": {refreshScope}}.Encode()
	d, h, err := c.request(ctx, "refresh", http.MethodPost, refreshURL, a, []byte(body), map[string]string{"Cookie": a.RefreshCookie, "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8"})
	if err != nil {
		return Account{}, err
	}
	token := str(d["access_token"])
	if token == "" {
		token = str(d["accessToken"])
	}
	id, expires, err := TokenIdentity(token)
	if err != nil {
		return Account{}, &Error{Stage: "refresh", Code: "token_missing_or_invalid"}
	}
	if id != a.AccountID {
		return Account{}, ErrIdentity
	}
	if expires <= time.Now().Unix() {
		return Account{}, &Error{Stage: "refresh", Code: "expired_token"}
	}
	a.Token = token
	a.ExpiresAt = expires
	resp := http.Response{Header: h}
	cookies := map[string]string{}
	for _, p := range strings.Split(a.RefreshCookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 {
			cookies[kv[0]] = kv[1]
		}
	}
	for _, cookie := range resp.Cookies() {
		if cookie.MaxAge < 0 {
			delete(cookies, cookie.Name)
		} else {
			cookies[cookie.Name] = cookie.Value
		}
	}
	parts := []string{}
	for k, v := range cookies {
		parts = append(parts, k+"="+v)
	}
	a.RefreshCookie = strings.Join(parts, "; ")
	return a, nil
}
func (c *Client) SubmitImage(ctx context.Context, a Account, r ImageRequest) (Job, error) {
	m, ok := FindModel(r.Model)
	if !ok || m.Kind != "image" || strings.TrimSpace(r.Prompt) == "" || r.N != 1 || fmt.Sprintf("%dx%d", r.Width, r.Height) != m.Size {
		return Job{}, &Error{Stage: "submit", Code: "unsupported_free_parameters"}
	}
	payload := map[string]any{"modelId": m.ModelID, "modelVersion": m.ModelVersion, "n": 1, "prompt": r.Prompt, "size": map[string]int{"width": r.Width, "height": r.Height}, "seeds": []int{seed()}, "output": map[string]bool{"storeInputs": true}, "generationMetadata": map[string]string{"module": "text2image", "submodule": "ff-image-generate"}, "referenceBlobs": []any{}}
	if m.ModelID == "gemini-flash" {
		payload["groundSearch"] = false
		payload["modelSpecificPayload"] = map[string]any{"parameters": map[string]bool{"addWatermark": false}, "aspectRatio": "1:1"}
	} else {
		payload["generationSettings"] = map[string]int{"detailLevel": 3}
	}
	return c.submit(ctx, a, m, r.Prompt, payload)
}
func (c *Client) SubmitVideo(ctx context.Context, a Account, r VideoRequest) (Job, error) {
	m, ok := FindModel(r.Model)
	if !ok || m.Kind != "video" || strings.TrimSpace(r.Prompt) == "" || fmt.Sprintf("%dx%d", r.Width, r.Height) != m.Size || r.Duration != m.Duration || r.Resolution != m.Resolution || r.AspectRatio != m.AspectRatio || r.GenerateAudio {
		return Job{}, &Error{Stage: "submit", Code: "unsupported_free_parameters"}
	}
	payload := map[string]any{"modelId": m.ModelID, "modelVersion": m.ModelVersion, "n": 1, "prompt": r.Prompt, "seeds": []int{seed()}, "size": map[string]int{"width": r.Width, "height": r.Height}, "resolution": r.Resolution, "duration": r.Duration, "generateAudio": false, "referenceBlobs": []any{}, "generationSettings": map[string]string{"aspectRatio": r.AspectRatio}, "generationMetadata": map[string]string{"module": "text2video", "submodule": "ff-video-generate"}, "output": map[string]bool{"storeInputs": true}}
	return c.submit(ctx, a, m, r.Prompt, payload)
}
func seed() int {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return 1 + (int(b[0])<<16|int(b[1])<<8|int(b[2]))%999999
}
func (c *Client) submit(ctx context.Context, a Account, m Model, prompt string, payload map[string]any) (Job, error) {
	j := Job{Kind: m.Kind, Model: m.ID, ModelID: m.ModelID, ModelVersion: m.ModelVersion, RequestID: requestID(), Status: "SUBMISSION_STARTED"}
	id, _, err := TokenIdentity(a.Token)
	if err != nil || id != a.AccountID {
		return j, ErrIdentity
	}
	sherlock, err := Sherlock(a.Cookie)
	if err != nil {
		return j, &Error{Stage: "submit", Code: "session_incomplete"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return j, err
	}
	d, h, err := c.request(ctx, "submit", http.MethodPost, "https://firefly-3p.ff.adobe.io/v2/3p-"+m.Kind+"s/generate-async", a, raw, map[string]string{"x-nonce": Nonce(a.AccountID, prompt), "x-arp-session-id": sherlock, "x-request-id": j.RequestID})
	if err != nil {
		j.Status = "SUBMISSION_UNKNOWN"
		if e, ok := err.(*Error); ok && !e.SubmissionUnknown {
			j.Status = "REJECTED"
		}
		return j, err
	}
	j.PollURL = h.Get("x-override-status-link")
	if j.PollURL == "" {
		result := object(d["links"])["result"]
		j.PollURL = str(result)
		if j.PollURL == "" {
			j.PollURL = str(object(result)["href"])
		}
	}
	if !trustedURL(j.PollURL, "poll") {
		j.PollURL = ""
		j.Status = "SUBMISSION_UNKNOWN"
		return j, &Error{Stage: "submit", Code: "missing_or_untrusted_poll_link", SubmissionUnknown: true}
	}
	j.Status = "SUBMITTED"
	return j, nil
}
func (c *Client) Poll(ctx context.Context, a Account, j Job) (Job, error) {
	if j.Kind != "image" && j.Kind != "video" {
		return j, &Error{Stage: "poll", Code: "invalid_kind"}
	}
	d, h, err := c.request(ctx, "poll", http.MethodGet, j.PollURL, a, nil, nil)
	if err != nil {
		return j, err
	}
	status := strings.ToUpper(str(d["status"]))
	if status == "" {
		status = strings.ToUpper(h.Get("x-task-status"))
	}
	if p := number(d["progress"]); p != nil {
		j.Progress = *p
	}
	switch status {
	case "FAILED", "ERROR", "CANCELLED", "CANCELED":
		j.Status = status
		j.OutputURLs = nil
		return j, nil
	case "COMPLETED", "SUCCEEDED":
		urls := []string{}
		outputs, _ := d["outputs"].([]any)
		for _, raw := range outputs {
			o := object(raw)
			media := object(o[j.Kind])
			u := str(media["presignedUrl"])
			if u == "" {
				u = str(media["url"])
			}
			if u == "" {
				u = str(o["presignedUrl"])
			}
			if !trustedURL(u, "download") {
				return j, &Error{Stage: "poll", Code: "untrusted_output_link"}
			}
			urls = append(urls, u)
		}
		if len(urls) != 1 {
			return j, &Error{Stage: "poll", Code: "missing_final_output"}
		}
		j.Status = "COMPLETED"
		j.OutputURLs = urls
		return j, nil
	default:
		j.Status = "IN_PROGRESS"
		j.OutputURLs = nil
		return j, nil
	}
}
func (c *Client) Download(ctx context.Context, endpoint string, maxBytes int64) ([]byte, string, error) {
	if !trustedURL(endpoint, "download") {
		return nil, "", &Error{Stage: "download", Code: "unexpected_host"}
	}
	if maxBytes <= 0 || maxBytes > 150*1024*1024 {
		maxBytes = 150 * 1024 * 1024
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", &Error{Stage: "download", Code: "invalid_request"}
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", &Error{Stage: "download", Code: "transport_error"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", &Error{Stage: "download", Code: "http_error", Status: resp.StatusCode}
	}
	if resp.ContentLength > maxBytes {
		return nil, "", &Error{Stage: "download", Code: "too_large"}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", &Error{Stage: "download", Code: "body_incomplete"}
	}
	if int64(len(b)) > maxBytes {
		return nil, "", &Error{Stage: "download", Code: "too_large"}
	}
	if len(b) == 0 {
		return nil, "", &Error{Stage: "download", Code: "body_empty"}
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// ParseSize converts the allowlisted dimension pair for the gateway adapter.
func ParseSize(v string) (int, int) {
	p := strings.Split(v, "x")
	if len(p) != 2 {
		return 0, 0
	}
	w, _ := strconv.Atoi(p[0])
	h, _ := strconv.Atoi(p[1])
	return w, h
}
