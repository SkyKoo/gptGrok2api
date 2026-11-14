package register

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
)

type AdobeRegistrationFlow interface {
	Register(context.Context, string, Mailbox, AdobeConfig, func(context.Context, time.Time) (string, error), Progress) (map[string]any, error)
	Close()
}
type AdobeFactory func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error)
type AdobeBrowserFlow struct {
	Client            *http.Client
	ResolveMediaProxy func(context.Context, string, string) (string, error)
	lease             *browserqueue.Lease
}

func (f *AdobeBrowserFlow) Close() {
	if f.lease != nil {
		f.lease.Release()
		f.lease = nil
	}
}

// Support the observed iCloud 14+8 opaque suffix as well as the legacy suffix.
// Arbitrary underscore segments can encode a domain after adobe.com; do not
// accept those as opaque data or trust the message's display name instead.
var adobeHMESender = regexp.MustCompile(`^(?:account[-_]?)?noreply_at_(?:[a-z0-9]+_)*adobe_com_(?:[a-z0-9-]+|[a-z0-9]{14}_[a-z0-9]{8})$`)

// Adobe messages may also contain unrelated six-digit footer numbers. Select
// an explicitly labelled code; conflicting labelled codes remain ambiguous.
var adobeLabeledCode = regexp.MustCompile(`(?i)\b(?:verification|security) code(?: is)?[\s:：]*([0-9]{6})(?:$|[^0-9])`)

func adobeMailCode(text string) string {
	matches := adobeLabeledCode.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return mailCode(text)
	}
	code := matches[0][1]
	for _, match := range matches[1:] {
		if match[1] != code {
			return ""
		}
	}
	return code
}

func isAdobeSender(sender string) bool {
	addr, err := mail.ParseAddress(sender)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.ToLower(addr.Address), "@")
	if len(parts) != 2 {
		return false
	}
	return parts[1] == "adobe.com" || strings.HasSuffix(parts[1], ".adobe.com") ||
		(parts[1] == "icloud.com" && adobeHMESender.MatchString(parts[0]))
}
func (f *AdobeBrowserFlow) Register(ctx context.Context, id string, box Mailbox, cfg AdobeConfig, waitCode func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	if cfg.mediaOnly {
		return f.prepareMedia(ctx, box, cfg, progress)
	}
	action, mode, stage := "run", "registration", "adobe_flow_started"
	switch cfg.RecoveryMode {
	case "", "retry_registration":
	case "resume_session":
		if cfg.BrowserProfileID == "" {
			return nil, fail("adobe", "recovery_profile_missing")
		}
		action, mode, stage = "resume", "resume", "adobe_recovery_started"
	default:
		return nil, fail("adobe", "recovery_mode_invalid")
	}
	profileID := firstNonEmpty(cfg.BrowserProfileID, id)
	profile := cloneMap(cfg.Profile)
	prefix := strings.Split(box.Email, "@")[0]
	names := strings.FieldsFunc(prefix, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	profile["firstName"], profile["lastName"] = "Adobe", "User"
	if len(names) > 0 {
		profile["firstName"] = names[0]
	}
	if len(names) > 1 {
		profile["lastName"] = names[1]
	}
	var err error
	// Every registration attempt owns new storage; only an accepted creation
	// continues the original profile and verification session.
	options := map[string]any{"provider": "adobe", "profile_id": profileID, "email": box.Email, "proxy": cfg.Proxy, "adobe_profile": profile, "adobe_mode": mode, "adobe_recovery_id": id, "adobe_require_new": mode == "registration"}
	if f.ResolveMediaProxy != nil {
		options["adobe_defer_media"] = true
	}
	f.lease, err = browserqueue.AcquireWithStartupProgress(ctx, f.Client, cfg.WorkerURL, cfg.WorkerKey, options, progress, cfg.startupProgress)
	if err != nil {
		var startup *browserqueue.StartupError
		if errors.As(err, &startup) {
			return nil, fail("adobe", startup.Code)
		}
		return nil, fail("browser", "adobe_browser_unavailable")
	}
	if cfg.countryProgress != nil {
		prepared, err := f.command(ctx, cfg, "prepare", "")
		if err != nil || prepared.Stage != "prepared" || !prepared.Country.valid() {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fail("adobe", "registration_country_preparation_failed")
		}
		if err := cfg.countryProgress(prepared.Country); err != nil {
			return nil, err
		}
	}
	before := time.Now().UTC()
	if err := progress(stage); err != nil {
		return nil, err
	}
	// The Node driver persists its own submission fence before clicking Create.
	// Go retains ownership on any loss of contact from this point onwards.
	command := func(action, code string) (adobeBrowserResult, error) {
		if cfg.creationProgress != nil {
			// Persist uncertainty before sending, including after a prior explicit
			// not_submitted response. A crash cannot leave false retry evidence.
			if err := cfg.creationProgress(adobeCreationObservation{Outcome: "unknown"}, false); err != nil {
				return adobeBrowserResult{}, err
			}
		}
		result, err := f.command(ctx, cfg, action, code)
		if err == nil {
			if result.CreationOutcome == "not_submitted" && (result.SubmissionStarted || result.Diagnostics.SubmissionStarted) {
				result.CreationOutcome = "unknown"
			}
			if result.Stage != "completed" && (result.Stage == "awaiting_verification" || result.Code == "verification_required") && result.CreationOutcome != "accepted" {
				result.CreationOutcome = "unknown"
			}
			if cfg.creationProgress != nil {
				err = cfg.creationProgress(adobeCreationObservation{Outcome: result.CreationOutcome, ErrorCode: result.CreationErrorCode, HTTPStatus: result.CreationHTTPStatus}, action == "run" && mode == "registration")
			}
		}
		return result, err
	}
	result, err := command(action, "")
	if err != nil {
		return nil, err
	}
	if result.Stage != "completed" && (result.Code == "verification_required" || result.Stage == "awaiting_verification") {
		if result.CreationOutcome != "accepted" {
			return nil, fail("adobe", "creation_result_unknown")
		}
		if stage := adobeSubmissionProgressStage(result, cfg.RecoveryMode); stage != "" {
			if err := progress(stage); err != nil {
				return nil, err
			}
		}
		if err := progress("awaiting_verification"); err != nil {
			return nil, err
		}
		after, err := AdobeVerificationMailAfter(before, result.VerificationRequestedAt)
		if err != nil {
			return nil, err
		}
		code, err := waitCode(ctx, after)
		if err != nil {
			return nil, err
		}
		result, err = command("verify", code)
		if err != nil {
			return nil, err
		}
	}
	if result.Stage == "media_pending" {
		if f.ResolveMediaProxy == nil || cfg.mediaProgress == nil || result.CreationOutcome != "accepted" || result.Code != "" || len(result.Account) != 0 {
			return nil, fail("adobe", "media_handoff_invalid")
		}
		// Commit the checkpoint acknowledgement before releasing the original
		// browser. Restart recovery must never depend on its registration proxy.
		if err := cfg.mediaProgress(); err != nil {
			return nil, err
		}
		if err := f.releaseForMedia(ctx, cfg); err != nil {
			return nil, err
		}
		cfg.BrowserProfileID = profileID
		return f.prepareMedia(ctx, box, cfg, progress)
	}
	if result.Stage != "completed" {
		if cfg.diagnosticProgress != nil {
			if err := cfg.diagnosticProgress(result.Diagnostics); err != nil {
				return nil, err
			}
		}
		if err := adobeVerificationProgress(result, cfg.RecoveryMode, progress); err != nil {
			return nil, err
		}
		return nil, fail("adobe", firstNonEmpty(result.Code, "registration_needs_review"))
	}
	if stringValue(result.Account["token"]) == "" || !strings.EqualFold(stringValue(result.Account["email"]), box.Email) || stringValue(result.Account["account_id"]) == "" {
		return nil, fail("adobe", "identity_not_verified")
	}
	return result.Account, nil
}

func (f *AdobeBrowserFlow) prepareMedia(ctx context.Context, box Mailbox, cfg AdobeConfig, progress Progress) (map[string]any, error) {
	if f.ResolveMediaProxy == nil || cfg.BrowserProfileID == "" || strings.TrimSpace(box.Email) == "" {
		return nil, fail("adobe", "media_handoff_invalid")
	}
	proxy, err := f.ResolveMediaProxy(ctx, box.Email, cfg.BrowserProfileID)
	if err != nil {
		return nil, fail("adobe", "media_proxy_unavailable")
	}
	if proxy == "" {
		proxy = "direct" // The resolver explicitly selected the configured direct route.
	}
	options := map[string]any{"provider": "adobe", "profile_id": cfg.BrowserProfileID, "email": box.Email,
		"proxy": proxy, "adobe_mode": "media"}
	if cfg.legacyMedia {
		options["adobe_legacy_media"] = true
	}
	if progress != nil {
		if err := progress("adobe_media_pending"); err != nil {
			return nil, err
		}
	}
	f.lease, err = browserqueue.AcquireWithStartupProgress(ctx, f.Client, cfg.WorkerURL, cfg.WorkerKey, options, progress, cfg.startupProgress)
	if err != nil {
		return nil, fail("adobe", "media_browser_unavailable")
	}
	if progress != nil {
		if err := progress("adobe_media_preparing"); err != nil {
			return nil, err
		}
	}
	result, err := f.command(ctx, cfg, "media", "")
	if err != nil {
		return nil, err
	}
	if result.Stage != "completed" {
		if cfg.diagnosticProgress != nil {
			if err := cfg.diagnosticProgress(result.Diagnostics); err != nil {
				return nil, err
			}
		}
		return nil, fail("adobe", firstNonEmpty(result.Code, "media_session_incomplete"))
	}
	if result.CreationOutcome != "accepted" || stringValue(result.Account["token"]) == "" ||
		!strings.EqualFold(strings.TrimSpace(stringValue(result.Account["email"])), strings.TrimSpace(box.Email)) ||
		stringValue(result.Account["account_id"]) == "" || stringValue(result.Account["profile_id"]) != cfg.BrowserProfileID {
		return nil, fail("adobe", "identity_not_verified")
	}
	return result.Account, nil
}

// A DELETE acknowledgement of "closing" still owns the profile and licence.
// Only confirmed removal permits the second reservation, regardless of capacity.
func (f *AdobeBrowserFlow) releaseForMedia(ctx context.Context, cfg AdobeConfig) error {
	if f.lease == nil {
		return fail("adobe", "media_previous_browser_closing")
	}
	closing, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	dedicated := *client
	dedicated.Timeout = 12 * time.Second
	dedicated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dedicated.Jar = nil
	method := http.MethodDelete
	for {
		req, err := http.NewRequestWithContext(closing, method, cfg.WorkerURL+"/v1/browser/reservations/"+f.lease.ID, nil)
		if err != nil {
			return fail("adobe", "media_previous_browser_closing")
		}
		if cfg.WorkerKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.WorkerKey)
		}
		resp, err := dedicated.Do(req)
		if err != nil {
			return fail("adobe", "media_previous_browser_closing")
		}
		var result struct {
			Status string `json:"status"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&result)
		resp.Body.Close()
		if method == http.MethodGet && resp.StatusCode == http.StatusNotFound ||
			resp.StatusCode == http.StatusOK && decodeErr == nil && result.Status == "released" {
			f.lease = nil
			return nil
		}
		if resp.StatusCode != http.StatusOK || decodeErr != nil || result.Status != "closing" {
			return fail("adobe", "media_previous_browser_closing")
		}
		method = http.MethodGet
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-closing.Done():
			timer.Stop()
			return fail("adobe", "media_previous_browser_closing")
		case <-timer.C:
		}
	}
}

type adobeBrowserResult struct {
	Country                 adobeCountrySelection        `json:"registration_country"`
	CreationOutcome         string                       `json:"creation_outcome"`
	CreationErrorCode       string                       `json:"creation_error_code"`
	CreationHTTPStatus      int                          `json:"creation_http_status"`
	VerificationRequestedAt string                       `json:"verification_requested_at"`
	SubmissionStarted       bool                         `json:"submission_started"`
	Diagnostics             adobeRegistrationDiagnostics `json:"diagnostics"`
	Stage                   string                       `json:"stage"`
	Code                    string                       `json:"code"`
	Account                 map[string]any               `json:"account"`
}

func (f *AdobeBrowserFlow) command(ctx context.Context, cfg AdobeConfig, action, code string) (adobeBrowserResult, error) {
	raw, _ := json.Marshal(map[string]any{"browser_lease": f.lease.ID, "action": action, "code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WorkerURL+"/v1/adobe/run", bytes.NewReader(raw))
	if err != nil {
		return adobeBrowserResult{}, fail("adobe", "worker_request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.WorkerKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.WorkerKey)
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 160 * time.Second}
	}
	dedicated := *client
	requestTimeout := time.Duration(0)
	// A registration command has one 300-second Worker budget. Preserve older
	// saved settings, but leave time for the Worker to return its final result.
	if action == "run" || action == "resume" || action == "verify" {
		requestTimeout = cfg.RequestTimeout
		if requestTimeout < 320*time.Second {
			requestTimeout = 320 * time.Second
		}
	}
	if action == "prepare" || action == "refresh" || action == "media" {
		requestTimeout = 150 * time.Second
	}
	if requestTimeout > 0 {
		// Set both the client timer and request context deadline. The latter is
		// observable by transports and ensures a short saved client timeout cannot
		// silently win over the registration command budget.
		dedicated.Timeout = requestTimeout
		requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		req = req.WithContext(requestCtx)
	}
	dedicated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dedicated.Jar = nil
	resp, err := dedicated.Do(req)
	if err != nil {
		if action == "media" {
			return adobeBrowserResult{}, fail("adobe", "media_result_unknown")
		}
		return adobeBrowserResult{}, fail("adobe", "submission_result_unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if action == "media" {
			return adobeBrowserResult{}, fail("adobe", "media_worker_result_unknown")
		}
		return adobeBrowserResult{}, fail("adobe", "worker_result_unknown")
	}
	var result adobeBrowserResult
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result) != nil {
		if action == "media" {
			return result, fail("adobe", "media_worker_result_unknown")
		}
		return result, fail("adobe", "worker_result_unknown")
	}
	return result, nil
}
