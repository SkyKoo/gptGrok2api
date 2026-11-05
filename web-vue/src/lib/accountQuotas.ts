type QuotaAccount = {
  quota?: number
  image_quota_unknown?: boolean
  capability_quotas?: Record<string, { remaining: number | null; reset_at?: string }>
  quota_pending?: Record<string, number>
}

const quotaLabels: Record<string, string> = {
  image_gen: '图片',
  reason: '推理',
  file_upload: '文件上传',
  paste_text_to_file: '长文本转文件',
  deep_research: '深度研究',
}

export function accountQuotaItems(account: QuotaAccount) {
  const snapshots = account.capability_quotas || {}
  const keys = [...new Set([...Object.keys(quotaLabels), ...Object.keys(snapshots), ...Object.keys(account.quota_pending || {})])]
  return keys.map((key) => {
    const snapshot = snapshots[key]
    // An explicitly unknown snapshot must not fall back to an older image count.
    const raw = snapshot ? snapshot.remaining : key === 'image_gen' && !account.image_quota_unknown ? account.quota : null
    const remaining = typeof raw === 'number' && Number.isFinite(raw) && raw >= 0 ? Math.trunc(raw) : null
    const pending = account.quota_pending?.[key] || 0
    return {
      key,
      label: quotaLabels[key] || key,
      remaining,
      pending: Number.isFinite(pending) && pending > 0 ? Math.trunc(pending) : 0,
      resetAt: snapshot?.reset_at || '',
      text: remaining === null ? '未知' : String(remaining),
      tone: remaining === null ? 'muted' as const : remaining === 0 ? 'danger' as const : 'success' as const,
    }
  })
}

export interface QuotaHistoryEvent {
  at: string
  source: string
  changes: Record<string, { before: number | null; after: number | null }>
}

export function quotaCheckReason(reason?: string): string {
  return ({
    unknown: '等待首次核对',
    periodic: '定期核对',
    reconcile: '等待核对使用量',
    reset_due: '核对到期额度',
    busy: '账号忙碌，稍后核对',
    backoff: '查询失败，等待重试',
    disabled: '账号已停用',
    account_validation: '等待账号验证',
  } as Record<string, string>)[reason || ''] || '等待检查计划'
}

export function quotaHistoryText(event: QuotaHistoryEvent): string {
  return Object.entries(event.changes || {}).map(([key, change]) => {
    const number = (value: number | null) => typeof value === 'number' && Number.isFinite(value) && value >= 0 ? String(value) : '未知'
    return `${quotaLabels[key] || key} ${number(change.before)} → ${number(change.after)}`
  }).join('；')
}
