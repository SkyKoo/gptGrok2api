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
	Client *http.Client
	lease  *browserqueue.Lease
}

func (f *AdobeBrowserFlow) Close() {
	if f.lease != nil {
		f.lease.Release()
	}
}

// HME preserves some original senders in its iCloud forwarding address. Keep
// the encoded Adobe domain anchored before the opaque suffix, not a substring.
var adobeHMESender = regexp.MustCompile(`^(?:account[-_]?)?noreply_at_(?:[a-z0-9]+_)*adobe_com_[a-z0-9-]+$`)

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
	f.lease, err = browserqueue.AcquireWithStartupProgress(ctx, f.Client, cfg.WorkerURL, cfg.WorkerKey, map[string]any{"provider": "adobe", "profile_id": profileID, "email": box.Email, "proxy": cfg.Proxy, "adobe_profile": profile, "adobe_mode": mode, "adobe_recovery_id": id, "adobe_require_new": mode == "registration"}, progress, cfg.startupProgress)
	if err != nil {
		var startup *browserqueue.StartupError
		if errors.As(err, &startup) {
			return nil, fail("adobe", startup.Code)
		}
		return nil, fail("browser", "adobe_browser_unavailable")
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

type adobeBrowserResult struct {
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
	dedicated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dedicated.Jar = nil
	resp, err := dedicated.Do(req)
	if err != nil {
		return adobeBrowserResult{}, fail("adobe", "submission_result_unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return adobeBrowserResult{}, fail("adobe", "worker_result_unknown")
	}
	var result adobeBrowserResult
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result) != nil {
		return result, fail("adobe", "worker_result_unknown")
	}
	return result, nil
}
