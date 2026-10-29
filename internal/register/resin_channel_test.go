package register

import (
	"context"
	"errors"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"strings"
	"testing"
	"time"
)

func TestRegistrationResinChannelConfigValidation(t *testing.T) {
	for _, platform := range []string{"openai", "grok"} {
		for _, raw := range []string{"", "http://resin.invalid:2260", "http://P.Account:secret@resin.invalid", "http://P:secret@resin.invalid:2260"} {
			cfg := freeTestConfig()
			if platform == "grok" {
				cfg = grokConfigFixture()
			}
			cfg["proxy"] = proxyruntime.RegistrationResinChannel
			cfg["resin_proxy_url"] = raw
			var err error
			if platform == "grok" {
				_, err = ParseGrokConfig(cfg)
			} else {
				_, err = ParseFreeConfig(cfg)
			}
			valid := raw == "http://P:secret@resin.invalid:2260"
			if valid && err != nil {
				t.Fatal(platform, err)
			}
			if !valid && safeFailure(err) != "config: invalid_resin_channel" {
				t.Fatalf("%s accepted malformed channel: %v", platform, err)
			}
			// Dormant Resin fields must never make optional direct mode mandatory.
			cfg["proxy"] = "direct"
			if platform == "grok" {
				_, err = ParseGrokConfig(cfg)
			} else {
				_, err = ParseFreeConfig(cfg)
			}
			if err != nil {
				t.Fatal("dormant channel invalidated direct", err)
			}
		}
	}
}

func TestChannelJournalAndTaskScopedCooldown(t *testing.T) {
	fixture := func() *ProxyRouting {
		m := proxyruntime.NewManager("", nil)
		m.ConfigureResinChannel(proxyruntime.RegistrationResinChannel, "http://P:private-secret@resin.invalid:2260")
		return &ProxyRouting{Manager: m}
	}
	routing := fixture()
	job := RegistrationJob{ID: "job-one"}
	get := func(string) (RegistrationJob, error) { return job, nil }
	change := func(_ string, f func(*RegistrationJob)) error { f(&job); return nil }
	selection := proxyruntime.RegistrationResinChannel
	release, err := routing.acquire(context.Background(), job.ID, "grok", &selection, get, change)
	if err != nil {
		t.Fatal(err)
	}
	release()
	first := selection
	routing.recordFailure(&job, fail("send_code", "email_signup_unavailable"))
	routing = fixture()
	routing.restore([]RegistrationJob{job})
	selection = proxyruntime.RegistrationResinChannel
	_, err = routing.acquire(context.Background(), job.ID, "grok", &selection, get, change)
	var f *Failure
	if !errors.As(err, &f) || f.Code != "registration_cooldown" || f.RetryAfter == "" || strings.Contains(f.Error(), "private-secret") {
		t.Fatal("missing safe cooldown diagnostic", err)
	}
	other, err := routing.Manager.AcquireTask(context.Background(), selection, "grok", "registration", nil, "job-two")
	if err != nil {
		t.Fatal("channel frozen", err)
	}
	other.Release()
	job.ProxyBinding.BlockedUntil = time.Now().Add(-time.Second)
	selection = "direct"
	release, err = routing.acquire(context.Background(), job.ID, "grok", &selection, get, change)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if selection != first {
		t.Fatal("cooldown expiration changed identity")
	}
}
