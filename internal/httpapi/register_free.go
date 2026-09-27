package httpapi

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func (s *Server) initFreeRegistration() {
	factory := func(cfg registerruntime.FreeConfig) (registerruntime.MailSource, registerruntime.RegistrationFlow, error) {
		proxy := cfg.Proxy
		if proxy == "" {
			proxy = s.proxyManager.Resolve(nil, false)
		}
		mail, err := registerruntime.NewHME(cfg.HME, cfg.PollInterval)
		if err != nil {
			return nil, nil, err
		}
		flow, err := registerruntime.NewWebRegistrar(strings.TrimSuffix(strings.TrimRight(s.cfg.OpenAIBaseURL, "/"), "/backend-api"), s.cfg.OpenAIAuthBaseURL, proxy)
		if err != nil {
			mail.Close()
			return nil, nil, err
		}
		return mail, flow, nil
	}
	journalDir := s.cfg.DataDir
	if s.cfg.RegisterPath != "" {
		journalDir = filepath.Dir(s.cfg.RegisterPath)
	}
	s.freeRegister = registerruntime.NewFreeEngine(filepath.Join(journalDir, "openai_registration_tasks.json"), factory, s.importRegisteredAccount, s.verifyRegisteredAccount)
}
func (s *Server) importRegisteredAccount(ctx context.Context, id string, account map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	items, err := s.store.AccountList()
	if err != nil {
		return err
	}
	for _, item := range items {
		if stringValue(item["registration_job_id"]) == id {
			return nil
		}
		if accountToken(item) == accountToken(account) {
			return errors.New("account already belongs to another source")
		}
	}
	_, _, _, err = s.store.AddAccounts(nil, []map[string]any{account})
	return err
}
func (s *Server) verifyRegisteredAccount(ctx context.Context, id string, account map[string]any) error {
	items, err := s.store.AccountList()
	if err != nil {
		return err
	}
	var stored map[string]any
	for _, item := range items {
		if stringValue(item["registration_job_id"]) == id {
			stored = item
			break
		}
	}
	if stored == nil {
		return errors.New("registered account missing")
	}
	result, err := s.openAIAccountClient().RefreshAccount(ctx, stored)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fields := result.Fields
	if fields == nil {
		fields = map[string]any{}
	}
	if !strings.EqualFold(stringValue(fields["email"]), stringValue(stored["email"])) {
		return errors.New("verified account email does not match registration")
	}
	fields["enabled"], fields["registration_verified"] = true, true
	if stringValue(fields["status"]) == "" {
		fields["status"] = "正常"
	}
	_, _, err = s.store.RotateAccountTokens(accountToken(stored), result.AccessToken, result.RefreshToken, result.IDToken, fields)
	return err
}
func (s *Server) registrationSnapshot() map[string]any {
	value := s.registerStore.Get()
	providers, _ := mapValue(value["mail"])["providers"].([]any)
	for _, raw := range providers {
		p := mapValue(raw)
		if stringValue(p["type"]) == "icloud_hme" {
			p["admin_password_set"] = stringValue(p["admin_password"]) != ""
			delete(p, "admin_password")
		}
	}
	if stringValue(value["target"]) == "openai" {
		state := s.freeRegister.Snapshot()
		value["enabled"], value["stats"], value["logs"], value["jobs"] = state["running"], state["stats"], state["logs"], state["jobs"]
		value["runtime_error"] = state["error"]
	}
	return value
}

// A blank password retains the saved secret only for the same HME endpoint and
// account. Changing the endpoint requires re-entry to avoid forwarding secrets.
func (s *Server) mergeHMEPassword(updates map[string]any) {
	old, _ := mapValue(s.registerStore.Get()["mail"])["providers"].([]any)
	providers, _ := mapValue(updates["mail"])["providers"].([]any)
	for _, raw := range providers {
		p := mapValue(raw)
		delete(p, "admin_password_set")
		if stringValue(p["type"]) != "icloud_hme" || stringValue(p["admin_password"]) != "" {
			continue
		}
		for _, previous := range old {
			before := mapValue(previous)
			if stringValue(before["type"]) == "icloud_hme" && stringValue(p["id"]) != "" && stringValue(p["id"]) == stringValue(before["id"]) && stringValue(p["api_base"]) == stringValue(before["api_base"]) && stringValue(p["account_id"]) == stringValue(before["account_id"]) {
				p["admin_password"] = before["admin_password"]
				break
			}
		}
	}
}
func registrationError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var failure *registerruntime.Failure
	if errors.As(err, &failure) {
		if failure.Stage == "storage" {
			status = http.StatusInternalServerError
		}
		if failure.Code == "already_running" {
			status = http.StatusConflict
		}
	}
	writeError(w, status, err.Error(), "registration_error")
}
func (s *Server) retryFreeRegistration(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.freeRegister.RetrySavedResult(body.ID); err != nil {
		registrationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"register": s.registrationSnapshot()})
}

func (s *Server) retryFreeRegistrationTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.freeRegister.RetryRegistration(s.registerStore.Get(), body.ID); err != nil {
		registrationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"register": s.registrationSnapshot()})
}

// ShutdownRegistration cancels background registration before the process exits.
func (s *Server) ShutdownRegistration(ctx context.Context) error { return s.freeRegister.Shutdown(ctx) }

func writeRegistrationJSON(w http.ResponseWriter, status int, value any) {
	redactHMECredentials(value)
	writeJSON(w, status, value)
}

// Legacy registration actions can also return the complete configuration.
// Redact the HME secret on every such response, including nested reset results.
func redactHMECredentials(value any) {
	switch v := value.(type) {
	case map[string]any:
		if stringValue(v["type"]) == "icloud_hme" {
			if password, exists := v["admin_password"]; exists {
				v["admin_password_set"] = stringValue(password) != ""
				delete(v, "admin_password")
			}
		}
		for _, child := range v {
			redactHMECredentials(child)
		}
	case []any:
		for _, child := range v {
			redactHMECredentials(child)
		}
	}
}
