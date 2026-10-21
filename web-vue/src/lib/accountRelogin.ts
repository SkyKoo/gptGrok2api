export interface AccountReloginTask {
  progress_id: string
  account_ref: string
  email: string
  stage: string
  started_at: string
  updated_at: string
  finished_at?: string
  failed_stage?: string
  token_expires_at?: string
  done: boolean
  error?: string
  unavailable?: boolean
  automatic?: boolean
  session_updated?: boolean
  quotas_refreshed?: boolean
  validation_only?: boolean
  validation_attempt?: number
  next_validation_at?: string
  diagnostic?: { stage: string; endpoint: string; status?: number; kind: string; message: string; retryable: boolean }
}

export const reloginSteps = ['准备邮箱', '初始化登录', '等待验证码', '验证验证码', '获取并保存会话']

export function reloginStep(task: AccountReloginTask): number {
  const stage = task.failed_stage || task.stage
  return ({ mail: 0, mailbox: 0, authorize: 1, send_code: 2, wait_code: 2, validate_code: 3, mfa: 4, session: 4, save_session: 4, refresh_account: 4, completed: 5 } as Record<string, number>)[stage] ?? -1
}

export function reloginStageLabel(stage: string): string {
  return ({ queued: '等待启动', config: '准备登录环境', mailbox: '准备邮箱', mail: '准备邮箱', authorize: '初始化登录', send_code: '重新发送验证码', wait_code: '等待邮箱验证码', validate_code: '验证验证码', mfa: '验证已有 2FA', session: '获取会话', save_session: '校验账号并保存会话', refresh_account: '刷新用户信息和额度', completed: '会话已更新', failed: '重新登录失败', unavailable: '进度记录已不可用' } as Record<string, string>)[stage] || '处理登录请求'
}

export function reloginErrorText(task: AccountReloginTask): string {
  if (task.unavailable) return '服务重启或记录已过期，无法确认此次结果。请先刷新账号列表核对。'
  if (task.session_updated && task.next_validation_at) return '新登录凭据已保存，账号保持异常并暂停调度。系统将使用已保存的凭据重试用户信息和额度校验，无需再次邮箱登录。'
  if (task.session_updated) return '新登录凭据已保存，但用户信息或额度刷新未完成。账号已标记异常并暂停调度，请刷新账号信息和额度完成校验。'
  const error = task.error || ''
  if (error.includes('mfa: secret_required')) return '上游要求验证已有 2FA。请在编辑账号中补填原 2FA Secret 后重试，无需再次开启 2FA。'
  if (error.includes('mfa: invalid_secret')) return '已保存的 2FA Secret 格式不正确，请核对原验证器密钥后重试。'
  if (error.startsWith('mfa:')) return '已有 2FA 验证未完成，原账号凭据保持不变并暂停调度。请核对原始密钥、验证器时间及错误详情后重试。'
  if (error.includes('timeout')) return '等待超时，请确认邮箱服务可用后重试。'
  if (error.includes('cancelled')) return '登录任务已中止，可以重新尝试。'
  if (error.includes('saved_browser_profile_unsupported')) return '已保存的登录环境不受支持，请检查账号配置。'
  if (error.includes('account_changed')) return '账号已删除或凭据已变更，本次登录结果未覆盖现有账号。'
  if (error.includes('mismatch')) return '登录结果与原账号不一致，未更新原账号。'
  return '重新登录未完成，原账号凭据保持不变，账号已标记异常并暂停调度。可查看错误详情后重试。'
}

export function reloginElapsed(task: AccountReloginTask, now: number): string {
  const start = Date.parse(task.started_at)
  const end = task.done ? Date.parse(task.finished_at || task.updated_at) : now
  if (!Number.isFinite(start) || !Number.isFinite(end)) return '—'
  const seconds = Math.max(0, Math.floor((end - start) / 1000))
  return seconds < 60 ? `${seconds} 秒` : `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`
}

export interface ReloginSnapshot {
  tasks: AccountReloginTask[]
  starting: string[]
  pollError: boolean
}

type Transport = {
  start: (ref: string) => Promise<AccountReloginTask>
  list: () => Promise<AccountReloginTask[]>
}

// Independent task IDs, not a single busy account. A failed GET must never be
// interpreted as a failed login or cause another POST.
export function createReloginTracker(transport: Transport, changed: (state: ReloginSnapshot) => void, completed: () => void) {
  const tasks = new Map<string, AccountReloginTask>()
  const starting = new Set<string>()
  let pollError = false
  let hasSnapshot = false
  let pendingSync: Promise<void> | undefined
  const emit = () => changed({ tasks: [...tasks.values()].sort((a, b) => Number(a.done) - Number(b.done) || Date.parse(b.started_at) - Date.parse(a.started_at)), starting: [...starting], pollError })
  const busy = (ref: string) => starting.has(ref) || [...tasks.values()].some(t => t.account_ref === ref && !t.done)
  function merge(task: AccountReloginTask) {
    const old = tasks.get(task.progress_id)
    // A POST can return its queued snapshot after a GET already saw completion.
    if (old && (old.done && !task.done || Date.parse(old.updated_at) > Date.parse(task.updated_at))) return
    tasks.set(task.progress_id, task)
  }
  function sync(): Promise<void> {
    if (pendingSync) return pendingSync
    const activeBeforeRead = [...tasks.values()].filter(t => !t.done)
    pendingSync = (async () => {
      try {
        const incoming = await transport.list()
        const ids = new Set(incoming.map(t => t.progress_id))
        let finished = false
        for (const task of incoming) {
          if (task.done && (tasks.has(task.progress_id) ? !tasks.get(task.progress_id)!.done : hasSnapshot)) finished = true
          merge(task)
        }
        for (const old of activeBeforeRead) {
          if (!ids.has(old.progress_id) && !tasks.get(old.progress_id)?.done) {
            // The list contains every active task. A vanished task may have
            // been interrupted by a service restart; do not claim it failed.
            tasks.set(old.progress_id, { ...old, done: true, unavailable: true, stage: 'unavailable', finished_at: new Date().toISOString() })
          }
        }
        pollError = false
        hasSnapshot = true
        emit()
        if (finished) completed()
      } catch {
        pollError = true
        emit()
      }
    })().finally(() => { pendingSync = undefined })
    return pendingSync
  }
  async function start(ref: string) {
    if (busy(ref)) return
    starting.add(ref)
    emit()
    try {
      const task = await transport.start(ref)
      merge(task)
      emit()
      return task
    } finally {
      starting.delete(ref)
      emit()
    }
  }
  return { start, sync, busy }
}
