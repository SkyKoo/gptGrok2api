package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

// accountReloginProgress is intentionally separate from the normal refresh
// progress. Relogin waits for an email OTP and can take several minutes; it
// also updates one existing account instead of probing a token in place.
type accountReloginProgress struct {
	AccountRef string         `json:"account_ref"`
	Stage      string         `json:"stage"`
	Done       bool           `json:"done"`
	Error      string         `json:"error,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
}

func (s *Server) accountReloginStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
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
	s.reloginProgress[progressID] = &accountReloginProgress{AccountRef: publicRef, Stage: "queued"}
	s.reloginMu.Unlock()
	go s.runAccountRelogin(progressID, publicRef)
	writeJSON(w, http.StatusOK, map[string]any{"progress_id": progressID, "account_ref": publicRef})
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
	if progress := s.reloginProgress[id]; progress != nil {
		progress.Stage = stage
	}
	s.reloginMu.Unlock()
}

func (s *Server) finishAccountRelogin(id string, result map[string]any, err error) {
	s.reloginMu.Lock()
	if progress := s.reloginProgress[id]; progress != nil {
		progress.Done = true
		progress.Stage = "completed"
		progress.Result = result
		if err != nil {
			progress.Stage = "failed"
			progress.Error = safeReloginError(err)
		}
	}
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

func (s *Server) reloginConfig() (registerruntime.FreeConfig, *registerruntime.HME, *registerruntime.WebRegistrar, error) {
	cfg, err := registerruntime.ParseFreeConfig(s.registerStore.Get())
	if err != nil {
		return cfg, nil, nil, err
	}
	mail, err := registerruntime.NewHME(cfg.HME, cfg.PollInterval)
	if err != nil {
		return cfg, nil, nil, err
	}
	proxy := cfg.Proxy
	if proxy == "" {
		proxy = s.proxyManager.Resolve(nil, false)
	}
	flow, err := registerruntime.NewWebRegistrar(strings.TrimSuffix(strings.TrimRight(s.cfg.OpenAIBaseURL, "/"), "/backend-api"), s.cfg.OpenAIAuthBaseURL, proxy)
	if err != nil {
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
	cfg, mail, flow, err := s.reloginConfig()
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
	fields := map[string]any{
		"email":                 firstNonEmpty(stringValue(result["email"]), stringValue(account["email"])),
		"user_id":               firstNonEmpty(stringValue(result["user_id"]), stringValue(account["user_id"])),
		"source_type":           "chatgpt_web",
		"type":                  firstNonEmpty(stringValue(account["type"]), "free"),
		"status":                "正常",
		"enabled":               boolValue(account["enabled"], true),
		"registration_verified": true,
	}
	if expired := stringValue(result["expired"]); expired != "" {
		fields["expires"] = expired
	}
	updated, _, err := s.store.RotateAccountTokens(oldToken, newToken, "", "", fields)
	if err != nil {
		s.finishAccountRelogin(progressID, nil, err)
		return
	}
	items, _ := s.store.AccountList()
	s.finishAccountRelogin(progressID, map[string]any{"updated": 1, "item": accountForAPI(updated), "items": accountsForAPI(items)}, nil)
}
