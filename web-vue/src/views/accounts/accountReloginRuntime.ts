import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { accountsApi } from '@/api/accounts'
import { useConfirmDialog } from '@/composables/useConfirmDialog'
import { useToast } from '@/composables/useToast'
import { createReloginTracker, type ReloginSnapshot } from '@/lib/accountRelogin'

const DISMISSED_KEY = 'chatgpt2api.relogin.dismissed'

export function useAccountReloginRuntime(options: {
  emailFor: (id: string) => string
  loadData: (options?: { silentErrorToast?: boolean }) => Promise<void>
}) {
  const snapshot = ref<ReloginSnapshot>({ tasks: [], starting: [], pollError: false })
  const dismissed = ref<string[]>([])
  try {
    const value: unknown = JSON.parse(window.localStorage.getItem(DISMISSED_KEY) || '[]')
    if (Array.isArray(value)) dismissed.value = value.filter((id): id is string => typeof id === 'string').slice(-300)
  } catch { /* Storage can be disabled; the progress panel still works. */ }
  const expanded = ref(true)
  const now = ref(Date.now())
  let disposed = false
  let confirming = false
  let pollTimer: ReturnType<typeof setTimeout> | undefined
  let clockTimer: ReturnType<typeof setInterval> | undefined
  const confirm = useConfirmDialog()
  const toast = useToast()
  const tracker = createReloginTracker({
    start: async (id) => (await accountsApi.startRelogin(id)).progress,
    list: async () => (await accountsApi.listRelogins()).items,
  }, (state) => { if (!disposed) snapshot.value = state }, () => {
    if (!disposed) void options.loadData({ silentErrorToast: true }).catch(() => {})
  })
  const tasks = computed(() => snapshot.value.tasks.filter(task => !task.done || !dismissed.value.includes(task.progress_id)))
  const busyIds = computed(() => new Set([
    ...snapshot.value.starting,
    ...snapshot.value.tasks.filter(t => !t.done).map(t => t.account_ref),
  ]))
  function showProgress() {
    expanded.value = true
    window.requestAnimationFrame(() => document.getElementById('account-relogin-progress')?.scrollIntoView({ behavior: 'smooth', block: 'nearest' }))
  }
  async function start(accountId: string) {
    if (tracker.busy(accountId)) { showProgress(); return }
    if (confirming) return
    confirming = true
    let allowed = false
    try {
      const email = options.emailFor(accountId) || snapshot.value.tasks.find(t => t.account_ref === accountId)?.email || accountId
      allowed = await confirm.ask({ title: '确认重新登录账号', message: `将读取 ${email} 的邮箱验证码，获取新会话并更新原账号。已有任务会继续运行。`, confirmText: '重新登录', cancelText: '取消' })
    } finally { confirming = false }
    if (!allowed || disposed) return
    try {
      const submitted = tracker.start(accountId)
      showProgress()
      await submitted
      showProgress()
      await tracker.sync()
    } catch {
      // Submission may have succeeded despite a lost response. Reconcile first;
      // only an explicit user retry may submit again (the server deduplicates).
      await tracker.sync()
      if (!tracker.busy(accountId)) toast.error('未能确认重新登录是否已启动，请查看进度区或稍后重试。')
    }
  }
  function dismiss(id: string) {
    if (!snapshot.value.tasks.some(t => t.progress_id === id && t.done)) return
    dismissed.value = [...new Set([...dismissed.value, id])].slice(-300)
    try { window.localStorage.setItem(DISMISSED_KEY, JSON.stringify(dismissed.value)) } catch { /* Optional UI preference. */ }
  }
  async function poll() {
    await tracker.sync()
    if (!disposed) pollTimer = setTimeout(poll, busyIds.value.size ? 1000 : 5000)
  }
  onMounted(() => {
    void poll()
    clockTimer = setInterval(() => { now.value = Date.now() }, 1000)
  })
  onBeforeUnmount(() => {
    disposed = true
    clearTimeout(pollTimer)
    clearInterval(clockTimer)
  })
  return { tasks, starting: computed(() => snapshot.value.starting), busyIds, pollError: computed(() => snapshot.value.pollError), expanded, now, start, dismiss, showProgress }
}
