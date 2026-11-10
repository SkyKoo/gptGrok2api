package register

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/mail"
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
func isAdobeSender(sender string) bool {
	addr, err := mail.ParseAddress(sender)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.ToLower(addr.Address), "@")
	if len(parts) != 2 {
		return false
	}
	return parts[1] == "adobe.com" || strings.HasSuffix(parts[1], ".adobe.com")
}
func (f *AdobeBrowserFlow) Register(ctx context.Context, id string, box Mailbox, cfg AdobeConfig, waitCode func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
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
	f.lease, err = browserqueue.Acquire(ctx, f.Client, cfg.WorkerURL, cfg.WorkerKey, map[string]any{"provider": "adobe", "profile_id": id, "email": box.Email, "proxy": cfg.Proxy, "adobe_profile": profile}, progress)
	if err != nil {
		return nil, fail("browser", "adobe_browser_unavailable")
	}
	before := time.Now().UTC()
	if err := progress("adobe_flow_started"); err != nil {
		return nil, err
	}
	// The Node driver persists its own submission fence before clicking Create.
	// Go retains ownership on any loss of contact from this point onwards.
	result, err := f.command(ctx, cfg, "run", "")
	if err != nil {
		return nil, err
	}
	if result.Stage != "completed" && (result.Code == "verification_required" || result.Stage == "awaiting_verification") {
		if err := progress("awaiting_verification"); err != nil {
			return nil, err
		}
		code, err := waitCode(ctx, before)
		if err != nil {
			return nil, err
		}
		result, err = f.command(ctx, cfg, "verify", code)
		if err != nil {
			return nil, err
		}
	}
	if result.Stage != "completed" {
		return nil, fail("adobe", firstNonEmpty(result.Code, "registration_needs_review"))
	}
	if stringValue(result.Account["token"]) == "" || !strings.EqualFold(stringValue(result.Account["email"]), box.Email) || stringValue(result.Account["account_id"]) == "" {
		return nil, fail("adobe", "identity_not_verified")
	}
	return result.Account, nil
}

type adobeBrowserResult struct {
	Stage   string         `json:"stage"`
	Code    string         `json:"code"`
	Account map[string]any `json:"account"`
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
