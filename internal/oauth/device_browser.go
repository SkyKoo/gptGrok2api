package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BrowserDeviceRequest is sent only over the private Worker connection, never
// persisted in queue payloads or exposed by management job endpoints.
type BrowserDeviceRequest struct {
	BrowserLease    string `json:"browser_lease,omitempty"`
	VerificationURL string `json:"verification_uri_complete"`
	UserCode        string `json:"user_code"`
	SSO             string `json:"sso"`
	SSORW           string `json:"sso_rw,omitempty"`
	Email           string `json:"expected_email"`
	Proxy           string `json:"proxy"`
	Timeout         int    `json:"timeout_s"`
}

type BrowserDeviceResult struct {
	Status            string `json:"status"`
	Code              string `json:"code"`
	Stage             string `json:"stage,omitempty"`
	Detail            string `json:"detail,omitempty"`
	ElapsedMS         int64  `json:"elapsed_ms,omitempty"`
	ConsentSubmitted  bool   `json:"consent_submitted,omitempty"`
	PagePath          string `json:"page_path,omitempty"`
	CodeFormSeen      bool   `json:"code_form_seen,omitempty"`
	ConsentPageSeen   bool   `json:"consent_page_seen,omitempty"`
	ConsentButtonSeen bool   `json:"consent_button_seen,omitempty"`
	NetworkFailures   int    `json:"network_failures,omitempty"`
	NetworkErrorCodes string `json:"network_error_codes,omitempty"`
}

func ConfirmDeviceInBrowser(ctx context.Context, client *http.Client, base, key string, payload BrowserDeviceRequest) (BrowserDeviceResult, error) {
	empty := BrowserDeviceResult{}
	u, err := url.Parse(payload.VerificationURL)
	if err != nil || u.Scheme != "https" || u.Host != "accounts.x.ai" || u.User != nil || u.Path != "/oauth2/device" || u.Fragment != "" || u.Query().Get("user_code") != payload.UserCode || len(u.Query()) != 1 || len(u.Query()["user_code"]) != 1 || payload.UserCode == "" {
		return empty, errors.New("oauth_verification_url_invalid")
	}
	if payload.SSO == "" || payload.Email == "" {
		return empty, errors.New("oauth_session_required")
	}
	workerURL, err := url.Parse(strings.TrimRight(base, "/") + "/v1/oauth/authorize-device")
	if err != nil || workerURL.Host == "" || workerURL.User != nil || (workerURL.Scheme != "http" && workerURL.Scheme != "https") || workerURL.RawQuery != "" || workerURL.Fragment != "" {
		return empty, errors.New("oauth_worker_config_invalid")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return empty, errors.New("oauth_worker_request_invalid")
	}
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, ""), http.MethodPost, workerURL.String(), bytes.NewReader(raw))
	if err != nil {
		return empty, errors.New("oauth_worker_request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if client == nil {
		client = http.DefaultClient
	}
	dedicated := *client
	dedicated.Timeout = 3 * time.Minute
	dedicated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := dedicated.Do(req)
	if err != nil {
		return empty, errors.New("oauth_worker_result_unknown")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return empty, errors.New("oauth_worker_rejected")
	}
	var result BrowserDeviceResult
	if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result) != nil {
		return empty, errors.New("oauth_worker_result_unknown")
	}
	switch result.Status {
	case "approved", "manual_action_required", "failed", "uncertain":
		return result, nil
	default:
		return empty, errors.New("oauth_worker_result_unknown")
	}
}
