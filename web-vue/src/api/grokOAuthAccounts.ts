import apiClient from '@/api/client'

export type GrokOAuthCandidate = {
  id: string
  email: string
  login_status: string
  oauth_status: 'pending' | 'queued' | 'running' | 'manual_action_required' | 'authorized' | 'failed' | 'requires_renewal'
  oauth_job_id?: string
  oauth_message?: string
  oauth_account_id: string
  has_access_token: boolean
  has_refresh_token: boolean
  updated_at?: string
}

export type GrokOAuthAccountStatus = 'active' | 'disabled' | 'expired' | 'invalid'
export type GrokOAuthProbeStatus = 'valid' | 'limited' | 'invalid' | 'unknown'

export type GrokOAuthProductUsage = {
  product: string
  used_percent?: number
  remaining_percent?: number
}

export type GrokOAuthQuota = {
  source?: 'billing' | 'model_probe'
  used_percent?: number
  remaining_percent?: number
  period?: {
    type?: string
    start?: string
    end?: string
  }
  product_usage?: GrokOAuthProductUsage[]
  updated_at?: string
}

export type GrokOAuthProbe = {
  status: GrokOAuthProbeStatus | (string & {})
  at: string
  model: string
  http_status: number
  code: string
  error: string
  usage?: {
    input_tokens?: number
    output_tokens?: number
    total_tokens?: number
    cost_in_usd_ticks?: number
  }
}

export type GrokOAuthRecovery = {
  status: 'pending' | 'running' | 'success' | 'failed' | (string & {})
  job_id: string
  source_account_id: string
  last_attempt_at: string
  last_success_at: string
  next_attempt_at: string
  attempts: number
  error: string
}

export type GrokOAuthAccount = {
  id: string
  provider: 'xai_cli_oauth'
  auth_kind: 'oauth'
  email: string
  subject_preview: string
  has_access_token: boolean
  has_refresh_token: boolean
  has_id_token: boolean
  token_type: string
  expires_at: string
  last_refresh_at: string
  status: GrokOAuthAccountStatus | (string & {})
  source_type: string
  metadata?: {
    oauth_delivery?: GrokOAuthDeliveryResults
    [key: string]: unknown
  }
  models: string[]
  probe?: GrokOAuthProbe
  quota?: GrokOAuthQuota
  recovery?: GrokOAuthRecovery
  use_count: number
  fail_count: number
  last_used_at: string
  last_error: string
  created_at: string
  updated_at: string
}

export type GrokOAuthDeliveryResult = {
  status: 'success' | 'failed' | 'skipped' | (string & {})
  target_id: string
  at: string
  error?: string
  remote?: Record<string, unknown>
}

export type GrokOAuthDeliveryResults = Record<string, GrokOAuthDeliveryResult>

export type GrokOAuthAccountsResponse = {
  provider: string
  items: GrokOAuthAccount[]
  total: number
  available_models: string[]
}

export type GrokOAuthDeviceSession = {
  id: string
  user_code: string
  verification_uri: string
  verification_uri_complete: string
  expires_at: number
  interval: number
  status: 'pending'
}

export type GrokOAuthDevicePollResult =
  | { status: 'pending'; interval: number; expires_at: number }
  | { status: 'authorized'; account: GrokOAuthAccount; models: string[] }

export type GrokOAuthProtocolJob = {
  id: string
  status: 'pending' | 'running' | 'authorized' | 'failed' | 'manual_action_required'
  stage: string
  message: string
  error: string
  source_account_id: string
  created_at: number
  updated_at: number
  account?: GrokOAuthAccount
  models: string[]
  delivery?: GrokOAuthDeliveryResults
}

export type GrokOAuthImportRequest = {
  access_token?: string
  refresh_token?: string
  id_token?: string
  email?: string
  subject?: string
  credential?: Record<string, unknown>
}

export type GrokOAuthAccountTestResponse = {
  account_id: string
  account: GrokOAuthAccount
  model: string
  content: string
  elapsed_ms: number
}

const BASE_PATH = '/api/grok/oauth'

export const grokOAuthAccountsApi = {
  candidates() {
    return apiClient.get<never, { items: GrokOAuthCandidate[]; total: number }>(`${BASE_PATH}/candidates`)
  },
  list(keyword = '', status = 'all') {
    return apiClient.get<never, GrokOAuthAccountsResponse>(`${BASE_PATH}/accounts`, {
      params: { keyword, status },
    })
  },

  importCredential(payload: GrokOAuthImportRequest) {
    return apiClient.post<GrokOAuthImportRequest, { account: GrokOAuthAccount; models: string[] }>(
      `${BASE_PATH}/accounts/import`,
      payload,
    )
  },

  startDevice() {
    return apiClient.post<Record<string, never>, GrokOAuthDeviceSession>(`${BASE_PATH}/device/start`, {})
  },

  pollDevice(sessionId: string) {
    return apiClient.post<{ session_id: string }, GrokOAuthDevicePollResult>(`${BASE_PATH}/device/poll`, {
      session_id: sessionId,
    })
  },

  startProtocol(accountId: string) {
    return apiClient.post<{ account_id: string }, { reused: boolean; job: GrokOAuthProtocolJob }>(
      `${BASE_PATH}/protocol/start`,
      { account_id: accountId },
    )
  },

  getProtocolJob(jobId: string) {
    return apiClient.get<never, { job: GrokOAuthProtocolJob }>(
      `${BASE_PATH}/protocol/jobs/${encodeURIComponent(jobId)}`,
    )
  },

  refresh(id: string) {
    return apiClient.post<Record<string, never>, { account: GrokOAuthAccount }>(`${BASE_PATH}/accounts/${encodeURIComponent(id)}/refresh`, {})
  },

  syncModels(id: string) {
    return apiClient.post<Record<string, never>, { account: GrokOAuthAccount; models: string[] }>(
      `${BASE_PATH}/accounts/${encodeURIComponent(id)}/models/sync`,
      {},
    )
  },

  testAccount(id: string, payload: { model: string; prompt: string }) {
    return apiClient.post<{ model: string; prompt: string }, GrokOAuthAccountTestResponse>(
      `${BASE_PATH}/accounts/${encodeURIComponent(id)}/test`,
      payload,
    )
  },

  setDisabled(ids: string[], disabled: boolean) {
    return apiClient.post<{ ids: string[]; disabled: boolean }, { updated: number; count: number; disabled: boolean }>(
      `${BASE_PATH}/accounts/status`,
      { ids, disabled },
    )
  },

  remove(ids: string[]) {
    return apiClient.delete<{ ids: string[] }, { removed: number; count: number }>(`${BASE_PATH}/accounts`, {
      data: { ids },
    })
  },
}
