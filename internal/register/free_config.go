package register

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Failure contains only a fixed diagnostic code, never an upstream response or secret.
type Failure struct {
	Stage, Code string
	HTTPStatus  int
}

func (e *Failure) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Stage, e.Code, e.HTTPStatus)
	}
	return e.Stage + ": " + e.Code
}
func fail(stage, code string) error { return &Failure{Stage: stage, Code: code} }
func safeFailure(err error) string {
	var f *Failure
	switch {
	case errors.Is(err, context.Canceled):
		return "task_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "task_timeout"
	case errors.As(err, &f):
		return f.Error()
	default:
		return "internal_error"
	}
}

type FreeConfig struct {
	Proxy                              string
	Timeout, MailTimeout, PollInterval time.Duration
	HME                                HMEConfig
}

// ParseFreeConfig deliberately supports a bounded, single-account run. Unknown
// legacy modes/providers are rejected before any mailbox or account is created.
func ParseFreeConfig(raw map[string]any) (FreeConfig, error) {
	c := FreeConfig{Timeout: 10 * time.Minute, MailTimeout: 3 * time.Minute, PollInterval: 3 * time.Second}
	if stringValue(raw["target"]) != "openai" {
		return c, fail("config", "openai_target_required")
	}
	if stringValue(raw["mode"]) != "total" || intValue(raw["total"]) != 1 || intValue(raw["threads"]) != 1 {
		return c, fail("config", "use_total_mode_with_total_1_and_threads_1")
	}
	for _, key := range []string{"checkout", "sub2api_sync", "cpa_sync", "agent_identity_archive"} {
		if boolValue(object(raw[key])["enabled"], false) {
			return c, fail("config", "disable_"+key)
		}
	}
	profile := object(raw["openai_free"])
	if value := intValue(profile["timeout_seconds"]); value != 0 {
		if value < 60 || value > 1800 {
			return c, fail("config", "timeout_must_be_60_to_1800_seconds")
		}
		c.Timeout = time.Duration(value) * time.Second
	}
	c.Proxy = stringValue(raw["proxy"])
	if strings.HasPrefix(c.Proxy, "group:") {
		return c, fail("config", "use_global_direct_or_explicit_proxy")
	}
	if c.Proxy != "" && c.Proxy != "direct" {
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
	providers, _ := mail["providers"].([]any)
	count := 0
	for _, rawProvider := range providers {
		p := object(rawProvider)
		if !boolValue(p["enable"], true) {
			continue
		}
		count++
		if stringValue(p["type"]) != "icloud_hme" {
			return c, fail("config", "only_icloud_hme_supported")
		}
		password, _ := p["admin_password"].(string)
		c.HME = HMEConfig{BaseURL: strings.TrimRight(stringValue(p["api_base"]), "/"), Password: password, AccountID: stringValue(p["account_id"])}
	}
	if count != 1 {
		return c, fail("config", "enable_exactly_one_icloud_hme_provider")
	}
	if err := c.HME.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func object(value any) map[string]any {
	v, _ := value.(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}
func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
