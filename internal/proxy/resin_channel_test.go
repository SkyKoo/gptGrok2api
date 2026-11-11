package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func channelManager(raw string) *Manager {
	m := NewManager("http://fallback.invalid", nil)
	m.ConfigureResinChannel(RegistrationResinChannel, raw)
	return m
}

func TestResinChannelTaskAndAccountRecoveryWithoutGroups(t *testing.T) {
	raw := "http://P:private-token@resin.invalid:2260"
	m := channelManager(raw)
	first, err := m.AcquireTask(context.Background(), RegistrationResinChannel, "grok", "registration", nil, "first")
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	if first.Binding.Reference != RegistrationResinChannel || first.Binding.GroupID != "" || first.Binding.NodeID != "" || first.Binding.ResinAccount == "" {
		t.Fatal("channel was treated as a node")
	}
	encoded, _ := json.Marshal(first.Binding)
	if strings.Contains(string(encoded), "private-token") {
		t.Fatal("credential leaked to binding")
	}
	var saved Binding
	if err := json.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	restarted := channelManager(raw)
	again, err := restarted.AcquireTask(context.Background(), "direct", "grok", "registration", &saved, "first")
	if err != nil || again.URL != first.URL {
		t.Fatal("retry lost identity", err)
	}
	for _, platform := range []string{"grok", "openai"} {
		other, err := restarted.AcquireTask(context.Background(), RegistrationResinChannel, platform, "registration", nil, "second")
		if err != nil || other.Binding.ResinAccount == saved.ResinAccount {
			t.Fatal("task identity reused", err)
		}
	}
	fields := map[string]any{"proxy_policy": AccountProxyExplicit, "proxy_binding": saved}
	got, bound, err := restarted.ResolveBound(fields)
	if err != nil || !bound || got != first.URL {
		t.Fatal("account lost channel binding", err)
	}
	if lease := restarted.AcquireImage(fields); lease.URL != first.URL {
		t.Fatal("image request lost channel binding")
	} else {
		lease.Release(true)
	}
	// Settings/group refresh must not discard the independent channel.
	restarted.ConfigureTaskGroups(nil)
	if got, _, err := restarted.ResolveBound(fields); err != nil || got != first.URL {
		t.Fatal("group refresh removed channel", err)
	}
	for _, changed := range []string{"", "http://Other:private-token@resin.invalid:2260", "http://P:changed-token@resin.invalid:2260"} {
		restarted.ConfigureResinChannel(RegistrationResinChannel, changed)
		if _, err := restarted.AcquireTask(context.Background(), "direct", "grok", "registration", &saved); !errors.Is(err, ErrBindingUnavailable) {
			t.Fatal("changed gateway allowed retry", err)
		}
		if _, _, err := restarted.ResolveBound(fields); !errors.Is(err, ErrBindingUnavailable) {
			t.Fatal("account fell back", err)
		}
	}
}

func TestResinCooldownDoesNotFreezeOtherAccountsOnChannelOrLegacyGateway(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		m := channelManager("http://P:token@resin.invalid:2260")
		selection := RegistrationResinChannel
		if legacy {
			m = resinManager("http://P:token@resin.invalid:2260")
			selection = "group:resin"
		}
		first, err := m.AcquireTask(context.Background(), selection, "grok", "registration", nil, "first")
		if err != nil {
			t.Fatal(err)
		}
		first.Release()
		first.Binding.BlockedUntil = time.Now().Add(time.Minute)
		m.BlockTaskScope(first.Binding, first.Binding.BlockedUntil)
		_, err = m.AcquireTask(context.Background(), selection, "grok", "registration", &first.Binding, "first")
		var cooldown *BindingCooldownError
		if !errors.As(err, &cooldown) || !cooldown.Until.Equal(first.Binding.BlockedUntil) {
			t.Fatal("retry ignored cooldown", err)
		}
		other, err := m.AcquireTask(context.Background(), selection, "grok", "registration", nil, "second")
		if err != nil {
			t.Fatal("single refusal froze channel", err)
		}
		other.Release()
		first.Binding.BlockedUntil = time.Now().Add(-time.Minute)
		resumed, err := m.AcquireTask(context.Background(), selection, "grok", "registration", &first.Binding, "first")
		if err != nil || resumed.URL != first.URL {
			t.Fatal("resuming changed identity", err)
		}
		resumed.Release()
	}
}

func TestLegacyResinRootPathBindingKeepsIdentityAcrossRestart(t *testing.T) {
	raw := "http://CFM-Grok:p%40ss%3A%2F%3F%23%25@resin.invalid:2260/"
	// This is the persisted binding produced before URL normalization existed.
	saved := Binding{Reference: RegistrationResinChannel, EndpointHash: proxyHash(raw),
		Platform: "grok", Purpose: "registration", ResinPlatform: "CFM-Grok",
		ResinAccount: "task-existing", BoundAt: "2026-11-06T13:43:22Z"}
	before, _ := json.Marshal(saved)
	manager := channelManager(raw)
	lease, err := manager.AcquireTask(context.Background(), "direct", "grok", "registration", &saved, "ignored-new-id")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	want := "http://CFM-Grok.task-existing:p%40ss%3A%2F%3F%23%25@resin.invalid:2260"
	if lease.URL != want || lease.Binding != saved || manager.resinChannels[RegistrationResinChannel] != raw {
		t.Fatal("URL normalization modified a stored binding or configuration")
	}
	after, _ := json.Marshal(saved)
	if string(before) != string(after) {
		t.Fatal("caller binding was mutated")
	}
	resolved, bound, err := manager.ResolveBound(map[string]any{"proxy_policy": AccountProxyExplicit, "proxy_binding": saved})
	if err != nil || !bound || resolved != want {
		t.Fatal("persisted account lost its normalized proxy", err)
	}
	// Removing the saved slash is still a configuration change: do not silently
	// weaken endpoint matching to make the runtime normalization work.
	manager.ConfigureResinChannel(RegistrationResinChannel, strings.TrimSuffix(raw, "/"))
	if _, err := manager.AcquireTask(context.Background(), "direct", "grok", "registration", &saved); !errors.Is(err, ErrBindingUnavailable) {
		t.Fatal("endpoint fingerprint check was weakened", err)
	}
}
