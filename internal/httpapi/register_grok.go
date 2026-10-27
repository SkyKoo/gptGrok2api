package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func (s *Server) initGrokRegistration() {
	journalDir := s.cfg.DataDir
	if s.cfg.RegisterPath != "" {
		journalDir = filepath.Dir(s.cfg.RegisterPath)
	}
	factory := func(cfg registerruntime.GrokConfig) (registerruntime.MailSource, registerruntime.GrokRegistrationFlow, error) {
		proxy := cfg.Proxy
		if proxy == "" {
			proxy = s.proxyManager.Resolve(nil, false)
		}
		cfg.Proxy = proxy
		if s.mailboxStore == nil {
			if s.mailboxInitErr != nil {
				return nil, nil, s.mailboxInitErr
			}
			return nil, nil, errors.New("mailbox asset database is required for registration")
		}
		mailClient, err := registerruntime.NewHME(cfg.HME, cfg.PollInterval)
		if err != nil {
			return nil, nil, err
		}
		mail := registerruntime.MailSource(registerruntime.NewCoordinatedMailSource("grok", mailClient, s.mailboxCoordinator()))
		// The browser worker is an internal Docker service. Do not reuse the
		// request client's proxy transport here: that transport is intended for
		// upstream traffic and can send `grok-browser` through Clash, making the
		// worker look unreachable even though it is healthy on the Compose network.
		solverClient := &http.Client{Timeout: cfg.Turnstile.Timeout + 30*time.Second}
		solver := registerruntime.GrokTurnstileSolver(&registerruntime.HTTPGrokBrowserSolver{
			BaseURL: cfg.Turnstile.APIBase,
			APIKey:  cfg.Turnstile.APIKey,
			Client:  solverClient,
			Timeout: cfg.Turnstile.Timeout,
		})
		flow, err := registerruntime.NewGrokProtocolFlow(cfg, solver)
		if err != nil {
			mail.Close()
			return nil, nil, err
		}
		return mail, flow, nil
	}
	s.grokRegister = registerruntime.NewGrokEngine(filepath.Join(journalDir, "grok_registration_tasks.json"), factory, s.importRegisteredGrokAccount, s.verifyRegisteredGrokAccount)
}

func (s *Server) importRegisteredGrokAccount(ctx context.Context, id string, account map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.registerStore == nil {
		return errors.New("grok registration store unavailable")
	}
	account = cloneMap(account)
	account["registration_job_id"] = id
	account["platform"] = "grok"
	account["source_type"] = "grok_protocol"
	account["enabled"] = false
	account["status"] = "待验证"
	_, _, err := s.registerStore.AddAccount(account)
	return err
}

func (s *Server) verifyRegisteredGrokAccount(ctx context.Context, id string, account map[string]any) error {
	if s.grokQuota == nil || strings.TrimSpace(s.grokQuota.URL) == "" {
		return errors.New("Grok rate-limits endpoint is not configured")
	}
	items, err := s.registerStore.ExportAccounts(nil)
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
		return errors.New("registered Grok account missing")
	}
	token := strings.TrimSpace(stringValue(stored["sso"]))
	if token == "" {
		return errors.New("registered Grok account missing SSO")
	}
	quota, err := s.grokQuota.ProbeFast(ctx, token, stored)
	if err != nil {
		if errors.Is(err, provider.ErrGrokInvalidCredentials) {
			return err
		}
		return err
	}
	checkedAt := time.Now().UTC().Format(time.RFC3339)
	_, _ = s.registerStore.UpdateRuntime(stringValue(stored["id"]), map[string]any{
		"present":   true,
		"status":    "active",
		"synced_at": checkedAt,
		"quota": map[string]any{
			"fast": map[string]any{"remaining": quota.Remaining, "total": quota.Total, "window_seconds": quota.WindowSeconds, "reset_at": quota.ResetAt},
		},
	}, map[string]any{"status": "valid", "checked_at": checkedAt}, "active", "")
	_, err = s.registerStore.UpdateAccountFields(stringValue(stored["id"]), map[string]any{
		"enabled":               true,
		"registration_verified": true,
		"fast_quota":            map[string]any{"remaining": quota.Remaining, "total": quota.Total, "window_seconds": quota.WindowSeconds, "reset_at": quota.ResetAt},
	})
	if err != nil {
		return err
	}
	_, _, err = s.syncRegisteredGrokRuntimeAccount(stored, token)
	return err
}

func (s *Server) retryGrokRegistration(w http.ResponseWriter, r *http.Request) {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if s.registrationClosing {
		writeError(w, http.StatusServiceUnavailable, "服务正在停止，请稍后重试", "server_error")
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		writeError(w, http.StatusServiceUnavailable, "服务维护中，请稍后重试", "server_error")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.grokRegister.RetrySavedResult(body.ID); err != nil {
		registrationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"register": s.registrationSnapshot()})
}

func (s *Server) retryGrokRegistrationTask(w http.ResponseWriter, r *http.Request) {
	s.registrationMu.Lock()
	defer s.registrationMu.Unlock()
	if s.registrationClosing {
		writeError(w, http.StatusServiceUnavailable, "服务正在停止，请稍后重试", "server_error")
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		writeError(w, http.StatusServiceUnavailable, "服务维护中，请稍后重试", "server_error")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.grokRegister.RetryRegistration(s.registerStore.Get(), body.ID); err != nil {
		registrationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"register": s.registrationSnapshot()})
}
