import type { AdobeQuota, AdobeGenerationQuotaPool } from '../api/adobeAccounts.ts'

export interface AdobeGenerationQuotaItem {
  key: string
  label: string
  text: string
  tone: 'success' | 'danger' | 'muted'
  models: string[]
}

function knownNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
}

function poolLabel(pool: AdobeGenerationQuotaPool) {
  const kind = pool.kinds.includes('image') && pool.kinds.includes('video') ? '媒体' : pool.kinds.includes('video') ? '视频' : '图片'
  if (pool.resource_type === 'firefly_limited_taste') return `${kind}体验`
  if (pool.refresh_period === 'P1D') return `每日${kind}`
  if (pool.refresh_period === 'P1M') return `每月${kind}`
  return `${kind}免费次数`
}

// Each shared pool is displayed once, regardless of how many logical models use it.
export function adobeGenerationQuotaItems(quota?: AdobeQuota): AdobeGenerationQuotaItem[] {
  const generation = quota?.generation
  const pools = [...new Map((generation?.pools || []).map(pool => [pool.id, pool])).values()]
  if (!pools.length) return [{ key: 'unknown', label: '生成次数', text: generation?.status === 'error' ? '未知（查询失败）' : '未知', tone: 'muted', models: [] }]
  const known = generation?.status === 'known'
  return pools.map(pool => ({
    key: pool.id,
    label: poolLabel(pool),
    text: !known || !knownNumber(pool.available) ? '未知' : knownNumber(pool.total) ? `${pool.available} / ${pool.total}` : String(pool.available),
    tone: !known || !knownNumber(pool.available) ? 'muted' : pool.available === 0 ? 'danger' : 'success',
    models: [...new Set(pool.models || [])],
  }))
}

export function adobeCreditLabel(quota?: AdobeQuota) {
  if (!quota || !['known', 'success'].includes(quota.status) || !knownNumber(quota.available)) return '通用积分未知'
  return `通用积分 ${quota.available}`
}
