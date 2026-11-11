package register

import (
	"strings"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// 任务参数和邮箱来源选择按渠道隔离；来源连接与凭据由共享邮箱资产模块管理。
var targetSettingKeys = []string{"mode", "total", "threads", "target_quota", "target_available", "check_interval", "proxy", "resin_proxy_url", "mail_source_id"}

func registrationTarget(value any) string {
	target := strings.ToLower(strings.TrimSpace(stringValue(value)))
	if target == "" {
		return "openai"
	}
	return target
}

func taskSettings(raw map[string]any) map[string]any {
	defaults := defaultConfig()
	result := map[string]any{"proxy": "", "mail_source_id": stringValue(object(raw["mail"])["source_id"])}
	for _, key := range targetSettingKeys {
		if value, ok := defaults[key]; ok {
			result[key] = value
		}
		if value, ok := raw[key]; ok {
			result[key] = value
		}
	}
	return result
}

// 旧配置仅有一套参数；首次读取时复制到各渠道独立配置，不改变已有渠道的出口。
func normalizeTargetSettings(value map[string]any) {
	normalizeAdobeSettings(value)
	target := registrationTarget(value["target"])
	profiles := object(value["target_settings"])
	normalized := map[string]any{}
	for _, name := range []string{"openai", "grok", "adobe"} {
		profile := taskSettings(value)
		for _, key := range targetSettingKeys {
			if v, ok := object(profiles[name])[key]; ok {
				profile[key] = v
			}
		}
		if name == "grok" || name == "adobe" {
			profile["mode"] = "total"
		}
		normalized[name] = profile
	}
	value["target_settings"] = normalized
	value["target"] = target
	// 旧任务/账号已经保存的 Resin 引用固定跟随迁移前的渠道，切换页面不会改绑。
	if stringValue(value["legacy_resin_target"]) == "" {
		value["legacy_resin_target"] = target
	}
	applyTargetSettings(value, target)
}

func applyTargetSettings(value map[string]any, target string) {
	for key, item := range object(object(value["target_settings"])[target]) {
		value[key] = item
	}
	mail := object(value["mail"])
	mail["source_id"] = value["mail_source_id"]
	value["mail"] = mail
	value["target"] = target
}

func updateTargetSettings(current, updates map[string]any) {
	profiles := object(current["target_settings"])
	for _, target := range []string{"openai", "grok", "adobe"} {
		profile := object(profiles[target])
		for _, key := range targetSettingKeys {
			if v, ok := object(object(updates["target_settings"])[target])[key]; ok {
				profile[key] = v
			}
		}
		profiles[target] = profile
	}
	target := registrationTarget(current["target"])
	if next, ok := updates["target"]; ok {
		target = registrationTarget(next)
	}
	applyTargetSettings(current, target)
	for key, value := range updates {
		switch key {
		case "logs", "grok_oauth_logs", "target", "target_settings", "legacy_resin_target":
			continue
		}
		current[key] = value
	}
	if source, ok := object(updates["mail"])["source_id"]; ok {
		current["mail_source_id"] = source
	}
	if target == "openai" || target == "grok" || target == "adobe" {
		profiles[target] = taskSettings(current)
	}
	normalizeTargetSettings(current)
}

// ConfigForTarget 读取指定渠道的已保存参数，不修改页面当前选中的渠道。
func (s *Store) ConfigForTarget(target string) map[string]any {
	value := s.Get()
	applyTargetSettings(value, registrationTarget(target))
	return value
}

func registrationProxyForTarget(raw map[string]any, target, selection string) string {
	if selection == proxyruntime.RegistrationResinChannel && len(object(raw["target_settings"])) > 0 {
		return proxyruntime.RegistrationResinChannel + ":" + target
	}
	return selection
}

// UpdateForTarget saves only the named channel. A browser tab is not a global
// routing switch; stale forms must not overwrite another channel's settings.
func (s *Store) UpdateForTarget(target string, updates map[string]any) (map[string]any, error) {
	target = strings.ToLower(strings.TrimSpace(target))
	if target != "openai" && target != "grok" && target != "adobe" {
		return nil, fail("config", "invalid_registration_target")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.getLocked()
	selected := registrationTarget(current["target"])
	scoped := map[string]any{"target": target}
	profile := object(object(updates["target_settings"])[target])
	for _, key := range targetSettingKeys {
		if value, ok := profile[key]; ok {
			scoped[key] = value
		}
		if value, ok := updates[key]; ok {
			scoped[key] = value
		}
	}
	if source, ok := object(updates["mail"])["source_id"]; ok {
		scoped["mail_source_id"] = source
	}
	keys := []string{target}
	if target == "openai" {
		keys = []string{"openai_free", "checkout", "sub2api_sync", "cpa_sync", "agent_identity_archive"}
	}
	for _, key := range keys {
		if value, ok := updates[key]; ok {
			scoped[key] = value
		}
	}
	updateTargetSettings(current, scoped)
	applyTargetSettings(current, selected)
	if err := writeJSON0600(s.configPath, current); err != nil {
		return nil, err
	}
	result := cloneMap(current)
	applyTargetSettings(result, target)
	return result, nil
}
