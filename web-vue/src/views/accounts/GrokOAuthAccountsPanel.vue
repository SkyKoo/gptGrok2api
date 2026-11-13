<template>
  <ModalShell :open="props.open" :inline="props.embedded" max-width="76rem" :z-index="150" @close="emit('close')">
    <ModalHeader title="xAI OAuth" subtitle="授权、CPA 交付和后续操作在同一账号行完成。" compact :show-close="!props.embedded" @close="emit('close')" />
    <ModalBody density="compact" class="min-w-0 space-y-5">
      <div class="flex flex-wrap items-end justify-between gap-3">
        <div class="min-w-0 flex-1 basis-64 space-y-2 md:max-w-sm">
          <label class="ui-field-label">授权后自动同步到</label>
          <GroupedSelectMenu v-model="cpaPoolId" :options="cpaPoolOptions" placeholder="选择 CPA 连接" aria-label="CPA 同步目标" block />
          <p v-if="cpaLoadError" class="text-xs text-rose-600">{{ cpaLoadError }}</p>
          <p v-else-if="!cpaPools.length && !loading" class="text-xs text-muted-foreground">请先在账号导入中配置 CPA 连接。</p>
        </div>
        <div class="flex items-center gap-2">
          <Button size="sm" variant="outline" :disabled="loading" @click="loadAccounts()">{{ loading ? '刷新中…' : '刷新列表' }}</Button>
          <Button size="sm" variant="outline" :disabled="!cpaPoolId || modalBusy" @click="openImport">导入凭据</Button>
        </div>
      </div>
      <SurfaceBox tone="muted" density="compact" wrap>
        开始授权后自动同步到所选 CPA，由 CPA 负责后续续期。同步失败可在原账号行单独处理；Grok 图片和视频仍使用 SSO 登录态。
      </SurfaceBox>
      <div v-if="rowScopeIds.length" class="flex flex-wrap items-center justify-between gap-2 text-sm text-muted-foreground">
        <span>已定位 {{ rowScopeIds.length }} 个所选账号；请选择 CPA 目标后，在各账号行开始授权。</span>
        <Button size="xs" variant="outline" @click="rowScopeIds = []">查看全部账号</Button>
      </div>
      <GrokOAuthCandidates :items="visibleRows" :loading="loading" :busy-rows="busyRows" :target-selected="Boolean(cpaPoolId)" :error="candidateError" @authorize="startProtocolAuthorization" @cpa="handleCPA" @device="startDeviceAuthorization" @details="openDetails" @toggle="toggleAccount" @remove="removeAccount" />
    </ModalBody>
    <ModalFooter v-if="!props.embedded" density="compact"><Button size="sm" variant="outline" @click="emit('close')">完成</Button></ModalFooter>
  </ModalShell>

  <ModalShell :open="dialog === 'device'" max-width="40rem" :z-index="160" @close="closeDialog">
    <ModalHeader title="手动设备码授权" :subtitle="deviceTarget?.email" compact @close="closeDialog" />
    <ModalBody density="compact" class="space-y-4">
      <p class="text-sm text-muted-foreground">请在 xAI 页面使用此账号完成授权，再检查结果。成功后自动同步到 {{ dialogPoolName }}。</p>
      <template v-if="device">
        <div class="rounded-lg border border-border p-4 text-center font-mono text-xl tracking-widest">{{ device.user_code }}</div>
        <div class="flex flex-wrap justify-end gap-2"><Button size="sm" variant="outline" @click="openDeviceAuthorization">打开授权页</Button><Button size="sm" variant="primary" :disabled="modalBusy" @click="pollDeviceAuthorization">{{ modalBusy ? '检查中…' : '检查授权' }}</Button></div>
      </template>
      <p v-if="dialogError" role="alert" class="break-words text-sm text-rose-600">{{ dialogError }}</p>
    </ModalBody>
  </ModalShell>

  <ModalShell :open="dialog === 'import'" max-width="42rem" :z-index="160" @close="closeDialog">
    <ModalHeader title="导入 xAI OAuth" :subtitle="`导入后自动同步到 ${dialogPoolName}`" compact @close="closeDialog" />
    <ModalBody density="compact" class="space-y-4">
      <label class="block space-y-2"><span class="ui-field-label">xAI OAuth JSON</span><textarea v-model.trim="credentialText" rows="8" class="ui-textarea-sm font-mono" placeholder="粘贴包含账号身份与 Token 的 xAI OAuth JSON"></textarea></label>
      <p v-if="dialogError" role="alert" class="break-words text-sm text-rose-600">{{ dialogError }}</p>
    </ModalBody>
    <ModalFooter density="compact"><Button size="sm" variant="outline" :disabled="modalBusy" @click="closeDialog">取消</Button><Button size="sm" variant="primary" :disabled="modalBusy || !credentialText" @click="importCredential">{{ modalBusy ? '导入中…' : '导入并同步' }}</Button></ModalFooter>
  </ModalShell>

  <ModalShell :open="dialog === 'details'" max-width="44rem" :z-index="160" @close="closeDialog">
    <ModalHeader title="OAuth 额度与测试" :subtitle="detailAccount?.email" compact @close="closeDialog" />
    <ModalBody v-if="detailAccount" density="compact" class="space-y-4">
      <p class="text-sm text-muted-foreground">月度上限为 0 不代表无法调用；SSO 图片和视频额度仍在账号管理中查看。</p>
      <dl class="grid grid-cols-2 gap-3 text-sm"><div><dt class="text-muted-foreground">月度额度</dt><dd class="mt-1">{{ grokOAuthRemainingQuotaText({ oauth: detailAccount } as GrokAccount) }}</dd></div><div><dt class="text-muted-foreground">Access Token 到期</dt><dd class="mt-1">{{ formatGrokAccountDate(detailAccount.expires_at) }}</dd></div></dl>
      <p v-if="detailAccount.quota?.error" class="text-sm text-rose-600">{{ detailAccount.quota.error }}</p>
      <Button size="sm" variant="outline" :disabled="modalBusy" @click="refreshAccount(detailAccount.id)">刷新额度</Button>
      <Button size="sm" variant="outline" :disabled="modalBusy" @click="syncModels(detailAccount.id)">探测模型</Button>
      <div class="space-y-3 border-t border-border pt-4">
        <label class="block space-y-2"><span class="ui-field-label">测试模型</span><input v-model.trim="testModel" class="ui-input-sm w-full" placeholder="输入模型 ID" /></label>
        <label class="block space-y-2"><span class="ui-field-label">测试提示词</span><textarea v-model.trim="testPrompt" rows="2" class="ui-textarea-sm" /></label>
        <p class="text-xs text-muted-foreground">模型测试会向上游发送一次请求并使用账号额度。</p>
        <Button size="sm" variant="outline" :disabled="modalBusy || !testModel || !testPrompt" @click="runModelTest">测试此账号</Button>
        <pre v-if="testResult" class="whitespace-pre-wrap break-words text-sm">{{ testResult }}</pre>
      </div>
      <p v-if="dialogError" role="alert" class="break-words text-sm text-rose-600">{{ dialogError }}</p>
    </ModalBody>
  </ModalShell>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Button } from 'nanocat-ui'
import { grokOAuthAccountsApi, type GrokOAuthAccount, type GrokOAuthCandidate, type GrokOAuthDeviceSession, type GrokOAuthProtocolJob } from '@/api/grokOAuthAccounts'
import { accountImportsApi, type CPAPool } from '@/api/accountImports'
import type { GrokAccount } from '@/api/grokAccounts'
import GrokOAuthCandidates from './GrokOAuthCandidates.vue'
import { cpaAction, grokOAuthImportPayload, isCPAManaged, mergeGrokOAuthRows, type GrokOAuthRow } from './grokOAuthCandidates'
import { formatGrokAccountDate, grokOAuthRemainingQuotaText } from './grokAccountView'
import SurfaceBox from '@/components/ai/SurfaceBox.vue'
import ModalBody from '@/components/ai/ModalBody.vue'
import ModalFooter from '@/components/ai/ModalFooter.vue'
import ModalHeader from '@/components/ai/ModalHeader.vue'
import ModalShell from '@/components/ai/ModalShell.vue'
import GroupedSelectMenu from '@/components/ui/GroupedSelectMenu.vue'
import { useToast } from '@/composables/useToast'
import { useConfirmDialog } from '@/composables/useConfirmDialog'
import { errorMessage } from '@/lib/errorMessage'

const toast = useToast()
const confirmDialog = useConfirmDialog()
const props = withDefaults(defineProps<{ open?: boolean; embedded?: boolean; focusAccountIds?: string[] }>(), { open: false, embedded: false, focusAccountIds: () => [] })
const emit = defineEmits<{ close: []; changed: [] }>()
const accounts = ref<GrokOAuthAccount[]>([]), candidates = ref<GrokOAuthCandidate[]>([])
const jobs = ref<Record<string, GrokOAuthProtocolJob>>({}), pollErrors = ref<Record<string, string>>({})
const busyRows = ref<Record<string, string>>({}), rowErrors = ref<Record<string, string>>({})
const loading = ref(false), candidateError = ref(''), cpaLoadError = ref('')
const cpaPools = ref<CPAPool[]>([]), cpaPoolId = ref('')
const cpaPoolOptions = computed(() => cpaPools.value.map(pool => ({ label: pool.name || pool.base_url, value: pool.id })))
const rows = computed(() => mergeGrokOAuthRows(candidates.value, accounts.value, Object.values(jobs.value), pollErrors.value).map(row => ({ ...row, action_error: rowErrors.value[row.id] })))
const rowScopeIds = ref<string[]>([])
const visibleRows = computed(() => rowScopeIds.value.length ? rows.value.filter(row => rowScopeIds.value.includes(row.source_account_id || row.id)) : rows.value)
watch(() => [props.open, props.focusAccountIds] as const, () => { if (props.open) rowScopeIds.value = [...props.focusAccountIds] }, { immediate: true })
const dialog = ref<'device' | 'import' | 'details' | null>(null), dialogError = ref(''), modalBusy = ref(false)
const dialogPoolId = ref(''), dialogPoolName = ref(''), credentialText = ref('')
const device = ref<GrokOAuthDeviceSession | null>(null), deviceTarget = ref<GrokOAuthRow | null>(null)
const devicePoolId = ref(''), devicePoolName = ref('')
const detailAccountId = ref(''), testModel = ref(''), testPrompt = ref('你好，请只回复 OK。'), testResult = ref('')
const detailAccount = computed(() => accounts.value.find(item => item.id === detailAccountId.value))
const pollFailures: Record<string, number> = {}
const jobReadVersions: Record<string, number> = {}
let pollTimer: number | null = null, disposed = false, loadVersion = 0

function activeJob(job: GrokOAuthProtocolJob) { return ['pending', 'running'].includes(job.status) }
function poolName(id: string) { return cpaPools.value.find(pool => pool.id === id)?.name || '所选 CPA' }
function setRowBusy(id: string, action: string) { busyRows.value = { ...busyRows.value, [id]: action } }
function clearRowBusy(id: string) { const next = { ...busyRows.value }; delete next[id]; busyRows.value = next }

async function loadAccounts(readJobs = true) {
  const version = ++loadVersion
  loading.value = true
  const results = await Promise.allSettled([grokOAuthAccountsApi.list(), grokOAuthAccountsApi.candidates(), accountImportsApi.listCPAPools()])
  if (disposed || version !== loadVersion) return
  const [accountResult, candidateResult, poolResult] = results
  if (accountResult.status === 'fulfilled') accounts.value = accountResult.value.items || []
  if (candidateResult.status === 'fulfilled') candidates.value = candidateResult.value.items || []
  candidateError.value = accountResult.status === 'rejected' || candidateResult.status === 'rejected' ? '账号读取不完整，请刷新列表重试。' : ''
  if (poolResult.status === 'fulfilled') {
    cpaPools.value = poolResult.value.pools || []; cpaLoadError.value = ''
    if (!cpaPools.value.some(pool => pool.id === cpaPoolId.value)) cpaPoolId.value = cpaPools.value.length === 1 ? cpaPools.value[0].id : ''
  } else { cpaLoadError.value = 'CPA 连接读取失败，请刷新列表'; cpaPoolId.value = '' }
  if (readJobs) {
    const ids = new Set([...Object.keys(jobs.value), ...candidates.value.map(item => item.oauth_job_id).filter((id): id is string => Boolean(id))])
    await Promise.allSettled([...ids].map(id => readJob(id)))
  }
  if (disposed || version !== loadVersion) return
  loading.value = false
  schedulePoll()
}

async function readJob(id: string) {
  const version = jobReadVersions[id] = (jobReadVersions[id] || 0) + 1
  try {
    const result = await grokOAuthAccountsApi.getProtocolJob(id)
    if (disposed || jobReadVersions[id] !== version) return false
    const previous = jobs.value[id]
    jobs.value = { ...jobs.value, [id]: result.job }
    pollFailures[id] = 0
    pollErrors.value = { ...pollErrors.value, [id]: '' }
    return Boolean(previous && activeJob(previous) && !activeJob(result.job))
  } catch {
    if (disposed || jobReadVersions[id] !== version) return false
    pollFailures[id] = (pollFailures[id] || 0) + 1
    pollErrors.value = { ...pollErrors.value, [id]: pollFailures[id] < 3 ? '任务状态读取失败，正在重新查询。' : '任务状态待确认，请刷新列表核对；页面状态不代表后台仍在运行。' }
    return false
  }
}

function schedulePoll() {
  if (pollTimer !== null) window.clearTimeout(pollTimer)
  pollTimer = null
  if (!disposed && Object.values(jobs.value).some(job => activeJob(job) && (pollFailures[job.id] || 0) < 3)) pollTimer = window.setTimeout(pollJobs, 1500)
}

async function pollJobs() {
  pollTimer = null
  const active = Object.values(jobs.value).filter(job => activeJob(job) && (pollFailures[job.id] || 0) < 3)
  const changed = await Promise.all(active.map(job => readJob(job.id)))
  if (disposed) return
  if (changed.some(Boolean)) {
    for (let i = 0; i < active.length; i++) {
      if (!changed[i]) continue
      const job = jobs.value[active[i].id]
      if (job.status === 'authorized') notifyDelivery(job.account, job.stage)
      else toast.warning(job.error || job.message || '后台授权需要处理')
    }
    await loadAccounts(false)
    emit('changed')
  } else schedulePoll()
}

function notifyDelivery(account?: GrokOAuthAccount, stage = '') {
  if (stage === 'cpa_sync_failed' || (account?.cpa_sync && account.cpa_sync.status !== 'synced')) toast.warning('授权已保存，CPA 交付待处理。请在账号行核对或重试同步。')
  else if (account?.cpa_sync?.status === 'synced') toast.success('授权成功，已同步到 CPA')
  else toast.info('授权已保存，请在账号行确认 CPA 交付状态')
}

async function startProtocolAuthorization(row: GrokOAuthRow) {
  const target = cpaPoolId.value
  if (!row.source_account_id || !target) return
  setRowBusy(row.id, 'authorize'); rowErrors.value = { ...rowErrors.value, [row.id]: '' }
  try {
    const result = await grokOAuthAccountsApi.startProtocol(row.source_account_id, target)
    if (disposed) return
    jobs.value = { ...jobs.value, [result.job.id]: result.job }
    pollFailures[result.job.id] = 0
    await loadAccounts(false)
  } catch (error) { rowErrors.value = { ...rowErrors.value, [row.id]: errorMessage(error, '无法启动后台授权') } }
  finally { clearRowBusy(row.id) }
}

async function handleCPA(row: GrokOAuthRow) {
  if (!row.oauth_account_id) return
  const action = cpaAction(row), target = row.cpa_sync?.pool_id || cpaPoolId.value
  if (!action || !target) return
  setRowBusy(row.id, 'cpa'); rowErrors.value = { ...rowErrors.value, [row.id]: '' }
  try {
    const result = action === 'check'
      ? await grokOAuthAccountsApi.checkCPA(row.oauth_account_id, target)
      : await grokOAuthAccountsApi.syncCPA(row.oauth_account_id, target)
    if (result.ok) toast.success(action === 'check' ? 'CPA 交付记录已确认' : '已同步到 CPA')
    else rowErrors.value = { ...rowErrors.value, [row.id]: result.account?.cpa_sync?.error || 'CPA 交付尚未确认，请核对后处理。' }
    await loadAccounts(false); emit('changed')
  } catch (error) { rowErrors.value = { ...rowErrors.value, [row.id]: errorMessage(error, 'CPA 操作失败') }; await loadAccounts(false) }
  finally { clearRowBusy(row.id) }
}

function pinDialogTarget() { dialogPoolId.value = cpaPoolId.value; dialogPoolName.value = poolName(cpaPoolId.value); dialogError.value = '' }
function openImport() { if (!cpaPoolId.value) return; pinDialogTarget(); dialog.value = 'import' }
function closeDialog() { if (!modalBusy.value) { dialog.value = null; dialogError.value = ''; credentialText.value = '' } }

async function startDeviceAuthorization(row: GrokOAuthRow) {
  if (!row.source_account_id || !cpaPoolId.value) return
  if (device.value && device.value.expires_at <= Date.now() / 1000) { device.value = null; deviceTarget.value = null }
  if (device.value) {
    if (deviceTarget.value?.id === row.id) {
      dialogPoolId.value = devicePoolId.value; dialogPoolName.value = devicePoolName.value
      dialogError.value = ''; dialog.value = 'device'
    }
    else toast.info('请先完成已创建的设备码授权。')
    return
  }
  pinDialogTarget(); deviceTarget.value = row; dialog.value = 'device'; modalBusy.value = true
  devicePoolId.value = dialogPoolId.value; devicePoolName.value = dialogPoolName.value
  try { device.value = await grokOAuthAccountsApi.startDevice(row.source_account_id, dialogPoolId.value) }
  catch (error) { dialogError.value = errorMessage(error, '无法创建设备授权') }
  finally { modalBusy.value = false }
}
function openDeviceAuthorization() { if (device.value?.verification_uri_complete) window.open(device.value.verification_uri_complete, '_blank', 'noopener,noreferrer') }
async function pollDeviceAuthorization() {
  if (!device.value) return
  if (device.value.expires_at <= Date.now() / 1000) { device.value = null; dialogError.value = '设备码已过期，请关闭后从账号行重新发起。'; return }
  modalBusy.value = true; dialogError.value = ''
  try {
    const result = await grokOAuthAccountsApi.pollDevice(device.value.id)
    if (result.status === 'authorized') {
      notifyDelivery(result.account)
      device.value = null; deviceTarget.value = null; dialog.value = null
      await loadAccounts(false); emit('changed')
    } else toast.info('授权仍在等待中')
  } catch (error) {
    const message = errorMessage(error, '授权状态检查失败；请核对当前授权结果。')
    if (/device authorization (?:has expired|failed:)|oauth_identity_(?:missing|mismatch)|oauth_account_changed/i.test(message)) device.value = null
    dialogError.value = message
  }
  finally { modalBusy.value = false }
}

async function importCredential() {
  modalBusy.value = true; dialogError.value = ''
  try {
    const result = await grokOAuthAccountsApi.importCredential(grokOAuthImportPayload(JSON.parse(credentialText.value), dialogPoolId.value))
    notifyDelivery(result.account)
    credentialText.value = ''; dialog.value = null
    await loadAccounts(false); emit('changed')
  } catch (error) { dialogError.value = errorMessage(error, '凭据导入失败') }
  finally { modalBusy.value = false }
}

function openDetails(row: GrokOAuthRow) {
  if (!row.account || isCPAManaged(row)) return
  detailAccountId.value = row.account.id; testModel.value = row.account.models?.[0] || ''; testResult.value = ''; dialogError.value = ''; dialog.value = 'details'
}
async function refreshAccount(id: string) {
  const account = accounts.value.find(item => item.id === id)
  if (!account || isCPAManaged(account)) return
  modalBusy.value = true; dialogError.value = ''
  try {
    const result = await grokOAuthAccountsApi.queryQuota(id)
    if (!result.ok) dialogError.value = result.account.quota?.error || '额度查询失败'
    await loadAccounts(false); emit('changed')
  } catch (error) { dialogError.value = errorMessage(error, '额度查询失败') }
  finally { modalBusy.value = false }
}
async function syncModels(id: string) {
  const account = accounts.value.find(item => item.id === id)
  if (!account || isCPAManaged(account)) return
  modalBusy.value = true; dialogError.value = ''
  try { await grokOAuthAccountsApi.syncModels(id); await loadAccounts(false); emit('changed') }
  catch (error) { dialogError.value = errorMessage(error, '模型探测失败') }
  finally { modalBusy.value = false }
}
async function toggleAccount(row: GrokOAuthRow) {
  if (!row.account || busyRows.value[row.id]) return
  const disabled = row.account.status !== 'disabled'
  const confirmed = await confirmDialog.ask({
    title: disabled ? '禁用 OAuth' : '启用 OAuth',
    message: `将${disabled ? '禁用' : '启用'} ${row.email || row.id} 在 CFM 中的 OAuth。CPA 中已交付的账号由 CPA 独立管理。是否继续？`,
    confirmText: disabled ? '禁用 OAuth' : '启用 OAuth', cancelText: '取消',
  })
  if (!confirmed) return
  setRowBusy(row.id, 'toggle')
  try { await grokOAuthAccountsApi.setDisabled([row.account.id], disabled); await loadAccounts(false); emit('changed') }
  catch (error) { rowErrors.value = { ...rowErrors.value, [row.id]: errorMessage(error, 'OAuth 状态更新失败') } }
  finally { clearRowBusy(row.id) }
}
async function removeAccount(row: GrokOAuthRow) {
  if (!row.account || busyRows.value[row.id]) return
  const confirmed = await confirmDialog.ask({
    title: '移除 OAuth',
    message: `将移除 ${row.email || row.id} 在 CFM 中保存的 OAuth 凭据。注册账号、SSO 登录态和 CPA 中的凭据保留。是否继续？`,
    confirmText: '移除 OAuth', cancelText: '取消',
  })
  if (!confirmed) return
  setRowBusy(row.id, 'remove')
  try {
    await grokOAuthAccountsApi.remove([row.account.id])
    jobs.value = Object.fromEntries(Object.entries(jobs.value).filter(([, job]) => job.account?.id !== row.account?.id))
    await loadAccounts(false); emit('changed')
  } catch (error) { rowErrors.value = { ...rowErrors.value, [row.id]: errorMessage(error, 'OAuth 移除失败') } }
  finally { clearRowBusy(row.id) }
}
async function runModelTest() {
  const account = detailAccount.value
  if (!account || isCPAManaged(account) || !testModel.value || !testPrompt.value) return
  modalBusy.value = true; dialogError.value = ''; testResult.value = ''
  try { const result = await grokOAuthAccountsApi.testAccount(account.id, { model: testModel.value, prompt: testPrompt.value }); testResult.value = result.content; await loadAccounts(false) }
  catch (error) { dialogError.value = errorMessage(error, '模型测试失败') }
  finally { modalBusy.value = false }
}

watch(() => props.open, open => { if (open) void loadAccounts() })
onMounted(() => { if (props.open) void loadAccounts() })
onBeforeUnmount(() => { disposed = true; loadVersion++; if (pollTimer !== null) window.clearTimeout(pollTimer) })
defineExpose({ loadAccounts, refreshAccount })
</script>
