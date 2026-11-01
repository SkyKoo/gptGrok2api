import type { GrokAccount, GrokMediaQuotaCategory } from '../../api/grokAccounts'

const labels: Record<GrokMediaQuotaCategory, string> = {
  image: '普通生图', imagePro: 'Pro 生图', imageEdit: '图片编辑', video: '普通视频', video720p: '720p 视频',
}

function displayDate(value?: string): string {
  if (!value) return ''
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return ''
  return date.toLocaleString('zh-CN', { hour12: false })
}

export function grokMediaQuotaEntries(item: Pick<GrokAccount, 'media_quota'>) {
  const snapshot = item.media_quota
  return (Object.keys(labels) as GrokMediaQuotaCategory[]).map((key) => {
    const quota = snapshot?.status === 'success' ? snapshot.quotas?.[key] : undefined
    const reset = displayDate(quota?.next_available_at)
    const remaining = quota?.remaining
    const knownCount = typeof remaining === 'number' && Number.isSafeInteger(remaining) && remaining >= 0
    // The official UI treats available + zero/omitted count without a reset as
    // unspecified availability. Do not turn it into "exhausted" or "unlimited".
    const unspecified = quota?.available === true && !quota.next_available_at && (!knownCount || remaining === 0)
    let text = '未知'
    if (quota?.available === false) text = '暂不可用'
    else if (unspecified) text = '可用 · 次数未知'
    else if (knownCount) text = `${remaining} 次`
    else if (quota?.available === true) text = '可用 · 次数未知'
    return { key, label: labels[key], text, reset, exhausted: text === '0 次' || quota?.available === false }
  })
}

export function grokMediaQuotaNote(item: Pick<GrokAccount, 'media_quota'>): string {
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
  return '上次查询结果，实际生成仍可能限流'
}

export function grokMediaQuotaCheckedAt(item: Pick<GrokAccount, 'media_quota'>): string {
  return displayDate(item.media_quota?.checked_at)
}
