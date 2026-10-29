package register

import (
	"net/url"
	"os"
	"strings"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// GrokTurnstileConfig describes the external Turnstile provider used by the
// protocol flow. The Grok protocol does not use OpenAI Sentinel; the provider
// is deliberately a separate seam so the registration client never fabricates
// a token itself.
type GrokTurnstileConfig struct {
	Provider     string
	APIBase      string
	APIKey       string
	CreatePath   string
	ResultPath   string
	Timeout      time.Duration
	PollInterval time.Duration
}

// GrokConfig is the Grok counterpart of FreeConfig. It keeps the same mailbox,
// proxy, timeout and batch controls while leaving the upstream protocol fields
// private to the Grok registrar.
type GrokConfig struct {
	Total, Threads                     int
	Proxy                              string
	Timeout, MailTimeout, PollInterval time.Duration
	RequestTimeout                     time.Duration
	HME                                HMEConfig
	MailboxSourceID                    string
	DeleteMailboxOnAccountDelete       bool
	Turnstile                          GrokTurnstileConfig
	BrowserProfile                     *BrowserProfile
}

func ParseGrokConfig(raw map[string]any) (GrokConfig, error) {
	c := GrokConfig{
		Timeout:        10 * time.Minute,
		MailTimeout:    3 * time.Minute,
		PollInterval:   3 * time.Second,
		RequestTimeout: 45 * time.Second,
	}
	if !strings.EqualFold(strings.TrimSpace(stringValue(raw["target"])), "grok") {
		return c, fail("config", "grok_target_required")
	}
	if mode := stringValue(raw["mode"]); mode != "" && mode != "total" {
		return c, fail("config", "grok_use_total_mode")
	}
	c.Total, c.Threads = intValue(raw["total"]), intValue(raw["threads"])
	if c.Total < 1 || c.Total > 100 {
		return c, fail("config", "total_must_be_1_to_100")
	}
	if c.Threads < 1 || c.Threads > 100 {
		return c, fail("config", "threads_must_be_1_to_100")
	}
	c.Proxy = stringValue(raw["proxy"])
	if c.Proxy == proxyruntime.RegistrationResinChannel && proxyruntime.ValidateNodeType("resin", stringValue(raw["resin_proxy_url"])) != nil {
		return c, fail("config", "invalid_resin_channel")
	}
	if c.Proxy == "group:" {
		return c, fail("config", "proxy_group_required")
	}
	if c.Proxy != "" && c.Proxy != "direct" && c.Proxy != proxyruntime.RegistrationResinChannel && !strings.HasPrefix(c.Proxy, "group:") {
		u, err := url.Parse(c.Proxy)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
			return c, fail("config", "invalid_proxy")
		}
	}
	mail := object(raw["mail"])
	if value := intValue(mail["wait_timeout"]); value != 0 {
		if value < 1 || value > 900 {
			return c, fail("config", "mail_timeout_must_be_1_to_900_seconds")
		}
		c.MailTimeout = time.Duration(value) * time.Second
	}
	if value := intValue(mail["wait_interval"]); value != 0 {
		if value < 1 || value > 60 {
			return c, fail("config", "mail_interval_must_be_1_to_60_seconds")
		}
		c.PollInterval = time.Duration(value) * time.Second
	}
	if value := intValue(object(raw["grok"])["request_timeout"]); value != 0 {
		if value < 10 || value > 180 {
			return c, fail("config", "grok_request_timeout_must_be_10_to_180_seconds")
		}
		c.RequestTimeout = time.Duration(value) * time.Second
	}
	if value := intValue(object(raw["grok"])["captcha_timeout"]); value != 0 {
		if value < 30 || value > 900 {
			return c, fail("config", "grok_captcha_timeout_must_be_30_to_900_seconds")
		}
		c.Turnstile.Timeout = time.Duration(value) * time.Second
	}
	if c.Turnstile.Timeout == 0 {
		c.Turnstile.Timeout = 180 * time.Second
	}
	if value := intValue(object(raw["grok"])["captcha_poll_interval"]); value != 0 {
		if value < 1 || value > 30 {
			return c, fail("config", "grok_captcha_poll_interval_must_be_1_to_30_seconds")
		}
		c.Turnstile.PollInterval = time.Duration(value) * time.Second
	}
	if c.Turnstile.PollInterval == 0 {
		c.Turnstile.PollInterval = 3 * time.Second
	}
	grok := object(raw["grok"])
	c.Turnstile.Provider = strings.ToLower(strings.TrimSpace(stringValue(grok["provider"])))
	// The persisted configuration from the clean baseline predates the Grok
	// provider block. Treat an omitted block as the built-in browser worker so
	// old saved settings can be migrated by the normal frontend autosave path.
	if c.Turnstile.Provider == "" {
		c.Turnstile.Provider = "browser"
	}
	c.Turnstile.APIBase = strings.TrimRight(strings.TrimSpace(stringValue(grok["api_base"])), "/")
	c.Turnstile.APIKey = strings.TrimSpace(stringValue(grok["api_key"]))
	c.Turnstile.CreatePath = stringValue(grok["create_path"])
	c.Turnstile.ResultPath = stringValue(grok["result_path"])
	if c.Turnstile.CreatePath == "" {
		c.Turnstile.CreatePath = "/createTask"
	}
	if c.Turnstile.ResultPath == "" {
		c.Turnstile.ResultPath = "/getTaskResult"
	}
	if c.Turnstile.Provider == "" {
		return c, fail("config", "grok_turnstile_provider_required")
	}
	if c.Turnstile.Provider != "browser" {
		return c, fail("config", "grok_browser_provider_required")
	}
	if c.Turnstile.APIBase == "" {
		c.Turnstile.APIBase = strings.TrimRight(strings.TrimSpace(os.Getenv("CFM_GROK_BROWSER_URL")), "/")
		if c.Turnstile.APIBase == "" {
			c.Turnstile.APIBase = "http://grok-browser:8899"
		}
	}
	if c.Turnstile.APIBase == "" {
		return c, fail("config", "grok_turnstile_api_base_required")
	}
	if parsed, err := url.Parse(c.Turnstile.APIBase); err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return c, fail("config", "invalid_grok_turnstile_api_base")
	}
	// Grok currently reuses the same HME mailbox source as GPT. Normalize the
	// target only for validation; the OpenAI flow itself is never started.
	normalized := cloneMap(raw)
	normalized["target"] = "openai"
	free, err := ParseFreeConfig(normalized)
	if err != nil {
		return c, err
	}
	c.HME = free.HME
	c.MailboxSourceID = free.MailboxSourceID
	c.DeleteMailboxOnAccountDelete = free.DeleteMailboxOnAccountDelete
	return c, nil
}
