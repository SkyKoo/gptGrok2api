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
          <Button size="sm" variant="outline" :disabled="loading || !qualityEligible.length" @click="checkVisibleQuality">检测已有账号</Button>
          <Button size="sm" variant="outline" :disabled="loading" @click="loadAccounts()">{{ loading ? '刷新中…' : '刷新列表' }}</Button>
          <Button size="sm" variant="outline" :disabled="!cpaPoolId || modalBusy" @click="openImport">导入凭据</Button>
        </div>
      </div>
      <SurfaceBox tone="muted" density="compact" wrap>
        授权后先检测 grok-4.7，通过后同步到所选 CPA，由 CPA 负责续期。降智风险账号会拦截同步；已交付的风险凭据会自动尝试移除。检测使用账号额度，每轮 3 题，出现异常或复测风险账号时追加 3 题。
      </SurfaceBox>
      <div class="space-y-2 rounded-lg border border-border p-3 text-sm" aria-label="定时降智检测">
        <div class="flex flex-wrap items-center gap-3">
          <label class="flex items-center gap-2"><input v-model="qualityScheduleEnabled" type="checkbox" :disabled="scheduleSaving || !qualitySchedule" />定时降智检测</label>
          <label class="flex items-center gap-2">每<input v-model.number="qualityIntervalHours" type="number" min="1" max="168" step="1" class="ui-input-sm w-20" aria-label="降智检测间隔小时" />小时</label>
          <Button size="xs" variant="outline" :disabled="scheduleSaving || !qualitySchedule" @click="saveQualitySchedule">{{ scheduleSaving ? '保存中' : '保存检测设置' }}</Button>
          <span v-if="qualitySchedule" class="text-xs text-muted-foreground">{{ qualitySchedule.enabled ? '已开启' : '已关闭' }} · 排队 {{ qualitySchedule.pending }} · 检测中 {{ qualitySchedule.running }}</span>
        </div>
        <p v-if="qualitySchedule" class="text-xs text-muted-foreground">最近检测：{{ qualitySchedule.last_checked_at ? formatGrokAccountDate(qualitySchedule.last_checked_at) : '暂无' }} · 下次到期：{{ qualitySchedule.enabled && qualitySchedule.next_due_at ? formatGrokAccountDate(qualitySchedule.next_due_at) : '暂无' }}</p>
        <p class="text-xs text-muted-foreground">按账号上次检测时间计算周期，默认 24 小时。定时检测使用 Build 额度；关闭定时检测后，新授权的交付前检测仍会执行。</p>
        <p v-if="scheduleError" role="alert" class="text-xs text-rose-600">{{ scheduleError }}</p>
      </div>
      <div v-if="rowScopeIds.length" class="flex flex-wrap items-center justify-between gap-2 text-sm text-muted-foreground">
        <span>已定位 {{ rowScopeIds.length }} 个所选账号；请选择 CPA 目标后，在各账号行开始授权。</span>
        <Button size="xs" variant="outline" @click="rowScopeIds = []">查看全部账号</Button>
      </div>
      <GrokOAuthCandidates :items="visibleRows" :loading="loading" :busy-rows="displayBusyRows" :target-selected="Boolean(cpaPoolId)" :error="candidateError" @authorize="startProtocolAuthorization" @cpa="handleCPA" @device="startDeviceAuthorization" @details="openDetails" @toggle="toggleAccount" @remove="removeAccount" @quality="checkQuality" @quality-details="openQualityDetails" @cleanup="row => checkQuality(row, true)" />
    </ModalBody>
    <ModalFooter v-if="!props.embedded" density="compact"><Button size="sm" variant="outline" @click="emit('close')">完成</Button></ModalFooter>
  </ModalShell>

  <ModalShell :open="dialog === 'device'" max-width="40rem" :z-index="160" @close="closeDialog">
    <ModalHeader title="手动设备码授权" :subtitle="deviceTarget?.email" compact @close="closeDialog" />
    <ModalBody density="compact" class="space-y-4">
      <p class="text-sm text-muted-foreground">请在 xAI 页面使用此账号完成授权，再检查结果。grok-4.7 检测通过后自动同步到 {{ dialogPoolName }}。</p>
      <template v-if="device">
        <div class="rounded-lg border border-border p-4 text-center font-mono text-xl tracking-widest">{{ device.user_code }}</div>
        <div class="flex flex-wrap justify-end gap-2"><Button size="sm" variant="outline" @click="openDeviceAuthorization">打开授权页</Button><Button size="sm" variant="primary" :disabled="modalBusy" @click="pollDeviceAuthorization">{{ modalBusy ? '检查中…' : '检查授权' }}</Button></div>
      </template>
      <p v-if="dialogError" role="alert" class="break-words text-sm text-rose-600">{{ dialogError }}</p>
    </ModalBody>
  </ModalShell>

  <ModalShell :open="dialog === 'import'" max-width="42rem" :z-index="160" @close="closeDialog">
    <ModalHeader title="导入 xAI OAuth" :subtitle="`检测通过后同步到 ${dialogPoolName}`" compact @close="closeDialog" />
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

  <ModalShell :open="dialog === 'quality'" max-width="52rem" :z-index="160" @close="closeDialog">
    <ModalHeader title="grok-4.7 降智检测依据" :subtitle="detailAccount?.email" compact @close="closeDialog" />
    <ModalBody v-if="detailAccount?.quality" density="compact" class="space-y-4">
      <p class="text-sm font-medium">{{ detailAccount.quality.summary }}</p>
      <p class="text-xs text-muted-foreground">{{ formatGrokAccountDate(detailAccount.quality.at) }} · {{ detailAccount.quality.source === 'cpa' ? '使用 CPA 当前凭据检测' : '交付前检测' }} · {{ detailAccount.quality.version }}</p>
      <p v-if="detailAccount.quality.blocked" class="text-sm text-rose-600">该账号已阻止同步。重新授权不会清除风险，复测全部通过后才能解除。</p>
      <div v-if="detailAccount.quality.removal" class="space-y-2 rounded-lg border border-border p-3 text-sm">
        <p>{{ detailAccount.quality.removal.status === 'removed' ? 'CPA 对应凭据已确认移除' : 'CPA 凭据需要处理' }}</p>
        <p class="break-all">{{ detailAccount.quality.removal.pool_name }} · {{ detailAccount.quality.removal.file_name }}</p>
        <p v-if="detailAccount.quality.removal.error" class="text-rose-600">{{ detailAccount.quality.removal.error }}</p>
      </div>
      <div v-for="(sample, index) in detailAccount.quality.samples || []" :key="index" class="space-y-2 rounded-lg border border-border p-3 text-sm">
        <p class="font-medium">第 {{ index + 1 }} 题 · {{ sample.status === 'pass' ? '通过' : sample.status === 'fail' ? '失败' : '无法判定' }}</p>
        <p class="whitespace-pre-wrap break-words text-muted-foreground">{{ sample.prompt }}</p>
        <p>预期答案：{{ sample.expected }} · 耗时 {{ (sample.duration_ms / 1000).toFixed(1) }} 秒 · 思考 Token：{{ sample.reasoning_tokens ?? '未提供' }}</p>
        <pre v-if="sample.answer" class="whitespace-pre-wrap break-all rounded bg-muted p-2">{{ sample.answer }}</pre>
        <p v-if="sample.reason" class="text-amber-600">{{ sample.reason }}</p>
      </div>
      <details v-if="detailAccount.quality.confirmed_at && detailAccount.quality.status !== 'high_risk' && detailAccount.quality.risk_samples?.length" class="text-sm">
        <summary class="cursor-pointer">上次确认风险的依据（{{ formatGrokAccountDate(detailAccount.quality.confirmed_at) }}）</summary>
        <div v-for="(sample, index) in detailAccount.quality.risk_samples" :key="index" class="mt-2 space-y-1 border-t border-border pt-2">
          <p class="break-words">{{ sample.prompt }}</p><p>预期 {{ sample.expected }} · {{ sample.reason || '通过' }}</p><pre class="whitespace-pre-wrap break-all">{{ sample.answer }}</pre>
        </div>
      </details>
      <p class="text-xs text-muted-foreground">这是答案质量与指令遵循检测，不能证明上游替换了模型。网络、额度、截断响应属于检测失败；速度和思考 Token 数量不单独作为降智依据。</p>
    </ModalBody>
  </ModalShell>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Button } from 'nanocat-ui'
import { grokOAuthAccountsApi, type GrokOAuthAccount, type GrokOAuthCandidate, type GrokOAuthDeviceSession, type GrokOAuthProtocolJob, type GrokQualityJob, type GrokQualitySchedule } from '@/api/grokOAuthAccounts'
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
const qualityJobs = ref<GrokQualityJob[]>([])
const qualitySchedule = ref<GrokQualitySchedule | null>(null), qualityScheduleEnabled = ref(true), qualityIntervalHours = ref(24), scheduleSaving = ref(false), scheduleError = ref('')
const qualityActiveIds = computed(() => new Set(qualityJobs.value.filter(job => ['queued', 'running'].includes(job.status)).map(job => job.account_id)))
const loading = ref(false), candidateError = ref(''), cpaLoadError = ref('')
const cpaPools = ref<CPAPool[]>([]), cpaPoolId = ref('')
const cpaPoolOptions = computed(() => cpaPools.value.map(pool => ({ label: pool.name || pool.base_url, value: pool.id })))
const rows = computed(() => mergeGrokOAuthRows(candidates.value, accounts.value, Object.values(jobs.value), pollErrors.value).map(row => ({ ...row, quality_running: qualityActiveIds.value.has(row.oauth_account_id), action_error: rowErrors.value[row.id] })))
const displayBusyRows = computed(() => ({ ...Object.fromEntries(rows.value.filter(row => row.quality_running).map(row => [row.id, 'quality'])), ...busyRows.value }))
const rowScopeIds = ref<string[]>([])
const visibleRows = computed(() => rowScopeIds.value.length ? rows.value.filter(row => rowScopeIds.value.includes(row.source_account_id || row.id)) : rows.value)
const qualityEligible = computed(() => visibleRows.value.filter(row => row.account && row.account.status !== 'disabled' && !row.quality_running && !['queued', 'running'].includes(row.oauth_status)))
watch(() => [props.open, props.focusAccountIds] as const, () => { if (props.open) rowScopeIds.value = [...props.focusAccountIds] }, { immediate: true })
const dialog = ref<'device' | 'import' | 'details' | 'quality' | null>(null), dialogError = ref(''), modalBusy = ref(false)
const dialogPoolId = ref(''), dialogPoolName = ref(''), credentialText = ref('')
const device = ref<GrokOAuthDeviceSession | null>(null), deviceTarget = ref<GrokOAuthRow | null>(null)
const devicePoolId = ref(''), devicePoolName = ref('')
const detailAccountId = ref(''), testModel = ref(''), testPrompt = ref('你好，请只回复 OK。'), testResult = ref('')
const detailAccount = computed(() => accounts.value.find(item => item.id === detailAccountId.value))
const pollFailures: Record<string, number> = {}
const jobReadVersions: Record<string, number> = {}
let pollTimer: number | null = null, qualityPollTimer: number | null = null, disposed = false, loadVersion = 0

function activeJob(job: GrokOAuthProtocolJob) { return ['pending', 'running'].includes(job.status) }
function poolName(id: string) { return cpaPools.value.find(pool => pool.id === id)?.name || '所选 CPA' }
function setRowBusy(id: string, action: string) { busyRows.value = { ...busyRows.value, [id]: action } }
function clearRowBusy(id: string) { const next = { ...busyRows.value }; delete next[id]; busyRows.value = next }

async function loadAccounts(readJobs = true) {
  const version = ++loadVersion
  loading.value = true
  const results = await Promise.allSettled([grokOAuthAccountsApi.list(), grokOAuthAccountsApi.candidates(), accountImportsApi.listCPAPools(), grokOAuthAccountsApi.qualityJobs(), grokOAuthAccountsApi.qualitySchedule()])
  if (disposed || version !== loadVersion) return
  const [accountResult, candidateResult, poolResult, qualityResult, scheduleResult] = results
  if (qualityResult.status === 'fulfilled') qualityJobs.value = qualityResult.value.items || []
  if (scheduleResult.status === 'fulfilled') {
    if (!qualitySchedule.value) { qualityScheduleEnabled.value = scheduleResult.value.enabled; qualityIntervalHours.value = scheduleResult.value.interval_hours }
    qualitySchedule.value = scheduleResult.value
  } else scheduleError.value = '检测设置读取失败，请刷新列表重试'
  if (accountResult.status === 'fulfilled') accounts.value = accountResult.value.items || []
  if (candidateResult.status === 'fulfilled') candidates.value = candidateResult.value.items || []
  candidateError.value = accountResult.status === 'rejected' || candidateResult.status === 'rejected' ? '账号读取不完整，请刷新列表重试。' : qualityResult.status === 'rejected' ? '检测任务状态读取失败，请刷新列表核对。' : ''
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
  if (qualityPollTimer !== null) window.clearTimeout(qualityPollTimer)
  qualityPollTimer = window.setTimeout(() => { void loadAccounts(false) }, qualityActiveIds.value.size ? 3000 : 30000)
  schedulePoll()
}

async function saveQualitySchedule() {
  if (!Number.isInteger(qualityIntervalHours.value) || qualityIntervalHours.value < 1 || qualityIntervalHours.value > 168) { scheduleError.value = '检测间隔应为1至168小时'; return }
  scheduleSaving.value = true; scheduleError.value = ''
  try { qualitySchedule.value = await grokOAuthAccountsApi.saveQualitySchedule(qualityScheduleEnabled.value, qualityIntervalHours.value); toast.success('定时检测设置已保存') }
  catch (error) { scheduleError.value = errorMessage(error, '设置保存失败') }
  finally { scheduleSaving.value = false }
}

function openQualityDetails(row: GrokOAuthRow) { detailAccountId.value = row.oauth_account_id; dialog.value = 'quality' }
async function checkQuality(row: GrokOAuthRow, cleanupOnly = false) {
  if (!row.oauth_account_id) return
  setRowBusy(row.id, 'quality')
  try { await grokOAuthAccountsApi.checkQuality([row.oauth_account_id], cleanupOnly); await loadAccounts(false); toast.info(cleanupOnly ? '已提交 CPA 移除核对' : '已提交 grok-4.7 检测，结果会显示在账号行') }
  catch (error) { rowErrors.value = { ...rowErrors.value, [row.id]: errorMessage(error, '无法启动检测') } }
  finally { clearRowBusy(row.id) }
}
async function checkVisibleQuality() {
  const ids = qualityEligible.value.map(row => row.oauth_account_id).slice(0, 500)
  if (!ids.length) return
  loading.value = true
  try { await grokOAuthAccountsApi.checkQuality(ids); await loadAccounts(false); toast.info(`已提交 ${ids.length} 个账号的 grok-4.7 检测`) }
  catch (error) { candidateError.value = errorMessage(error, '无法启动检测'); loading.value = false }
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
  if (account?.cpa_sync?.status === 'quality_blocked') { toast.warning('授权已保存，grok-4.7 检测未通过，未同步 CPA。请查看账号行的检测依据。'); return }
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
onBeforeUnmount(() => { disposed = true; loadVersion++; if (pollTimer !== null) window.clearTimeout(pollTimer); if (qualityPollTimer !== null) window.clearTimeout(qualityPollTimer) })
defineExpose({ loadAccounts, refreshAccount })
</script>
