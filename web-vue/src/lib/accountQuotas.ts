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
