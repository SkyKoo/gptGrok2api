import type { GrokReloginJob } from '@/api/grokAccounts'

export const grokReloginSteps = [
  { key: 'waiting_browser', label: '等待浏览器' },
  { key: 'starting_browser', label: '启动浏览器' },
  { key: 'signing_in', label: '正在登录' },
  { key: 'syncing', label: '同步调用池' },
  { key: 'refreshing_quota', label: '刷新额度' },
]

export function grokReloginActive(job: GrokReloginJob) {
  return job.status === 'queued' || job.status === 'running'
}

const details: Record<string, string> = {
  grok_relogin_verification_required: 'Grok 要求额外验证，请在官网完成',
  grok_relogin_verification_widget_visible: '登录页显示安全验证组件，自动重登已停止，请在 Grok 官网查看',
  grok_relogin_password_submit_disabled: '点击密码提交按钮超时，复查时按钮处于禁用状态',
  grok_relogin_age_confirmation_required: '请用该账号在 Grok 官网完成年龄确认，再回到这里重新登录',
  grok_relogin_login_page_unrecognized: '未识别当前 Grok 登录页面',
  grok_relogin_login_action_ambiguous: '登录表单中的按钮用途不明确，本次已停止',
  grok_relogin_credentials_rejected: 'Grok 未接受账号或密码',
  grok_relogin_upstream_rate_limited: 'Grok 暂时限制登录请求',
  grok_relogin_password_input_unconfirmed: '密码输入未确认，登录未完成',
  grok_relogin_password_request_unobserved: '点击提交后仍停在密码页，未观察到发往认证服务的 POST 请求',
  grok_relogin_login_entry_forbidden: 'Grok 登录入口拒绝访问（HTTP 403），本次未提交账号密码',
  grok_relogin_session_wait_timeout: '已点击密码提交，等待新会话超时，原会话仍保留',
  grok_relogin_login_timeout: '等待 Grok 登录完成超时',
  grok_relogin_identity_mismatch: '新会话与所选账号身份不一致',
  grok_relogin_identity_unconfirmed: '会话缺少可核对的账号信息，无法确认属于所选账号',
  grok_relogin_session_unchanged: '返回的登录凭据与原来相同，未取得新会话',
  grok_relogin_proxy_unavailable: '账号绑定的代理出口不可用',
  grok_relogin_browser_unavailable: '共享浏览器暂不可用',
  grok_relogin_account_busy: '账号正在运行请求或等待额度结算',
  grok_relogin_account_changed: '账号或登录凭据已变化',
  grok_relogin_sync_failed: '调用池同步未完成，再次点击将继续同步已保存的会话',
  grok_relogin_interrupted: '任务中断，请核对登录状态后重试',
  grok_relogin_result_unknown: '浏览器登录结果未知',
}

export function grokReloginDetail(job?: GrokReloginJob) {
  if (!job) return ''
  if (job.status === 'completed' && job.result?.session_updated) {
    const missing = []
    if (!job.result.text_quota) missing.push('文字')
    if (!job.result.image_quota) missing.push('图片')
    if (!job.result.video_quota) missing.push('视频')
    return missing.length ? '会话已更新，额度待确认（' + missing.join('、') + '）' : '会话已更新，文字、图片和视频额度已刷新'
  }
  if (job.status === 'cancelled') return '任务已取消'
  return details[job.error || ''] || (job.status === 'failed' ? '重新登录失败，未确认取得新会话' : '')
}

export function grokReloginLabel(job?: GrokReloginJob) {
  if (!job) return ''
  if (job.status === 'failed' && job.error === 'grok_relogin_age_confirmation_required') return '需确认年龄'
  if (job.status === 'failed' && job.error === 'grok_relogin_verification_required') return '需人工验证'
  if (job.status === 'completed') {
    if (!job.result?.session_updated) return '已结束，待确认会话'
    return job.result.text_quota && job.result.image_quota && job.result.video_quota ? '已重新登录' : '已登录，待确认额度'
  }
  if (job.status === 'failed' || job.status === 'cancelled') return '重新登录未完成'
  if (job.status === 'queued') return '等待浏览器'
  return grokReloginSteps.find(step => step.key === job.result?.stage)?.label || '重新登录中'
}
