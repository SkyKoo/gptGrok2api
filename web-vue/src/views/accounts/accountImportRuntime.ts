import { onUnmounted, ref } from 'vue'
import { parseSessionJsonAccount } from '@/lib/sessionImport'

import { accountsApi } from '@/api/accounts'
import { accountImportsApi } from '@/api/accountImports'
import { useConfirmDialog } from '@/composables/useConfirmDialog'
import { useToast } from '@/composables/useToast'
import type { useAccountBulkProgressRuntime } from './accountBulkProgressRuntime'

export type AccountImportMode = 'oauth_login' | 'codex_oauth' | 'access_token' | 'session_json' | 'cpa_json' | 'remote_cpa' | 'sub2api'

const IMPORT_BATCH_SIZE = 20

type AccountImportRuntimeOptions = {
  bulkProgress: ReturnType<typeof useAccountBulkProgressRuntime>
  normalizeErrorMessage: (error: unknown) => string
  setError: (prefix: string, error: unknown, notify?: boolean) => void
  loadData: (options?: { silentErrorToast?: boolean }) => Promise<void>
  revealImportedAccount: (ref: string) => Promise<void>
}

function uniqueTokens(tokens: string[]) {
  return Array.from(new Set(tokens.map((token) => token.trim()).filter(Boolean)))
}

function parseTokenLines(text: string) {
  return uniqueTokens(
    text
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter((line) => line && !line.startsWith('#')),
  )
}

type AccountImportPayload = Record<string, unknown>

function accountPayloadToken(value: unknown): string {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return ''
  const source = value as AccountImportPayload
  return String(source.access_token || source.accessToken || '').trim()
}

function tokenFromCPAAccount(value: unknown): string {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return ''
  const source = value as Record<string, unknown>
  return String(source.access_token || source.accessToken || '').trim()
}

function parseCPAJsonTokens(rawText: string, label: string) {
  const text = rawText.trim()
  if (!text) throw new Error(`${label} 是空文件`)
  const parsed = JSON.parse(text)
  const candidates: unknown[] = []

  if (Array.isArray(parsed)) {
    candidates.push(...parsed)
  } else if (parsed && typeof parsed === 'object') {
    if (tokenFromCPAAccount(parsed)) {
      candidates.push(parsed)
    } else {
      const source = parsed as Record<string, unknown>
      for (const key of ['accounts', 'items', 'data', 'results']) {
        const rows = source[key]
        if (Array.isArray(rows)) candidates.push(...rows)
      }
    }
  }

  const tokens = uniqueTokens(candidates.map(tokenFromCPAAccount).filter(Boolean))
  if (!tokens.length) throw new Error(`${label} 中没有找到 access_token`)
  return tokens
}

export function useAccountImportRuntime(options: AccountImportRuntimeOptions) {
  const importBusy = ref(false)
  const showImportModal = ref(false)
  const importMode = ref<AccountImportMode>('access_token')
  const oauthEmailHint = ref('')
  const oauthCallbackText = ref('')
  const oauthSessionId = ref('')
  const oauthAuthorizeUrl = ref('')
  const oauthRedirectUriPrefix = ref('')
  const codexAccountRef = ref('')
  const codexCPAPoolID = ref('')
  const codexCPAPools = ref<Array<{ id: string; name: string; base_url: string }>>([])
  const codexOAuthStage = ref('')
  const codexOAuthMessage = ref('')
  let codexProgressTimer: ReturnType<typeof setInterval> | null = null
  const manualTokenText = ref('')
  const sessionJsonText = ref('')
  const toast = useToast()
  const confirmDialog = useConfirmDialog()

  onUnmounted(() => stopCodexOAuthProgressPolling())

  const importModeOptions = [
    { label: 'OAuth 登录已有账号', value: 'oauth_login' },
    { label: 'Codex OAuth + CPA', value: 'codex_oauth' },
    { label: '导入 Access Token', value: 'access_token' },
    { label: '导入 Session JSON', value: 'session_json' },
    { label: '导入 CPA JSON 文件', value: 'cpa_json' },
    { label: '从远程 CPA 服务器导入', value: 'remote_cpa' },
    { label: '从 Sub2API 服务器导入', value: 'sub2api' },
  ] as const

  function setImportMode(mode: AccountImportMode) {
    importMode.value = mode
  }

  async function openImportModal(mode: AccountImportMode = 'access_token') {
    showImportModal.value = true
    setImportMode(mode)
  }

  function closeImportModal() {
    if (importBusy.value) return
    stopCodexOAuthProgressPolling()
    showImportModal.value = false
  }

  async function promptRemoveImportedAbnormalAccounts(importedTokens: string[], errorCount: number) {
    if (errorCount <= 0 || options.bulkProgress.bulkStopRequested.value) return

    let preview: Awaited<ReturnType<typeof accountsApi.cleanupImportedAbnormalAccounts>>
    try {
      preview = await accountsApi.cleanupImportedAbnormalAccounts(importedTokens, false)
    } catch (error) {
      options.setError('检查本次异常账号失败，已先保留', error)
      return
    }

    if (!preview.abnormal) {
      toast.info('本次导入有刷新异常，但没有找到可清理的异常账号，可能未写入本地或状态已变化')
      return
    }

    const confirmed = await confirmDialog.ask({
      title: '移除本次异常账号？',
      message: `本次导入刷新返回 ${errorCount} 条异常。\n后端确认 ${preview.abnormal} 个本次导入账号当前状态为异常，是否直接删除？\n\n只会删除本次导入且状态为异常的账号，正常、限流和历史账号会保留。`,
      confirmText: `删除 ${preview.abnormal} 个`,
      cancelText: '先保留',
    })

    if (!confirmed) return

    try {
      const result = await accountsApi.cleanupImportedAbnormalAccounts(importedTokens, true)
      toast.success(`已移除 ${result.removed || 0} 个本次异常账号`)
    } catch (error) {
      options.setError('移除本次异常账号失败', error)
    } finally {
      await options.loadData({ silentErrorToast: true })
    }
  }

  async function importAccountPayloadBatch(accountPayloads: AccountImportPayload[], sourceType: string, title: string) {
    const payloadsByToken = new Map<string, AccountImportPayload>()
    for (const payload of accountPayloads) {
      const accessToken = accountPayloadToken(payload)
      if (accessToken) payloadsByToken.set(accessToken, payload)
    }
    const normalizedAccountPayloads = Array.from(payloadsByToken.values())
    const normalizedTokens = Array.from(payloadsByToken.keys())
    if (!normalizedAccountPayloads.length) {
      toast.warning('没有可导入的 access token')
      return
    }

    const confirmed = await confirmDialog.ask({
      title,
      message: `即将导入 ${normalizedTokens.length} 个账号，已存在账号会刷新远端信息。是否继续？`,
      confirmText: '确认导入',
      cancelText: '取消',
    })
    if (!confirmed) return

    importBusy.value = true
    options.bulkProgress.start(title, normalizedTokens.length, 'mutation')
    let addedCount = 0
    let skippedCount = 0
    let refreshedCount = 0
    let processed = 0
    const errors: string[] = []
    try {
      for (let index = 0; index < normalizedTokens.length; index += IMPORT_BATCH_SIZE) {
        if (options.bulkProgress.bulkStopRequested.value) break
        const batch = normalizedAccountPayloads.slice(index, index + IMPORT_BATCH_SIZE)
        try {
          const result = await accountsApi.importAccounts(
            batch,
            sourceType,
            { refresh: true, returnItems: false },
          )
          addedCount += Number(result.added || 0)
          skippedCount += Number(result.skipped || 0)
          refreshedCount += Number(result.refreshed || 0)
          errors.push(...(Array.isArray(result.errors) ? result.errors.filter(Boolean) : []))
        } catch (error) {
          errors.push(`${accountPayloadToken(batch[0])?.slice(0, 6) || '-'}... 等 ${batch.length} 个账号：${options.normalizeErrorMessage(error)}`)
        } finally {
          processed = Math.min(normalizedTokens.length, processed + batch.length)
          options.bulkProgress.update({
            total: normalizedTokens.length,
            processed,
            done: processed >= normalizedTokens.length,
            total_quota: 0,
          })
        }
      }

      await options.loadData({ silentErrorToast: true })
      const stopped = options.bulkProgress.bulkStopRequested.value && processed < normalizedTokens.length
      options.bulkProgress.finish({
        total: normalizedTokens.length,
        processed,
        total_quota: 0,
      })
      if (stopped) {
        toast.warning(`${title}已停止：已处理 ${processed}/${normalizedTokens.length} 个`)
      } else if (errors.length > 0) {
        toast.warning(`${title}完成：新增 ${addedCount}，跳过 ${skippedCount}，刷新 ${refreshedCount}，失败 ${errors.length}`)
      } else {
        toast.success(`${title}完成：新增 ${addedCount}，跳过 ${skippedCount}，刷新 ${refreshedCount}`)
      }
      if (addedCount + skippedCount + refreshedCount > 0) {
        manualTokenText.value = ''
        sessionJsonText.value = ''
      }
      if (!stopped && errors.length > 0) {
        await promptRemoveImportedAbnormalAccounts(normalizedTokens, errors.length)
      }
    } catch (error) {
      options.bulkProgress.finish({
        total: normalizedTokens.length,
        processed,
        error: options.normalizeErrorMessage(error),
        total_quota: 0,
      })
      options.setError(`${title}失败`, error)
    } finally {
      importBusy.value = false
      options.bulkProgress.end()
    }
  }

  async function importTokenBatch(tokens: string[], sourceType: string, title: string) {
    await importAccountPayloadBatch(
      uniqueTokens(tokens).map((accessToken) => ({
        access_token: accessToken,
        type: 'free',
        source_type: sourceType,
      })),
      sourceType,
      title,
    )
  }

  async function importManualTokenText() {
    await importTokenBatch(parseTokenLines(manualTokenText.value), 'manual', '导入 Access Token')
  }

  async function importTokenTextFile(file: File | null | undefined) {
    if (!file) return
    const text = await file.text()
    manualTokenText.value = text
    await importManualTokenText()
  }

  async function importSessionJson() {
    if (importBusy.value) return
    let started = false
    try {
      const session = parseSessionJsonAccount(sessionJsonText.value)
      const confirmed = await confirmDialog.ask({
        title: '导入 Session JSON',
        message: '将保存账号并校验账号信息和额度。同一 Token 已存在时补全缺失资料；校验失败仍保留账号。',
        confirmText: '确认导入',
        cancelText: '取消',
      })
      if (!confirmed) return
      importBusy.value = true
      options.bulkProgress.start('导入 Session JSON', 1, 'mutation')
      started = true
      const result = await accountsApi.importSession(session)
      await options.revealImportedAccount(result.account_ref)
      sessionJsonText.value = ''
      showImportModal.value = false
      options.bulkProgress.finish({ total: 1, processed: 1, total_quota: 0 })
      options.bulkProgress.close()
      const action = result.added ? '已新增账号' : '已更新已有账号'
      if (result.refresh_error) toast.warning(`${action}，账号已保留；信息校验未完成：${result.refresh_error}`)
      else toast.success(`${action}，账号信息和额度已校验`)
    } catch (error) {
      if (started) options.bulkProgress.fail(1, 0, options.normalizeErrorMessage(error))
      options.setError('导入 Session JSON 失败', error)
    } finally {
      if (importBusy.value) options.bulkProgress.end()
      importBusy.value = false
    }
  }

  async function startOAuthLogin() {
    importBusy.value = true
    try {
      const result = await accountImportsApi.startOAuthLogin(oauthEmailHint.value)
      oauthSessionId.value = String(result.session_id || '')
      oauthAuthorizeUrl.value = String(result.authorize_url || '')
      oauthRedirectUriPrefix.value = String(result.redirect_uri_prefix || '')
      oauthCallbackText.value = ''
      if (!oauthSessionId.value || !oauthAuthorizeUrl.value) {
        throw new Error('后端没有返回完整的 OAuth 授权会话')
      }
      window.open(oauthAuthorizeUrl.value, '_blank', 'noopener,noreferrer')
      toast.success('OAuth 授权链接已生成')
    } catch (error) {
      options.setError('生成 OAuth 授权链接失败', error)
    } finally {
      importBusy.value = false
    }
  }

  async function startCodexOAuthLogin() {
    if (!codexAccountRef.value.trim()) {
      toast.warning('请填写要绑定的 CFM 账号 ID、邮箱或账号引用')
      return
    }
    importBusy.value = true
    try {
      if (!codexCPAPools.value.length) {
        const response = await accountImportsApi.listCPAPools()
        codexCPAPools.value = response.pools || []
      }
      if (!codexCPAPoolID.value && codexCPAPools.value.length) codexCPAPoolID.value = codexCPAPools.value[0].id
      const result = await accountImportsApi.startCodexOAuthLogin({
        account_ref: codexAccountRef.value.trim(),
        email_hint: oauthEmailHint.value.trim() || undefined,
        cpa_pool_id: codexCPAPoolID.value || undefined,
      })
      oauthSessionId.value = String(result.session_id || '')
      oauthAuthorizeUrl.value = String(result.authorize_url || '')
      oauthRedirectUriPrefix.value = String(result.redirect_uri_prefix || '')
      oauthCallbackText.value = ''
      codexOAuthStage.value = String(result.stage || 'waiting_authorization')
      codexOAuthMessage.value = String(result.message || '')
      if (!oauthSessionId.value || !oauthAuthorizeUrl.value) throw new Error('后端没有返回完整的 Codex OAuth 授权会话')
      startCodexOAuthProgressPolling(oauthSessionId.value)
      window.open(oauthAuthorizeUrl.value, '_blank', 'noopener,noreferrer')
      toast.success('Codex 授权页面已打开；请在页面内完成手机验证码和需要时的 2FA')
    } catch (error) {
      options.setError('生成 Codex OAuth 授权链接失败', error)
    } finally {
      importBusy.value = false
    }
  }

  async function finishCodexOAuthLogin() {
    const sessionId = oauthSessionId.value.trim()
    const callback = oauthCallbackText.value.trim()
    if (!sessionId || !callback) {
      toast.warning('请先完成授权并粘贴 callback URL')
      return
    }
    importBusy.value = true
    try {
      stopCodexOAuthProgressPolling()
      codexOAuthStage.value = 'exchanging_token'
      const result = await accountImportsApi.finishCodexOAuthLogin({
        session_id: sessionId,
        callback,
        account_ref: codexAccountRef.value.trim(),
        cpa_pool_id: codexCPAPoolID.value || undefined,
      })
      await options.loadData({ silentErrorToast: true })
      if (result.ok === false) toast.warning(result.message || 'Codex 登录成功，但 CPA 同步失败，可稍后重试')
      else toast.success('Codex 登录并同步 CPA 完成')
      oauthSessionId.value = ''
      oauthAuthorizeUrl.value = ''
      oauthCallbackText.value = ''
      codexOAuthStage.value = result.ok === false ? 'cpa_sync_failed' : 'done'
      codexOAuthMessage.value = result.message || ''
    } catch (error) {
      options.setError('Codex OAuth 登录失败', error)
    } finally {
      importBusy.value = false
    }
  }

  async function syncCodexCredentialToCPA() {
    const accountRef = codexAccountRef.value.trim()
    if (!accountRef) {
      toast.warning('请填写要同步的 CFM 账号引用')
      return
    }
    importBusy.value = true
    try {
      const result = await accountImportsApi.syncCodexCPA(accountRef, codexCPAPoolID.value)
      await options.loadData({ silentErrorToast: true })
      if (result.ok === false) {
        codexOAuthStage.value = 'cpa_sync_failed'
        codexOAuthMessage.value = result.error || 'CPA 同步失败，可稍后重试'
        toast.warning(codexOAuthMessage.value)
      } else {
        codexOAuthStage.value = 'done'
        codexOAuthMessage.value = 'Codex 凭据已同步到 CPA'
        toast.success(codexOAuthMessage.value)
      }
    } catch (error) {
      options.setError('同步 Codex 凭据到 CPA 失败', error)
    } finally {
      importBusy.value = false
    }
  }

  function stopCodexOAuthProgressPolling() {
    if (codexProgressTimer !== null) {
      clearInterval(codexProgressTimer)
      codexProgressTimer = null
    }
  }

  function startCodexOAuthProgressPolling(sessionId: string) {
    stopCodexOAuthProgressPolling()
    const poll = async () => {
      try {
        const progress = await accountImportsApi.getCodexOAuthProgress(sessionId)
        codexOAuthStage.value = progress.stage || codexOAuthStage.value
        codexOAuthMessage.value = progress.error || progress.message || codexOAuthMessage.value
        if (progress.status === 'completed' || progress.status === 'failed') stopCodexOAuthProgressPolling()
      } catch {
        // The callback can still be submitted while a progress read is delayed.
      }
    }
    void poll()
    codexProgressTimer = setInterval(() => void poll(), 1000)
  }

  function openOAuthAuthorizeUrl() {
    if (!oauthAuthorizeUrl.value) {
      void startOAuthLogin()
      return
    }
    window.open(oauthAuthorizeUrl.value, '_blank', 'noopener,noreferrer')
  }

  async function copyOAuthAuthorizeUrl() {
    const value = oauthAuthorizeUrl.value.trim()
    if (!value) {
      toast.warning('请先生成 OAuth 授权链接')
      return
    }
    try {
      await navigator.clipboard.writeText(value)
      toast.success('授权链接已复制')
    } catch (error) {
      options.setError('复制 OAuth 授权链接失败', error)
    }
  }

  async function finishOAuthLogin() {
    const sessionId = oauthSessionId.value.trim()
    const callback = oauthCallbackText.value.trim()
    if (!sessionId) {
      toast.warning('请先生成 OAuth 授权链接')
      return
    }
    if (!callback) {
      toast.warning('请先粘贴 callback URL 或 code')
      return
    }

    importBusy.value = true
    try {
      const result = await accountImportsApi.finishOAuthLogin(sessionId, callback)
      await options.loadData({ silentErrorToast: true })
      const added = Number(result.added || 0)
      const skipped = Number(result.skipped || 0)
      const refreshed = Number(result.refreshed || 0)
      const errors = Array.isArray(result.errors) ? result.errors.length : 0
      if (errors > 0) {
        toast.warning(`OAuth 登录导入完成：新增 ${added}，跳过 ${skipped}，刷新 ${refreshed}，异常 ${errors}`)
      } else {
        toast.success(`OAuth 登录导入完成：新增 ${added}，跳过 ${skipped}，刷新 ${refreshed}`)
      }
      oauthEmailHint.value = ''
      oauthCallbackText.value = ''
      oauthSessionId.value = ''
      oauthAuthorizeUrl.value = ''
      oauthRedirectUriPrefix.value = ''
    } catch (error) {
      options.setError('OAuth 登录导入失败', error)
    } finally {
      importBusy.value = false
    }
  }

  async function importLocalCPAFiles(files: FileList | File[] | null | undefined) {
    const fileList = Array.from(files || [])
    if (!fileList.length) return
    importBusy.value = true
    try {
      const tokens: string[] = []
      for (const file of fileList) {
        const text = await file.text()
        tokens.push(...parseCPAJsonTokens(text, file.name))
      }
      importBusy.value = false
      await importTokenBatch(tokens, 'cpa_json', '导入 CPA JSON 文件')
    } catch (error) {
      options.setError('导入 CPA JSON 文件失败', error)
    } finally {
      importBusy.value = false
    }
  }

  return {
    importBusy,
    showImportModal,
    importMode,
    importModeOptions,
    oauthEmailHint,
    oauthCallbackText,
    oauthSessionId,
    oauthAuthorizeUrl,
    oauthRedirectUriPrefix,
    codexAccountRef,
    codexCPAPoolID,
    codexCPAPools,
    codexOAuthStage,
    codexOAuthMessage,
    manualTokenText,
    sessionJsonText,
    setImportMode,
    openImportModal,
    closeImportModal,
    importManualTokenText,
    importTokenTextFile,
    importSessionJson,
    startOAuthLogin,
    startCodexOAuthLogin,
    openOAuthAuthorizeUrl,
    copyOAuthAuthorizeUrl,
    finishOAuthLogin,
    finishCodexOAuthLogin,
    syncCodexCredentialToCPA,
    importLocalCPAFiles,
  }
}
