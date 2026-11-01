import type { GrokAccount } from '../../api/grokAccounts'

const labels = {
  imagePro: '图片额度（Pro／编辑共用）', video720p: '视频额度（720p）',
}

function displayDate(value?: string): string {
  if (!value) return ''
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return ''
  return date.toLocaleString('zh-CN', { hour12: false })
}

export function grokMediaQuotaEntries(item: Pick<GrokAccount, 'media_quota' | 'media_quota_refresh_needed' | 'media_quota_pending'>) {
  const snapshot = item.media_quota
  return (Object.keys(labels) as (keyof typeof labels)[]).map((key) => {
    const quota = snapshot?.status === 'success' ? snapshot.quotas?.[key] : undefined
    const reset = displayDate(quota?.next_available_at)
    const remaining = quota?.remaining
    const knownCount = typeof remaining === 'number' && Number.isSafeInteger(remaining) && remaining >= 0
    // The official UI treats available + zero/omitted count without a reset as
    // unspecified availability. Do not turn it into "exhausted" or "unlimited".
    const unspecified = quota?.available === true && !quota.next_available_at && (!knownCount || remaining === 0)
    let text = snapshot?.status === 'success' ? '上游未提供额度' : '未知'
    if (quota?.available === false) text = '暂不可用'
    else if (unspecified) text = '可用 · 次数未知'
    else if (knownCount) text = `${remaining} 次`
    else if (quota?.available === true) text = '可用 · 次数未知'
    const pending = item.media_quota_pending?.[key === 'imagePro' ? 'image_gen' : 'video_gen'] || 0
    const exhausted = text === '0 次' || quota?.available === false
    if (snapshot?.status === 'success' && item.media_quota_refresh_needed) text = '等待结算刷新'
    else if (knownCount && pending > 0) text = `${remaining} 次 · 已预占 ${pending}`
    return { key, label: labels[key], text, reset, exhausted }
  })
}

export function grokMediaQuotaNote(item: Pick<GrokAccount, 'media_quota' | 'media_quota_refresh_needed' | 'media_quota_pending'>): string {
  const snapshot = item.media_quota
  if (!snapshot) return '尚未查询'
  if (snapshot.status !== 'success') {
    const code = snapshot.error || ''
    const reason = /^HTTP \d{3}$/.test(code) ? code : ({
      timeout: '请求超时', network_error: '网络错误', invalid_response: '返回数据无效',
      not_configured: '未配置', missing_credentials: '缺少登录态',
    } as Record<string, string>)[code] || '查询未完成'
    return `查询失败 · ${reason}`
  }
  if (item.media_quota_refresh_needed) return '生成后等待额度结算，刷新前暂停该账号的媒体请求'
  return '共享额度，生成与编辑统一预占；生成后等待结算并刷新，实际扣减以上游为准'
}

export function grokMediaQuotaCheckedAt(item: Pick<GrokAccount, 'media_quota' | 'media_quota_refresh_needed' | 'media_quota_pending'>): string {
  return displayDate(item.media_quota?.checked_at)
}
