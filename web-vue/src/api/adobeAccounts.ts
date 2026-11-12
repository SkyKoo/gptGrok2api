import apiClient from './client'
import type { AdobeReloginTask } from '@/lib/adobeRelogin'

export interface AdobeGenerationQuotaPool {
  id: string
  resource_type: string
  total?: number
  used?: number
  available?: number
  refresh_period?: string
  kinds: Array<'image' | 'video'>
  models: string[]
}
export interface AdobeGenerationModelQuota {
  model: string
  kind: 'image' | 'video'
  authorized: boolean
  reason_code?: string
  resource_type?: string
  pool_id?: string
  cost?: number
}
export interface AdobeGenerationQuota {
  status: 'known' | 'error' | 'unknown' | string
  checked_at?: string | number
  error?: string
  pools: AdobeGenerationQuotaPool[]
  models: AdobeGenerationModelQuota[]
}
export interface AdobeQuota {
  status: 'unknown' | 'known' | 'success' | 'stale' | 'error' | 'failed' | string
  generation?: AdobeGenerationQuota
  total?: number
  used?: number
  available?: number
  checked_at?: string
  last_success_at?: string
  error?: string
}
export interface AdobeAccount {
  id: string
  account_id: string
  email: string
  provider: 'adobe'
  status: string
  disabled: boolean
  expires_at?: number
  last_error?: string
  quota?: AdobeQuota
  pending?: boolean
  in_flight?: boolean
  submission_unknown?: boolean
  has_profile?: boolean
  has_token?: boolean
  has_cookie?: boolean
  credential_revision?: number
  media_session_refreshed?: boolean
  media_session_status?: 'imported' | 'refreshed' | 'preserved' | string
  media_session_updated_at?: number
  login_status?: 'valid' | 'expired' | 'invalid' | 'missing'
  login_updated_at?: number
  task_status?: 'submission_unknown' | 'running' | 'settling' | 'idle'
  task_key?: string
  can_query_quota?: boolean
  can_resolve_submission?: boolean
  task_reconciliation?: AdobeTaskReconciliation
}
export interface AdobeTaskReconciliation {
  state: 'waiting' | 'checking' | 'retry_wait' | 'polling' | string
  first_observed_at?: string
  last_checked_at?: string
  next_check_at?: string
  release_after?: string
  last_error?: string
  quota_status?: 'known' | 'error' | 'not_checked'
  attempts?: number
}
export interface AdobeSubmissionResolution {
  task_key: string
  resolved_at: string
  reason: string
  reviewed_by: string
  task_record_state: string
  method?: string
  outcome?: string
  quota_status?: string
}
export interface AdobeSubmissionResolutionRequest {
  id: string
  expected_task_key: string
  expected_credential_revision: number
  confirmed: true
  reason: string
}
export interface AdobeAccountsResponse {
  items: AdobeAccount[]
  total: number
  all_total: number
  page: number
  page_size: number
}
export interface AdobeMutationResponse {
  summary?: { total: number; ok: number; fail: number }
  results?: Array<{ id: string; ok: boolean; error?: string; account?: AdobeAccount }>
  error?: string
  hme_cleanup?: { failed: number; results?: Array<{status:string; message?:string}> }
}
export interface AdobeMailboxLinkResponse {
  linked: number
  already_linked: number
  unmatched: number
  conflicts: number
  failed: number
  results: Array<{ id: string; status: 'linked' | 'already_linked' | 'unmatched' | 'conflict' | 'failed'; message?: string }>
}
const path = '/api/adobe/accounts'
export const adobeAccountsApi = {
  list: (params: { page: number; page_size: number; keyword?: string; status?: string }) => apiClient.get<never, AdobeAccountsResponse>(path, { params }),
  import: (accounts: unknown[]) => apiClient.post<unknown, AdobeMutationResponse>(`${path}/import`, { accounts }),
  async refresh(ids: string[], credentials = false): Promise<AdobeMutationResponse> {
    const results: NonNullable<AdobeMutationResponse['results']> = []
    for (const id of ids) {
      try {
        const result = await apiClient.post<unknown, AdobeMutationResponse>(`${path}/refresh`, { ids: [id], credentials }, { timeout: 120000 })
        results.push(...(result.results || [{ id, ok: !result.error, error: result.error }]))
      } catch (cause) { results.push({ id, ok: false, error: cause instanceof Error ? cause.message : '刷新失败' }) }
    }
    return { results }
  },
  relogin: (ids: string[], requestId: string) => apiClient.post<unknown, { request_id: string; items: AdobeReloginTask[] }>(`${path}/relogin`, { request_id: requestId, ids }, { timeout: 15000 }),
  listRelogins: (requestId?: string) => apiClient.get<never, { request_id?: string; items: AdobeReloginTask[] }>(`${path}/relogin`, { params: requestId ? { request_id: requestId } : undefined, timeout: 15000 }),
  resolveSubmission: (request: AdobeSubmissionResolutionRequest) => apiClient.post<unknown, { ok: boolean; account: AdobeAccount }>(`${path}/resolve-submission`, request),
  submissionResolutions: (id: string) => apiClient.get<never, { items: AdobeSubmissionResolution[] }>(`${path}/submission-resolutions`, { params: { id } }),
  setDisabled: (ids: string[], disabled: boolean) => apiClient.post<unknown, AdobeMutationResponse>(`${path}/disabled`, { ids, disabled }),
  linkMailboxes: (ids: string[]) => apiClient.post<unknown, AdobeMailboxLinkResponse>(`${path}/link-mailboxes`, { ids }),
  remove: (ids: string[]) => apiClient.delete<never, AdobeMutationResponse>(path, { data: { ids } }),
}
