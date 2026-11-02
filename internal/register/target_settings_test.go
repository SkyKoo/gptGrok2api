package register

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTargetSettingsLegacyMigrationAndIndependentPersistence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "register.json")
	legacy := `{"target":"openai","mode":"quota","total":12,"threads":3,"target_quota":80,"target_available":15,"check_interval":120,"proxy":"http://proxy.example.test:8080","resin_proxy_url":"http://P:fixture@resin.example.test:2260"}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(path, filepath.Join(root, "accounts.json"))
	value := s.Get()
	profiles := object(value["target_settings"])
	if stringValue(object(profiles["openai"])["mode"]) != "quota" || stringValue(object(profiles["grok"])["mode"]) != "total" {
		t.Fatal("migration lost task mode")
	}
	if stringValue(object(profiles["grok"])["proxy"]) != "http://proxy.example.test:8080" {
		t.Fatal("migration silently changed egress")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != legacy {
		t.Fatal("read rewrote old configuration")
	}
	object(profiles["grok"])["total"] = 99
	if intValue(s.ConfigForTarget("openai")["total"]) != 12 {
		t.Fatal("read returned aliased profiles")
	}
	update := func(v map[string]any) {
		t.Helper()
		if _, err := s.Update(v); err != nil {
			t.Fatal(err)
		}
	}
	update(map[string]any{"target": "grok"})
	update(map[string]any{"total": 2, "threads": 1, "proxy": "direct", "resin_proxy_url": ""})
	update(map[string]any{"target": "openai"})
	value = s.Get()
	if value["mode"] != "quota" || intValue(value["total"]) != 12 || intValue(value["threads"]) != 3 || value["proxy"] != "http://proxy.example.test:8080" {
		t.Fatal("Grok overwrote OpenAI settings")
	}
	update(map[string]any{"target_settings": map[string]any{"grok": map[string]any{"total": 4, "enabled": true}}, "total": 10})
	s = New(path, filepath.Join(root, "accounts.json"))
	grok := s.ConfigForTarget("grok")
	if intValue(grok["total"]) != 4 || grok["proxy"] != "direct" || grok["enabled"] != false {
		t.Fatal("Grok profile did not survive restart or accepted runtime state")
	}
	if intValue(s.Get()["total"]) != 10 || s.Get()["target"] != "openai" {
		t.Fatal("target lookup changed selection")
	}
	update(map[string]any{"openai_survival": map[string]any{"enabled": false}})
	if s.ConfigForTarget("grok")["proxy"] != "direct" {
		t.Fatal("runtime update overwrote task profile")
	}
}

func TestTargetSettingsEnginesUseTheirOwnProxyAndBatch(t *testing.T) {
	root := t.TempDir()
	s := New(filepath.Join(root, "register.json"), filepath.Join(root, "accounts.json"))
	raw := freeTestConfig()
	raw["proxy"] = "http://openai-proxy.example.test:8080"
	raw["total"], raw["threads"] = 5, 2
	if _, err := s.Update(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(map[string]any{"target": "grok", "total": 1, "threads": 1, "proxy": "direct"}); err != nil {
		t.Fatal(err)
	}
	free, err := ParseFreeConfig(s.ConfigForTarget("openai"))
	if err != nil {
		t.Fatal(err)
	}
	grok, err := ParseGrokConfig(s.ConfigForTarget("grok"))
	if err != nil {
		t.Fatal(err)
	}
	if free.Total != 5 || free.Threads != 2 || free.Proxy != "http://openai-proxy.example.test:8080" || grok.Total != 1 || grok.Proxy != "direct" {
		t.Fatal("runtime configuration crossed targets")
	}
	for _, target := range []string{"openai", "grok"} {
		if _, err := s.Update(map[string]any{"target": target, "proxy": "resin:registration", "resin_proxy_url": "http://P:fixture@resin.example.test:2260"}); err != nil {
			t.Fatal(err)
		}
	}
	free, err = ParseFreeConfig(s.ConfigForTarget("openai"))
	if err != nil {
		t.Fatal(err)
	}
	grok, err = ParseGrokConfig(s.ConfigForTarget("grok"))
	if err != nil {
		t.Fatal(err)
	}
	if free.Proxy != "resin:registration:openai" || grok.Proxy != "resin:registration:grok" {
		t.Fatal("Resin channels were not scoped")
	}
}
