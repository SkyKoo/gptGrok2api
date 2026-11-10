import apiClient from './client'

export interface AdobeQuota {
  status: 'unknown' | 'known' | 'success' | 'stale' | 'error' | 'failed' | string
  total?: number
  used?: number
  available?: number
  checked_at?: string
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
  has_token?: boolean
  has_cookie?: boolean
  credential_revision?: number
  media_session_refreshed?: boolean
  media_session_status?: 'imported' | 'refreshed' | 'preserved' | string
  media_session_updated_at?: number
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
  results?: Array<{ id: string; ok: boolean; error?: string }>
  error?: string
  hme_cleanup?: { failed: number; results?: Array<{status:string; message?:string}> }
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
  setDisabled: (ids: string[], disabled: boolean) => apiClient.post<unknown, AdobeMutationResponse>(`${path}/disabled`, { ids, disabled }),
  remove: (ids: string[]) => apiClient.delete<never, AdobeMutationResponse>(path, { data: { ids } }),
}
