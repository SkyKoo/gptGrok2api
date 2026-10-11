package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

// accountReloginProgress is intentionally separate from the normal refresh
// progress. Relogin waits for an email OTP and can take several minutes; it
// also updates one existing account instead of probing a token in place.
type accountReloginProgress struct {
	Automatic         bool `json:"automatic,omitempty"`
	SessionUpdated    bool `json:"session_updated,omitempty"`
	QuotasRefreshed   bool `json:"quotas_refreshed,omitempty"`
	refreshAfterLogin bool
	autoJoined        bool
	accountToken      string
	ProgressID        string         `json:"progress_id"`
	AccountRef        string         `json:"account_ref"`
	Email             string         `json:"email"`
	Stage             string         `json:"stage"`
	StartedAt         string         `json:"started_at"`
	UpdatedAt         string         `json:"updated_at"`
	FinishedAt        string         `json:"finished_at,omitempty"`
	FailedStage       string         `json:"failed_stage,omitempty"`
	TokenExpiresAt    string         `json:"token_expires_at,omitempty"`
	Done              bool           `json:"done"`
	Error             string         `json:"error,omitempty"`
	Result            map[string]any `json:"result,omitempty"`
}

// List responses intentionally exclude account results and credentials.
func reloginSummary(progress *accountReloginProgress) accountReloginProgress {
	summary := *progress
	summary.Result = nil
	summary.accountToken = ""
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
	summary, err := s.startAccountRelogin(account, false)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"progress_id": summary.ProgressID, "account_ref": summary.AccountRef, "progress": summary})
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
		// Mark the exact credential this task used before publishing completion.
		// Runtime state blocks scheduling even if the subsequent disk flush fails.
		if err != nil && progress.accountToken != "" && s.store != nil {
			if _, saveErr := s.store.UpdateAccountRuntime(progress.accountToken, reloginFailureUpdates(err)); saveErr == nil {
				if flushErr := s.store.FlushAccounts(); flushErr != nil {
					log.Printf("persist relogin failure status: %v", flushErr)
				}
			} else if !errors.Is(saveErr, os.ErrNotExist) {
				log.Printf("record relogin failure status: %v", saveErr)
			}
		}
		previousStage := progress.Stage
		progress.Done = true
		progress.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		progress.UpdatedAt = progress.FinishedAt
		progress.Stage = "completed"
		progress.Result = result
		progress.SessionUpdated = boolValue(result["session_updated"], false)
		progress.QuotasRefreshed = boolValue(result["quotas_refreshed"], false)
		progress.TokenExpiresAt = stringValue(result["token_expires_at"])
		if err != nil {
			progress.Stage = "failed"
			progress.FailedStage = previousStage
			var failure *registerruntime.Failure
			if errors.As(err, &failure) && failure.Stage != "" {
				progress.FailedStage = failure.Stage
			}
			progress.Error = safeReloginError(err)
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
	s.reloginMu.RLock()
	task := s.reloginProgress[progressID]
	changed := task != nil && task.accountToken != "" && task.accountToken != accountToken(account)
	s.reloginMu.RUnlock()
	if changed {
		s.finishAccountRelogin(progressID, nil, &registerruntime.Failure{Stage: "config", Code: "account_changed"})
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
		"registration_verified": true,
		"browser_profile":       result["browser_profile"],
		"fp":                    fingerprint,
	}
	if expired := stringValue(result["expired"]); expired != "" {
		fields["expires"] = expired
	}
	// Re-read before saving: never revive a deleted account or overwrite credentials
	// rotated by another process while we were waiting for the email code.
	latest, err := s.accountByRef(ref)
	if err != nil || accountToken(latest) != oldToken {
		s.finishAccountRelogin(progressID, nil, &registerruntime.Failure{Stage: "save_session", Code: "account_changed"})
		return
	}
	s.reloginMu.RLock()
	refreshAfter := s.reloginProgress[progressID] != nil && s.reloginProgress[progressID].refreshAfterLogin
	s.reloginMu.RUnlock()
	var refreshErr error
	if refreshAfter {
		s.updateAccountReloginStage(progressID, "refresh_account")
		candidate := cloneMap(latest)
		for key, value := range fields {
			candidate[key] = value
		}
		candidate["access_token"] = newToken
		refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 90*time.Second)
		refreshed, e := s.openAIAccountClient().RefreshAccount(refreshCtx, candidate)
		refreshCancel()
		refreshErr = e
		if e == nil && (!strings.EqualFold(stringValue(refreshed.Fields["email"]), stringValue(latest["email"])) ||
			stringValue(latest["user_id"]) != "" && stringValue(refreshed.Fields["user_id"]) != stringValue(latest["user_id"])) {
			s.finishAccountRelogin(progressID, nil, &registerruntime.Failure{Stage: "refresh_account", Code: "account_identity_mismatch"})
			return
		}
		if errors.Is(e, provider.ErrInvalidAccessToken) {
			s.finishAccountRelogin(progressID, nil, &registerruntime.Failure{Stage: "refresh_account", Code: "new_access_token_invalid"})
			return
		}
		limits, hasLimits := refreshed.Fields["limits_progress"].([]any)
		if e == nil && (!hasLimits || limits == nil) {
			refreshErr = errors.New("quota snapshot missing")
		}
		if refreshErr == nil {
			for key, value := range refreshed.Fields {
				fields[key] = value
			}
			fields["quota_pending"] = map[string]int{}
			fields["quota_refresh_needed"] = false
			fields["quota_refresh_error"] = nil
			fields["capability_cooldowns"] = map[string]any{}
			fields["survival_alive"] = true
			fields["survival_status"] = fields["type"]
			fields["survival_last_probe_status"] = fields["type"]
			fields["survival_check_error"] = nil
			fields["survival_last_checked_at"] = time.Now().UTC().Format(time.RFC3339)
		} else {
			// Keep the new browser credential even if the quota service is temporarily
			// unavailable. Do not claim the old quota snapshot was refreshed.
			fields["quota_refresh_needed"] = true
			fields["quota_refresh_error"] = "upstream quota refresh failed; previous snapshot retained"
		}
	}
	// Keep existing enabled/disabled state in Store rather than copying an old
	// snapshot over an administrator action during the remote probe.
	latest, err = s.accountByRef(ref)
	if err != nil || accountToken(latest) != oldToken {
		s.finishAccountRelogin(progressID, nil, &registerruntime.Failure{Stage: "save_session", Code: "account_changed"})
		return
	}
	if refreshErr != nil {
		for key, value := range reloginFailureUpdates(&registerruntime.Failure{Stage: "refresh_account", Code: "account_refresh_failed"}) {
			fields[key] = value
		}
	}
	updated, _, err := s.store.RotateAccountTokens(oldToken, newToken, "", "", fields)
	if err != nil {
		s.finishAccountRelogin(progressID, nil, err)
		return
	}
	s.reloginMu.Lock()
	if task := s.reloginProgress[progressID]; task != nil {
		task.accountToken = newToken
	}
	s.reloginMu.Unlock()
	claims, _ := jwtClaims(newToken)
	resultSummary := map[string]any{"updated": 1, "item": accountForAPI(updated), "token_expires_at": jwtTime(claims["exp"]), "session_updated": true, "quotas_refreshed": refreshAfter && refreshErr == nil}
	if refreshErr != nil {
		s.finishAccountRelogin(progressID, resultSummary, &registerruntime.Failure{Stage: "refresh_account", Code: "account_refresh_failed"})
		return
	}
	s.finishAccountRelogin(progressID, resultSummary, nil)
}

func reloginFailureUpdates(err error) map[string]any {
	return map[string]any{
		"status":                "异常",
		"relogin_blocked":       true,
		"last_relogin_error":    safeReloginError(err),
		"last_relogin_error_at": time.Now().UTC().Format(time.RFC3339),
	}
}
