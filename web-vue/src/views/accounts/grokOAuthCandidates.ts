import type { GrokOAuthCandidate } from '@/api/grokOAuthAccounts'

export const xaiOAuthStatusLabels: Record<GrokOAuthCandidate['oauth_status'], string> = {
  pending: '待授权', authorized: '已授权', failed: '授权失败', requires_renewal: '需重新授权',
}
export function filterGrokOAuthCandidates(items: GrokOAuthCandidate[], keyword: string, status: string) {
  const query = keyword.trim().toLowerCase()
  return items.filter(item => (!status || item.oauth_status === status)
    && (!query || [item.email, item.id, item.login_status, xaiOAuthStatusLabels[item.oauth_status]].some(value => value?.toLowerCase().includes(query))))
}
