<template>
  <PagePanel class="space-y-5">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div><h2 class="font-semibold">Adobe 账号</h2><p class="text-xs text-muted-foreground">免费生成次数按额度池共享；图片、视频和体验次数分别显示。通用积分不代表生成次数。</p></div>
      <Button variant="outline" size="sm" :disabled="loading || operationLocked" @click="load">刷新列表</Button>
    </div>
    <div class="flex flex-wrap items-center gap-3">
      <Input v-model="keyword" placeholder="搜索邮箱 / 账号 ID" aria-label="搜索 Adobe 账号" block root-class="min-w-[14rem] flex-1 md:max-w-sm" />
      <GroupedSelectMenu
        v-model="status"
        :options="statusOptions"
        selected-indicator="none"
        aria-label="Adobe 账号状态"
      />
      <Button size="sm" variant="outline" :disabled="operationLocked" @click="showImport ? closeImport() : showImport = true">导入已有账号</Button>
    </div>
    <form v-if="showImport" class="space-y-3" @submit.prevent="importAccounts">
      <label class="block text-sm" for="adobe-session-import">账号登录会话 JSON</label>
      <p id="adobe-session-import-help" class="text-xs leading-relaxed text-muted-foreground">导入已有 Adobe 账号的登录凭据，包含邮箱、账号 ID、Token 和 Cookie。通过 CFM 注册成功的账号会自动入库，无需手动导入。</p>
      <textarea id="adobe-session-import" v-model="importText" class="ui-textarea-sm font-mono" rows="8" aria-describedby="adobe-session-import-help" placeholder='{"accounts":[{"email":"","account_id":"","token":"","cookie":""}]}' autocomplete="off" spellcheck="false" />
      <div class="flex gap-2"><Button type="submit" size="sm" :disabled="operationLocked || !importText.trim()">导入</Button><Button size="sm" variant="outline" @click="closeImport">取消</Button></div>
    </form>
    <p v-if="error" class="text-sm text-rose-500" role="alert">{{ error }}</p>
    <p v-if="message" class="text-sm text-emerald-600" role="status">{{ message }}</p>
    <div class="overflow-auto">
      <table class="w-full min-w-[960px] text-left text-sm">
        <thead class="text-xs text-muted-foreground"><tr>
          <th class="p-3"><input type="checkbox" aria-label="选择本页 Adobe 账号" :checked="allSelected" :disabled="operationLocked || !accounts.length" @change="selectPage" /></th>
          <th class="p-3">邮箱 / 账号</th><th class="p-3">状态</th><th class="p-3">免费生成次数<div class="mt-1 font-normal">剩余 / 总量</div></th><th class="p-3">登录态有效期</th><th class="p-3 text-right">操作</th>
        </tr></thead>
        <tbody><tr v-for="account in accounts" :key="account.id" class="border-t align-top">
          <td class="p-3"><input v-model="selected" type="checkbox" :value="account.id" :disabled="operationLocked" :aria-label="`选择 ${account.email}`" /></td>
          <td class="p-3 break-all max-w-[16rem]"><div>{{ account.email }}</div><div class="text-xs text-muted-foreground">{{ account.account_id || account.id }}</div><p v-if="account.last_error" class="text-xs text-rose-500">{{ account.last_error }}</p></td>
          <td class="p-3 whitespace-nowrap">{{ account.disabled ? '已禁用' : statusLabel(account.status) }}<div v-if="account.in_flight" class="text-xs">生成中</div></td>
          <td class="p-3 min-w-[11rem]">
            <p v-if="adobeGenerationQuotaNotice(account.quota)" class="mb-1 text-xs text-muted-foreground">{{ adobeGenerationQuotaNotice(account.quota) }}</p>
            <div v-for="item in adobeGenerationQuotaItems(account.quota)" :key="item.key" class="flex items-baseline justify-between gap-3 text-xs leading-6" :title="item.models.join('、')">
              <span class="whitespace-nowrap">{{ item.label }}</span>
              <span class="whitespace-nowrap font-medium" :class="item.tone === 'danger' ? 'text-rose-500' : item.tone === 'muted' ? 'text-muted-foreground' : ''">{{ item.text }}</span>
            </div>
            <div class="mt-1 text-xs text-muted-foreground">{{ adobeCreditLabel(account.quota) }}</div>
            <p class="mt-1 text-xs text-muted-foreground">查询：{{ time(account.quota?.generation?.checked_at) }}</p>
            <p v-if="account.quota?.generation?.error" class="mt-1 text-xs text-rose-500">{{ account.quota.generation.error }}</p>
          </td>
          <td class="p-3 text-xs max-w-[13rem]"><div>{{ time(account.expires_at ? account.expires_at * 1000 : undefined) }}</div><div class="mt-1 max-w-52 text-muted-foreground">{{ mediaSessionLabel(account.media_session_status) }}</div><div v-if="account.media_session_updated_at" class="mt-1 text-muted-foreground" :title="`最近保存完整媒体会话：${time(account.media_session_updated_at * 1000)}`">会话保存：{{ time(account.media_session_updated_at * 1000) }}</div></td>
          <td class="p-3 text-right align-middle">
            <div class="flex items-center justify-end gap-2 whitespace-nowrap">
              <Button size="xs" variant="outline" :disabled="operationLocked || account.pending || account.in_flight" @click="mutate('quota', [account.id])">
                {{ operationIds.includes(account.id) && operation === 'quota' ? '刷新中…' : '刷新额度' }}
              </Button>
              <FloatingActionMenu
                label="更多"
                :items="rowActions(account)"
                :disabled="operationLocked"
                align="right"
                size="sm"
                trigger-class="h-7 justify-center px-2 text-[11px]"
                :trigger-width="64"
                @select="handleAction($event, [account.id])"
              />
            </div>
            <p v-if="operationIds.includes(account.id) && operation !== 'quota'" class="mt-1 text-xs text-muted-foreground">{{ operationLabel }}</p>
          </td>
        </tr><tr v-if="!accounts.length"><td colspan="6" class="p-8 text-center text-muted-foreground">{{ loading ? '正在加载…' : '暂无 Adobe 账号，可在注册页创建或导入已有账号。' }}</td></tr></tbody>
      </table>
    </div>
    <div class="flex items-center justify-between gap-3 text-sm" :class="selected.length ? 'pb-20' : ''"><span>共 {{ total }} 个账号</span><div class="flex items-center gap-3"><Button size="sm" variant="outline" :disabled="loading || page <= 1" @click="page--">上一页</Button><span>{{ page }} / {{ Math.max(1, Math.ceil(total / pageSize)) }}</span><Button size="sm" variant="outline" :disabled="loading || page * pageSize >= total" @click="page++">下一页</Button></div></div>
    <AccountBulkBar
      :selected-count="selected.length"
      :busy="operationLocked"
      :busy-label="operationLabel"
      :items="bulkActions"
      @select="handleAction($event, selected)"
      @clear="selected = []"
    />
  </PagePanel>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Button, Input, type ActionMenuItem } from 'nanocat-ui'
import PagePanel from '@/components/ai/PagePanel.vue'
import AccountBulkBar from '@/components/ai/AccountBulkBar.vue'
import FloatingActionMenu from '@/components/ai/FloatingActionMenu.vue'
import { useConfirmDialog } from '@/composables/useConfirmDialog'
import GroupedSelectMenu from '@/components/ui/GroupedSelectMenu.vue'
import { adobeAccountsApi, type AdobeAccount, type AdobeMutationResponse } from '@/api/adobeAccounts'
import { adobeGenerationQuotaItems, adobeGenerationQuotaNotice, adobeCreditLabel } from '@/lib/adobeQuotas'
const emit = defineEmits<{ (e: 'total', value: number): void }>()
const accounts = ref<AdobeAccount[]>([]), total = ref(0), page = ref(1), pageSize = 25
const keyword = ref(''), status = ref(''), selected = ref<string[]>([]), loading = ref(false), busy = ref(false)
const statusOptions = [
  { value: '', label: '全部状态' },
  { value: 'active', label: '可用' },
  { value: 'disabled', label: '已禁用' },
  { value: 'expired', label: '凭据过期' },
  { value: 'invalid', label: '凭据异常' },
  { value: 'pending', label: '待检查' },
  { value: 'exhausted', label: '额度耗尽' },
  { value: 'busy', label: '生成中' },
  { value: 'submission_unknown', label: '提交结果待核对' },
]

type Action = 'quota' | 'credentials' | 'enable' | 'disable' | 'delete'
const error = ref(''), message = ref(''), showImport = ref(false), importText = ref('')
const confirming = ref(false), operationIds = ref<string[]>([]), operation = ref<Action | ''>('')
const confirmDialog = useConfirmDialog()
let mounted = true
const operationLocked = computed(() => busy.value || confirming.value)
const operationLabel = computed(() => ({ quota: '刷新额度中…', credentials: '刷新登录态中…', enable: '启用中…', disable: '禁用中…', delete: '删除中…' }[operation.value as Action] || '处理中…'))
function accountBusy(account: AdobeAccount) { return Boolean(account.pending || account.in_flight) }
function rowActions(account: AdobeAccount): ActionMenuItem[] {
  return [
    { key: 'credentials', label: '刷新登录态', disabled: accountBusy(account) },
    { key: account.disabled ? 'enable' : 'disable', label: account.disabled ? '启用账号' : '禁用账号', disabled: accountBusy(account) },
    { key: 'delete', label: '删除账号', danger: true, dividerBefore: true, disabled: accountBusy(account) },
  ]
}
const bulkActions = computed<ActionMenuItem[]>(() => {
  const blocked = selected.value.some(id => { const account = accounts.value.find(item => item.id === id); return !account || accountBusy(account) })
  return [
    { key: 'quota', label: '刷新额度', disabled: blocked },
    { key: 'credentials', label: '刷新登录态', disabled: blocked },
    { key: 'enable', label: '启用账号', disabled: blocked },
    { key: 'disable', label: '禁用账号', disabled: blocked },
    { key: 'delete', label: '删除账号', danger: true, dividerBefore: true, disabled: blocked },
  ]
})
let sequence = 0, searchTimer: ReturnType<typeof setTimeout> | undefined
const allSelected = computed(() => accounts.value.length > 0 && accounts.value.every(item => selected.value.includes(item.id)))
function selectPage() { selected.value = allSelected.value ? [] : accounts.value.map(item => item.id) }
function time(value?: string | number) { if (!value) return '未知'; const date = new Date(value); return Number.isFinite(date.getTime()) ? date.toLocaleString() : '未知' }
function statusLabel(value: string) { return ({ active: '可用', expired: '凭据过期', invalid: '凭据异常', pending: '待检查', refreshing: '刷新中', busy: '生成中', exhausted: '额度耗尽', submission_unknown: '提交结果待核对' } as Record<string, string>)[value] || value }
function mediaSessionLabel(value?: string) {
  return ({ imported: '已保存导入的媒体会话', refreshed: '媒体会话已更新', preserved: '凭据已刷新，保留原媒体会话' } as Record<string, string>)[value || ''] || '媒体会话状态未知'
}
async function load() {
  const current = ++sequence; loading.value = true; error.value = ''
  try { const result = await adobeAccountsApi.list({ page: page.value, page_size: pageSize, keyword: keyword.value.trim() || undefined, status: status.value || undefined }); if (current !== sequence) return; accounts.value = result.items; total.value = result.total; emit('total', result.all_total); selected.value = selected.value.filter(id => result.items.some(item => item.id === id)) }
  catch (cause) { if (current === sequence) error.value = cause instanceof Error ? cause.message : 'Adobe 账号加载失败' }
  finally { if (current === sequence) loading.value = false }
}
function mutationMessage(result: AdobeMutationResponse) { if (result.error) throw new Error(result.error); if (result.hme_cleanup?.failed) throw new Error(`账号已删除，但 ${result.hme_cleanup.failed} 个邮箱清理失败，请查看邮箱管理的操作日志。`); const failures = result.results?.filter(item => !item.ok) || []; if (failures.length) throw new Error(failures.map(item => item.error || '操作失败').join('；')); if ((result.summary?.fail || 0) > 0) throw new Error(`部分操作失败：成功 ${result.summary?.ok}，失败 ${result.summary?.fail}。请刷新列表查看账号状态。`); return result.summary ? `完成 ${result.summary.ok}/${result.summary.total} 个账号` : '操作已完成' }
async function handleAction(key: string, targets: string[]) {
  if (!['quota', 'credentials', 'enable', 'disable', 'delete'].includes(key) || operationLocked.value) return
  // Capture the requested rows before any dialog or asynchronous work.
  const ids = [...new Set(targets)]
  if (!ids.length) return
  if (key === 'delete') {
    const labels = ids.map(id => accounts.value.find(account => account.id === id)?.email || id)
    confirming.value = true
    const confirmed = await confirmDialog.ask({
      title: '删除 Adobe 账号',
      message: `确认删除以下 ${ids.length} 个账号？\n${labels.join('\n')}\n按邮箱来源配置执行同步清理；同一邮箱仍有其他渠道占用或注册预留时保留邮箱。`,
      confirmText: '确认删除',
    })
    confirming.value = false
    if (!confirmed || !mounted) return
  }
  await mutate(key as Action, ids)
}
async function mutate(action: Action, targets: string[]) {
  const ids = [...new Set(targets)]
  if (!ids.length || operationLocked.value) return
  if (ids.some(id => { const account = accounts.value.find(item => item.id === id); return !account || accountBusy(account) })) {
    error.value = '账号正在生成或等待结算，请稍后再操作。'
    return
  }
  busy.value = true; operation.value = action; operationIds.value = ids; error.value = ''; message.value = ''
  try {
    const result = action === 'delete' ? await adobeAccountsApi.remove(ids) : action === 'enable' || action === 'disable' ? await adobeAccountsApi.setDisabled(ids, action === 'disable') : await adobeAccountsApi.refresh(ids, action === 'credentials')
    await load()
    message.value = mutationMessage(result)
  }
  catch (cause) { error.value = cause instanceof Error ? cause.message : '操作失败' }
  finally { busy.value = false; operation.value = ''; operationIds.value = [] }
}
function closeImport() { importText.value = ''; showImport.value = false }
async function importAccounts() {
  busy.value = true; error.value = ''; message.value = ''
  try { const parsed = JSON.parse(importText.value); const rows = Array.isArray(parsed) ? parsed : parsed.accounts; if (!Array.isArray(rows) || !rows.length) throw new Error('JSON 需要包含非空 accounts 数组'); const result = await adobeAccountsApi.import(rows); closeImport(); message.value = mutationMessage(result); await load() }
  catch (cause) { error.value = cause instanceof Error ? cause.message : '导入失败' }
  finally { busy.value = false }
}
watch(page, () => { selected.value = []; void load() })
watch([keyword, status], () => { clearTimeout(searchTimer); searchTimer = setTimeout(() => { if (page.value !== 1) page.value = 1; else void load() }, 250) })
onMounted(load)
onBeforeUnmount(() => { mounted = false; if (confirming.value) confirmDialog.cancel(); sequence++; clearTimeout(searchTimer); importText.value = '' })
</script>
