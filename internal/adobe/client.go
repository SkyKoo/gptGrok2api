package adobe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	case "preflight":
		api := u.Query().Get("api")
		return u.Hostname() == "firefly-3p.ff.adobe.io" && u.Path == "/v2/models/generation-preflight" && len(u.Query()) == 1 && len(u.Query()["api"]) == 1 && (api == "/v2/3p-images/generate-async" || api == "/v2/3p-videos/generate-async")
	case "upload":
		return u.Hostname() == "firefly-3p.ff.adobe.io" && u.Path == "/v2/storage/image" && u.RawQuery == ""
	case "entitlements":
		return u.Hostname() == "bks.adobe.io" && u.Path == "/v1/feature-auth-profile" && u.RawQuery == "include-feature-cost=true&include-effective-quota-balance=true"
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
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, &Error{Stage: stage, Code: "transport_error", SubmissionUnknown: stage == "submit", Diagnostic: requestDiagnostic(resp, started, "transport_error", "transport", nil)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if err != nil || len(raw) > 4*1024*1024 {
		return nil, resp.Header, &Error{Stage: stage, Status: resp.StatusCode, Code: "response_unreadable", SubmissionUnknown: stage == "submit", Diagnostic: requestDiagnostic(resp, started, "response_unreadable", "response_read", nil)}
	}
	var data map[string]any
	decodeErr := json.Unmarshal(raw, &data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := responseCode(resp.StatusCode, data)
		return nil, resp.Header, &Error{Stage: stage, Status: resp.StatusCode, Code: code, SubmissionUnknown: stage == "submit" && (resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout), Diagnostic: requestDiagnostic(resp, started, code, "http_status", data)}
	}
	if decodeErr != nil || data == nil {
		return nil, resp.Header, &Error{Stage: stage, Status: resp.StatusCode, Code: "invalid_response", SubmissionUnknown: stage == "submit", Diagnostic: requestDiagnostic(resp, started, "invalid_response", "response_decode", nil)}
	}
	return data, resp.Header, nil
}

func knownResponseCode(code string) bool {
	switch code {
	case "access_error", "rate_limit_exceeded", "insufficient_credits", "content_policy_violation", "authentication_error", "invalid_request", "timeout_error", "video_unsafe":
		return true
	}
	return false
}

func requestDiagnostic(resp *http.Response, started time.Time, code, source string, data map[string]any) *SubmissionDiagnostic {
	d := &SubmissionDiagnostic{ErrorCode: code, ErrorSource: source, ElapsedMS: time.Since(started).Milliseconds()}
	if resp == nil {
		return d
	}
	d.ResponseReceived, d.HTTPStatus = true, resp.StatusCode
	if resp.ProtoMajor == 2 {
		d.HTTPProto = "2"
	} else if resp.ProtoMajor == 1 && resp.ProtoMinor == 1 {
		d.HTTPProto = "1.1"
	}
	// Only the exact category observed from Adobe is admitted. In particular,
	// never persist arbitrary header values or x-access-user-context.
	if access := resp.Header.Get("x-access-error"); access != "" {
		d.AccessError = "unknown"
		if access == "taste_exhausted" {
			d.AccessError = access
		}
	}
	if candidate, source := responseErrorCode(data); candidate != "" {
		d.ErrorCode, d.ErrorSource = candidate, source
	}
	return d
}

func responseErrorCode(data map[string]any) (string, string) {
	for _, candidate := range []struct{ code, source string }{
		{str(data["error_code"]), "top_error_code"},
		{str(data["code"]), "top_code"},
		{str(object(data["error"])["error_code"]), "nested_error_code"},
		{str(object(data["error"])["code"]), "nested_code"},
	} {
		if knownResponseCode(candidate.code) {
			return candidate.code, candidate.source
		}
	}
	return "", ""
}

func responseCode(status int, data map[string]any) string {
	if code, _ := responseErrorCode(data); code != "" {
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
	m, v, err := ValidateImageRequest(r)
	if err != nil {
		return Job{}, err
	}
	if _, err = generationSession(a); err != nil {
		return Job{}, err
	}
	refs, err := c.uploadReferences(ctx, a, m, r.References)
	if err != nil {
		return Job{Kind: m.Kind, Model: m.ID, Status: "REJECTED"}, err
	}
	payload := imagePayload(m, v, r, refs)
	j, err := c.submit(ctx, a, m, r.Prompt, payload)
	j.Width, j.Height, j.AspectRatio, j.Resolution, j.ReferenceCount = r.Width, r.Height, v.AspectRatio, v.Resolution, len(refs)
	return j, err
}
func (c *Client) SubmitVideo(ctx context.Context, a Account, r VideoRequest) (Job, error) {
	m, v, err := ValidateVideoRequest(r)
	if err != nil {
		return Job{}, err
	}
	if _, err = generationSession(a); err != nil {
		return Job{}, err
	}
	inputs, err := fitVideoReferences(r.References, r.Width, r.Height)
	if err != nil {
		return Job{Kind: m.Kind, Model: m.ID, Status: "REJECTED"}, err
	}
	if err = validateReferences(m, inputs); err != nil {
		return Job{Kind: m.Kind, Model: m.ID, Status: "REJECTED"}, err
	}
	refs, err := c.uploadReferences(ctx, a, m, inputs)
	if err != nil {
		return Job{Kind: m.Kind, Model: m.ID, Status: "REJECTED"}, err
	}
	payload := videoPayload(m, v, r, refs)
	j, err := c.submit(ctx, a, m, r.Prompt, payload)
	j.Width, j.Height, j.AspectRatio, j.Resolution, j.Duration, j.GenerateAudio, j.ReferenceCount = r.Width, r.Height, v.AspectRatio, v.Resolution, r.Duration, r.GenerateAudio, len(refs)
	return j, err
}
func imagePayload(m Model, v Variant, r ImageRequest, refs []referenceBlob) map[string]any {
	payload := map[string]any{"modelId": m.ModelID, "modelVersion": m.ModelVersion, "n": 1, "prompt": r.Prompt, "size": map[string]int{"width": r.Width, "height": r.Height}, "resolution": v.Resolution, "seeds": []int{seed()}, "output": map[string]bool{"storeInputs": true}, "generationMetadata": map[string]string{"module": "text2image", "submodule": "ff-image-generate"}, "referenceBlobs": refs}
	switch m.ModelID {
	case "gemini-flash":
		if m.ModelVersion == "nano-banana-2026-09" {
			delete(payload, "size")
			payload["groundSearch"] = false
			payload["modelSpecificPayload"] = map[string]any{}
			payload["generationSettings"] = map[string]string{"aspectRatio": v.AspectRatio}
		} else {
			payload["modelSpecificPayload"] = map[string]any{"aspectRatio": v.AspectRatio}
		}
	case "gpt-image":
		delete(payload, "seeds")
		delete(payload, "resolution")
		payload["generationSettings"] = map[string]int{"detailLevel": 3}
	case "humain":
		delete(payload, "n")
		delete(payload, "size")
		payload["generationSettings"] = map[string]string{"aspectRatio": v.AspectRatio}
	default:
		payload["generationSettings"] = map[string]string{"aspectRatio": v.AspectRatio}
	}
	return payload
}
func videoPayload(m Model, v Variant, r VideoRequest, refs []referenceBlob) map[string]any {
	payload := map[string]any{"modelId": m.ModelID, "modelVersion": m.ModelVersion, "n": 1, "prompt": r.Prompt, "seeds": []int{seed()}, "size": map[string]int{"width": r.Width, "height": r.Height}, "resolution": v.Resolution, "duration": r.Duration, "generateAudio": r.GenerateAudio, "generationSettings": map[string]string{"aspectRatio": v.AspectRatio}, "generationMetadata": map[string]string{"module": "text2video", "submodule": "ff-video-generate"}, "output": map[string]bool{"storeInputs": true}}
	// The official UGS video mapper omits this optional field for text-only
	// requests. Keep uploaded frame handles only when references are present.
	if len(refs) > 0 {
		payload["referenceBlobs"] = refs
	}
	switch m.ModelID {
	case "firefly-video":
		for _, key := range []string{"duration", "resolution", "generateAudio", "generationSettings"} {
			delete(payload, key)
		}
	case "gemini-omni":
		delete(payload, "seeds")
		delete(payload, "size")
	case "luma":
		delete(payload, "seeds")
		delete(payload, "generateAudio")
		payload["modelSpecificPayload"] = map[string]string{"resolution": v.Resolution, "aspect_ratio": v.AspectRatio}
	}
	return payload
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
	sherlock, err := generationSession(a)
	if err != nil {
		return j, &Error{Stage: "submit", Code: "session_incomplete"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return j, err
	}
	headers := map[string]string{"x-nonce": Nonce(a.AccountID, prompt), "x-request-id": j.RequestID}
	if sherlock != "" {
		headers["x-arp-session-id"] = sherlock
	}
	d, h, err := c.request(ctx, "submit", http.MethodPost, "https://firefly-3p.ff.adobe.io/v2/3p-"+m.Kind+"s/generate-async", a, raw, headers)
	if err != nil {
		j.Status = "SUBMISSION_UNKNOWN"
		if e, ok := err.(*Error); ok {
			j.SubmissionDiagnostic = e.Diagnostic
			if !e.SubmissionUnknown {
				j.Status = "REJECTED"
			}
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
		var upstream *Error
		// This exact terminal response was observed on an existing Adobe job.
		// A bare 451, transport error or another code does not prove completion.
		if j.Kind == "video" && errors.As(err, &upstream) && upstream.Status == http.StatusUnavailableForLegalReasons && upstream.Code == "video_unsafe" {
			j.Status, j.OutputURLs, j.PollDiagnostic = "FAILED", nil, upstream.Diagnostic
			return j, nil
		}
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
