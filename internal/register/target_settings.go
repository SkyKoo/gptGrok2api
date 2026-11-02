package register

import (
	"strings"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// 仅任务参数按渠道隔离，邮箱来源、渠道专属功能和运行状态保持各自的原有归属。
var targetSettingKeys = []string{"mode", "total", "threads", "target_quota", "target_available", "check_interval", "proxy", "resin_proxy_url"}

func registrationTarget(value any) string {
	target := strings.ToLower(strings.TrimSpace(stringValue(value)))
	if target == "" {
		return "openai"
	}
	return target
}

func taskSettings(raw map[string]any) map[string]any {
	defaults := defaultConfig()
	result := map[string]any{"proxy": ""}
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

// 旧配置仅有一套参数；首次读取时复制到两套独立配置，不改变已有渠道的出口。
func normalizeTargetSettings(value map[string]any) {
	target := registrationTarget(value["target"])
	profiles := object(value["target_settings"])
	normalized := map[string]any{}
	for _, name := range []string{"openai", "grok"} {
		profile := taskSettings(value)
		for _, key := range targetSettingKeys {
			if v, ok := object(profiles[name])[key]; ok {
				profile[key] = v
			}
		}
		if name == "grok" {
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
	value["target"] = target
}

func updateTargetSettings(current, updates map[string]any) {
	profiles := object(current["target_settings"])
	for _, target := range []string{"openai", "grok"} {
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
	if target == "openai" || target == "grok" {
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
