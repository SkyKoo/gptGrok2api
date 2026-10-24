import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { accountsApi, type AccountTwoFATask } from '@/api/accounts'
import { useToast } from '@/composables/useToast'

export const twoFAStageLabels: Record<string, string> = {
  queued: '准备中', reauth: '邮箱重认证', waiting_email_code: '等待邮箱验证码',
  verifying_email_code: '验证邮箱验证码', enroll: '申请 TOTP 密钥',
  activate: '激活 2FA', completed: '开启完成', interrupted: '服务重启中断',
}

export function useAccountTwoFARuntime(options: {
  loadData: (options?: { silentErrorToast?: boolean }) => Promise<void>
  errorMessage: (error: unknown) => string
}) {
  const tasks = ref<AccountTwoFATask[]>([])
  const starting = ref<string[]>([])
  // Keep the account list as the primary workspace. Users can expand the
  // progress panel when they need to enter a code or inspect a task.
  const expanded = ref(false)
  const pollError = ref(false)
  const dismissed = ref<string[]>([])
  const submitting = ref<string[]>([])
  const toast = useToast()
  let disposed = false
  let timer: ReturnType<typeof setTimeout> | undefined
  const busyIds = computed(() => new Set([...starting.value, ...tasks.value.filter(t => t.status === 'running').map(t => t.account_ref)]))
  const visibleTasks = computed(() => tasks.value.filter(t => t.status === 'running' || !dismissed.value.includes(t.progress_id)))
  function showProgress() {
    expanded.value = true
    window.requestAnimationFrame(() => document.getElementById('account-twofa-progress')?.scrollIntoView({ behavior: 'smooth', block: 'nearest' }))
  }
  async function sync() {
    try {
      const response = await accountsApi.listTwoFA()
      if (disposed) return
      const previous = new Map(tasks.value.map(t => [t.progress_id, t.status]))
      const finished = response.items.some(t => t.status !== 'running' && previous.get(t.progress_id) === 'running')
      tasks.value = response.items
      pollError.value = false
      if (finished) void options.loadData({ silentErrorToast: true }).catch(() => {})
    } catch { if (!disposed) pollError.value = true }
  }
  async function start(accountId: string) {
    if (busyIds.value.has(accountId)) return
    starting.value = [...starting.value, accountId]
    try {
      const result = await accountsApi.startTwoFA(accountId)
      if (disposed) return
      tasks.value = [result.progress, ...tasks.value.filter(t => t.account_ref !== accountId)]
      dismissed.value = dismissed.value.filter(id => id !== result.progress.progress_id)
    } catch (error) {
      // A lost response can hide an accepted task; reconcile without resubmitting.
      await sync()
      if (!tasks.value.some(t => t.account_ref === accountId && t.status === 'running')) toast.error(options.errorMessage(error))
    } finally { starting.value = starting.value.filter(id => id !== accountId) }
  }
  async function submit(task: AccountTwoFATask, action: 'code' | 'cancel', code = '') {
    if (submitting.value.includes(task.progress_id)) return
    submitting.value = [...submitting.value, task.progress_id]
    try {
      await accountsApi.twoFAAction(task.progress_id, action, code)
      await sync()
    } catch (error) { toast.error(options.errorMessage(error)) }
    finally { submitting.value = submitting.value.filter(id => id !== task.progress_id) }
  }
  async function poll() {
    await sync()
    if (!disposed) timer = setTimeout(poll, busyIds.value.size ? 1000 : 5000)
  }
  onMounted(() => { void poll() })
  onBeforeUnmount(() => { disposed = true; clearTimeout(timer) })
  return reactive({ tasks: visibleTasks, starting, expanded, pollError, busyIds, submitting, start, submit, showProgress,
    dismiss: (id: string) => { if (tasks.value.some(t => t.progress_id === id && t.status !== 'running')) dismissed.value.push(id) },
  })
}
