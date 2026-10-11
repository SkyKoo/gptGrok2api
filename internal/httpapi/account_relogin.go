package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

// accountReloginProgress is intentionally separate from the normal refresh
// progress. Relogin waits for an email OTP and can take several minutes; it
// also updates one existing account instead of probing a token in place.
type accountReloginProgress struct {
	ProgressID     string         `json:"progress_id"`
	AccountRef     string         `json:"account_ref"`
	Email          string         `json:"email"`
	Stage          string         `json:"stage"`
	StartedAt      string         `json:"started_at"`
	UpdatedAt      string         `json:"updated_at"`
	FinishedAt     string         `json:"finished_at,omitempty"`
	FailedStage    string         `json:"failed_stage,omitempty"`
	TokenExpiresAt string         `json:"token_expires_at,omitempty"`
	Done           bool           `json:"done"`
	Error          string         `json:"error,omitempty"`
	Result         map[string]any `json:"result,omitempty"`
}

// List responses intentionally exclude account results and credentials.
func reloginSummary(progress *accountReloginProgress) accountReloginProgress {
	summary := *progress
	summary.Result = nil
	return summary
}

func reloginTimeAfter(a, b string) bool {
	first, _ := time.Parse(time.RFC3339Nano, a)
	second, _ := time.Parse(time.RFC3339Nano, b)
	return first.After(second)
}

// Keep all running tasks and the latest 100 finished tasks in this process.
// Reading progress never initiates or repeats a login.
func (s *Server) pruneAccountReloginsLocked() {
	var finished []*accountReloginProgress
	for _, p := range s.reloginProgress {
		if p.Done {
			finished = append(finished, p)
		}
	}
	sort.Slice(finished, func(i, j int) bool { return reloginTimeAfter(finished[i].FinishedAt, finished[j].FinishedAt) })
	for i := 100; i < len(finished); i++ {
		delete(s.reloginProgress, finished[i].ProgressID)
	}
}

func (s *Server) accountReloginList(w http.ResponseWriter, r *http.Request) {
	s.reloginMu.RLock()
	items := make([]accountReloginProgress, 0, len(s.reloginProgress))
	for _, progress := range s.reloginProgress {
		items = append(items, reloginSummary(progress))
	}
	s.reloginMu.RUnlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].Done != items[j].Done {
			return !items[i].Done
		}
		return reloginTimeAfter(items[i].StartedAt, items[j].StartedAt)
	})
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) accountReloginStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		s.accountReloginList(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccountRef  string `json:"account_ref"`
		AccessToken string `json:"access_token"`
		ID          string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ref := firstNonEmpty(body.AccountRef, body.ID, body.AccessToken)
	account, err := s.accountByRef(ref)
	if err != nil {
		writeError(w, http.StatusNotFound, "account not found", "not_found")
		return
	}
	if strings.TrimSpace(stringValue(account["email"])) == "" {
		writeError(w, http.StatusBadRequest, "account email is required for relogin", "invalid_request_error")
		return
	}
	publicRef := accountPublicRef(account)
	progressID := newChatID()
	s.reloginMu.Lock()
	for _, existing := range s.reloginProgress {
		if existing.AccountRef == publicRef && !existing.Done {
			summary := reloginSummary(existing)
			s.reloginMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"progress_id": summary.ProgressID, "account_ref": publicRef, "progress": summary})
			return
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	progress := &accountReloginProgress{ProgressID: progressID, AccountRef: publicRef, Email: stringValue(account["email"]), Stage: "queued", StartedAt: now, UpdatedAt: now}
	s.reloginProgress[progressID] = progress
	summary := reloginSummary(progress)
	s.reloginMu.Unlock()
	go s.runAccountRelogin(progressID, publicRef)
	writeJSON(w, http.StatusOK, map[string]any{"progress_id": progressID, "account_ref": publicRef, "progress": summary})
}

func (s *Server) accountReloginProgressAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/accounts/relogin/progress/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "progress not found", "not_found")
		return
	}
	s.reloginMu.RLock()
	progress, ok := s.reloginProgress[id]
	if ok {
		copy := *progress
		copy.Result = cloneMap(progress.Result)
		progress = &copy
	}
	s.reloginMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "progress not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func (s *Server) updateAccountReloginStage(id, stage string) {
	s.reloginMu.Lock()
	if progress := s.reloginProgress[id]; progress != nil && !progress.Done {
		progress.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		progress.Stage = stage
	}
	s.reloginMu.Unlock()
}

func (s *Server) finishAccountRelogin(id string, result map[string]any, err error) {
	s.reloginMu.Lock()
	if progress := s.reloginProgress[id]; progress != nil {
		previousStage := progress.Stage
		progress.Done = true
		progress.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		progress.UpdatedAt = progress.FinishedAt
		progress.Stage = "completed"
		progress.Result = result
		if err != nil {
			progress.Stage = "failed"
			progress.FailedStage = previousStage
			var failure *registerruntime.Failure
			if errors.As(err, &failure) && failure.Stage != "" {
				progress.FailedStage = failure.Stage
			}
			progress.Error = safeReloginError(err)
		} else {
			progress.TokenExpiresAt = stringValue(result["token_expires_at"])
		}
	}
	s.pruneAccountReloginsLocked()
	s.reloginMu.Unlock()
}

func safeReloginError(err error) string {
	if err == nil {
		return ""
	}
	var failure *registerruntime.Failure
	if errors.As(err, &failure) {
		return failure.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "task_cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "task_timeout"
	}
	return "relogin_failed"
}

func (s *Server) accountReloginProfile(account map[string]any) (*registerruntime.BrowserProfile, error) {
	if profile, ok := registerruntime.ParseBrowserProfile(account["browser_profile"]); ok {
		return &profile, nil
	}
	if s.freeRegister != nil {
		if profile, ok := s.freeRegister.BrowserProfileForAccount(stringValue(account["registration_job_id"]), stringValue(account["email"])); ok {
			return &profile, nil
		}
	}
	if profile, ok := registerruntime.BrowserProfileFromFingerprint(account["fp"]); ok {
		return &profile, nil
	}
	if account["browser_profile"] != nil || stringValue(mapValue(account["fp"])["profile"]) != "" {
		return nil, &registerruntime.Failure{Stage: "config", Code: "saved_browser_profile_unsupported"}
	}
	// Accounts imported without an environment retain the legacy fresh-profile
	// behavior. A successful login saves it for all subsequent logins.
	return nil, nil
}

func (s *Server) reloginConfig(account map[string]any) (registerruntime.FreeConfig, *registerruntime.HME, *registerruntime.WebRegistrar, error) {
	cfg, err := registerruntime.ParseFreeConfig(s.registerStore.Get())
	if err != nil {
		return cfg, nil, nil, err
	}
	profile, err := s.accountReloginProfile(account)
	if err != nil {
		return cfg, nil, nil, err
	}
	var profiles []registerruntime.BrowserProfile
	if profile != nil {
		profiles = append(profiles, *profile)
	}
	mail, err := registerruntime.NewHME(cfg.HME, cfg.PollInterval)
	if err != nil {
		return cfg, nil, nil, err
	}
	proxy := cfg.Proxy
	if proxy == "" {
		proxy = s.proxyManager.Resolve(nil, false)
	}
	flow, err := registerruntime.NewWebRegistrar(strings.TrimSuffix(strings.TrimRight(s.cfg.OpenAIBaseURL, "/"), "/backend-api"), s.cfg.OpenAIAuthBaseURL, proxy, profiles...)
	if err != nil {
		mail.Close()
		return cfg, nil, nil, err
	}
	if err := flow.SetDeviceID(stringValue(mapValue(account["fp"])["oai-device-id"])); err != nil {
		flow.Close()
		mail.Close()
		return cfg, nil, nil, err
	}
	return cfg, mail, flow, nil
}

func (s *Server) runAccountRelogin(progressID, ref string) {
	account, err := s.accountByRef(ref)
	if err != nil {
		s.finishAccountRelogin(progressID, nil, err)
		return
	}
	s.updateAccountReloginStage(progressID, "config")
	cfg, mail, flow, err := s.reloginConfig(account)
	if err != nil {
		s.finishAccountRelogin(progressID, nil, err)
		return
	}
	defer mail.Close()
	defer flow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	box := registerruntime.Mailbox{Email: stringValue(account["email"]), AccountID: cfg.HME.AccountID}
	s.updateAccountReloginStage(progressID, "mailbox")
	if preparer, ok := any(mail).(registerruntime.MailSessionPreparer); ok {
		if err := preparer.Prepare(ctx, box); err != nil {
			s.finishAccountRelogin(progressID, nil, err)
			return
		}
	}
	s.updateAccountReloginStage(progressID, "authorize")
	result, err := flow.LoginExisting(ctx, box, cfg, func(waitCtx context.Context, after time.Time) (string, error) {
		s.updateAccountReloginStage(progressID, "wait_code")
		return mail.WaitCode(waitCtx, box, after)
	}, func(stage string) error {
		s.updateAccountReloginStage(progressID, stage)
		return nil
	})
	if err != nil {
		s.finishAccountRelogin(progressID, nil, err)
		return
	}
	s.updateAccountReloginStage(progressID, "save_session")
	newToken := stringValue(result["access_token"])
	oldToken := accountToken(account)
	if newToken == "" || oldToken == "" {
		s.finishAccountRelogin(progressID, nil, errors.New("missing account token"))
		return
	}
	if email := stringValue(result["email"]); email != "" && !strings.EqualFold(email, stringValue(account["email"])) {
		s.finishAccountRelogin(progressID, nil, errors.New("account email mismatch"))
		return
	}
	if userID := stringValue(result["user_id"]); userID != "" && stringValue(account["user_id"]) != "" && userID != stringValue(account["user_id"]) {
		s.finishAccountRelogin(progressID, nil, errors.New("account user mismatch"))
		return
	}
	fingerprint := cloneMap(mapValue(account["fp"]))
	for key, value := range mapValue(result["fp"]) {
		fingerprint[key] = value
	}
	fields := map[string]any{
		"email":                 firstNonEmpty(stringValue(result["email"]), stringValue(account["email"])),
		"user_id":               firstNonEmpty(stringValue(result["user_id"]), stringValue(account["user_id"])),
		"source_type":           "chatgpt_web",
		"type":                  firstNonEmpty(stringValue(account["type"]), "free"),
		"status":                "正常",
		"enabled":               boolValue(account["enabled"], true),
		"registration_verified": true,
		"browser_profile":       result["browser_profile"],
		"fp":                    fingerprint,
	}
	if expired := stringValue(result["expired"]); expired != "" {
		fields["expires"] = expired
	}
	updated, _, err := s.store.RotateAccountTokens(oldToken, newToken, "", "", fields)
	if err != nil {
		s.finishAccountRelogin(progressID, nil, err)
		return
	}
	claims, _ := jwtClaims(newToken)
	s.finishAccountRelogin(progressID, map[string]any{"updated": 1, "item": accountForAPI(updated), "token_expires_at": jwtTime(claims["exp"])}, nil)
}
