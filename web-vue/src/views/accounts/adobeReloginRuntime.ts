import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { adobeAccountsApi } from '@/api/adobeAccounts'
import { createAdobeReloginTracker, isAdobeReloginNotStarted, type AdobeReloginSnapshot, type AdobeReloginRequest } from '@/lib/adobeRelogin'

const pendingKey = 'chatgpt2api.adobeRelogin.pending'
export function useAdobeReloginRuntime(onFinished: () => void) {
  const state = ref<AdobeReloginSnapshot>({ tasks: [], requests: [], pollError: false })
  const expanded = ref(true), now = ref(Date.now()), dismissed = ref<string[]>([])
  let disposed = false
  let pollTimer: ReturnType<typeof setTimeout> | undefined
  let clockTimer: ReturnType<typeof setInterval> | undefined
  let restored: AdobeReloginRequest[] = []
  try {
    const saved: unknown = JSON.parse(sessionStorage.getItem(pendingKey) || '[]')
    if (Array.isArray(saved)) restored = saved.filter(item => item && typeof item.request_id === 'string' && Array.isArray(item.ids) && item.ids.length && item.ids.every((id: unknown) => typeof id === 'string')).slice(-100)
  } catch { /* Optional persistence; accepted tasks remain available from the server. */ }
  const tracker = createAdobeReloginTracker({
    start: (ids, requestId) => adobeAccountsApi.relogin(ids, requestId),
    list: requestId => adobeAccountsApi.listRelogins(requestId),
    isRejected: isAdobeReloginNotStarted,
  }, snapshot => {
    if (disposed) return
    state.value = snapshot
    try { sessionStorage.setItem(pendingKey, JSON.stringify(snapshot.requests)) } catch { /* Storage may be disabled. */ }
  }, () => { if (!disposed) onFinished() }, restored)
  const busyIds = computed(() => new Set([...state.value.requests.flatMap(request => request.ids), ...state.value.tasks.filter(task => !task.done).map(task => task.account_ref)]))
  const tasks = computed(() => state.value.tasks.filter(task => !task.done || !dismissed.value.includes(task.progress_id)))
  async function poll() {
    await tracker.sync()
    if (!disposed) pollTimer = setTimeout(poll, busyIds.value.size ? 1500 : 5000)
  }
  async function start(ids: string[]) {
    expanded.value = true
    await tracker.start(ids, crypto.randomUUID())
    await tracker.sync()
  }
  function dismiss(id: string) {
    if (state.value.tasks.some(task => task.progress_id === id && task.done)) dismissed.value.push(id)
  }
  onMounted(() => {
    void poll()
    clockTimer = setInterval(() => { now.value = Date.now() }, 1000)
  })
  onBeforeUnmount(() => { disposed = true; clearTimeout(pollTimer); clearInterval(clockTimer) })
  return { tasks, requests: computed(() => state.value.requests), pollError: computed(() => state.value.pollError), busyIds, expanded, now, start, dismiss, sync: tracker.sync }
}
