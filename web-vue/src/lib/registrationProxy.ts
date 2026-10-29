export const registrationResinChannel = 'resin:registration'

export function registrationProxyFailureText(value: string): string {
  if (value.includes('worker_error=signup_unavailable') || value.includes('upstream_code=email_signup_unavailable')) {
    return `xAI 拒绝本次邮箱注册，未进入收取验证码步骤；这不是邮件接收超时。具体限制原因未由上游说明，不自动补偿。诊断：${value}`
  }
  if (value.includes('worker_error=turnstile_incomplete')) {
    const network = value.includes('network_failures=') ? '检测到验证相关域名的网络失败，请核查代理链路。' : '请核查页面和代理链路。'
    return `安全验证未完成，尚未点击最终注册提交按钮；不自动补偿。${network}诊断：${value}`
  }
  if (value.startsWith('proxy: registration_cooldown')) {
    const until = value.match(/retry_after="([^"]+)"/)?.[1]
    const parsed = until ? new Date(until) : null
    const time = parsed && Number.isFinite(parsed.getTime()) ? parsed.toLocaleString() : ''
    return `注册代理冷却中${time ? `，可在 ${time} 后重试` : '，请稍后重试'}。到期不代表上游已恢复。`
  }
  if (value === 'proxy: binding_unavailable') return '注册代理绑定不可用，请检查所选渠道或节点配置；应用不会主动更换已绑定代理；Resin 内部故障切换仍由 Resin 管理。'
  if (value === 'config: invalid_resin_channel') return 'Resin 配置不完整或格式错误，请检查网关地址、Platform 和代理 Token。'
  return value
}
