<template>
  <div class="space-y-6 codex-oauth-page">
    <PagePanel class="space-y-5">
      <PanelHeader title="Codex OAuth" align="start">
        <template #copy>
          <p class="mt-1 text-sm text-muted-foreground">
            在这里获取或管理 Codex OAuth 凭据，并手动同步到 CPA。已从远程 CPA 绑定的凭据无需重新授权。
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
          点击授权后由 CFM 自动推进 Codex OAuth。按上游要求依次验证邮箱、已有 2FA，再进行手机号和短信验证；已保存 2FA 密钥时自动生成验证码，未保存时才需要手动输入。此流程只验证已有 2FA，不会开启或重新绑定 2FA。
      </SurfaceBox>

      <div class="flex flex-wrap items-center justify-between gap-3">
        <Input v-model.trim="keyword" type="text" placeholder="搜索邮箱、账号 ID 或 Codex 状态" block root-class="min-w-[18rem] flex-1" />
        <ConsoleSegmentedTabs
          :model-value="filter"
          :options="filterOptions"
          fit="content"
          aria-label="Codex OAuth 状态筛选"
          @update:model-value="filter = String($event) as CodexFilter"
        />
      </div>
    </PagePanel>

    <PagePanel>
      <PageLoadingState v-if="loading && !accounts.length" title="正在加载 Codex 账号" description="读取账号和 Codex 授权状态。" compact dashed />
      <EmptyState v-else-if="!filteredAccounts.length" plain title="暂无可授权账号" description="请先注册账号并入库，或在账号管理中将远程 CPA 凭据绑定到已有账号。" />
      <div v-else class="overflow-x-auto">
        <table class="min-w-[900px] w-full text-left text-sm">
          <thead class="text-xs uppercase tracking-[0.14em] text-muted-foreground">
            <tr>
              <th class="py-3 pr-5">ChatGPT 账号</th>
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
        <div class="grid gap-3 md:grid-cols-6">
          <div v-for="step in oauthSteps" :key="step.key" class="rounded-lg border p-3" :class="stepClass(step.key)">
            <div class="text-xs text-muted-foreground">{{ step.index }}</div>
            <div class="mt-1 text-sm font-medium">{{ step.label }}</div>
          </div>
        </div>
        <SurfaceBox tone="muted" density="compact" wrap>
          1. 点击“开始授权”；2. CFM 自动推进上游流程；3. 已保存密钥的 2FA 自动验证，其他验证步骤按上游要求处理；4. 完成后自动保存 Token 并同步 CPA，不会开启 2FA。
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
            <Button size="sm" variant="primary" :disabled="busy || Boolean(sessionId)" @click="startAuthorize">{{ busy && !sessionId ? '准备中...' : '开始授权' }}</Button>
          </div>
        </div>
        <div v-if="sessionId" class="space-y-3 rounded-lg border border-sky-500/40 bg-sky-500/5 p-4">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div>
              <div class="text-sm font-medium">{{ progressStage === 'failed' ? '授权已停止' : progressStage === 'done' ? '授权已完成' : '授权流程进行中' }}</div>
              <div class="mt-1 text-xs text-muted-foreground">当前阶段：{{ progressMessage || progressStage || '等待授权' }}</div>
            </div>
            <Button v-if="authorizeUrl && progressStage === 'waiting_authorization'" size="sm" variant="outline" @click="openAuthorizeUrl">打开授权页</Button>
          </div>
          <div v-if="progressStage === 'waiting_phone' || progressStage === 'waiting_phone_retry'" class="grid gap-2 sm:grid-cols-[1fr_auto]">
            <Input v-model.trim="phoneText" type="tel" :placeholder="progressStage === 'waiting_phone_retry' ? '输入新的手机号，不要重复上次号码' : '输入手机号，例如 +8613800138000'" block :disabled="busy" @keyup.enter="submitPhone" />
            <Button size="sm" variant="primary" :disabled="busy || !phoneText" @click="submitPhone">{{ busy ? '提交中...' : progressStage === 'waiting_phone_retry' ? '提交其他手机号' : '提交手机号' }}</Button>
          </div>
          <div v-else-if="progressStage === 'waiting_email_code'" class="grid gap-2 sm:grid-cols-[1fr_auto]">
            <Input v-model.trim="emailCode" inputmode="numeric" type="text" placeholder="输入邮箱验证码" block :disabled="busy" @keyup.enter="submitEmailCode" />
            <Button size="sm" variant="primary" :disabled="busy || !emailCode" @click="submitEmailCode">{{ busy ? '验证中...' : '验证邮箱验证码' }}</Button>
          </div>
          <div v-else-if="progressStage === 'waiting_sms_code'" class="grid gap-2 sm:grid-cols-[1fr_auto]">
            <Input v-model.trim="smsCode" inputmode="numeric" type="text" placeholder="输入短信验证码" block :disabled="busy" @keyup.enter="submitSMS" />
            <Button size="sm" variant="primary" :disabled="busy || !smsCode" @click="submitSMS">{{ busy ? '验证中...' : '验证短信验证码' }}</Button>
          </div>
          <div v-else-if="progressStage === 'waiting_2fa'" class="grid gap-2 sm:grid-cols-[1fr_auto]">
            <Input v-model.trim="twoFactorCode" inputmode="numeric" type="text" placeholder="输入 2FA 一次性验证码" block :disabled="busy" @keyup.enter="submit2FA" />
            <Button size="sm" variant="primary" :disabled="busy || !twoFactorCode" @click="submit2FA">{{ busy ? '验证中...' : '提交 2FA' }}</Button>
          </div>
          <div v-else-if="progressStage === 'waiting_authorization' && authorizeUrl" class="space-y-2">
            <p class="text-xs text-muted-foreground">当前上游要求在授权页面继续操作。完成后如页面提供 callback 地址，可粘贴到这里作为兼容兜底。</p>
            <Input v-model.trim="callbackText" type="text" placeholder="可选：粘贴 callback URL" block :disabled="busy" />
            <Button size="sm" variant="primary" :disabled="busy || !callbackText" @click="finishAuthorize">{{ busy ? '提交中...' : '提交回调并保存' }}</Button>
          </div>
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
const phoneText = ref('')
const emailCode = ref('')
const smsCode = ref('')
const twoFactorCode = ref('')
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
  { key: 'authorize', index: '1', label: '自动进入授权' },
  { key: 'email', index: '2', label: '按需验证邮箱' },
  { key: '2fa', index: '3', label: '自动验证 2FA' },
  { key: 'phone', index: '4', label: '输入手机号' },
  { key: 'sms', index: '5', label: '验证短信' },
  { key: 'cpa', index: '6', label: '保存并同步 CPA' },
]

const eligibleAccounts = computed(() => accounts.value.filter((item) => {
  const type = String(item.type || '').toLowerCase()
  const source = String(item.source_type || '').toLowerCase()
  return Boolean(item.codex_credential?.has_access_token) || type === 'free' || source.includes('chatgpt') || source === 'web'
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
    { key: 'total', label: 'Codex 账号', value: list.length },
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
function cpaStatusLabel(account: Account) { const status = account.codex_credential?.cpa_sync_status; return status === 'imported' ? '从 CPA 导入' : status === 'synced' ? '已同步' : status === 'failed' ? '同步失败' : account.codex_credential?.has_access_token ? '待同步' : '—' }
function cpaStatusTone(account: Account) { const status = account.codex_credential?.cpa_sync_status; return status === 'imported' ? 'info' : status === 'synced' ? 'success' : status === 'failed' ? 'danger' : account.codex_credential?.has_access_token ? 'warning' : 'muted' }
function formatDate(value?: string | number) { if (!value) return '—'; const numeric = typeof value === 'number' || /^\d+$/.test(String(value)); const date = numeric ? new Date(Number(value) < 10_000_000_000 ? Number(value) * 1000 : Number(value)) : new Date(value); return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString() }
function stepClass(key: string) {
  if (progressStage.value === 'done' && key === 'cpa') return 'border-emerald-500/50 bg-emerald-500/5'
  const emailStages = ['waiting_email_code', 'verifying_email_code']
  const phoneStages = ['waiting_phone', 'waiting_phone_retry', 'sending_phone']
  const smsStages = ['waiting_sms_code', 'verifying_sms']
  if (key === '2fa' && ['waiting_2fa', 'verifying_2fa'].includes(progressStage.value)) return 'border-sky-500/50 bg-sky-500/5'
  if (key === 'email' && emailStages.includes(progressStage.value)) return 'border-sky-500/50 bg-sky-500/5'
  if (key === 'phone' && phoneStages.includes(progressStage.value)) return 'border-sky-500/50 bg-sky-500/5'
  if (key === 'sms' && smsStages.includes(progressStage.value)) return 'border-sky-500/50 bg-sky-500/5'
  if (progressStage.value === key || (key === 'authorize' && authorizeUrl.value)) return 'border-sky-500/50 bg-sky-500/5'
  return 'border-border/60'
}
function openAuthorize(account: Account) { selectedAccount.value = account; cpaPoolId.value = account.codex_credential?.cpa_pool_id || cpaPools.value[0]?.id || ''; resetProgress() }
function resetProgress() { stopPolling(); sessionId.value = ''; authorizeUrl.value = ''; callbackText.value = ''; phoneText.value = ''; emailCode.value = ''; smsCode.value = ''; twoFactorCode.value = ''; progressStage.value = ''; progressMessage.value = ''; progressError.value = '' }
function closeAuthorize() { if (busy.value) return; selectedAccount.value = null; resetProgress() }
async function startAuthorize() {
  if (!selectedAccount.value) return
  busy.value = true; progressError.value = ''
  try {
    const result = await accountImportsApi.startCodexOAuthLogin({ account_ref: selectedAccount.value.id, cpa_pool_id: cpaPoolId.value || undefined })
    sessionId.value = String(result.session_id || ''); authorizeUrl.value = String(result.authorize_url || '')
    progressStage.value = String(result.stage || 'waiting_authorization'); progressMessage.value = String(result.message || '')
    if (!sessionId.value || !authorizeUrl.value) throw new Error('后端没有返回完整的 Codex OAuth 授权会话')
    startPolling(); if (progressStage.value === 'waiting_authorization') openAuthorizeUrl(); toast.success('Codex 授权流程已启动')
  } catch (e: any) { progressError.value = e?.message || '启动 Codex OAuth 失败' } finally { busy.value = false }
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
async function submitProtocolInput(kind: 'phone' | 'email' | 'sms' | '2fa', value: string) {
  if (!sessionId.value || !selectedAccount.value || !value.trim()) return
  busy.value = true; progressError.value = ''
  try {
    const payload = { session_id: sessionId.value, value: value.trim(), account_ref: selectedAccount.value.id, cpa_pool_id: cpaPoolId.value || undefined }
    const result = kind === 'phone' ? await accountImportsApi.submitCodexPhone(payload) : kind === 'email' ? await accountImportsApi.submitCodexEmailCode(payload) : kind === 'sms' ? await accountImportsApi.submitCodexSMSCode(payload) : await accountImportsApi.submitCodex2FA(payload)
    progressStage.value = String(result.stage || progressStage.value)
    progressMessage.value = String(result.message || progressMessage.value)
    if (result.ok === true) { toast.success(result.message || 'Codex 授权完成'); await loadAccounts() }
    if (kind === 'phone') phoneText.value = ''
    if (kind === 'email') emailCode.value = ''
    if (kind === 'sms') smsCode.value = ''
    if (kind === '2fa') twoFactorCode.value = ''
    startPolling()
  } catch (e: any) { progressError.value = e?.message || '提交授权信息失败' } finally { busy.value = false }
}
function submitPhone() { return submitProtocolInput('phone', phoneText.value) }
function submitEmailCode() { return submitProtocolInput('email', emailCode.value) }
function submitSMS() { return submitProtocolInput('sms', smsCode.value) }
function submit2FA() { return submitProtocolInput('2fa', twoFactorCode.value) }
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
