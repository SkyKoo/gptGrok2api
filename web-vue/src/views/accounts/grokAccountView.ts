import type { GrokAccount } from '@/api/grokAccounts'
import { PILL_TONE_CLASS } from '../../lib/pillTones.ts'

function cleanString(value: unknown): string {
  return String(value || '').trim()
}

function accountEventTimestamp(value: unknown): number | null {
  if (typeof value === 'number' && Number.isFinite(value) && value > 0) return value
  const text = cleanString(value)
  if (!text) return null
  const numeric = Number(text)
  if (Number.isFinite(numeric)) return numeric > 0 ? numeric : null
  const parsed = Date.parse(text)
  return Number.isFinite(parsed) ? parsed : null
}

function hasCurrentValidProbe(item: GrokAccount): boolean {
  if (cleanString(item.probe_status).toLowerCase() !== 'valid') return false
  const quota = item.probe_quota
  if (!quota || !Number.isFinite(Number(quota.remaining)) || !Number.isFinite(Number(quota.total))) return false
  const probedAt = accountEventTimestamp(item.probe_at)
  const refreshedAt = accountEventTimestamp(item.refresh_at)
  return probedAt === null || refreshedAt === null || probedAt >= refreshedAt
}

export function grokRefreshFailed(item: GrokAccount): boolean {
  return cleanString(item.refresh_status).toLowerCase() === 'failed' && !hasCurrentValidProbe(item)
}

export function grokRefreshStatusTitle(item: GrokAccount): string {
  if (!grokRefreshFailed(item)) return ''
  const error = cleanString(item.refresh_error) || '上游未返回真实额度数据'
  const refreshedAt = formatGrokAccountDate(item.refresh_at)
  return refreshedAt === '-' ? error : `${error}（${refreshedAt}）`
}

export function grokProbeStatusText(item: GrokAccount): string {
  const status = cleanString(item.probe_status).toLowerCase()
  if (status === 'valid') return '探测有效'
  if (status === 'invalid') return '探测失效'
  if (status === 'unknown') return '探测未知'
  return ''
}

export function grokProbeStatusClass(item: GrokAccount): string {
  const status = cleanString(item.probe_status).toLowerCase()
  if (status === 'valid') return PILL_TONE_CLASS.success
  if (status === 'invalid') return PILL_TONE_CLASS.danger
  if (status === 'unknown') return PILL_TONE_CLASS.warning
  return PILL_TONE_CLASS.neutral
}

export function grokProbeStatusTitle(item: GrokAccount): string {
  const label = grokProbeStatusText(item)
  if (!label) return ''
  const at = formatGrokAccountDate(item.probe_at)
  const error = cleanString(item.probe_error)
  return [label, at !== '-' ? at : '', error].filter(Boolean).join(' · ')
}

export function grokRecoveryStatusText(item: GrokAccount): string {
  const status = cleanString(item.recovery_status).toLowerCase()
  if (status === 'pending') return '等待恢复'
  if (status === 'running') return '恢复中'
  if (status === 'success') return '恢复成功'
  if (status === 'failed') return '恢复失败'
  return ''
}

export function grokRecoveryStatusClass(item: GrokAccount): string {
  const status = cleanString(item.recovery_status).toLowerCase()
  if (status === 'success') return PILL_TONE_CLASS.success
  if (status === 'failed') return PILL_TONE_CLASS.danger
  if (status === 'pending' || status === 'running') return PILL_TONE_CLASS.warning
  return PILL_TONE_CLASS.neutral
}

export function grokRecoveryStatusTitle(item: GrokAccount): string {
  const label = grokRecoveryStatusText(item)
  if (!label) return ''
  const nextAttempt = formatGrokAccountDate(item.recovery_next_attempt_at)
  const lastSuccess = formatGrokAccountDate(item.recovery_last_success_at)
  const error = cleanString(item.recovery_error)
  const time = nextAttempt !== '-'
    ? `下次重试 ${nextAttempt}`
    : lastSuccess !== '-' ? `最近成功 ${lastSuccess}` : ''
  return [label, time, error].filter(Boolean).join(' · ')
}

export function grokAccountStatusText(item: GrokAccount): string {
  const status = cleanString(item.status).toLowerCase()
  if (status === 'active') return '可用'
  if (status === 'pending_sso') return '待登录态'
  if (status === 'submission_failed') return '提交失败'
  if (status === 'submission_unknown') return '提交结果未知'
  if (status === 'submission_unconfirmed') return '提交待确认'
  if (status === 'submitting') return '提交中'
  if (status === 'pending_submit') return '待提交'
  return cleanString(item.status) || '未知'
}

export function grokAccountStatusClass(item: GrokAccount): string {
  const status = cleanString(item.status).toLowerCase()
  if (status === 'active') return PILL_TONE_CLASS.success
  if (status === 'submission_failed') return PILL_TONE_CLASS.danger
  if (
    status === 'pending_sso'
    || status === 'submission_unknown'
    || status === 'submission_unconfirmed'
    || status === 'submitting'
    || status === 'pending_submit'
  ) return PILL_TONE_CLASS.warning
  return PILL_TONE_CLASS.neutral
}

export function grokRuntimeStatusText(item: GrokAccount): string {
  const status = cleanString(item.runtime_status).toLowerCase()
  if (!status) return item.sync_state === 'synced' ? '待刷新' : '未加入'
  if (status === 'active') return grokRefreshFailed(item) ? '刷新失败' : '正常'
  if (status === 'cooling' || status === 'rate_limited') return '限流'
  if (status === 'invalid' || status === 'expired') return '异常'
  if (status === 'disabled') return '禁用'
  return cleanString(item.runtime_status)
}

export function grokRuntimeStatusClass(item: GrokAccount): string {
  const status = cleanString(item.runtime_status).toLowerCase()
  if (status === 'active') return grokRefreshFailed(item) ? PILL_TONE_CLASS.warning : PILL_TONE_CLASS.success
  if (status === 'cooling' || status === 'rate_limited') return PILL_TONE_CLASS.warning
  if (status === 'invalid' || status === 'expired') return PILL_TONE_CLASS.danger
  return PILL_TONE_CLASS.neutral
}

export function grokSyncStateText(item: GrokAccount): string {
  if (item.sync_state === 'synced') return '已加入调用池'
  if (item.sync_state === 'not_ready') return '登录态未就绪'
  if (item.sync_state === 'not_synced') return '未加入调用池'
  if (item.sync_state === 'runtime_unavailable') return '调用池不可用'
  if (item.sync_state === 'sync_failed' || item.sync_state === 'failed') return '加入失败'
  return '状态未知'
}

type GrokOAuthDisplayStatus = 'not_ready' | 'unauthorized' | 'denied' | 'normal' | 'limited' | 'no_quota' | 'expired' | 'invalid' | 'unknown'

function grokOAuthDisplayStatus(item: GrokAccount): GrokOAuthDisplayStatus {
  if (!item.oauth) {
    if (cleanString(item.oauth_authorization?.status).toLowerCase() === 'denied') return 'denied'
    return item.status === 'active' && item.has_sso ? 'unauthorized' : 'not_ready'
  }
  const probeCode = cleanString(item.oauth.probe?.code).toLowerCase()
  if (probeCode === 'personal-team-blocked' || probeCode.startsWith('personal-team-blocked:')) return 'no_quota'
  const status = cleanString(item.oauth.status).toLowerCase()
  if (status === 'expired') return 'expired'
  const expiry = accountEventTimestamp(item.oauth.expires_at)
  if (expiry !== null && (expiry < 10_000_000_000 ? expiry * 1000 : expiry) <= Date.now()) return 'expired'
  if (status === 'disabled' || status === 'invalid') return 'invalid'
  const probeStatus = cleanString(item.oauth?.probe?.status).toLowerCase()
  if (probeStatus === 'valid') return 'normal'
  if (probeStatus === 'limited') return 'limited'
  if (probeStatus === 'invalid') return 'invalid'
  if (probeStatus === 'unknown') return 'unknown'
  return status === 'active' ? 'unknown' : 'invalid'
}

export function grokOAuthStatusText(item: GrokAccount): string {
  return ({
    not_ready: '等待登录态',
    unauthorized: 'OAuth 未授权',
    denied: 'OAuth 拒绝',
    normal: 'OAuth 正常',
    limited: 'OAuth 限流',
    no_quota: '无额度/无订阅',
    expired: 'OAuth 过期',
    invalid: 'OAuth 失效',
    unknown: 'OAuth 未确认',
  } as const)[grokOAuthDisplayStatus(item)]
}

export function grokOAuthShortStatusText(item: GrokAccount): string {
  return ({
    not_ready: '待 SSO',
    unauthorized: '未授权',
    denied: '拒绝',
    normal: '正常',
    limited: '限流',
    no_quota: '无额度/无订阅',
    expired: '过期',
    invalid: '失效',
    unknown: '未确认',
  } as const)[grokOAuthDisplayStatus(item)]
}

export function grokOAuthStatusClass(item: GrokAccount): string {
  const status = grokOAuthDisplayStatus(item)
  if (status === 'normal') return PILL_TONE_CLASS.success
  if (status === 'limited' || status === 'no_quota' || status === 'unknown') return PILL_TONE_CLASS.warning
  if (status === 'denied' || status === 'expired' || status === 'invalid') return PILL_TONE_CLASS.danger
  return PILL_TONE_CLASS.neutral
}

export function grokOAuthRecoveryStatusText(item: GrokAccount): string {
  const status = cleanString(item.oauth?.recovery?.status).toLowerCase()
  if (status === 'pending') return '等待恢复'
  if (status === 'running') return '恢复中'
  if (status === 'success') return '恢复成功'
  if (status === 'failed') return '恢复失败'
  return ''
}

export function grokOAuthRecoveryStatusClass(item: GrokAccount): string {
  const status = cleanString(item.oauth?.recovery?.status).toLowerCase()
  if (status === 'success') return 'text-emerald-600'
  if (status === 'failed') return 'text-rose-600'
  if (status === 'pending' || status === 'running') return 'text-amber-600'
  return 'text-muted-foreground'
}

export function grokOAuthRecoveryStatusTitle(item: GrokAccount): string {
  const recovery = item.oauth?.recovery
  const label = grokOAuthRecoveryStatusText(item)
  if (!recovery || !label) return ''
  const next = formatGrokAccountDate(recovery.next_attempt_at)
  const attempted = formatGrokAccountDate(recovery.last_attempt_at)
  return [
    label,
    next !== '-' ? `下次重试 ${next}` : '',
    attempted !== '-' ? `最近尝试 ${attempted}` : '',
    cleanString(recovery.error),
  ].filter(Boolean).join(' · ')
}

function oauthQuotaPercentText(value: unknown): string {
  if (value === null || value === undefined || value === '') return '-'
  const number = Number(value)
  if (!Number.isFinite(number)) return '-'
  const bounded = Math.min(100, Math.max(0, number))
  return `${bounded.toLocaleString('zh-CN', { maximumFractionDigits: 2 })}%`
}

export function grokOAuthRemainingQuotaText(item: GrokAccount): string {
  return oauthQuotaPercentText(item.oauth?.quota?.remaining_percent)
}

export function grokOAuthQuotaResetText(item: GrokAccount): string {
  return formatGrokAccountDate(item.oauth?.quota?.period?.end)
}

export function grokOAuthQuotaText(item: GrokAccount): string {
  if (!item.oauth?.quota || typeof item.oauth.quota !== 'object') return '-'
  const remaining = grokOAuthRemainingQuotaText(item)
  const reset = grokOAuthQuotaResetText(item)
  return [remaining !== '-' ? `剩余 ${remaining}` : '', reset !== '-' ? `重置 ${reset}` : ''].filter(Boolean).join(' · ') || '-'
}

export function grokOAuthStatusTitle(item: GrokAccount): string {
  const oauth = item.oauth
  if (!oauth) return '未完成 Grok OAuth 授权'
  const probe = oauth.probe
  const model = cleanString(probe?.model) || (oauth.models || []).join('、')
  const at = formatGrokAccountDate(probe?.at)
  const quota = grokOAuthQuotaText(item)
  const httpStatus = Number(probe?.http_status || 0)
  const error = cleanString(probe?.error)
  const code = cleanString(probe?.code)
  const recovery = grokOAuthRecoveryStatusTitle(item)
  return [
    grokOAuthStatusText(item),
    model ? `模型 ${model}` : '',
    quota !== '-' ? quota : '',
    httpStatus ? `HTTP ${httpStatus}` : '',
    code,
    error,
    recovery,
    at !== '-' ? at : '',
  ].filter(Boolean).join(' · ')
}

export function grokAccountRowClass(item: GrokAccount): string {
  const status = cleanString(item.status).toLowerCase()
  const runtimeStatus = cleanString(item.runtime_status).toLowerCase()
  if (runtimeStatus === 'disabled') return 'bg-muted/50'
  if (cleanString(item.probe_status).toLowerCase() === 'invalid') return 'bg-rose-500/5'
  if (cleanString(item.probe_status).toLowerCase() === 'unknown') return 'bg-amber-500/5'
  if (runtimeStatus === 'invalid' || runtimeStatus === 'expired') return 'bg-rose-500/5'
  if (runtimeStatus === 'cooling' || runtimeStatus === 'rate_limited') return 'bg-amber-500/5'
  if (grokRefreshFailed(item)) return 'bg-amber-500/5'
  if (status === 'submission_failed') return 'bg-rose-500/5'
  if (status && status !== 'active') return 'bg-amber-500/5'
  return ''
}

export function grokCredentialText(present: boolean, kind: 'sso' | 'password'): string {
  if (kind === 'sso') return present ? 'SSO 已就绪' : 'SSO 缺失'
  return present ? '密码已保存' : '密码缺失'
}

export function grokCredentialClass(present: boolean): string {
  return present ? PILL_TONE_CLASS.success : PILL_TONE_CLASS.danger
}

export function grokAccountSourceText(item: GrokAccount): string {
  const source = cleanString(item.source_type)
  return source === 'protocol' ? '纯协议注册' : (source || '-')
}

export function grokAccountPoolText(item: GrokAccount): string {
  const pool = cleanString(item.pool).toLowerCase()
  if (pool === 'basic') return 'Basic'
  if (pool === 'super') return 'Super'
  if (pool === 'heavy') return 'Heavy'
  if (pool === 'auto') return 'Auto'
  return cleanString(item.pool) || '-'
}

export function formatGrokAccountDate(value: unknown): string {
  const raw = cleanString(value)
  if (!raw) return '-'
  const numeric = Number(raw)
  if (Number.isFinite(numeric) && numeric <= 0) return '-'
  const date = Number.isFinite(numeric) && numeric > 0
    ? new Date(numeric > 10_000_000_000 ? numeric : numeric * 1000)
    : new Date(raw)
  if (Number.isNaN(date.getTime()) || date.getUTCFullYear() < 1971) return '-'
  const yyyy = date.getFullYear()
  const mm = String(date.getMonth() + 1).padStart(2, '0')
  const dd = String(date.getDate()).padStart(2, '0')
  const hh = String(date.getHours()).padStart(2, '0')
  const mi = String(date.getMinutes()).padStart(2, '0')
  return `${yyyy}-${mm}-${dd} ${hh}:${mi}`
}

type GrokQuotaMode = 'auto' | 'fast' | 'expert' | 'heavy' | 'console'

const GROK_QUOTA_MODE_LABELS: Record<GrokQuotaMode, string> = {
  auto: '自动',
  fast: '快速',
  expert: '专家',
  heavy: '重度',
  console: '控制台',
}

function quotaRemaining(value: unknown): number | null {
  if (typeof value === 'number' && Number.isFinite(value)) return Math.max(0, Math.trunc(value))
  if (!value || typeof value !== 'object') return null
  const source = value as Record<string, unknown>
  const remaining = Number(source.remaining)
  return Number.isFinite(remaining) ? Math.max(0, Math.trunc(remaining)) : null
}

export function grokQuotaEntries(item: GrokAccount): Array<{ key: GrokQuotaMode; label: string; remaining: number }> {
  if (!item.quota || typeof item.quota !== 'object') return []
  const quota = item.quota as Record<string, unknown>
  return (Object.keys(GROK_QUOTA_MODE_LABELS) as GrokQuotaMode[])
    .map((key) => ({ key, label: GROK_QUOTA_MODE_LABELS[key], remaining: quotaRemaining(quota[key]) }))
    .filter((entry): entry is { key: GrokQuotaMode; label: string; remaining: number } => entry.remaining !== null)
}

export function grokQuotaText(item: GrokAccount): string {
  const entries = grokQuotaEntries(item)
  if (!entries.length) return '-'
  return entries.map((entry) => `${entry.label} ${entry.remaining}`).join(' · ')
}

export function grokSuccessRate(item: GrokAccount): string {
  if (item.sync_state !== 'synced' || item.use_count == null || item.fail_count == null) return '-'
  const success = Math.max(0, Number(item.use_count || 0))
  const failure = Math.max(0, Number(item.fail_count || 0))
  const total = success + failure
  if (!total) return '-'
  return `${Math.round((success / total) * 100)}%`
}

export function grokUsageText(item: GrokAccount): string {
  if (item.sync_state !== 'synced') return '- / -'
  if (item.use_count == null && item.fail_count == null) return '暂无记录'
  return `${item.use_count ?? '-'} / ${item.fail_count ?? '-'}`
}

export function grokLastUsedText(item: GrokAccount): string {
  const value = formatGrokAccountDate(item.last_used_at)
  return value === '-' ? '暂无记录' : value
}

export function grokAccountDetailItems(item: GrokAccount) {
  return [
    { label: '账号 ID', value: cleanString(item.id) || '-' },
    { label: '类型', value: grokAccountPoolText(item) },
    { label: '注册状态', value: grokAccountStatusText(item) },
    { label: '运行状态', value: grokRuntimeStatusText(item) },
    { label: 'OAuth 状态', value: grokOAuthShortStatusText(item) },
    { label: 'SSO 文字额度', value: grokQuotaText(item) },
    { label: '成功 / 失败', value: grokUsageText(item) },
    { label: '成功率', value: grokSuccessRate(item) },
    { label: '创建时间（CFM 入库）', value: formatGrokAccountDate(item.created_at) },
    { label: '最近调用', value: grokLastUsedText(item) },
    { label: '最近刷新', value: formatGrokAccountDate(item.refresh_at) },
    { label: '最近探测', value: formatGrokAccountDate(item.probe_at) },
    { label: '探测结果', value: grokProbeStatusText(item) || '-' },
    {
      label: '刷新结果',
      value: grokRefreshFailed(item)
        ? (cleanString(item.refresh_error) || '刷新失败')
        : cleanString(item.refresh_status) === 'success' ? '成功' : '-',
    },
  ]
}

export function grokAccountRowSignature(item: GrokAccount): string {
  return [
    item.id,
    item.email,
    item.status,
    item.source_type,
    item.token_preview,
    item.pool,
    item.runtime_status,
    JSON.stringify(item.quota || {}),
    JSON.stringify(item.media_quota || {}),
    item.use_count || 0,
    item.fail_count || 0,
    item.last_used_at,
    item.refresh_status,
    item.refresh_at,
    item.refresh_error,
    item.probe_status,
    item.probe_at,
    JSON.stringify(item.probe_quota || {}),
    item.probe_error,
    item.recovery_status,
    item.recovery_last_attempt_at,
    item.recovery_last_success_at,
    item.recovery_next_attempt_at,
    item.recovery_error,
    item.recovery_attempts || 0,
    (item.tags || []).join(','),
    item.sync_state,
    item.oauth?.id,
    item.oauth?.status,
    (item.oauth?.models || []).join(','),
    item.oauth?.probe?.status,
    item.oauth?.probe?.at,
    item.oauth?.probe?.http_status,
    item.oauth?.probe?.code,
    item.oauth?.probe?.error,
    JSON.stringify(item.oauth?.quota || {}),
    item.oauth?.recovery?.status,
    item.oauth?.recovery?.last_attempt_at,
    item.oauth?.recovery?.last_success_at,
    item.oauth?.recovery?.next_attempt_at,
    item.oauth?.recovery?.attempts,
    item.oauth?.recovery?.error,
    item.oauth?.expires_at,
    item.has_sso ? 1 : 0,
    item.has_password ? 1 : 0,
    item.created_at,
    item.updated_at,
  ].map((value) => cleanString(value).replaceAll('|', '/')).join('|')
}
