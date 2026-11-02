<template>
  <ModalShell :open="props.open" :inline="props.embedded" max-width="48rem" :z-index="150" @close="emit('close')">
    <ModalHeader
      title="OAuth 接入"
      :subtitle="`${accounts.length} 个 OAuth 账号 · ${availableModels.length ? availableModels.join('、') : '暂无已探测模型'}`"
      compact
      :show-close="!props.embedded"
      @close="emit('close')"
    />
    <ModalBody density="compact" class="space-y-4">
      <GrokOAuthCandidates :items="candidates" :loading="loading" :busy="busy || Boolean(device) || protocolRunning" :error="candidateError" @authorize="startProtocolAuthorization" />
      <p class="text-xs text-muted-foreground">点击账号的“后台授权”后，系统使用该账号登录态自动处理。若 xAI 要求额外验证，会显示具体原因；手动设备码作为备用入口。</p>
      <div class="flex flex-wrap items-center gap-2">
        <Button size="sm" variant="outline" :disabled="busy || loading" @click="loadAccounts">
          {{ loading ? '刷新中...' : '刷新列表' }}
        </Button>
        <Button size="sm" variant="outline" :disabled="busy || loading || !accounts.length" @click="openModelTest">
          账号测试
        </Button>
        <Button size="sm" variant="outline" :disabled="busy" @click="showImport = !showImport">
          导入凭据
        </Button>
        <Button size="sm" variant="outline" :disabled="busy || protocolRunning" @click="startDeviceAuthorization()">
          手动设备码
        </Button>

      </div>

      <div class="space-y-2">
        <label class="ui-field-label">CPA 同步目标</label>
        <GroupedSelectMenu v-model="cpaPoolId" :options="cpaPoolOptions" placeholder="选择已配置的 CPA 连接" aria-label="CPA 同步目标" block />
        <p v-if="cpaLoadError" class="text-xs text-rose-600">{{ cpaLoadError }}</p>
        <p v-else-if="!cpaPools.length" class="text-xs text-muted-foreground">请先在账号导入中配置 CPA 连接。</p>
      </div>
      <section class="space-y-2" aria-label="OAuth 额度与有效期">
        <h3 class="text-sm font-medium text-foreground">已授权凭据 · 额度与有效期</h3>
        <p class="text-xs text-muted-foreground">这里显示上游返回的 OAuth 月度额度；月度上限为 0 不代表无法调用，也无法据此计算可用总量。实际调用结果在模型测试中确认。SSO 图片和视频额度仍在账号管理中查看。</p>
        <p v-if="!accounts.length" class="py-3 text-xs text-muted-foreground">暂无 OAuth 凭据，完成授权后显示。</p>
        <div v-else class="overflow-x-auto">
          <table class="w-full min-w-[700px] text-left text-xs">
            <thead class="text-muted-foreground"><tr>
              <th class="py-2 pr-4">邮箱 / OAuth 账号 ID</th><th class="py-2 pr-4">状态</th>
              <th class="py-2 pr-4">月度额度 / 剩余</th><th class="py-2 pr-4">账期结束时间</th>
              <th class="py-2 pr-4">凭据到期时间</th><th class="py-2 pr-4">CPA 同步</th><th class="py-2">操作</th>
            </tr></thead>
            <tbody><tr v-for="account in accounts" :key="account.id" class="border-t border-border">
              <td class="max-w-[18rem] break-all py-3 pr-4"><p>{{ account.email || '未返回邮箱' }}</p><p class="mt-1 font-mono text-muted-foreground">{{ account.id }}</p></td>
              <td class="py-3 pr-4">{{ grokOAuthAuthorizationText(oauthView(account)) }}</td>
              <td class="max-w-[18rem] py-3 pr-4"><p>{{ grokOAuthRemainingQuotaText(oauthView(account)) === '-' ? '待查询' : grokOAuthRemainingQuotaText(oauthView(account)) }}</p><p v-if="account.quota?.error" class="mt-1 break-words text-rose-600">{{ account.quota.error }}</p></td>
              <td class="whitespace-nowrap py-3 pr-4">{{ grokOAuthQuotaResetText(oauthView(account)) === '-' ? (account.quota?.status === 'error' ? '查询失败' : account.quota ? '上游未提供' : '待查询') : grokOAuthQuotaResetText(oauthView(account)) }}</td>
              <td class="whitespace-nowrap py-3 pr-4">{{ formatGrokAccountDate(account.expires_at) }}</td>
              <td class="max-w-[18rem] py-3 pr-4"><p>{{ cpaSyncText(account) }}</p><p v-if="account.cpa_sync?.pool_name" class="mt-1 text-muted-foreground">{{ account.cpa_sync.pool_name }}</p><p v-if="account.cpa_sync?.error" class="mt-1 break-words text-rose-600">{{ account.cpa_sync.error }}</p></td>
              <td class="py-3"><div class="flex gap-2 whitespace-nowrap"><Button size="xs" variant="outline" :disabled="busy || account.status === 'disabled'" @click="refreshAccount(account.id)">刷新额度</Button><Button size="xs" variant="outline" :disabled="busy || !cpaPoolId || !account.has_access_token || !account.has_refresh_token || ['disabled','invalid'].includes(account.status)" @click="syncCPA(account)">{{ account.cpa_sync?.status === 'synced' ? '重新同步 CPA' : '同步 CPA' }}</Button></div></td>
            </tr></tbody>
          </table>
        </div>
      </section>

      <SurfaceBox v-if="protocolJob" tone="muted" density="compact" class="space-y-2">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <span class="text-sm font-medium text-foreground">后台授权 · {{ protocolStageLabel }}</span>
          <StateBadge :tone="protocolJobTone" size="xs">{{ protocolPollError ? '状态待确认' : protocolStatusLabel }}</StateBadge>
        </div>
        <p class="text-xs text-muted-foreground">{{ protocolJob.message }}</p><p v-if="protocolPollError" class="text-xs text-amber-600">{{ protocolPollError }}</p>
        <p v-if="protocolJob.error" class="break-words text-xs text-rose-600">{{ protocolJob.error }}</p>
        <div v-if="protocolDeliveryItems.length" class="grid gap-1 border-t border-border pt-2">
          <div v-for="item in protocolDeliveryItems" :key="item.target" class="flex min-w-0 items-center justify-between gap-3 text-xs">
            <span class="truncate text-muted-foreground">{{ item.label }}</span>
            <StateBadge :tone="item.status === 'success' ? 'success' : 'danger'" size="xs">
              {{ item.status === 'success' ? '已上传' : '上传失败' }}
            </StateBadge>
          </div>
        </div>
      </SurfaceBox>

      <SurfaceBox v-if="device" tone="muted" density="compact" class="space-y-3">
        <p v-if="deviceTarget" class="text-sm text-foreground">正在授权：{{ deviceTarget.email }}。请在 xAI 页面确认登录的是此邮箱。</p>
        <p class="text-xs text-muted-foreground">完成后点击“检查授权”。仅当返回凭据的邮箱与账号邮箱一致时，列表才显示该账号已授权。</p>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <span class="font-mono text-sm text-foreground">{{ device.user_code }}</span>
          <div class="flex flex-wrap gap-2">
            <Button size="xs" variant="outline" @click="openDeviceAuthorization">打开授权页</Button>
            <Button size="xs" variant="primary" :disabled="busy" @click="pollDeviceAuthorization">检查授权</Button>
          </div>
        </div>
        <p class="truncate font-mono text-xs text-muted-foreground" :title="device.verification_uri_complete">
          {{ device.verification_uri_complete }}
        </p>
      </SurfaceBox>

      <SurfaceBox v-if="showImport" tone="muted" density="compact" class="space-y-3">
        <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
          <label class="text-xs md:col-span-2">
            <span class="ui-field-label">CPA OAuth JSON</span>
            <textarea v-model.trim="credentialText" rows="5" class="ui-textarea-sm font-mono" placeholder="粘贴 xAI OAuth CPA JSON"></textarea>
          </label>
          <label class="text-xs">
            <span class="ui-field-label">Access Token</span>
            <textarea v-model.trim="accessToken" rows="3" class="ui-textarea-sm font-mono" placeholder="可选：直接粘贴"></textarea>
          </label>
          <label class="text-xs">
            <span class="ui-field-label">Refresh Token</span>
            <textarea v-model.trim="refreshToken" rows="3" class="ui-textarea-sm font-mono" placeholder="可选：直接粘贴"></textarea>
          </label>
        </div>
        <div class="flex justify-end gap-2">
          <Button size="xs" variant="outline" :disabled="busy" @click="showImport = false">取消</Button>
          <Button size="xs" variant="primary" :disabled="busy || !hasImportValue" @click="importCredential">
            {{ busy ? '导入中...' : '导入' }}
          </Button>
        </div>
      </SurfaceBox>

      <SurfaceBox v-if="showModelTest" tone="muted" density="compact" class="space-y-3">
        <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
          <div class="text-xs">
            <span class="ui-field-label">测试范围</span>
            <ConsoleSegmentedTabs v-model="testScope" :options="testScopeOptions" aria-label="OAuth 账号测试范围" />
          </div>
          <div v-if="testScope === 'single'" class="text-xs">
            <span class="ui-field-label">指定账号</span>
            <GroupedSelectMenu
              v-model="testAccountId"
              :options="accountTestOptions"
              placeholder="选择 OAuth 账号"
              selected-indicator="none"
              aria-label="指定 OAuth 测试账号"
              block
            />
          </div>
        </div>
        <div class="grid grid-cols-1 gap-3 md:grid-cols-[minmax(12rem,0.35fr)_1fr]">
          <div class="text-xs">
            <span class="ui-field-label">模型</span>
            <GroupedSelectMenu
              v-model="testModel"
              :options="modelTestOptions"
              placeholder="选择模型"
              selected-indicator="none"
              aria-label="Grok 测试模型"
              block
            />
          </div>
          <label class="text-xs">
            <span class="ui-field-label">测试提示词</span>
            <textarea v-model.trim="testPrompt" rows="2" class="ui-textarea-sm" placeholder="输入测试提示词"></textarea>
          </label>
        </div>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <p v-if="testTotal" class="text-xs text-muted-foreground">
            已完成 {{ testCompleted }} / {{ testTotal }} · 成功 {{ testSuccessCount }} · 失败 {{ testFailureCount }}
          </p>
          <span v-else></span>
          <div class="flex gap-2">
            <Button size="xs" variant="outline" :disabled="busy" @click="closeModelTest">收起</Button>
            <Button size="xs" variant="primary" :disabled="busy || !canRunModelTest" @click="runModelTest">
              {{ testRunning ? `测试中 ${testCompleted}/${testTotal}` : testScope === 'all' ? '测试全部账号' : '测试指定账号' }}
            </Button>
          </div>
        </div>
        <div v-if="testResults.length" class="max-h-72 divide-y divide-border overflow-auto border-t border-border">
          <div v-for="item in testResults" :key="item.accountId" class="space-y-1 py-2.5">
            <div class="flex min-w-0 items-center justify-between gap-3">
              <div class="min-w-0">
                <p class="truncate text-xs font-medium text-foreground" :title="item.label">{{ item.label }}</p>
                <p class="truncate font-mono text-[11px] text-muted-foreground" :title="item.accountId">{{ item.accountId }}</p>
              </div>
              <div class="flex shrink-0 items-center gap-2">
                <span v-if="item.elapsedMs !== null" class="text-[11px] text-muted-foreground">{{ (item.elapsedMs / 1000).toFixed(1) }}s</span>
                <StateBadge :tone="testResultTone(item.status)" size="xs">{{ testResultStatusLabel(item.status) }}</StateBadge>
              </div>
            </div>
            <p v-if="item.error" class="break-words text-xs text-rose-600">{{ item.error }}</p>
            <p v-else-if="item.content" class="break-words text-xs text-foreground">{{ item.content }}</p>
          </div>
        </div>
      </SurfaceBox>
    </ModalBody>
    <ModalFooter v-if="!props.embedded">
      <Button size="sm" variant="outline" :disabled="busy" @click="emit('close')">完成</Button>
    </ModalFooter>
  </ModalShell>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Button } from 'nanocat-ui'

import {
  grokOAuthAccountsApi,
  type GrokOAuthAccount,
  type GrokOAuthCandidate,
  type GrokOAuthDeviceSession,
  type GrokOAuthProtocolJob,
} from '@/api/grokOAuthAccounts'
import GrokOAuthCandidates from './GrokOAuthCandidates.vue'
import { accountImportsApi, type CPAPool } from '@/api/accountImports'
import type { GrokAccount } from '@/api/grokAccounts'
import { formatGrokAccountDate, grokOAuthQuotaResetText, grokOAuthRemainingQuotaText, grokOAuthAuthorizationText } from './grokAccountView'

function oauthView(account: GrokOAuthAccount): GrokAccount {
  return { oauth: account } as GrokAccount
}

import ConsoleSegmentedTabs from '@/components/ai/ConsoleSegmentedTabs.vue'
import StateBadge from '@/components/ai/StateBadge.vue'
import SurfaceBox from '@/components/ai/SurfaceBox.vue'
import ModalBody from '@/components/ai/ModalBody.vue'
import ModalFooter from '@/components/ai/ModalFooter.vue'
import ModalHeader from '@/components/ai/ModalHeader.vue'
import ModalShell from '@/components/ai/ModalShell.vue'
import GroupedSelectMenu from '@/components/ui/GroupedSelectMenu.vue'
import { useToast } from '@/composables/useToast'
import { errorMessage } from '@/lib/errorMessage'

const toast = useToast()
const props = withDefaults(defineProps<{ open?: boolean; embedded?: boolean }>(), { open: false, embedded: false })
const emit = defineEmits<{
  (e: 'close'): void
  (e: 'changed'): void
}>()
const accounts = ref<GrokOAuthAccount[]>([])
const candidates = ref<GrokOAuthCandidate[]>([])
const candidateError = ref('')
const deviceTarget = ref<GrokOAuthCandidate | null>(null)
const availableModels = ref<string[]>([])
const loading = ref(false)
const busy = ref(false)
const showImport = ref(false)
const device = ref<GrokOAuthDeviceSession | null>(null)
const protocolJob = ref<GrokOAuthProtocolJob | null>(null)
const protocolPollError = ref('')
let protocolPollFailures = 0
let disposed = false
const cpaPools = ref<CPAPool[]>([])
const cpaPoolId = ref('')
const cpaLoadError = ref('')
const cpaPoolOptions = computed(() => cpaPools.value.map(pool => ({ label: pool.name || pool.base_url, value: pool.id })))
function cpaSyncText(account: GrokOAuthAccount) {
 return ({ synced: '已同步', failed: '同步失败', pending: '凭据已更新，待同步' } as Record<string,string>)[account.cpa_sync?.status || ''] || '未同步'
}
async function syncCPA(account: GrokOAuthAccount) {
 if (!cpaPoolId.value) return
 busy.value = true
 try {
  await grokOAuthAccountsApi.syncCPA(account.id, cpaPoolId.value)
  toast.success('xAI OAuth 凭据已同步到 CPA')
 } catch(error) { toast.error(errorMessage(error, 'CPA 同步失败')) }
 finally { await loadAccounts(); busy.value = false }
}
const credentialText = ref('')
const accessToken = ref('')
const refreshToken = ref('')
const showModelTest = ref(false)
const testModel = ref('')
const testPrompt = ref('你好，请只回复 OK。')
const testScope = ref<'all' | 'single'>('all')
const testAccountId = ref('')
const testRunning = ref(false)
const testCompleted = ref(0)
const testTotal = ref(0)
type OAuthAccountTestStatus = 'pending' | 'success' | 'failed'
type OAuthAccountTestItem = {
  accountId: string
  label: string
  status: OAuthAccountTestStatus
  elapsedMs: number | null
  content: string
  error: string
}
const testResults = ref<OAuthAccountTestItem[]>([])
let protocolPollTimer: number | null = null

const hasImportValue = computed(() => Boolean(credentialText.value || (accessToken.value && refreshToken.value)))
const testScopeOptions = [
  { label: '全部账号', value: 'all' },
  { label: '指定账号', value: 'single' },
]
const modelTestOptions = computed(() => Array.from(new Set([
  ...availableModels.value,
  ...accounts.value.flatMap((account) => account.models || []),
])).map((model) => ({ label: model, value: model })))
const accountTestOptions = computed(() => accounts.value.map((account) => ({
  label: `${account.email || account.subject_preview || account.id} · ${account.status}`,
  value: account.id,
})))
const canRunModelTest = computed(() => Boolean(
  testModel.value
  && testPrompt.value.trim()
  && (testScope.value === 'all' ? accounts.value.length : testAccountId.value),
))
const testSuccessCount = computed(() => testResults.value.filter((item) => item.status === 'success').length)
const testFailureCount = computed(() => testResults.value.filter((item) => item.status === 'failed').length)
const protocolRunning = computed(() => ['pending', 'running'].includes(protocolJob.value?.status || ''))
const protocolJobTone = computed(() => {
  if (protocolJob.value?.status === 'authorized') return 'success'
  if (protocolJob.value?.status === 'failed') return 'danger'
  return 'warning'
})
const protocolStatusLabel = computed(() => ({
  pending: '等待',
  running: '运行中',
  authorized: '已完成',
  failed: '失败',
  manual_action_required: '需处理',
}[protocolJob.value?.status || 'pending'] || protocolJob.value?.status || '等待'))
const protocolStageLabel = computed(() => ({
  queued: '排队', authorize: '授权处理中', completed: '完成', failed: '失败', attention: '需处理',
}[protocolJob.value?.stage || 'queued'] || protocolJob.value?.stage || '排队'))
const protocolDeliveryItems = computed(() => Object.entries(protocolJob.value?.delivery || {})
  .filter(([, result]) => result?.status === 'success' || result?.status === 'failed')
  .map(([target, result]) => ({
    target,
    label: target === 'sub2api' ? 'Sub2API' : target === 'cpa' ? 'CPA' : '外部投递',
    status: result.status,
  })))

async function loadAccounts() {
  loading.value = true
  try {
    const [result] = await Promise.all([
      grokOAuthAccountsApi.list(),
      accountImportsApi.listCPAPools().then(result => {
        cpaPools.value = result.pools || []; cpaLoadError.value = ''
        if (!cpaPools.value.some(pool => pool.id === cpaPoolId.value)) cpaPoolId.value = cpaPools.value.length === 1 ? cpaPools.value[0].id : ''
      }).catch(() => { cpaLoadError.value = 'CPA 连接加载失败，请刷新列表'; cpaPoolId.value = '' }),
      grokOAuthAccountsApi.candidates().then(result => { candidates.value = result.items || []; candidateError.value = '' }).catch(() => { candidateError.value = 'Grok 待授权账号加载失败，请刷新重试' }),
    ])
    const active = candidates.value.find(item => item.oauth_job_id && ['queued', 'running'].includes(item.oauth_status))
    const trackedJobId = protocolJob.value?.id || active?.oauth_job_id
    if (trackedJobId) {
      try {
        const resumed = await grokOAuthAccountsApi.getProtocolJob(trackedJobId)
        if (disposed || (protocolJob.value && protocolJob.value.id !== trackedJobId)) return
        protocolJob.value = resumed.job
        protocolPollError.value = ''; protocolPollFailures = 0
        if (['pending', 'running'].includes(resumed.job.status)) scheduleProtocolPoll()
        else if (protocolPollTimer !== null) { window.clearTimeout(protocolPollTimer); protocolPollTimer = null }
      } catch { protocolPollError.value = '任务状态读取失败，请刷新列表核对；页面状态不代表后台仍在运行' }
    }
    if (disposed) return
    accounts.value = result.items || []
    availableModels.value = result.available_models || Array.from(new Set(accounts.value.flatMap(account => account.models || [])))
    const modelIds = modelTestOptions.value.map((option) => option.value)
    if (!modelIds.includes(testModel.value)) testModel.value = modelIds[0] || ''
    if (!accounts.value.some((account) => account.id === testAccountId.value)) {
      testAccountId.value = accounts.value[0]?.id || ''
    }
  } catch (error) {
    toast.error(error instanceof Error ? error.message : 'OAuth 账号加载失败')
  } finally {
    loading.value = false
  }
}

function openModelTest() {
  if (!testModel.value) testModel.value = modelTestOptions.value[0]?.value || ''
  if (!testAccountId.value) testAccountId.value = accounts.value[0]?.id || ''
  showModelTest.value = true
}

function closeModelTest() {
  showModelTest.value = false
  testResults.value = []
  testCompleted.value = 0
  testTotal.value = 0
}

async function runModelTest() {
  const model = testModel.value.trim()
  const prompt = testPrompt.value.trim()
  const targets = testScope.value === 'all'
    ? [...accounts.value]
    : accounts.value.filter((account) => account.id === testAccountId.value)
  if (!model || !prompt || !targets.length) return

  busy.value = true
  testRunning.value = true
  testCompleted.value = 0
  testTotal.value = targets.length
  testResults.value = targets.map((account) => ({
    accountId: account.id,
    label: account.email || account.subject_preview || account.id,
    status: 'pending',
    elapsedMs: null,
    content: '',
    error: '',
  }))
  let nextIndex = 0
  try {
    const worker = async () => {
      while (nextIndex < targets.length) {
        const index = nextIndex
        nextIndex += 1
        const account = targets[index]
        const startedAt = performance.now()
        try {
          const result = await grokOAuthAccountsApi.testAccount(account.id, { model, prompt })
          testResults.value[index] = {
            ...testResults.value[index],
            status: 'success',
            elapsedMs: result.elapsed_ms,
            content: result.content,
          }
        } catch (error) {
          testResults.value[index] = {
            ...testResults.value[index],
            status: 'failed',
            elapsedMs: Math.round(performance.now() - startedAt),
            error: errorMessage(error, '账号测试失败'),
          }
        } finally {
          testCompleted.value += 1
        }
      }
    }
    await Promise.all(Array.from({ length: Math.min(3, targets.length) }, () => worker()))
    if (testFailureCount.value) {
      toast.warning(`账号测试完成：成功 ${testSuccessCount.value}，失败 ${testFailureCount.value}`)
    } else {
      toast.success(`账号测试完成：${testSuccessCount.value} 个账号全部可用`)
    }
    await loadAccounts()
    emit('changed')
  } finally {
    testRunning.value = false
    busy.value = false
  }
}

function testResultTone(status: OAuthAccountTestStatus) {
  if (status === 'success') return 'success'
  if (status === 'failed') return 'danger'
  return 'warning'
}

function testResultStatusLabel(status: OAuthAccountTestStatus) {
  if (status === 'success') return '可用'
  if (status === 'failed') return '失败'
  return '等待'
}

async function startDeviceAuthorization(candidate?: GrokOAuthCandidate) {
  if (device.value) { toast.info('请先完成当前设备码授权'); return }
  busy.value = true
  try {
    device.value = await grokOAuthAccountsApi.startDevice()
    deviceTarget.value = candidate || null
    window.open(device.value.verification_uri_complete, '_blank', 'noopener,noreferrer')
  } catch (error) {
    toast.error(error instanceof Error ? error.message : '无法创建设备授权')
  } finally {
    busy.value = false
  }
}

function scheduleProtocolPoll() {
  if (disposed) return
  if (protocolPollTimer !== null) window.clearTimeout(protocolPollTimer)
  protocolPollTimer = window.setTimeout(pollProtocolAuthorization, 1500)
}

async function pollProtocolAuthorization() {
  const jobId = protocolJob.value?.id
  if (disposed || !jobId) return
  protocolPollTimer = null
  try {
    const result = await grokOAuthAccountsApi.getProtocolJob(jobId)
    if (disposed || protocolJob.value?.id !== jobId) return
    protocolJob.value = result.job
    protocolPollError.value = ''; protocolPollFailures = 0
    candidates.value = candidates.value.map(item => item.id === result.job.source_account_id
      ? { ...item, oauth_status: result.job.status === 'pending' ? 'queued' : result.job.status }
      : item)
    if (result.job.status === 'authorized') {
      toast.success(`后台授权成功${result.job.models.length ? `：${result.job.models.join('、')}` : ''}`)
      await loadAccounts()
      emit('changed')
      return
    }
    if (['failed', 'manual_action_required'].includes(result.job.status)) {
      toast.warning(result.job.error || '后台授权需要处理')
      await loadAccounts()
      return
    }
    scheduleProtocolPoll()
  } catch (error) {
    if (disposed || protocolJob.value?.id !== jobId) return
    protocolPollError.value = '任务状态读取失败，正在重新查询；可点击刷新列表核对'
    protocolPollFailures += 1
    if (protocolPollFailures < 3) scheduleProtocolPoll()
    else protocolPollError.value = '任务状态读取失败，请刷新列表核对；页面状态不代表后台仍在运行'
  }
}

async function startProtocolAuthorization(candidate: GrokOAuthCandidate) {
  busy.value = true
  try {
    const result = await grokOAuthAccountsApi.startProtocol(candidate.id)
    protocolJob.value = result.job
    await loadAccounts()
    if (protocolRunning.value) scheduleProtocolPoll()
  } catch (error) {
    toast.error(errorMessage(error, '无法启动后台授权'))
  } finally {
    busy.value = false
  }
}

function openDeviceAuthorization() {
  if (device.value?.verification_uri_complete) {
    window.open(device.value.verification_uri_complete, '_blank', 'noopener,noreferrer')
  }
}

async function pollDeviceAuthorization() {
  if (!device.value) return
  busy.value = true
  try {
    const result = await grokOAuthAccountsApi.pollDevice(device.value.id)
    if (result.status === 'authorized') {
      device.value = null
      if (deviceTarget.value && result.account.email?.trim().toLowerCase() !== deviceTarget.value.email.trim().toLowerCase()) {
        toast.warning('OAuth 凭据已保存，但返回邮箱与所选账号不一致或缺失；未将所选账号标记为已授权。')
      } else { toast.success('xAI OAuth 凭据已保存') }
      deviceTarget.value = null
      await loadAccounts()
      emit('changed')
      return
    }
    toast.info('授权仍在等待中')
  } catch (error) {
    device.value = null
    toast.error(error instanceof Error ? error.message : '授权状态检查失败')
  } finally {
    busy.value = false
  }
}

async function importCredential() {
  busy.value = true
  try {
    let credential: Record<string, unknown> | undefined
    if (credentialText.value) {
      const parsed = JSON.parse(credentialText.value)
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        throw new Error('CPA OAuth JSON 必须是对象')
      }
      credential = parsed as Record<string, unknown>
    }
    await grokOAuthAccountsApi.importCredential({
      credential,
      access_token: accessToken.value || undefined,
      refresh_token: refreshToken.value || undefined,
    })
    credentialText.value = ''
    accessToken.value = ''
    refreshToken.value = ''
    showImport.value = false
    toast.success('Grok Build OAuth 凭据已导入')
    await loadAccounts()
    emit('changed')
  } catch (error) {
    toast.error(error instanceof Error ? error.message : 'OAuth 凭据导入失败')
  } finally {
    busy.value = false
  }
}

async function refreshAccount(id: string) {
  busy.value = true
  try {
    const result = await grokOAuthAccountsApi.queryQuota(id)
    if (result.ok) toast.success('OAuth 额度已更新')
    else toast.warning(result.account.quota?.error || 'OAuth 额度查询失败，已保留具体原因')
    await loadAccounts()
    emit('changed')
  } catch (error) {
    toast.error(error instanceof Error ? error.message : 'OAuth 额度查询失败')
  } finally {
    busy.value = false
  }
}

async function syncModels(id: string) {
  busy.value = true
  try {
    await grokOAuthAccountsApi.syncModels(id)
    toast.success('可用模型已更新')
    await loadAccounts()
    emit('changed')
  } catch (error) {
    toast.error(error instanceof Error ? error.message : '模型探测失败')
  } finally {
    busy.value = false
  }
}

async function setDisabled(account: GrokOAuthAccount) {
  busy.value = true
  try {
    await grokOAuthAccountsApi.setDisabled([account.id], account.status !== 'disabled')
    await loadAccounts()
    emit('changed')
  } catch (error) {
    toast.error(error instanceof Error ? error.message : '账号状态更新失败')
  } finally {
    busy.value = false
  }
}

async function removeAccount(id: string) {
  busy.value = true
  try {
    await grokOAuthAccountsApi.remove([id])
    toast.success('OAuth 账号已删除')
    await loadAccounts()
    emit('changed')
  } catch (error) {
    toast.error(error instanceof Error ? error.message : '账号删除失败')
  } finally {
    busy.value = false
  }
}

onMounted(loadAccounts)
onBeforeUnmount(() => {
  disposed = true
  if (protocolPollTimer !== null) window.clearTimeout(protocolPollTimer)
})

defineExpose({ loadAccounts, refreshAccount, syncModels, setDisabled, removeAccount })
</script>
