import { computed, ref, watch } from 'vue'
import { getAuthToken } from '@/api/client'
import { proxyApi, type ProxyGroup } from '@/api/proxy'
import { registerApi, type LegacyRegisterConfig } from '@/api/register'
import { usePageQuery } from '@/composables/usePageQuery'
import type { PageRuntime } from '@/composables/usePageRuntime'
import {
  legacyRegisterPayload,
  normalizeRegisterConfig,
  normalizeRegisterProxyMode,
  normalizeRegisterTarget,
  providerForRegisterTarget,
  registerProxyControlFromValue,
  registerProxyGroupOptions as buildRegisterProxyGroupOptions,
  registerProxyHint as buildRegisterProxyHint,
  registerProxyValueFromControl,
  type RegisterProxyMode,
} from '@/views/register/registerProviderView'

type ConfirmOptions = {
  title?: string
  message: string
  confirmText?: string
  cancelText?: string
}

export type RegisterConfigRuntimeInput = {
  runtime: PageRuntime
  confirm: (options: ConfirmOptions) => Promise<boolean>
  notifySuccess: (message: string) => void
  notifyError: (message: string) => void
  startLiveUpdates?: () => void
}

const REGISTER_CONFIG_REQUEST_KEY = 'register:config'
const PROXY_GROUPS_REQUEST_KEY = 'register:proxy-groups'
const AUTOSAVE_DELAY_MS = 800
const PROVIDER_RUNTIME_KEYS = [
  'mailboxes_count',
  'mailboxes_base_count',
  'mailboxes_alias_count',
  'mailboxes_preview',
  'mailboxes_stats',
  'mailboxes_parse_stats',
  'mailboxes_failed',
] as const

export function useRegisterConfigRuntime(input: RegisterConfigRuntimeInput) {
  const loading = ref(false)
  const saving = ref(false)
  const autosaveStatus = ref<'idle' | 'pending' | 'saving' | 'saved' | 'error'>('idle')
  const autosaveMessage = ref('')
  const proxyGroups = ref<ProxyGroup[]>([])
  const proxyMode = ref<RegisterProxyMode>('global')
  const selectedProxyGroupId = ref('')
  const customProxyInput = ref('')
  const config = ref<LegacyRegisterConfig | null>(null)
  let savedConfigSnapshot = ''
  let autosaveTimer: ReturnType<typeof setTimeout> | undefined
  let savePromise: Promise<boolean> | null = null
  const applyListeners = new Set<() => void>()

  const configQuery = usePageQuery({
    runtime: input.runtime,
    key: REGISTER_CONFIG_REQUEST_KEY,
    loading,
    errorMessage: '加载注册配置失败',
  })
  const proxyGroupsQuery = usePageQuery({
    runtime: input.runtime,
    key: PROXY_GROUPS_REQUEST_KEY,
  })

  const providers = computed(() => config.value?.mail.providers || [])
  const proxyGroupOptions = computed(() => buildRegisterProxyGroupOptions(proxyGroups.value, selectedProxyGroupId.value))
  const proxyGroupGroups = computed(() => [{ options: proxyGroupOptions.value }])
  const proxyHint = computed(() => buildRegisterProxyHint(proxyMode.value))
  const configSnapshot = (value: LegacyRegisterConfig) => JSON.stringify(legacyRegisterPayload(value))
  const hasUnsavedChanges = computed(() => {
    const current = config.value
    if (!current || !savedConfigSnapshot) return false
    return configSnapshot(current) !== savedConfigSnapshot
  })

  function onConfigApplied(callback: () => void) {
    applyListeners.add(callback)
    return () => applyListeners.delete(callback)
  }

  function syncProxyControlsFromValue(value: unknown) {
    const controls = registerProxyControlFromValue(value)
    proxyMode.value = controls.mode
    selectedProxyGroupId.value = controls.groupId
    customProxyInput.value = controls.customProxy
  }

  function applyConfig(nextConfig: LegacyRegisterConfig) {
    config.value = normalizeRegisterConfig(nextConfig)
    savedConfigSnapshot = configSnapshot(config.value)
    autosaveStatus.value = 'saved'
    autosaveMessage.value = '配置已保存'
    syncProxyControlsFromValue(config.value.proxy)
    applyListeners.forEach((callback) => callback())
  }

  function applyRuntimeConfig(nextConfig: LegacyRegisterConfig) {
    if (!config.value) {
      applyConfig(nextConfig)
      return
    }

    const incoming = normalizeRegisterConfig(nextConfig)
    const incomingProviders = incoming.mail.providers || []
    const currentProviders = config.value.mail.providers || []
    const providersWithRuntimeState = currentProviders.map((provider, index) => {
      const incomingProvider = incomingProviders[index]
      if (!incomingProvider) return provider
      const nextProvider = { ...provider }
      for (const key of PROVIDER_RUNTIME_KEYS) {
        if (Object.prototype.hasOwnProperty.call(incomingProvider, key)) {
          Object.assign(nextProvider, { [key]: incomingProvider[key] })
        }
      }
      return nextProvider
    })

    config.value.enabled = incoming.enabled
    config.value.stats = incoming.stats
    config.value.logs = incoming.logs
    config.value.jobs = incoming.jobs
    config.value.grok_oauth_logs = incoming.grok_oauth_logs
    config.value.checkout_logs = incoming.checkout_logs
    config.value.checkout_tasks = incoming.checkout_tasks
    config.value.checkout_retries_active = incoming.checkout_retries_active
    config.value.checkout_retry_job_count = incoming.checkout_retry_job_count
    config.value.mail.providers = providersWithRuntimeState
  }

  function setProxyMode(mode: string) {
    proxyMode.value = normalizeRegisterProxyMode(mode)
    if (!config.value) return
    config.value.proxy = registerProxyValueFromControl(proxyMode.value, selectedProxyGroupId.value, customProxyInput.value)
  }

  function setTarget(value: string) {
    if (!config.value) return
    const target = normalizeRegisterTarget(value)
    config.value.target = target
    if (target === 'grok') config.value.mode = 'total'
    config.value.mail.providers = providers.value.map(provider => providerForRegisterTarget(provider, target))
  }

  function selectProxyGroup(groupId: string) {
    selectedProxyGroupId.value = String(groupId || '').trim()
    proxyMode.value = 'group'
    if (config.value) {
      config.value.proxy = registerProxyValueFromControl(proxyMode.value, selectedProxyGroupId.value, customProxyInput.value)
    }
  }

  function setCustomProxyInput(value: string) {
    customProxyInput.value = String(value || '').trim()
    proxyMode.value = 'custom'
    if (config.value) {
      config.value.proxy = registerProxyValueFromControl(proxyMode.value, selectedProxyGroupId.value, customProxyInput.value)
    }
  }

  function payload(): Partial<LegacyRegisterConfig> {
    if (!config.value) return {}
    return legacyRegisterPayload({
      ...config.value,
      mail: {
        ...config.value.mail,
        providers: providers.value,
      },
    })
  }

  function clearAutosaveTimer() {
    if (autosaveTimer === undefined) return
    clearTimeout(autosaveTimer)
    autosaveTimer = undefined
  }

  function scheduleAutosave() {
    clearAutosaveTimer()
    if (!config.value || !savedConfigSnapshot || !hasUnsavedChanges.value) return
    autosaveStatus.value = 'pending'
    autosaveMessage.value = '等待自动保存...'
    autosaveTimer = setTimeout(() => {
      autosaveTimer = undefined
      void persistConfig()
    }, AUTOSAVE_DELAY_MS)
  }

  async function persistConfig(notify = false): Promise<boolean> {
    clearAutosaveTimer()
    if (!config.value || !hasUnsavedChanges.value) return true
    if (savePromise) return savePromise

    const requestSnapshot = configSnapshot(config.value)
    saving.value = true
    autosaveStatus.value = 'saving'
    autosaveMessage.value = '正在自动保存...'
    savePromise = (async () => {
      try {
        const response = await registerApi.updateConfig(payload())
        const normalizedResponse = normalizeRegisterConfig(response.register)
        if (config.value && configSnapshot(config.value) === requestSnapshot) {
          applyConfig(normalizedResponse)
        } else {
          // Keep edits made while the request was in flight and use the
          // server response as the new baseline for the next autosave.
          savedConfigSnapshot = configSnapshot(normalizedResponse)
          autosaveStatus.value = 'saved'
          autosaveMessage.value = '已保存，正在同步最新修改...'
        }
        if (notify) input.notifySuccess('注册配置已自动保存')
        return true
      } catch (error: any) {
        const message = error?.message || '自动保存注册配置失败'
        autosaveStatus.value = 'error'
        autosaveMessage.value = message
        if (notify) input.notifyError(message)
        return false
      } finally {
        saving.value = false
        savePromise = null
        if (autosaveStatus.value !== 'error' && hasUnsavedChanges.value) scheduleAutosave()
      }
    })()
    return savePromise
  }

  async function flushAutosave(): Promise<boolean> {
    clearAutosaveTimer()
    if (savePromise) return savePromise
    return persistConfig()
  }

  watch(
    config,
    () => {
      if (config.value && savedConfigSnapshot && hasUnsavedChanges.value) scheduleAutosave()
    },
    { deep: true, flush: 'post' },
  )

  async function loadConfig(silent = false) {
    await configQuery.run(
      () => registerApi.getConfig(),
      {
        apply: (response) => {
          applyConfig(response.register)
        },
        onError: (message) => {
          if (!silent) input.notifyError(message)
        },
        silentLoading: silent,
      },
    )
  }

  async function loadRuntimeConfig() {
    await configQuery.run(
      () => registerApi.getConfig(),
      {
        apply: (response) => {
          applyRuntimeConfig(response.register)
        },
        silentLoading: true,
      },
    )
  }

  async function loadProxyGroups() {
    await proxyGroupsQuery.run(
      () => proxyApi.listGroups(),
      {
        apply: (response) => {
          proxyGroups.value = Array.isArray(response.groups)
            ? response.groups.filter((group) => String(group?.id || '').trim())
            : []
        },
        onError: () => {
          proxyGroups.value = []
        },
      },
    )
  }

  async function saveConfig() {
    await persistConfig(true)
  }

  async function toggleTask() {
    if (!config.value) return
    const starting = !config.value.enabled
    const ok = await input.confirm({
      title: starting ? '启动注册任务' : '停止注册任务',
      message: starting ? '将保存当前配置并启动注册任务。' : '将停止当前注册任务。',
      confirmText: starting ? '启动' : '停止',
    })
    if (!ok) return
    saving.value = true
    try {
      if (starting) {
        const saved = await flushAutosave()
        if (!saved) {
          input.notifyError('配置自动保存失败，暂未启动注册任务')
          return
        }
      }
      const response = starting ? await registerApi.startLegacy() : await registerApi.stopLegacy()
      applyConfig(response.register)
      input.notifySuccess(starting ? '注册任务已启动' : '注册任务已停止')
      if (starting) input.startLiveUpdates?.()
    } catch (error: any) {
      input.notifyError(error?.message || '切换注册任务失败')
    } finally {
      saving.value = false
    }
  }

  async function resetStats() {
    const free = config.value?.target === 'openai'
    const ok = await input.confirm({
      title: free ? '清理历史记录' : '重置注册统计',
      message: free ? '清理成功记录，以及未保存邮箱或账号的失败、取消和中断记录。已创建的邮箱、待恢复任务、账号及配置会保留。' : '将清空当前注册任务的统计和运行日志。',
      confirmText: free ? '清理' : '重置',
    })
    if (!ok) return
    saving.value = true
    try {
      const response = await registerApi.resetLegacy()
      applyConfig(response.register)
      input.notifySuccess(free ? `已清理 ${response.removed || 0} 条任务记录` : '注册统计已重置')
    } catch (error: any) {
      input.notifyError(error?.message || (free ? '清理任务记录失败' : '重置注册统计失败'))
    } finally {
      saving.value = false
    }
  }

  function invalidate() {
    configQuery.invalidate()
    proxyGroupsQuery.invalidate()
  }

  function isTaskEnabled() {
    return Boolean(config.value?.enabled)
  }

  return {
    authToken: getAuthToken,
    loading,
    saving,
    autosaveStatus,
    autosaveMessage,
    proxyGroups,
    proxyMode,
    selectedProxyGroupId,
    customProxyInput,
    config,
    hasUnsavedChanges,
    providers,
    proxyGroupOptions,
    proxyGroupGroups,
    proxyHint,
    applyConfig,
    applyRuntimeConfig,
    onConfigApplied,
    setTarget,
    setProxyMode,
    selectProxyGroup,
    setCustomProxyInput,
    payload,
    loadConfig,
    loadRuntimeConfig,
    loadProxyGroups,
    saveConfig,
    flushAutosave,
    toggleTask,
    resetStats,
    invalidate,
    isTaskEnabled,
  }
}
