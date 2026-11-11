export interface AdobeReloginTask {
  progress_id: string
  request_id?: string
  account_ref: string
  email: string
  stage: 'queued' | 'running' | 'completed' | 'failed' | 'interrupted' | string
  started_at: string
  updated_at: string
  finished_at?: string
  done: boolean
  error?: string
  session_updated?: boolean
  submission_unknown?: boolean
  result_unknown?: boolean
}
export interface AdobeReloginRequest { request_id: string; ids: string[]; uncertain: boolean }
export interface AdobeReloginSnapshot {
  tasks: AdobeReloginTask[]
  requests: AdobeReloginRequest[]
  pollError: boolean
}
type Transport = {
  start: (ids: string[], requestId: string) => Promise<{ items: AdobeReloginTask[] }>
  list: (requestId?: string) => Promise<{ items: AdobeReloginTask[] }>
  isRejected: (cause: unknown) => boolean
}

// Only a known application rejection proves that admission did not happen.
// Proxy/CDN 5xx responses may arrive after the server persisted the request.
export function isAdobeReloginNotStarted(cause: unknown): boolean {
  const failure = cause as { status?: number; data?: { error?: { type?: unknown } } } | null
  const status = failure?.status, type = failure?.data?.error?.type
  return status === 503 && type === 'adobe_relogin_not_started'
    || status === 401 && type === 'authentication_error'
    || (status === 400 || status === 413) && type === 'invalid_request_error'
    || status === 409 && type === 'adobe_relogin_start_failed'
}

// A lost POST response is reconciled by its request ID. Polling never submits
// another batch, and unresolved starts keep the affected accounts locked.
export function createAdobeReloginTracker(transport: Transport, changed: (state: AdobeReloginSnapshot) => void, finished: () => void, restored: AdobeReloginRequest[] = []) {
  const tasks = new Map<string, AdobeReloginTask>()
  const requests = new Map(restored.map(request => [request.request_id, { ...request, uncertain: true }]))
  let pollError = false
  let syncing: Promise<void> | undefined
  const emit = () => changed({ tasks: [...tasks.values()].sort((a, b) => Number(a.done) - Number(b.done) || Date.parse(b.started_at) - Date.parse(a.started_at)), requests: [...requests.values()], pollError })
  const busy = (id: string) => [...requests.values()].some(request => request.ids.includes(id)) || [...tasks.values()].some(task => task.account_ref === id && !task.done)
  function merge(incoming: AdobeReloginTask[]) {
    let completed = false
    for (const task of incoming) {
      const old = tasks.get(task.progress_id)
      if (old && (old.done && !task.done || Date.parse(old.updated_at) > Date.parse(task.updated_at))) continue
      if (old && !old.done && task.done) completed = true
      tasks.set(task.progress_id, task)
    }
    return completed
  }
  async function reconcile(requestId: string) {
    const response = await transport.list(requestId)
    if (!response.items.length) return false
    const completed = merge(response.items)
    requests.delete(requestId)
    return completed || response.items.some(task => task.done)
  }
  function sync(): Promise<void> {
    if (syncing) return syncing
    syncing = (async () => {
      let completed = false
      try {
        completed = merge((await transport.list()).items)
        for (const requestId of [...requests.keys()]) completed = await reconcile(requestId) || completed
        pollError = false
      } catch { pollError = true }
      emit()
      if (completed) finished()
    })().finally(() => { syncing = undefined })
    return syncing
  }
  async function start(ids: string[], requestId: string) {
    if (!ids.length || ids.some(busy)) return
    if (ids.length > 100) throw new Error('一次最多重新登录 100 个账号。')
    requests.set(requestId, { request_id: requestId, ids: [...ids], uncertain: false })
    emit()
    try {
      const response = await transport.start(ids, requestId)
      if (!response.items.length) throw new Error('adobe_relogin_result_unknown')
      const completed = merge(response.items)
      requests.delete(requestId)
      if (completed || response.items.every(task => task.done)) finished()
    } catch (cause) {
      if (transport.isRejected(cause)) {
        requests.delete(requestId)
        throw cause
      }
      const request = requests.get(requestId)
      if (request) requests.set(requestId, { ...request, uncertain: true })
      try { if (await reconcile(requestId)) finished() } catch { pollError = true }
    } finally { emit() }
  }
  emit()
  return { start, sync, busy }
}
