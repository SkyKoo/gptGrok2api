package register

import (
	"testing"
	"time"
)

func grokConfigFixture() map[string]any {
	return map[string]any{
		"target":  "grok",
		"mode":    "total",
		"total":   2,
		"threads": 1,
		"mail": map[string]any{
			"providers": []any{map[string]any{
				"type":           "icloud_hme",
				"enable":         true,
				"api_base":       "https://mail.example.test",
				"admin_password": "secret",
				"account_id":     "hme-account",
			}},
		},
		"grok": map[string]any{
			"provider":              "browser",
			"api_base":              "http://grok-browser:8899",
			"request_timeout":       45,
			"captcha_timeout":       180,
			"captcha_poll_interval": 3,
		},
	}
}

func TestParseGrokConfigReusesHMEAndProtocolDefaults(t *testing.T) {
	cfg, err := ParseGrokConfig(grokConfigFixture())
	if err != nil {
		t.Fatalf("ParseGrokConfig() error = %v", err)
	}
	if cfg.Total != 2 || cfg.Threads != 1 {
		t.Fatalf("unexpected batch settings: %+v", cfg)
	}
	if cfg.HME.BaseURL != "https://mail.example.test" || cfg.HME.AccountID != "hme-account" {
		t.Fatalf("HME config was not reused: %+v", cfg.HME)
	}
	if cfg.Turnstile.APIBase != "http://grok-browser:8899" {
		t.Fatalf("unexpected Turnstile config: %+v", cfg.Turnstile)
	}
	if cfg.RequestTimeout != 45*time.Second || cfg.Turnstile.Timeout != 180*time.Second {
		t.Fatalf("unexpected timeouts: request=%s captcha=%s", cfg.RequestTimeout, cfg.Turnstile.Timeout)
	}
}

func TestParseGrokConfigRejectsUnsupportedTarget(t *testing.T) {
	raw := grokConfigFixture()
	raw["target"] = "openai"
	if _, err := ParseGrokConfig(raw); err == nil {
		t.Fatal("ParseGrokConfig() accepted a non-Grok target")
	}
}

func TestParseGrokConfigRejectsLegacySolver(t *testing.T) {
	raw := grokConfigFixture()
	raw["grok"] = map[string]any{"provider": "local", "api_base": "http://127.0.0.1:8877"}
	if _, err := ParseGrokConfig(raw); err == nil {
		t.Fatal("ParseGrokConfig() accepted the removed legacy solver")
	}
}

func TestParseGrokConfigDefaultsBuiltInBrowserWorker(t *testing.T) {
	raw := grokConfigFixture()
	delete(raw, "grok")
	cfg, err := ParseGrokConfig(raw)
	if err != nil {
		t.Fatalf("ParseGrokConfig() error = %v", err)
	}
	if cfg.Turnstile.Provider != "browser" {
		t.Fatalf("unexpected default provider: %q", cfg.Turnstile.Provider)
	}
	if cfg.Turnstile.APIBase == "" {
		t.Fatal("expected the built-in browser worker URL")
	}
}
