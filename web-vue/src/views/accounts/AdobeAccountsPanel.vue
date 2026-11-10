<template>
  <PagePanel class="space-y-5">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div><h2 class="font-semibold">Adobe 账号</h2><p class="text-xs text-muted-foreground">Web Free 图片与视频共用账号额度。未知或过期数据不代表可用次数。</p></div>
      <Button variant="outline" size="sm" :disabled="loading || busy" @click="load">刷新列表</Button>
    </div>
    <div class="flex flex-wrap gap-3">
      <Input v-model="keyword" placeholder="搜索邮箱 / 账号 ID" aria-label="搜索 Adobe 账号" />
      <select v-model="status" class="ui-input" aria-label="Adobe 账号状态">
        <option value="">全部状态</option><option value="active">可用</option><option value="disabled">已禁用</option><option value="expired">凭据过期</option><option value="invalid">凭据异常</option><option value="pending">待验证 / 额度待查询</option><option value="exhausted">额度耗尽</option><option value="busy">生成中</option><option value="submission_unknown">提交结果待核对</option>
      </select>
      <Button size="sm" variant="outline" :disabled="busy" @click="showImport = !showImport">导入会话 JSON</Button>
    </div>
    <form v-if="showImport" class="space-y-3" @submit.prevent="importAccounts">
      <label class="block text-sm" for="adobe-session-import">账号会话 JSON</label>
      <textarea id="adobe-session-import" v-model="importText" class="ui-input w-full min-h-32 font-mono text-xs" placeholder='{"accounts":[{"email":"","account_id":"","token":"","cookie":""}]}' autocomplete="off" spellcheck="false" />
      <div class="flex gap-2"><Button type="submit" size="sm" :disabled="busy || !importText.trim()">导入</Button><Button size="sm" variant="outline" @click="closeImport">取消</Button></div>
    </form>
    <div class="flex flex-wrap gap-2">
      <span class="self-center text-xs text-muted-foreground">已选 {{ selected.length }} 个</span>
      <Button size="sm" variant="outline" :disabled="!selected.length || busy" @click="mutate('quota')">刷新额度</Button>
      <Button size="sm" variant="outline" :disabled="!selected.length || busy" @click="mutate('credentials')">刷新登录态</Button>
      <Button size="sm" variant="outline" :disabled="!selected.length || busy" @click="mutate('enable')">启用</Button>
      <Button size="sm" variant="outline" :disabled="!selected.length || busy" @click="mutate('disable')">禁用</Button>
      <Button size="sm" variant="outline" :disabled="!selected.length || busy" @click="confirmDelete = true">删除</Button>
    </div>
    <div v-if="confirmDelete" class="rounded border p-3 space-y-2" role="alert">
      <p class="text-sm">删除选中的 {{ selected.length }} 个 Adobe 账号？按邮箱来源配置执行同步清理；同一邮箱仍有其他渠道占用或注册预留时保留邮箱。</p>
      <Button size="sm" :disabled="busy" @click="mutate('delete')">确认删除</Button>
      <Button size="sm" variant="outline" :disabled="busy" @click="confirmDelete = false">取消</Button>
    </div>
    <p v-if="error" class="text-sm text-rose-500" role="alert">{{ error }}</p>
    <p v-if="message" class="text-sm text-emerald-600" role="status">{{ message }}</p>
    <div class="overflow-auto">
      <table class="w-full min-w-[780px] text-left text-sm">
        <thead class="text-xs text-muted-foreground"><tr>
          <th class="p-3"><input type="checkbox" aria-label="选择本页 Adobe 账号" :checked="allSelected" :disabled="busy || !accounts.length" @change="selectPage" /></th>
          <th class="p-3">邮箱 / 账号</th><th class="p-3">状态</th><th class="p-3">共享额度</th><th class="p-3">最近查询</th><th class="p-3">登录态有效期</th>
        </tr></thead>
        <tbody><tr v-for="account in accounts" :key="account.id" class="border-t align-top">
          <td class="p-3"><input v-model="selected" type="checkbox" :value="account.id" :disabled="busy" :aria-label="`选择 ${account.email}`" /></td>
          <td class="p-3 break-all"><div>{{ account.email }}</div><div class="text-xs text-muted-foreground">{{ account.account_id || account.id }}</div><p v-if="account.last_error" class="text-xs text-rose-500">{{ account.last_error }}</p></td>
          <td class="p-3">{{ account.disabled ? '已禁用' : statusLabel(account.status) }}<div v-if="account.in_flight" class="text-xs">生成中</div></td>
          <td class="p-3"><div>{{ quotaLabel(account.quota) }}</div><div v-if="account.quota?.total !== undefined" class="text-xs text-muted-foreground">总额 {{ account.quota.total }} · 已用 {{ account.quota.used ?? '未知' }}</div><p v-if="account.quota?.error" class="text-xs text-rose-500">{{ account.quota.error }}</p></td>
          <td class="p-3 text-xs">{{ time(account.quota?.checked_at) }}</td>
          <td class="p-3 text-xs"><div>{{ time(account.expires_at ? account.expires_at * 1000 : undefined) }}</div><div class="mt-1 max-w-52 text-muted-foreground">{{ mediaSessionLabel(account.media_session_status) }}</div><div v-if="account.media_session_updated_at" class="mt-1 text-muted-foreground" :title="`最近保存完整媒体会话：${time(account.media_session_updated_at * 1000)}`">会话保存：{{ time(account.media_session_updated_at * 1000) }}</div></td>
        </tr><tr v-if="!accounts.length"><td colspan="6" class="p-8 text-center text-muted-foreground">{{ loading ? '正在加载…' : '暂无 Adobe 账号，可在注册页创建或导入已有会话。' }}</td></tr></tbody>
      </table>
    </div>
    <div class="flex items-center justify-between gap-3 text-sm"><span>共 {{ total }} 个账号</span><div class="flex items-center gap-3"><Button size="sm" variant="outline" :disabled="loading || page <= 1" @click="page--">上一页</Button><span>{{ page }} / {{ Math.max(1, Math.ceil(total / pageSize)) }}</span><Button size="sm" variant="outline" :disabled="loading || page * pageSize >= total" @click="page++">下一页</Button></div></div>
  </PagePanel>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Button, Input } from 'nanocat-ui'
import PagePanel from '@/components/ai/PagePanel.vue'
import { adobeAccountsApi, type AdobeAccount, type AdobeQuota, type AdobeMutationResponse } from '@/api/adobeAccounts'
const emit = defineEmits<{ (e: 'total', value: number): void }>()
const accounts = ref<AdobeAccount[]>([]), total = ref(0), page = ref(1), pageSize = 25
const keyword = ref(''), status = ref(''), selected = ref<string[]>([]), loading = ref(false), busy = ref(false)
const error = ref(''), message = ref(''), showImport = ref(false), importText = ref(''), confirmDelete = ref(false)
let sequence = 0, searchTimer: ReturnType<typeof setTimeout> | undefined
const allSelected = computed(() => accounts.value.length > 0 && accounts.value.every(item => selected.value.includes(item.id)))
function selectPage() { selected.value = allSelected.value ? [] : accounts.value.map(item => item.id) }
function time(value?: string | number) { if (!value) return '未知'; const date = new Date(value); return Number.isFinite(date.getTime()) ? date.toLocaleString() : '未知' }
function statusLabel(value: string) { return ({ active: '可用', expired: '凭据过期', invalid: '凭据异常', pending: '待验证', refreshing: '刷新中', busy: '生成中', exhausted: '额度耗尽', submission_unknown: '提交结果待核对' } as Record<string, string>)[value] || value }
function mediaSessionLabel(value?: string) {
  return ({ imported: '已保存导入的媒体会话', refreshed: '媒体会话已更新', preserved: '凭据已刷新，保留原媒体会话' } as Record<string, string>)[value || ''] || '媒体会话状态未知'
}
function quotaLabel(quota?: AdobeQuota) {
  if (!quota) return '额度未知'
  const suffix = quota.available === undefined ? '' : ` · 上次可用 ${quota.available}`
  if (quota.status === 'stale') return `已过期${suffix}`
  if (quota.status === 'error' || quota.status === 'failed') return `查询失败${suffix}`
  if (!['known', 'success'].includes(quota.status) || quota.available === undefined) return `额度未知${suffix}`
  return quota.available === 0 ? '额度已用完（0）' : `可用 ${quota.available}`
}
async function load() {
  const current = ++sequence; loading.value = true; error.value = ''
  try { const result = await adobeAccountsApi.list({ page: page.value, page_size: pageSize, keyword: keyword.value.trim() || undefined, status: status.value || undefined }); if (current !== sequence) return; accounts.value = result.items; total.value = result.total; emit('total', result.all_total); selected.value = selected.value.filter(id => result.items.some(item => item.id === id)) }
  catch (cause) { if (current === sequence) error.value = cause instanceof Error ? cause.message : 'Adobe 账号加载失败' }
  finally { if (current === sequence) loading.value = false }
}
function mutationMessage(result: AdobeMutationResponse) { if (result.error) throw new Error(result.error); if (result.hme_cleanup?.failed) throw new Error(`账号已删除，但 ${result.hme_cleanup.failed} 个邮箱清理失败，请查看邮箱管理的操作日志。`); const failures = result.results?.filter(item => !item.ok) || []; if (failures.length) throw new Error(failures.map(item => item.error || '操作失败').join('；')); if ((result.summary?.fail || 0) > 0) throw new Error(`部分操作失败：成功 ${result.summary?.ok}，失败 ${result.summary?.fail}。请刷新列表查看账号状态。`); return result.summary ? `完成 ${result.summary.ok}/${result.summary.total} 个账号` : '操作已完成' }
async function mutate(action: 'quota' | 'credentials' | 'enable' | 'disable' | 'delete') {
  const ids = [...selected.value]; if (!ids.length || busy.value) return
  busy.value = true; error.value = ''; message.value = ''
  try { const result = action === 'delete' ? await adobeAccountsApi.remove(ids) : action === 'enable' || action === 'disable' ? await adobeAccountsApi.setDisabled(ids, action === 'disable') : await adobeAccountsApi.refresh(ids, action === 'credentials'); confirmDelete.value = false; await load(); message.value = mutationMessage(result) }
  catch (cause) { error.value = cause instanceof Error ? cause.message : '操作失败' }
  finally { busy.value = false }
}
function closeImport() { importText.value = ''; showImport.value = false }
async function importAccounts() {
  busy.value = true; error.value = ''; message.value = ''
  try { const parsed = JSON.parse(importText.value); const rows = Array.isArray(parsed) ? parsed : parsed.accounts; if (!Array.isArray(rows) || !rows.length) throw new Error('JSON 需要包含非空 accounts 数组'); const result = await adobeAccountsApi.import(rows); closeImport(); message.value = mutationMessage(result); await load() }
  catch (cause) { error.value = cause instanceof Error ? cause.message : '导入失败' }
  finally { busy.value = false }
}
watch(page, () => { selected.value = []; confirmDelete.value = false; void load() })
watch([keyword, status], () => { clearTimeout(searchTimer); searchTimer = setTimeout(() => { if (page.value !== 1) page.value = 1; else void load() }, 250) })
onMounted(load)
onBeforeUnmount(() => { sequence++; clearTimeout(searchTimer); importText.value = '' })
</script>
