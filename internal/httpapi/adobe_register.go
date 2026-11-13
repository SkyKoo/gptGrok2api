package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) initAdobeRegistration() {
	journalDir := s.cfg.DataDir
	if s.cfg.RegisterPath != "" {
		journalDir = filepath.Dir(s.cfg.RegisterPath)
	}
	factory := func(cfg registerruntime.AdobeConfig) (registerruntime.MailSource, registerruntime.AdobeRegistrationFlow, error) {
		if s.mailboxManager == nil || !s.mailboxManager.Enabled() {
			return nil, nil, errors.New("mailbox asset database is required for registration")
		}
		if cfg.Proxy == "" {
			cfg.Proxy = s.proxyManager.Resolve(nil, false)
		}
		mail, err := newMailboxResourceSource(context.Background(), s.mailboxManager, "adobe", cfg.MailboxSourceID, cfg.PollInterval)
		if err != nil {
			return nil, nil, err
		}
		return mail, s.newAdobeMediaRegistrationFlow(cfg), nil
	}
	s.adobeRegister = registerruntime.NewAdobeEngine(filepath.Join(journalDir, "adobe_registration_tasks.enc.json"), firstNonEmpty(s.cfg.AdminKey, s.cfg.APIKey, "gptgrok2api"), factory, s.importRegisteredAdobeAccount, s.verifyRegisteredAdobeAccount)
	s.adobeRegister.SetReregistrationAccountCheck(s.adobeReregistrationAccountExists)
	s.adobeRegister.SetProxyRouting(&registerruntime.ProxyRouting{Manager: s.proxyManager})
	s.adobeRegister.SetMediaFlowFactory(s.newAdobeMediaRegistrationFlow)
	s.configureAdobeMailboxReservations()
	s.adobeRegistrationSchedule = registerruntime.NewSchedule(filepath.Join(journalDir, "adobe_registration_schedule.json"))
	_ = s.adobeRegistrationSchedule.BindTarget("adobe")
}

// Release and reacquisition are local resource operations. Holding the Adobe
// account-store lock makes an imported account authoritative even when it has
// not yet been manually associated with its mailbox asset.
func (s *Server) configureAdobeMailboxReservations() {
	if s.adobeRegister == nil || s.mailboxStore == nil || s.adobeStore == nil {
		return
	}
	s.adobeRegister.SetMailboxReservationHandlers(
		func(ctx context.Context, job registerruntime.RegistrationJob) error {
			root := firstNonEmpty(job.RootJobID, job.ID)
			accountID := ""
			if len(job.Account) > 0 {
				accountID = "adobe:" + stringValue(job.Account["account_id"])
			}
			prepare := func() error {
				return s.mailboxStore.PrepareRegistrationClaim(ctx, "adobe", root, accountID, mailboxFromRegister(job.Mailbox))
			}
			if accountID != "" {
				return s.adobeStore.WithRegistrationMailboxOwner(ctx, job.Mailbox.Email, root, accountID, prepare)
			}
			executed, err := s.adobeStore.WithoutEmailAccount(ctx, job.Mailbox.Email, prepare)
			if err != nil {
				return err
			}
			if !executed {
				return adobe.ErrIdentity
			}
			return nil
		},
		func(ctx context.Context, job registerruntime.RegistrationJob) error {
			_, err := s.adobeStore.WithoutEmailAccount(ctx, job.Mailbox.Email, func() error {
				return s.mailboxStore.Release(ctx, "adobe", firstNonEmpty(job.RootJobID, job.ID), mailboxFromRegister(job.Mailbox), "registration_failed_without_account")
			})
			return err
		})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.adobeRegister.ReconcileMailboxReservations(ctx); err != nil {
		log.Printf("adobe failed mailbox reservation cleanup unavailable")
	}
}

func (s *Server) newAdobeMediaRegistrationFlow(cfg registerruntime.AdobeConfig) registerruntime.AdobeRegistrationFlow {
	return &registerruntime.AdobeBrowserFlow{
		Client:            &http.Client{Timeout: cfg.RequestTimeout, Transport: &http.Transport{Proxy: nil}},
		ResolveMediaProxy: s.adobeRegistrationMediaProxy,
	}
}

// A new identity follows today's configured daily route. An existing account's
// override is used only when this exact mailbox and browser profile already own it.
func (s *Server) adobeRegistrationMediaProxy(ctx context.Context, email, profileID string) (string, error) {
	if s.adobeStore == nil || strings.TrimSpace(email) == "" || profileID == "" {
		return "", errors.New("adobe_media_identity_missing")
	}
	items, err := s.adobeStore.List()
	if err != nil {
		return "", err
	}
	account := adobe.Account{Email: email, ProfileID: profileID, ProxyPolicy: "inherit"}
	found := false
	for _, candidate := range items {
		if !strings.EqualFold(strings.TrimSpace(candidate.Email), strings.TrimSpace(email)) {
			continue
		}
		if found || candidate.ProfileID != profileID {
			return "", adobe.ErrIdentity
		}
		account, found = candidate, true
	}
	fixed, err := s.adobeAccountContext(ctx, account)
	if err != nil {
		return "", err
	}
	endpoint, selected := proxyruntime.URLSelectionFromContext(fixed)
	if !selected {
		return "", errors.New("adobe_media_egress_required")
	}
	if endpoint != "" {
		u, parseErr := url.Parse(endpoint)
		if parseErr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return "", errors.New("adobe_media_proxy_unavailable")
		}
	}
	return endpoint, nil
}

// Manually imported accounts may not yet have a mailbox asset association or
// registration journal. Check the account store before starting a fresh signup.
func (s *Server) adobeReregistrationAccountExists(email string) (bool, error) {
	if s.adobeStore == nil {
		return false, errors.New("adobe account store unavailable")
	}
	accounts, err := s.adobeStore.List()
	if err != nil {
		return false, err
	}
	for _, account := range accounts {
		if strings.EqualFold(strings.TrimSpace(account.Email), strings.TrimSpace(email)) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) retryAdobeRegistration(w http.ResponseWriter, r *http.Request) {
	s.retryAdobeRegistrationAction(w, r, "saved_result")
}

func (s *Server) retryAdobeRegistrationTask(w http.ResponseWriter, r *http.Request) {
	s.retryAdobeRegistrationAction(w, r, "retry_registration")
}

func (s *Server) reregisterAdobeRegistrationTask(w http.ResponseWriter, r *http.Request) {
	s.retryAdobeRegistrationAction(w, r, "reregister")
}

func (s *Server) retryAdobeRegistrationAction(w http.ResponseWriter, r *http.Request, action string) {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if s.registrationClosing {
		writeError(w, 503, "服务正在停止", "server_error")
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		writeError(w, 503, "服务维护中", "server_error")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	config := s.registerStore.ConfigForTarget("adobe")
	var err error
	switch action {
	case "saved_result":
		err = s.adobeRegister.RetrySavedResultWithConfig(config, body.ID)
	case "retry_registration":
		err = s.adobeRegister.RetryRegistration(config, body.ID)
	case "reregister":
		err = s.adobeRegister.Reregister(config, body.ID)
	default:
		writeError(w, http.StatusBadRequest, "无效的注册操作", "invalid_request_error")
		return
	}
	if err != nil {
		registrationError(w, err)
		return
	}
	writeRegistrationJSON(w, http.StatusAccepted, map[string]any{"register": s.registrationSnapshotForTarget("adobe")})
}

// Browser fallback uses the existing account profile and checks identity before
// returning credentials. It never starts signup or accepts a different account.
func (s *Server) refreshAdobeBrowserAccount(ctx context.Context, account adobe.Account) (adobe.Account, error) {
	failure := func(code string) (adobe.Account, error) {
		return adobe.Account{}, &adobe.Error{Stage: "relogin", Code: code}
	}
	if account.ProfileID == "" {
		return failure("profile_required")
	}
	raw := mapValue(s.registerStore.ConfigForTarget("adobe")["adobe"])
	base := strings.TrimRight(firstNonEmpty(stringValue(raw["api_base"]), os.Getenv("CFM_BROWSER_WORKER_URL"), os.Getenv("CFM_GROK_BROWSER_URL"), "http://cfm-browser-worker:8899"), "/")
	key := firstNonEmpty(stringValue(raw["api_key"]), os.Getenv("CFM_BROWSER_WORKER_TOKEN"), os.Getenv("GROK_BROWSER_TOKEN"))
	client := &http.Client{Timeout: 160 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ctx, err := s.adobeAccountContext(ctx, account)
	if err != nil {
		return failure("proxy_binding_unavailable")
	}
	proxyURL := proxyruntime.URLFromContext(ctx)
	if proxyURL == "" {
		proxyURL = "direct"
	}
	lease, err := browserqueue.Acquire(ctx, client, base, key, map[string]any{"provider": "adobe", "profile_id": account.ProfileID, "email": account.Email, "adobe_mode": "refresh", "account_id": account.AccountID, "password": account.Password, "proxy": proxyURL}, nil)
	if err != nil {
		return failure("browser_unavailable")
	}
	defer lease.Release()
	// Keep the original browser and request budget while HME receives the code.
	// Each call owns and closes its response before the next stage starts.
	run := func(code string) (adobeBrowserRefreshResult, int, error) {
		payload := map[string]any{"browser_lease": lease.ID, "action": "refresh"}
		if code != "" {
			payload["code"] = code
		}
		return runAdobeBrowserRefresh(ctx, client, base, key, payload)
	}
	before := time.Now().UTC()
	result, status, err := run("")
	if err != nil {
		return adobe.Account{}, err
	}
	if status == http.StatusOK && result.Stage == "awaiting_verification" && result.Code == "refresh_email_otp_required" {
		code, err := s.waitAdobeReloginEmailCode(ctx, account, before, result.VerificationRequestedAt)
		if err != nil {
			return adobe.Account{}, err
		}
		if ctx.Err() != nil {
			return failure("cancelled")
		}
		// Exactly one continuation on this lease; a repeated OTP requirement,
		// challenge or lost response is terminal and never resends the code.
		result, status, err = run(code)
		if err != nil {
			return adobe.Account{}, err
		}
	}
	if status != http.StatusOK || result.Stage != "completed" {
		// Only known machine codes may leave the Worker. Raw diagnostics, URLs,
		// page text and credentials are deliberately excluded from the API error.
		switch result.Code {
		case "refresh_login_required":
			return failure("login_required")
		case "refresh_verification_required", "challenge_required", "refresh_challenge_required":
			return failure("verification_required")
		case "refresh_otp_required", "otp_required", "verification_code_required", "refresh_email_unverified", "refresh_email_otp_required":
			return failure("email_verification_required")
		case "refresh_otp_expired":
			return failure("email_verification_expired")
		case "refresh_otp_already_submitted":
			return failure("email_verification_already_submitted")
		case "refresh_otp_invalid":
			return failure("email_verification_invalid")
		case "refresh_otp_not_pending":
			return failure("email_verification_not_pending")
		case "refresh_otp_form_changed":
			return failure("email_verification_form_changed")
		case "refresh_otp_result_unknown":
			return failure("email_verification_result_unknown")
		case "refresh_account_mismatch", "refresh_email_mismatch":
			return adobe.Account{}, adobe.ErrIdentity
		case "media_session_incomplete":
			var diagnostics struct {
				MediaRuntime json.RawMessage `json:"media_runtime"`
			}
			_ = json.Unmarshal(result.Diagnostics, &diagnostics)
			return adobe.Account{}, &adobe.Error{Stage: "relogin", Code: "media_session_incomplete", MediaDiagnostic: adobe.SanitizeMediaPreparationDiagnostic(diagnostics.MediaRuntime)}
		case "media_session_cancelled", "refresh_session_ended":
			return failure("cancelled")
		case "refresh_login_form_unrecognized":
			return failure("login_form_unrecognized")
		case "refresh_login_entry_unrecognized":
			return failure("login_entry_unrecognized")
		case "refresh_login_form_unavailable":
			return failure("login_form_unavailable")
		case "refresh_password_form_unavailable":
			return failure("password_form_unavailable")
		case "refresh_login_pending", "refresh_login_completion_unconfirmed", "refresh_login_result_unknown":
			return failure("login_pending")
		default:
			return failure("browser_failed")
		}
	}
	if result.Account.AccountID != account.AccountID || !strings.EqualFold(strings.TrimSpace(result.Account.Email), strings.TrimSpace(account.Email)) {
		return adobe.Account{}, adobe.ErrIdentity
	}
	return result.Account, nil
}

func (s *Server) mergeAdobeWorkerKey(updates map[string]any) {
	next, ok := updates["adobe"].(map[string]any)
	if !ok {
		return
	}
	before := mapValue(s.registerStore.Get()["adobe"])
	merged := cloneMap(before)
	for key, value := range next {
		if key != "api_key_set" {
			merged[key] = value
		}
	}
	if stringValue(merged["api_base"]) != stringValue(before["api_base"]) {
		// An endpoint change must never forward the old authentication token.
		merged["api_key"] = stringValue(next["api_key"])
	} else if stringValue(merged["api_key"]) == "" {
		merged["api_key"] = before["api_key"]
	}
	updates["adobe"] = merged
}
