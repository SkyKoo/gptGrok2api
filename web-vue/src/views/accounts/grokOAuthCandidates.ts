import type { GrokOAuthAccount, GrokOAuthCandidate, GrokOAuthImportRequest, GrokOAuthProtocolJob } from '@/api/grokOAuthAccounts'

export const xaiOAuthStatusLabels: Record<GrokOAuthCandidate['oauth_status'], string> = {
  pending: '待授权', queued: '排队中', running: '授权中', manual_action_required: '需处理', authorized: '已授权', failed: '授权失败', requires_renewal: '需重新授权',
}

export type GrokOAuthRow = GrokOAuthCandidate & {
  source_account_id?: string
  account?: GrokOAuthAccount
  job?: GrokOAuthProtocolJob
  poll_error?: string
  action_error?: string
  quality_running?: boolean
}

export function isCPAManaged(item: Pick<GrokOAuthCandidate, 'cpa_managed' | 'cpa_sync'>) {
  return Boolean(item.cpa_managed || (item.cpa_sync?.at && item.cpa_sync.pool_id))
}

export function mergeGrokOAuthRows(candidates: GrokOAuthCandidate[], accounts: GrokOAuthAccount[], jobs: GrokOAuthProtocolJob[] = [], pollErrors: Record<string, string> = {}): GrokOAuthRow[] {
  const byId = new Map(accounts.map(item => [item.id, item]))
  const byEmail = new Map(accounts.filter(item => item.email).map(item => [item.email.trim().toLowerCase(), item]))
  const linked = new Set<string>()
  const bySource = new Map<string, GrokOAuthProtocolJob>()
  for (const job of jobs) {
    const previous = bySource.get(job.source_account_id)
    if (!previous || job.created_at > previous.created_at || (job.created_at === previous.created_at && job.updated_at >= previous.updated_at)) bySource.set(job.source_account_id, job)
  }
  const result = candidates.map(candidate => {
    const account = byId.get(candidate.oauth_account_id) || byEmail.get(candidate.email.trim().toLowerCase())
    if (account) linked.add(account.id)
    const row: GrokOAuthRow = {
      ...candidate,
      source_account_id: candidate.id,
      account,
      oauth_account_id: account?.id || candidate.oauth_account_id,
      cpa_sync: account?.cpa_sync || candidate.cpa_sync,
      cpa_managed: account?.cpa_managed || candidate.cpa_managed,
      quality: account?.quality || candidate.quality,
    }
    const job = bySource.get(candidate.id)
    if (job) {
      row.job = job
      row.poll_error = pollErrors[job.id]
      if (job.status === 'pending' || job.status === 'running') {
        row.oauth_status = job.status === 'pending' ? 'queued' : 'running'
        row.oauth_message = job.message
      } else if (candidate.oauth_job_id === job.id) {
        // Inventory and task reads can straddle completion. Apply the confirmed
        // terminal result so a stale running candidate cannot disable this row.
        row.oauth_status = job.status
        row.oauth_message = job.status === 'authorized' ? '' : job.error || job.message
        if (job.account) {
          row.account = job.account
          row.oauth_account_id = job.account.id
          row.has_access_token = job.account.has_access_token
          row.has_refresh_token = job.account.has_refresh_token
          row.cpa_sync = job.account.cpa_sync
          row.cpa_managed = job.account.cpa_managed
          row.quality = job.account.quality
          linked.add(job.account.id)
        }
      }
    }
    if (isCPAManaged(row) && row.oauth_status === 'requires_renewal') row.oauth_status = 'authorized'
    return row
  })
  for (const account of accounts) {
    if (linked.has(account.id)) continue
    const row: GrokOAuthRow = {
      id: `oauth:${account.id}`, email: account.email, login_status: 'unlinked', oauth_account_id: account.id,
      has_access_token: account.has_access_token, has_refresh_token: account.has_refresh_token,
      oauth_status: ['invalid', 'expired', 'disabled'].includes(account.status) ? 'requires_renewal' : 'authorized',
      cpa_sync: account.cpa_sync, cpa_managed: account.cpa_managed, quality: account.quality, updated_at: account.updated_at, account,
    }
    if (isCPAManaged(row)) row.oauth_status = 'authorized'
    result.push(row)
  }
  return result
}

export function cpaStatusText(item: GrokOAuthRow) {
  if (item.cpa_sync?.status === 'quality_removed' || item.quality?.removal?.status === 'removed') return '降智凭据已移除'
  if (item.quality?.removal) return '降智凭据待移除'
  if (item.quality?.status === 'error' && !item.quality.blocked) {
    if (item.cpa_sync?.status === 'synced') return '已交付，检测失败待复测'
    return isCPAManaged(item) ? 'CPA 凭据保留，待复测' : '检测失败，暂缓同步'
  }
  if (item.quality?.blocked || item.cpa_sync?.status === 'quality_blocked') return isCPAManaged(item) ? 'CPA 凭据保留，检测未通过' : '检测未通过，未同步'
  if (item.job?.stage === 'syncing_cpa' && item.job.status === 'running') return '正在同步'
  if (isCPAManaged(item)) return item.cpa_sync?.status === 'synced' ? '已交付 CPA' : '待核对 CPA'
  return ({ synced: '已交付 CPA', pending: '待同步', failed: '同步失败', unknown: '待核对 CPA', uploading: '正在同步' } as Record<string, string>)[item.cpa_sync?.status || ''] || '未同步'
}

export function cpaAction(item: GrokOAuthRow): 'check' | 'sync' | null {
  if (!item.oauth_account_id) return null
  if (item.quality?.blocked || item.quality?.status === 'error' || item.quality_running || item.cpa_sync?.status === 'quality_removed') return null
  if (isCPAManaged(item) || ['unknown', 'uploading'].includes(item.cpa_sync?.status || '')) return 'check'
  return item.has_access_token && item.has_refresh_token ? 'sync' : null
}

export function oauthProgressText(item: GrokOAuthRow) {
  if (item.poll_error) return '状态待确认'
  if (item.job?.status === 'running') return ({ waiting_browser: '等待浏览器', starting_browser: '启动浏览器', authorize: '授权中', checking_quality: '检测 grok-4.7', syncing_cpa: '正在同步 CPA' } as Record<string, string>)[item.job.stage] || item.job.message || '授权中'
  return xaiOAuthStatusLabels[item.oauth_status] || item.oauth_status
}

export function grokOAuthImportPayload(value: unknown, poolId: string, now = Date.now()): GrokOAuthImportRequest {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('OAuth JSON 必须是对象')
  const raw = value as Record<string, unknown>
  const text = (key: string) => typeof raw[key] === 'string' ? (raw[key] as string).trim() : ''
  if (text('type') && text('type') !== 'xai') throw new Error('请使用 xAI OAuth 凭据')
  if (!text('access_token') || !text('refresh_token') || !text('email')) throw new Error('凭据需要包含邮箱、Access Token 和 Refresh Token')
  const payload: GrokOAuthImportRequest = { access_token: text('access_token'), refresh_token: text('refresh_token'), id_token: text('id_token'), email: text('email'), subject: text('subject') || text('sub'), cpa_pool_id: poolId }
  const expiry = raw.expired || raw.expires_at
  if (expiry) {
    const expiryMs = typeof expiry === 'number' ? (expiry > 1e12 ? expiry : expiry * 1000) : Date.parse(String(expiry))
    if (!Number.isFinite(expiryMs)) throw new Error('凭据到期时间格式无效')
    if (expiryMs <= now) throw new Error('这份 Access Token 已过期，请使用当前凭据或重新授权')
    payload.expires_in = Math.floor((expiryMs - now) / 1000)
  } else if (typeof raw.expires_in === 'number' && raw.expires_in > 0) payload.expires_in = Math.floor(raw.expires_in)
  if (Array.isArray(raw.models)) payload.models = raw.models.filter((model): model is string => typeof model === 'string')
  return payload
}

export function filterGrokOAuthCandidates<T extends GrokOAuthCandidate>(items: T[], keyword: string, status: string): T[] {
  const query = keyword.trim().toLowerCase()
  return items.filter(item => (!status || (status === 'cpa_attention'
    ? Boolean(item.quality?.blocked || item.quality?.status === 'error' || item.cpa_sync && item.cpa_sync.status !== 'synced')
    : status === 'quality_risk' ? Boolean(item.quality?.blocked)
    : status === 'quality_error' ? item.quality?.status === 'error'
    : item.oauth_status === status))
    && (!query || [item.email, item.id, item.login_status, xaiOAuthStatusLabels[item.oauth_status], item.cpa_sync?.pool_name, item.cpa_sync?.error].some(value => value?.toLowerCase().includes(query))))
}

export function qualityFailureReason(item: Pick<GrokOAuthRow, 'quality'>) {
  if (item.quality?.status !== 'error') return ''
  return item.quality.samples?.find(sample => sample.status === 'error')?.reason || item.quality.summary
}

export function qualityStatusText(item: Pick<GrokOAuthRow, 'quality' | 'quality_running'>) {
  const q = item.quality
  if (q?.blocked && q.confirmed_at) return '降智风险'
  if (q?.blocked) return '疑似降智'
  if (item.quality_running) return '检测中'
  return ({ normal: '检测正常', suspect: '疑似降智', high_risk: '降智风险', error: '检测失败' } as Record<string, string>)[q?.status || ''] || '未检测'
}
