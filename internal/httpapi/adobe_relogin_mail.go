package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

type adobeBrowserRefreshResult struct {
	Stage                   string          `json:"stage"`
	Code                    string          `json:"code"`
	VerificationRequestedAt string          `json:"verification_requested_at"`
	Account                 adobe.Account   `json:"account"`
	Diagnostics             json.RawMessage `json:"diagnostics"`
}

var adobeReloginEmailCode = regexp.MustCompile(`^[0-9]{6}$`)

func adobeReloginMailError(code string) error { return &adobe.Error{Stage: "relogin", Code: code} }

func runAdobeBrowserRefresh(ctx context.Context, client *http.Client, base, key string, payload map[string]any) (adobeBrowserRefreshResult, int, error) {
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/adobe/run", bytes.NewReader(raw))
	if err != nil {
		return adobeBrowserRefreshResult{}, 0, adobeReloginMailError("request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return adobeBrowserRefreshResult{}, 0, adobeReloginMailError("result_unknown")
	}
	defer resp.Body.Close()
	var result adobeBrowserRefreshResult
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result) != nil {
		return result, resp.StatusCode, adobeReloginMailError("browser_failed")
	}
	return result, resp.StatusCode, nil
}

// Existing-account login only reopens its recorded mailbox source. It does not
// acquire an alias, reserve a registration claim, or choose the current default
// source when the saved source is missing or ambiguous.
func (s *Server) waitAdobeReloginEmailCode(ctx context.Context, account adobe.Account, started time.Time, requestedAt string) (string, error) {
	after, err := registerruntime.AdobeVerificationMailAfter(started, requestedAt)
	if err != nil {
		return "", adobeReloginMailError("email_verification_time_invalid")
	}
	// Registration recovery can reuse an earlier send marker, whereas this is
	// a new login attempt. Do not accept a prior attempt's code just because its
	// timestamp is still inside registration's recovery window.
	earliest := started.Add(-5 * time.Second)
	if requestedAt != "" && after.Add(5*time.Second).Before(earliest) {
		return "", adobeReloginMailError("email_verification_time_invalid")
	}
	if after.Before(earliest) {
		after = earliest
	}
	box, _, ok := mailboxAccount(map[string]any{"registration_mailbox": account.RegistrationMailbox})
	box.Email, box.AccountID, box.SourceID = strings.TrimSpace(box.Email), strings.TrimSpace(box.AccountID), strings.TrimSpace(box.SourceID)
	if !ok || box.AccountID == "" || !strings.EqualFold(box.Email, strings.TrimSpace(account.Email)) || s.mailboxManager == nil {
		return "", adobeReloginMailError("email_source_unavailable")
	}
	if validateAdobeMailboxMetadata(account.RegistrationMailbox, mailboxFromRegister(box)) != nil {
		return "", adobeReloginMailError("email_source_unavailable")
	}
	waitCtx, cancel := context.WithTimeout(ctx, registerruntime.AdobeVerificationWaitTimeout)
	defer cancel()
	failure := func(code string) error {
		if waitCtx.Err() == context.DeadlineExceeded {
			return adobeReloginMailError("email_verification_timeout")
		}
		if waitCtx.Err() == context.Canceled {
			return adobeReloginMailError("cancelled")
		}
		return adobeReloginMailError(code)
	}
	source, err := s.mailboxManager.SourceForMailbox(waitCtx, mailboxFromRegister(box))
	if err != nil || !source.Enabled || !strings.EqualFold(source.Type, "icloud_hme") {
		return "", failure("email_source_unavailable")
	}
	box.SourceID = source.ID
	mail, err := newMailboxResourceSource(waitCtx, s.mailboxManager, "adobe", source.ID, registerruntime.AdobeVerificationPollInterval)
	if err != nil {
		return "", failure("email_source_unavailable")
	}
	defer mail.Close()
	if err := mail.Prepare(waitCtx, box); err != nil {
		return "", failure("email_verification_unavailable")
	}
	code, err := mail.WaitCodeFor(waitCtx, box, after, registerruntime.IsAdobeVerificationSender)
	if err != nil {
		return "", failure("email_verification_unavailable")
	}
	if waitCtx.Err() != nil {
		return "", failure("email_verification_unavailable")
	}
	if !adobeReloginEmailCode.MatchString(code) {
		return "", adobeReloginMailError("email_verification_invalid")
	}
	return code, nil
}
