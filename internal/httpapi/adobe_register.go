package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
	"io"
	"net/http"
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
		return mail, &registerruntime.AdobeBrowserFlow{Client: &http.Client{Timeout: cfg.RequestTimeout, Transport: &http.Transport{Proxy: nil}}}, nil
	}
	s.adobeRegister = registerruntime.NewAdobeEngine(filepath.Join(journalDir, "adobe_registration_tasks.enc.json"), firstNonEmpty(s.cfg.AdminKey, s.cfg.APIKey, "gptgrok2api"), factory, s.importRegisteredAdobeAccount, s.verifyRegisteredAdobeAccount)
	s.adobeRegister.SetProxyRouting(&registerruntime.ProxyRouting{Manager: s.proxyManager})
	s.adobeRegistrationSchedule = registerruntime.NewSchedule(filepath.Join(journalDir, "adobe_registration_schedule.json"))
	_ = s.adobeRegistrationSchedule.BindTarget("adobe")
}
func (s *Server) retryAdobeRegistration(w http.ResponseWriter, r *http.Request) {
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
	if err := s.adobeRegister.RetrySavedResult(body.ID); err != nil {
		registrationError(w, err)
		return
	}
	writeRegistrationJSON(w, http.StatusAccepted, map[string]any{"register": s.registrationSnapshotForTarget("adobe")})
}

// Browser fallback uses the existing account profile and checks identity before
// returning credentials. It never starts signup or accepts a different account.
func (s *Server) refreshAdobeBrowserAccount(ctx context.Context, account adobe.Account) (adobe.Account, error) {
	if account.ProfileID == "" {
		return adobe.Account{}, errors.New("adobe_profile_required")
	}
	raw := mapValue(s.registerStore.ConfigForTarget("adobe")["adobe"])
	base := strings.TrimRight(firstNonEmpty(stringValue(raw["api_base"]), os.Getenv("CFM_BROWSER_WORKER_URL"), os.Getenv("CFM_GROK_BROWSER_URL"), "http://cfm-browser-worker:8899"), "/")
	key := firstNonEmpty(stringValue(raw["api_key"]), os.Getenv("CFM_BROWSER_WORKER_TOKEN"), os.Getenv("GROK_BROWSER_TOKEN"))
	client := &http.Client{Timeout: 160 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, err := s.adobeAccountContext(ctx, account)
	if err != nil {
		return adobe.Account{}, errors.New("adobe_proxy_binding_unavailable")
	}
	proxyURL := proxyruntime.URLFromContext(ctx)
	if proxyURL == "" {
		proxyURL = "direct"
	}
	lease, err := browserqueue.Acquire(ctx, client, base, key, map[string]any{"provider": "adobe", "profile_id": account.ProfileID, "email": account.Email, "adobe_mode": "refresh", "account_id": account.AccountID, "password": account.Password, "proxy": proxyURL}, nil)
	if err != nil {
		return adobe.Account{}, errors.New("adobe_browser_unavailable")
	}
	defer lease.Release()
	payload, _ := json.Marshal(map[string]any{"browser_lease": lease.ID, "action": "refresh"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/adobe/run", bytes.NewReader(payload))
	if err != nil {
		return adobe.Account{}, errors.New("adobe_browser_request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return adobe.Account{}, errors.New("adobe_browser_refresh_unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return adobe.Account{}, errors.New("adobe_browser_refresh_failed")
	}
	var result struct {
		Stage   string        `json:"stage"`
		Account adobe.Account `json:"account"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result) != nil || result.Stage != "completed" {
		return adobe.Account{}, errors.New("adobe_browser_refresh_failed")
	}
	if result.Account.AccountID != account.AccountID || !strings.EqualFold(result.Account.Email, account.Email) {
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
