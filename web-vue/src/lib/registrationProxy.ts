export const registrationResinChannel = 'resin:registration'

export function registrationProxyFailureText(value: string): string {
  if (value.startsWith('proxy: registration_cooldown')) {
    const until = value.match(/retry_after="([^"]+)"/)?.[1]
    const parsed = until ? new Date(until) : null
    const time = parsed && Number.isFinite(parsed.getTime()) ? parsed.toLocaleString() : ''
    return `注册代理冷却中${time ? `，可在 ${time} 后重试` : '，请稍后重试'}。到期不代表上游已恢复。`
  }
  if (value === 'proxy: binding_unavailable') return '注册代理绑定不可用，请检查所选渠道或节点配置；已有任务不会自动切换出口。'
  if (value === 'config: invalid_resin_channel') return 'Resin 配置不完整或格式错误，请检查网关地址、Platform 和代理 Token。'
  return value
}
