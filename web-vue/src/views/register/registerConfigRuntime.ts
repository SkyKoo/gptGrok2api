import { computed, reactive, ref, watch } from 'vue'
import { getAuthToken } from '@/api/client'
import { proxyApi, type ProxyGroup } from '@/api/proxy'
import { registerApi, type LegacyRegisterConfig, type RegisterTarget } from '@/api/register'
import { usePageQuery } from '@/composables/usePageQuery'
import type { PageRuntime } from '@/composables/usePageRuntime'
import {
  legacyRegisterPayload, normalizeRegisterConfig, normalizeRegisterProxyMode,
  normalizeRegisterTarget, registerProxyControlFromValue,
  registerProxyGroupOptions as buildRegisterProxyGroupOptions,
  registerProxyHint as buildRegisterProxyHint, registerProxyValueFromControl,
  type RegisterProxyMode,
} from '@/views/register/registerProviderView'

type ConfirmOptions = { title?: string; message: string; confirmText?: string; cancelText?: string }
export type RegisterConfigRuntimeInput = {
  runtime: PageRuntime
  confirm: (options: ConfirmOptions) => Promise<boolean>
  notifySuccess: (message: string) => void
  notifyError: (message: string) => void
  startLiveUpdates?: () => void
}
const TARGETS: RegisterTarget[] = ['openai', 'grok', 'adobe']
const RUNTIME_KEYS = ['enabled', 'stats', 'logs', 'jobs', 'jobs_total', 'jobs_page', 'jobs_page_size', 'runtime_error', 'compensation_error', 'registration_schedule'] as const
const PROVIDER_RUNTIME_KEYS = ['mailboxes_count', 'mailboxes_base_count', 'mailboxes_alias_count', 'mailboxes_preview', 'mailboxes_stats', 'mailboxes_parse_stats', 'mailboxes_failed'] as const
const label = (target: RegisterTarget) => target === 'adobe' ? 'Adobe' : target === 'grok' ? 'Grok' : 'OpenAI'
const emptyRuntime = () => ({ enabled: false, stats: {}, logs: [], jobs: [], jobs_total: 0, jobs_page: 1, jobs_page_size: 50, runtime_error: '', compensation_error: '' })
const newControls = () => ({
  saving: false, actionBusy: false,
  autosaveStatus: 'idle' as 'idle' | 'pending' | 'saving' | 'saved' | 'error',
  autosaveMessage: '', proxyMode: 'global' as RegisterProxyMode,
  selectedProxyGroupId: '', customProxyInput: '',
})

export function useRegisterConfigRuntime(input: RegisterConfigRuntimeInput) {
  const loading = ref(false)
  const proxyGroups = ref<ProxyGroup[]>([])
  // Channel selection is local view state. It never changes the backend's
  // legacy default target or the destination of an in-flight action.
  const selectedTarget = ref<RegisterTarget>('openai')
  const configurations = ref<Partial<Record<RegisterTarget, LegacyRegisterConfig>>>({})
  const controls = reactive({ openai: newControls(), grok: newControls(), adobe: newControls() })
  const savedSnapshots = ref<Partial<Record<RegisterTarget, string>>>({})
  const observedDrafts: Partial<Record<RegisterTarget, string>> = {}
  const timers: Partial<Record<RegisterTarget, ReturnType<typeof setTimeout>>> = {}
  const saves: Partial<Record<RegisterTarget, Promise<boolean>>> = {}
  const applyListeners = new Set<() => void>()
  let responseSequence = 0
  const appliedRuntimeSequence: Record<RegisterTarget, number> = { openai: 0, grok: 0, adobe: 0 }
  const saveRevision: Record<RegisterTarget, number> = { openai: 0, grok: 0, adobe: 0 }
  const config = computed({
    get: () => configurations.value[selectedTarget.value] || null,
    set: (value) => { if (value) configurations.value[selectedTarget.value] = value },
  })
  const saving = computed({ get: () => controls[selectedTarget.value].saving || controls[selectedTarget.value].actionBusy, set: v => { controls[selectedTarget.value].saving = v } })
  const autosaveStatus = computed(() => controls[selectedTarget.value].autosaveStatus)
  const autosaveMessage = computed(() => controls[selectedTarget.value].autosaveMessage)
  const proxyMode = computed({ get: () => controls[selectedTarget.value].proxyMode, set: v => { controls[selectedTarget.value].proxyMode = v } })
  const selectedProxyGroupId = computed({ get: () => controls[selectedTarget.value].selectedProxyGroupId, set: v => { controls[selectedTarget.value].selectedProxyGroupId = v } })
  const customProxyInput = computed({ get: () => controls[selectedTarget.value].customProxyInput, set: v => { controls[selectedTarget.value].customProxyInput = v } })
  const providers = computed(() => config.value?.mail.providers || [])
  const proxyGroupOptions = computed(() => buildRegisterProxyGroupOptions(proxyGroups.value, selectedProxyGroupId.value))
  const proxyGroupGroups = computed(() => [{ options: proxyGroupOptions.value }])
  const proxyHint = computed(() => buildRegisterProxyHint(proxyMode.value, selectedTarget.value))
  const configQuery = usePageQuery({ runtime: input.runtime, key: 'register:config', loading, errorMessage: '加载注册配置失败' })
  const proxyGroupsQuery = usePageQuery({ runtime: input.runtime, key: 'register:proxy-groups' })

  function payload(target = selectedTarget.value): Partial<LegacyRegisterConfig> {
    const current = configurations.value[target]
    if (!current) return {}
    const raw = legacyRegisterPayload(current)
    const result: Partial<LegacyRegisterConfig> = {
      target, ...raw.target_settings?.[target],
      target_settings: { [target]: raw.target_settings?.[target] },
      mail: { source_id: String(current.mail.source_id || '').trim() },
    }
    if (target === 'adobe') result.adobe = raw.adobe
    else if (target === 'grok') result.grok = raw.grok
    else {
      result.openai_free = raw.openai_free
      result.checkout = raw.checkout
      result.sub2api_sync = raw.sub2api_sync
      result.cpa_sync = raw.cpa_sync
      result.agent_identity_archive = raw.agent_identity_archive
    }
    return result
  }
  const snapshot = (target: RegisterTarget) => JSON.stringify(payload(target))
  const unsaved = (target: RegisterTarget) => Boolean(configurations.value[target] && savedSnapshots.value[target] && snapshot(target) !== savedSnapshots.value[target])
  const hasUnsavedChanges = computed(() => unsaved(selectedTarget.value))

  function syncProxyControls(target: RegisterTarget, value: unknown) {
    const next = registerProxyControlFromValue(value)
    Object.assign(controls[target], { proxyMode: next.mode, selectedProxyGroupId: next.groupId, customProxyInput: next.customProxy })
  }
  function onConfigApplied(callback: () => void) {
    applyListeners.add(callback)
    return () => applyListeners.delete(callback)
  }
  function channelConfig(incoming: LegacyRegisterConfig, target: RegisterTarget) {
    const state = incoming.target_runtime?.[target] || (normalizeRegisterTarget(incoming.target) === target ? incoming : emptyRuntime())
    return normalizeRegisterConfig(JSON.parse(JSON.stringify({ ...incoming, ...state, target })))
  }
  function seedChannels(incoming: LegacyRegisterConfig) {
    for (const target of TARGETS) {
      if (configurations.value[target]) continue
      configurations.value[target] = channelConfig(incoming, target)
      savedSnapshots.value[target] = snapshot(target)
      observedDrafts[target] = snapshot(target)
      controls[target].autosaveStatus = 'saved'
      controls[target].autosaveMessage = '配置已保存'
      syncProxyControls(target, configurations.value[target]!.proxy)
    }
  }
  function applyConfig(next: LegacyRegisterConfig, preserveProxyControls = false, sequence = ++responseSequence) {
    const target = normalizeRegisterTarget(next.target)
    seedChannels(next)
    const previous = configurations.value[target]!
    configurations.value[target] = channelConfig(next, target)
    if (sequence < appliedRuntimeSequence[target]) {
      for (const key of RUNTIME_KEYS) Object.assign(configurations.value[target]!, { [key]: previous[key] })
    }
    savedSnapshots.value[target] = snapshot(target)
    observedDrafts[target] = snapshot(target)
    controls[target].autosaveStatus = 'saved'
    controls[target].autosaveMessage = '配置已保存'
    if (!preserveProxyControls) syncProxyControls(target, configurations.value[target]!.proxy)
    applyRuntimeConfig(next, sequence)
    if (target === selectedTarget.value) applyListeners.forEach(callback => callback())
  }
  function applyRuntimeConfig(next: LegacyRegisterConfig, sequence = ++responseSequence) {
    seedChannels(next)
    for (const target of TARGETS) {
      if (sequence < appliedRuntimeSequence[target]) continue
      appliedRuntimeSequence[target] = sequence
      const current = configurations.value[target]!
      const state = next.target_runtime?.[target] || (normalizeRegisterTarget(next.target) === target ? next : null)
      if (state) {
        for (const key of RUNTIME_KEYS) {
          if (Object.prototype.hasOwnProperty.call(state, key)) Object.assign(current, { [key]: state[key as keyof typeof state] })
        }
      }
      // Source metadata is shared, but IDs (not array positions) identify it.
      current.mail.providers = (current.mail.providers || []).map(provider => {
        const incoming = (next.mail?.providers || []).find(row => row.id === provider.id && row.type === provider.type)
        if (!incoming) return provider
        const merged = { ...provider }
        for (const key of PROVIDER_RUNTIME_KEYS) if (Object.prototype.hasOwnProperty.call(incoming, key)) Object.assign(merged, { [key]: incoming[key] })
        return merged
      })
      if (target === 'openai') {
        current.checkout_logs = next.checkout_logs
        current.checkout_tasks = next.checkout_tasks
        current.checkout_retries_active = next.checkout_retries_active
        current.checkout_retry_job_count = next.checkout_retry_job_count
      } else current.grok_oauth_logs = next.grok_oauth_logs
    }
  }
  function setTarget(value: string) {
    const target = normalizeRegisterTarget(value)
    if (!configurations.value[target]) return
    selectedTarget.value = target
    applyListeners.forEach(callback => callback())
  }
  function setProxyMode(mode: string) {
    proxyMode.value = normalizeRegisterProxyMode(mode)
    if (config.value) config.value.proxy = registerProxyValueFromControl(proxyMode.value, selectedProxyGroupId.value, customProxyInput.value)
  }
  function selectProxyGroup(id: string) {
    selectedProxyGroupId.value = String(id || '').trim()
    setProxyMode('group')
  }
  function setCustomProxyInput(value: string) {
    customProxyInput.value = String(value || '').trim()
    setProxyMode('custom')
  }
  function clearAutosaveTimer(target: RegisterTarget) {
    if (timers[target] !== undefined) clearTimeout(timers[target])
    delete timers[target]
  }
  function scheduleAutosave(target: RegisterTarget) {
    clearAutosaveTimer(target)
    if (!unsaved(target)) return
    controls[target].autosaveStatus = 'pending'
    controls[target].autosaveMessage = '等待自动保存...'
    timers[target] = setTimeout(() => { delete timers[target]; void persistConfig(false, target) }, 800)
  }
  async function persistConfig(notify = false, target = selectedTarget.value): Promise<boolean> {
    clearAutosaveTimer(target)
    if (saves[target]) return saves[target]!
    if (!unsaved(target)) return true
    const sequence = ++responseSequence
    saveRevision[target]++
    const requestPayload = payload(target)
    const requestSnapshot = JSON.stringify(requestPayload)
    controls[target].saving = true
    controls[target].autosaveStatus = 'saving'
    controls[target].autosaveMessage = '正在自动保存...'
    saves[target] = (async () => {
      try {
        const response = await registerApi.updateConfig(requestPayload, target)
        if (normalizeRegisterTarget(response.register.target) !== target) throw new Error('返回的注册渠道不匹配，请刷新后重试')
        if (snapshot(target) === requestSnapshot) {
          applyConfig(response.register, response.register.proxy === configurations.value[target]?.proxy, sequence)
        } else {
          // A later draft remains local; the saved request is this channel's
          // baseline. A response from the other tab can never replace it.
          savedSnapshots.value[target] = requestSnapshot
          applyRuntimeConfig(response.register, sequence)
          controls[target].autosaveStatus = 'saved'
          controls[target].autosaveMessage = '已保存，正在同步最新修改...'
        }
        if (notify) input.notifySuccess(`${label(target)} 配置已保存`)
        return true
      } catch (error: any) {
        controls[target].autosaveStatus = 'error'
        controls[target].autosaveMessage = error?.message || '自动保存注册配置失败'
        if (notify) input.notifyError(`${label(target)}：${controls[target].autosaveMessage}`)
        return false
      } finally {
        controls[target].saving = false
        delete saves[target]
        if (controls[target].autosaveStatus !== 'error' && unsaved(target)) scheduleAutosave(target)
      }
    })()
    return saves[target]!
  }
  async function flushAutosave(target = selectedTarget.value): Promise<boolean> {
    clearAutosaveTimer(target)
    while (saves[target] || unsaved(target)) {
      if (!await (saves[target] || persistConfig(false, target))) return false
      clearAutosaveTimer(target)
    }
    return true
  }
  watch(configurations, () => {
    for (const target of TARGETS) {
      if (!configurations.value[target]) continue
      const current = snapshot(target)
      if (observedDrafts[target] === current) continue
      observedDrafts[target] = current
      if (unsaved(target)) scheduleAutosave(target)
    }
  }, { deep: true, flush: 'post' })

  async function loadConfig(silent = false) {
    const target = selectedTarget.value
    const sequence = ++responseSequence
    const revision = saveRevision[target]
    await configQuery.run(() => registerApi.getConfig(target), {
      apply: response => {
        if (unsaved(target) || saves[target] || revision !== saveRevision[target]) applyRuntimeConfig(response.register, sequence)
        else applyConfig(response.register, false, sequence)
      },
      onError: message => { if (!silent) input.notifyError(message) }, silentLoading: silent,
    })
  }
  async function loadRuntimeConfig() {
    const target = selectedTarget.value
    const sequence = ++responseSequence
    await configQuery.run(() => registerApi.getConfig(target), { apply: response => applyRuntimeConfig(response.register, sequence), silentLoading: true })
  }
  async function loadProxyGroups() {
    await proxyGroupsQuery.run(() => proxyApi.listGroups(), {
      apply: response => { proxyGroups.value = (response.groups || []).filter(group => String(group?.id || '').trim()) },
      onError: () => { proxyGroups.value = [] },
    })
  }
  async function toggleTask() {
    const target = selectedTarget.value
    const current = configurations.value[target]
    if (!current || controls[target].actionBusy) return
    const starting = !current.enabled
    controls[target].actionBusy = true
    try {
      const ok = await input.confirm({
        title: `${starting ? '启动' : '停止'} ${label(target)} 注册任务`,
        message: starting ? `将保存 ${label(target)} 配置并启动该渠道注册任务。` : `将停止 ${label(target)} 的注册任务。`,
        confirmText: starting ? '启动' : '停止',
      })
      if (!ok) return
      if (starting && !await flushAutosave(target)) { input.notifyError(`${label(target)} 配置保存失败，暂未启动`); return }
      const sequence = ++responseSequence
      const response = starting ? await registerApi.startLegacy(target) : await registerApi.stopLegacy(target)
      applyRuntimeConfig(response.register, sequence)
      input.notifySuccess(`${label(target)} 注册任务已${starting ? '启动' : '停止'}`)
      input.startLiveUpdates?.()
    } catch (error: any) { input.notifyError(error?.message || `${label(target)} 注册任务操作失败`) }
    finally { controls[target].actionBusy = false }
  }
  async function resetStats() {
    const target = selectedTarget.value
    if (controls[target].actionBusy) return
    controls[target].actionBusy = true
    try {
      const ok = await input.confirm({ title: `清理 ${label(target)} 历史记录`, message: `仅清理 ${label(target)} 已结束且允许清理的任务记录；邮箱和待恢复任务会保留。`, confirmText: '清理' })
      if (!ok) return
      const sequence = ++responseSequence
      const response = await registerApi.resetLegacy(target)
      applyRuntimeConfig(response.register, sequence)
      input.notifySuccess(`${label(target)} 已清理 ${response.removed || 0} 条任务记录`)
    } catch (error: any) { input.notifyError(error?.message || '清理任务记录失败') }
    finally { controls[target].actionBusy = false }
  }
  function applySchedule(schedule: LegacyRegisterConfig['registration_schedule'], target: RegisterTarget) {
    const current = configurations.value[target]
    if (current) current.registration_schedule = schedule
  }
  function invalidate() { configQuery.invalidate(); proxyGroupsQuery.invalidate() }
  function hasScheduledRegistration() { return TARGETS.some(target => configurations.value[target]?.registration_schedule?.enabled) }
  function hasActiveCheckoutRetries() {
    const current = configurations.value.openai
    return current?.checkout_retries_active === true || Boolean(current?.checkout_tasks?.some(task => ['queued', 'running', 'retrying', 'pending'].includes(String(task.status || '').toLowerCase())))
  }
  function isTaskEnabled() { return TARGETS.some(target => configurations.value[target]?.enabled) }
  return {
    authToken: getAuthToken, loading, saving, autosaveStatus, autosaveMessage,
    proxyGroups, proxyMode, selectedProxyGroupId, customProxyInput, config,
    hasUnsavedChanges, providers, proxyGroupOptions, proxyGroupGroups, proxyHint,
    applyConfig, applyRuntimeConfig, onConfigApplied, setTarget, setProxyMode,
    selectProxyGroup, setCustomProxyInput, payload, loadConfig, loadRuntimeConfig,
    loadProxyGroups, saveConfig: () => persistConfig(true), flushAutosave,
    toggleTask, resetStats, invalidate, isTaskEnabled, applySchedule, hasScheduledRegistration, hasActiveCheckoutRetries,
  }
}
