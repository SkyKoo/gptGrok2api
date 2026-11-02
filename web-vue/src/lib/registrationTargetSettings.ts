import type { LegacyRegisterConfig, RegisterTarget, RegistrationTaskSettings } from '../api/register'

type TaskConfig = Partial<RegistrationTaskSettings> & {
  mail?: { source_id?: string }
  target?: string
  target_settings?: Partial<Record<RegisterTarget, Partial<RegistrationTaskSettings>>>
}

export function registrationTaskSettings(raw: Partial<RegistrationTaskSettings>, target: RegisterTarget): RegistrationTaskSettings {
  return {
    mail_source_id: String(raw.mail_source_id || '').trim(),
    proxy: String(raw.proxy || '').trim(),
    resin_proxy_url: String(raw.resin_proxy_url || '').trim(),
    total: Math.max(1, Number(raw.total) || 1),
    threads: Math.max(1, Number(raw.threads) || 1),
    mode: target === 'grok' ? 'total' : raw.mode || 'total',
    target_quota: Math.max(1, Number(raw.target_quota) || 1),
    target_available: Math.max(1, Number(raw.target_available) || 1),
    check_interval: Math.max(1, Number(raw.check_interval) || 5),
  }
}

export function normalizeRegistrationTargetSettings(raw: TaskConfig): Record<RegisterTarget, RegistrationTaskSettings> {
  return {
    openai: registrationTaskSettings({ ...raw, mail_source_id: raw.mail_source_id ?? raw.mail?.source_id, ...raw.target_settings?.openai }, 'openai'),
    grok: registrationTaskSettings({ ...raw, mail_source_id: raw.mail_source_id ?? raw.mail?.source_id, ...raw.target_settings?.grok }, 'grok'),
  }
}

// 当前表单可能有尚未提交的修改，切换或保存前先写回所属渠道的草稿。
export function registrationTargetSettingsPayload(config: TaskConfig): Record<RegisterTarget, RegistrationTaskSettings> {
  const profiles = normalizeRegistrationTargetSettings(config)
  const target = config.target === 'grok' ? 'grok' : 'openai'
  profiles[target] = registrationTaskSettings({ ...config, mail_source_id: config.mail?.source_id ?? config.mail_source_id }, target)
  return profiles
}

export function switchRegistrationTarget(config: LegacyRegisterConfig, target: RegisterTarget): void {
  const profiles = registrationTargetSettingsPayload(config)
  config.target_settings = profiles
  config.target = target
  Object.assign(config, profiles[target])
  if (config.mail) config.mail.source_id = profiles[target].mail_source_id
}
