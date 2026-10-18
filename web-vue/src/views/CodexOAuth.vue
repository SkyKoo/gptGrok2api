<template>
  <div class="space-y-6 codex-oauth-page">
    <PagePanel class="space-y-5">
      <PanelHeader title="Codex 授权" align="start">
        <template #copy>
          <p class="mt-1 text-sm text-muted-foreground">
            Web Free 注册完成后，在这里独立获取 Codex OAuth 凭据并同步到 CPA。账号管理页面只负责 Web 登录 Token。
          </p>
        </template>
        <template #actions>
          <Button size="sm" variant="outline" :disabled="loading || busy" @click="loadAccounts">
            {{ loading ? '刷新中...' : '刷新' }}
          </Button>
        </template>
      </PanelHeader>

      <MetricStrip :items="metricItems" columns-class="grid-cols-2 md:grid-cols-4" density="compact" />

      <SurfaceBox tone="muted" density="compact" wrap>
        授权过程由你在上游页面手动完成手机验证码；如果上游要求 2FA，再输入 TOTP。CFM 不接入自动短信平台，Token 与 Web 登录 Token 分开保存。
      </SurfaceBox>

      <div class="flex flex-wrap items-center justify-between gap-3">
        <Input v-model.trim="keyword" type="text" placeholder="搜索邮箱、账号 ID 或 Codex 状态" block root-class="min-w-[18rem] flex-1" />
        <ConsoleSegmentedTabs
          :model-value="filter"
          :options="filterOptions"
          fit="content"
          aria-label="Codex 授权状态筛选"
          @update:model-value="filter = String($event) as CodexFilter"
        />
      </div>
    </PagePanel>

    <PagePanel>
      <PageLoadingState v-if="loading && !accounts.length" title="正在加载 Codex 账号" description="读取 Web Free 账号和 Codex 授权状态。" compact dashed />
      <EmptyState v-else-if="!filteredAccounts.length" plain title="暂无可授权账号" description="请先在“注册账号”完成 Web Free 注册并入库，账号随后会出现在这里。" />
      <div v-else class="overflow-x-auto">
        <table class="min-w-[980px] w-full text-left text-sm">
          <thead class="text-xs uppercase tracking-[0.14em] text-muted-foreground">
            <tr>
              <th class="py-3 pr-5">Web Free 账号</th>
              <th class="py-3 pr-5">Web 登录状态</th>
              <th class="py-3 pr-5">Codex OAuth</th>
              <th class="py-3 pr-5">CPA 同步</th>
              <th class="py-3 pr-5">最近更新</th>
              <th class="py-3 text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="account in filteredAccounts" :key="account.id" class="border-t border-border/60 align-top">
              <td class="py-4 pr-5">
                <div class="font-medium">{{ account.email || account.name }}</div>
                <div class="mt-1 text-xs text-muted-foreground">{{ account.id }} · {{ account.type || 'free' }} / {{ account.source_type || 'chatgpt_web' }}</div>
              </td>
              <td class="py-4 pr-5">
                <StateBadge :tone="accountStatusTone(account)" shape="rounded" size="sm">{{ account.status_label || accountStatusLabel(account) }}</StateBadge>
                <div v-if="account.status_reason" class="mt-1 max-w-[16rem] text-xs text-muted-foreground">{{ account.status_reason }}</div>
              </td>
              <td class="py-4 pr-5">
                <StateBadge :tone="codexStatusTone(account)" shape="rounded" size="sm">{{ codexStatusLabel(account) }}</StateBadge>
                <div v-if="account.codex_credential?.email" class="mt-1 text-xs text-muted-foreground">{{ account.codex_credential.email }}</div>
              </td>
              <td class="py-4 pr-5">
                <StateBadge :tone="cpaStatusTone(account)" shape="rounded" size="sm">{{ cpaStatusLabel(account) }}</StateBadge>
                <div v-if="account.codex_credential?.cpa_file_name" class="mt-1 max-w-[16rem] truncate text-xs text-muted-foreground">{{ account.codex_credential.cpa_file_name }}</div>
                <div v-else-if="account.codex_credential?.cpa_last_error" class="mt-1 max-w-[16rem] text-xs text-rose-600">{{ account.codex_credential.cpa_last_error }}</div>
              </td>
              <td class="py-4 pr-5 text-xs text-muted-foreground">{{ formatDate(account.codex_credential?.updated_at || account.updated_at) }}</td>
              <td class="py-4 text-right">
                <div class="flex flex-wrap justify-end gap-2">
                  <Button size="xs" variant="outline" :disabled="busy" @click="openAuthorize(account)">{{ account.codex_credential?.has_access_token ? '重新授权' : '开始授权' }}</Button>
                  <Button v-if="account.codex_credential?.has_access_token" size="xs" variant="outline" :disabled="busy" @click="syncCPA(account)">同步 CPA</Button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </PagePanel>

    <ModalShell :open="Boolean(selectedAccount)" max-width="52rem" :close-on-backdrop="!busy" @close="closeAuthorize">
      <div v-if="selectedAccount" class="space-y-5 p-6">
        <ModalHeader title="Codex OAuth 授权" :subtitle="selectedAccount.email || selectedAccount.name" compact @close="closeAuthorize" />
        <div class="grid gap-3 md:grid-cols-4">
          <div v-for="step in oauthSteps" :key="step.key" class="rounded-lg border p-3" :class="stepClass(step.key)">
            <div class="text-xs text-muted-foreground">{{ step.index }}</div>
            <div class="mt-1 text-sm font-medium">{{ step.label }}</div>
          </div>
        </div>
        <SurfaceBox tone="muted" density="compact" wrap>
          1. 点击“打开授权页”；2. 在上游完成手机验证码，若出现 2FA 则输入 TOTP；3. 授权完成后复制回调地址，粘贴到下方提交。不要把 Token 粘贴到页面或聊天中。
        </SurfaceBox>
        <div class="grid gap-3 md:grid-cols-[1fr_auto]">
          <label class="space-y-1 text-sm">
            <span class="text-muted-foreground">CPA 目标</span>
            <select v-model="cpaPoolId" class="h-9 w-full rounded-md border border-border bg-background px-3 text-sm" :disabled="busy">
              <option value="">不自动同步 CPA</option>
              <option v-for="pool in cpaPools" :key="pool.id" :value="pool.id">{{ pool.name }}</option>
            </select>
          </label>
          <div class="flex items-end">
            <Button size="sm" variant="primary" :disabled="busy || Boolean(authorizeUrl)" @click="startAuthorize">{{ busy && !authorizeUrl ? '准备中...' : '生成授权链接' }}</Button>
          </div>
        </div>
        <div v-if="authorizeUrl" class="space-y-3 rounded-lg border border-sky-500/40 bg-sky-500/5 p-4">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div>
              <div class="text-sm font-medium">授权页已准备</div>
              <div class="mt-1 text-xs text-muted-foreground">当前阶段：{{ progressMessage || progressStage || '等待授权' }}</div>
            </div>
            <Button size="sm" variant="outline" @click="openAuthorizeUrl">打开授权页</Button>
          </div>
          <Input v-model.trim="callbackText" type="text" placeholder="粘贴完成授权后得到的 callback URL" block :disabled="busy" />
          <Button size="sm" variant="primary" :disabled="busy || !callbackText" @click="finishAuthorize">{{ busy ? '提交中...' : '提交回调并保存' }}</Button>
        </div>
        <SurfaceBox v-if="progressError" tone="danger" density="compact" wrap>{{ progressError }}</SurfaceBox>
        <SurfaceBox v-else-if="progressMessage && progressStage !== 'waiting_authorization'" tone="muted" density="compact" wrap>{{ progressMessage }}</SurfaceBox>
      </div>
    </ModalShell>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { Button, EmptyState, Input } from 'nanocat-ui'
import { accountsApi, type Account } from '@/api/accounts'
import { accountImportsApi, type CPAPool } from '@/api/accountImports'
import ConsoleSegmentedTabs from '@/components/ai/ConsoleSegmentedTabs.vue'
import MetricStrip from '@/components/ai/MetricStrip.vue'
import ModalHeader from '@/components/ai/ModalHeader.vue'
import ModalShell from '@/components/ai/ModalShell.vue'
import PageLoadingState from '@/components/ai/PageLoadingState.vue'
import PagePanel from '@/components/ai/PagePanel.vue'
import PanelHeader from '@/components/ai/PanelHeader.vue'
import StateBadge from '@/components/ai/StateBadge.vue'
import SurfaceBox from '@/components/ai/SurfaceBox.vue'
import { useToast } from '@/composables/useToast'

type CodexFilter = 'all' | 'pending' | 'authorized' | 'synced' | 'failed'
defineOptions({ name: 'CodexOAuth' })
const toast = useToast()
const accounts = ref<Account[]>([])
const loading = ref(false)
const busy = ref(false)
const error = ref('')
const keyword = ref('')
const filter = ref<CodexFilter>('all')
const selectedAccount = ref<Account | null>(null)
const cpaPools = ref<CPAPool[]>([])
const cpaPoolId = ref('')
const sessionId = ref('')
const authorizeUrl = ref('')
const callbackText = ref('')
const progressStage = ref('')
const progressMessage = ref('')
const progressError = ref('')
let pollTimer: ReturnType<typeof setInterval> | null = null

const filterOptions = [
  { label: '全部', value: 'all' },
  { label: '待授权', value: 'pending' },
  { label: '已授权', value: 'authorized' },
  { label: 'CPA 已同步', value: 'synced' },
  { label: '失败待处理', value: 'failed' },
]
const oauthSteps = [
  { key: 'authorize', index: '1', label: '打开授权页' },
  { key: 'phone', index: '2', label: '手机验证码 / 2FA' },
  { key: 'callback', index: '3', label: '提交回调' },
  { key: 'cpa', index: '4', label: '保存并同步 CPA' },
]

const eligibleAccounts = computed(() => accounts.value.filter((item) => {
  const type = String(item.type || '').toLowerCase()
  const source = String(item.source_type || '').toLowerCase()
  return type === 'free' || source.includes('chatgpt') || source === 'web'
}))
const filteredAccounts = computed(() => {
  const term = keyword.value.toLowerCase()
  return eligibleAccounts.value.filter((item) => {
    const credential = item.codex_credential
    const haystack = [item.email, item.name, item.id, item.user_id, credential?.email, credential?.account_id, credential?.cpa_last_error].join(' ').toLowerCase()
    if (term && !haystack.includes(term)) return false
    if (filter.value === 'pending') return !credential?.has_access_token
    if (filter.value === 'authorized') return Boolean(credential?.has_access_token) && credential?.cpa_sync_status !== 'synced'
    if (filter.value === 'synced') return credential?.cpa_sync_status === 'synced'
    if (filter.value === 'failed') return credential?.status === 'failed' || credential?.cpa_sync_status === 'failed'
    return true
  })
})
const metricItems = computed(() => {
  const list = eligibleAccounts.value
  const authorized = list.filter((item) => item.codex_credential?.has_access_token).length
  const synced = list.filter((item) => item.codex_credential?.cpa_sync_status === 'synced').length
  const failed = list.filter((item) => item.codex_credential?.status === 'failed' || item.codex_credential?.cpa_sync_status === 'failed').length
  return [
    { key: 'total', label: '可授权 Web Free', value: list.length },
    { key: 'authorized', label: '已获得 Codex Token', value: authorized },
    { key: 'synced', label: 'CPA 已同步', value: synced },
    { key: 'failed', label: '待处理失败', value: failed, valueClass: failed ? 'text-rose-600' : '' },
  ]
})

async function loadAccounts() {
  loading.value = true; error.value = ''
  try {
    const result = await accountsApi.list({ page: 1, page_size: 500 })
    accounts.value = result.accounts
    if (!cpaPools.value.length) {
      try { cpaPools.value = (await accountImportsApi.listCPAPools()).pools || [] } catch { /* CPA is optional */ }
    }
  } catch (e: any) {
    error.value = e?.message || 'Codex 账号加载失败'
  } finally { loading.value = false }
}
function accountStatusLabel(account: Account) { return account.status_label || (account.enabled ? '正常' : '禁用') }
function accountStatusTone(account: Account) { return account.status_category === 'abnormal' ? 'danger' : account.status_category === 'limited' ? 'warning' : account.enabled ? 'success' : 'muted' }
function codexStatusLabel(account: Account) { const status = account.codex_credential?.status; return status === 'failed' ? '授权失败' : account.codex_credential?.has_access_token ? '已授权' : '待授权' }
function codexStatusTone(account: Account) { return account.codex_credential?.status === 'failed' ? 'danger' : account.codex_credential?.has_access_token ? 'success' : 'muted' }
function cpaStatusLabel(account: Account) { const status = account.codex_credential?.cpa_sync_status; return status === 'synced' ? '已同步' : status === 'failed' ? '同步失败' : account.codex_credential?.has_access_token ? '待同步' : '—' }
function cpaStatusTone(account: Account) { const status = account.codex_credential?.cpa_sync_status; return status === 'synced' ? 'success' : status === 'failed' ? 'danger' : account.codex_credential?.has_access_token ? 'warning' : 'muted' }
function formatDate(value?: string | number) { if (!value) return '—'; const date = new Date(value); return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString() }
function stepClass(key: string) { if (progressStage.value === 'done' && key === 'cpa') return 'border-emerald-500/50 bg-emerald-500/5'; if (progressStage.value === key || (key === 'authorize' && authorizeUrl.value)) return 'border-sky-500/50 bg-sky-500/5'; return 'border-border/60' }
function openAuthorize(account: Account) { selectedAccount.value = account; cpaPoolId.value = account.codex_credential?.cpa_pool_id || cpaPools.value[0]?.id || ''; resetProgress() }
function resetProgress() { stopPolling(); sessionId.value = ''; authorizeUrl.value = ''; callbackText.value = ''; progressStage.value = ''; progressMessage.value = ''; progressError.value = '' }
function closeAuthorize() { if (busy.value) return; selectedAccount.value = null; resetProgress() }
async function startAuthorize() {
  if (!selectedAccount.value) return
  busy.value = true; progressError.value = ''
  try {
    const result = await accountImportsApi.startCodexOAuthLogin({ account_ref: selectedAccount.value.id, cpa_pool_id: cpaPoolId.value || undefined })
    sessionId.value = String(result.session_id || ''); authorizeUrl.value = String(result.authorize_url || '')
    progressStage.value = String(result.stage || 'waiting_authorization'); progressMessage.value = String(result.message || '')
    if (!sessionId.value || !authorizeUrl.value) throw new Error('后端没有返回完整的 Codex OAuth 授权会话')
    startPolling(); openAuthorizeUrl(); toast.success('Codex 授权页面已打开')
  } catch (e: any) { progressError.value = e?.message || '生成 Codex 授权链接失败' } finally { busy.value = false }
}
function openAuthorizeUrl() { if (authorizeUrl.value) window.open(authorizeUrl.value, '_blank', 'noopener,noreferrer') }
async function finishAuthorize() {
  if (!sessionId.value || !callbackText.value || !selectedAccount.value) return
  busy.value = true; progressError.value = ''; stopPolling(); progressStage.value = 'exchanging_token'; progressMessage.value = '正在交换 Codex OAuth Token'
  try {
    const result = await accountImportsApi.finishCodexOAuthLogin({ session_id: sessionId.value, callback: callbackText.value, account_ref: selectedAccount.value.id, cpa_pool_id: cpaPoolId.value || undefined })
    if (result.ok === false) { progressError.value = result.message || 'Codex 已授权，但 CPA 同步失败' } else { progressStage.value = 'done'; progressMessage.value = result.message || 'Codex 登录并同步 CPA 完成'; toast.success(progressMessage.value) }
    await loadAccounts()
  } catch (e: any) { progressError.value = e?.message || 'Codex OAuth 失败' } finally { busy.value = false }
}
async function syncCPA(account: Account) {
  busy.value = true; error.value = ''
  try {
    const result = await accountImportsApi.syncCodexCPA(account.id, account.codex_credential?.cpa_pool_id || cpaPools.value[0]?.id || '')
    if (result.ok === false) throw new Error(result.error || 'CPA 同步失败')
    toast.success('Codex 凭据已同步到 CPA'); await loadAccounts()
  } catch (e: any) { error.value = e?.message || 'CPA 同步失败' } finally { busy.value = false }
}
function stopPolling() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null } }
function startPolling() { stopPolling(); const poll = async () => { if (!sessionId.value) return; try { const result = await accountImportsApi.getCodexOAuthProgress(sessionId.value); progressStage.value = result.stage || progressStage.value; progressMessage.value = result.error || result.message || progressMessage.value; if (result.status === 'completed' || result.status === 'failed') stopPolling() } catch { /* callback submission remains available */ } }; void poll(); pollTimer = setInterval(() => void poll(), 1000) }

onMounted(loadAccounts)
onUnmounted(stopPolling)
</script>

<style scoped>
.codex-oauth-page :deep(.metric-strip-card) { min-height: 92px; }
.codex-oauth-page table th, .codex-oauth-page table td { vertical-align: top; }
</style>
