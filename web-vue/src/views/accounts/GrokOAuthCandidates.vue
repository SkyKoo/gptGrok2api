<template>
  <section class="min-w-0 space-y-4" aria-label="Grok 账号授权列表">
    <MetricStrip :items="metrics" columns-class="grid-cols-2 md:grid-cols-4" density="compact" />
    <div class="flex flex-wrap items-center gap-3">
      <Input v-model="keyword" block placeholder="搜索邮箱、账号 ID 或 CPA" root-class="min-w-0 flex-1 basis-64" />
      <ConsoleSegmentedTabs :model-value="filter" :options="filters" aria-label="xAI OAuth 状态筛选" @update:model-value="filter = String($event)" />
    </div>
    <p v-if="error" role="alert" class="text-sm text-rose-600">{{ error }}</p>
    <p v-else-if="loading && !items.length" class="py-5 text-center text-sm text-muted-foreground">正在读取 Grok 账号…</p>
    <p v-else-if="!filtered.length" class="rounded-lg border border-dashed border-border px-4 py-6 text-center text-sm text-muted-foreground">{{ items.length ? '没有符合筛选条件的账号。' : '暂无 Grok 账号，请先注册或在账号管理中导入。' }}</p>
    <p v-if="filtered.length" class="text-xs text-muted-foreground md:hidden">左右滑动表格，可查看 CPA 状态和账号操作。</p>
    <div v-if="filtered.length" class="max-w-full overflow-x-auto">
      <table class="w-full min-w-[1180px] table-fixed text-left text-sm" aria-label="xAI OAuth 账号">
        <colgroup><col class="w-[18%]" /><col class="w-24" /><col class="w-[14%]" /><col class="w-40" /><col /><col class="w-28" /><col class="w-56" /></colgroup>
        <thead class="whitespace-nowrap text-xs text-muted-foreground"><tr><th class="py-3 pr-5">Grok 账号</th><th class="py-3 pr-5">SSO 登录状态</th><th class="py-3 pr-5">xAI OAuth</th><th class="py-3 pr-5">grok-4.7 质量</th><th class="py-3 pr-5">CPA 同步</th><th class="py-3 pr-5">最近更新</th><th class="py-3 pl-4 text-right">操作</th></tr></thead>
        <tbody>
          <tr v-for="item in pageItems" :key="item.id" :data-account-row="item.id" class="border-t border-border/60 align-top">
            <td class="py-4 pr-5"><p class="break-all font-medium">{{ item.email || '未保存邮箱' }}</p><p class="mt-1 break-all text-xs text-muted-foreground">{{ item.source_account_id || item.oauth_account_id }}</p></td>
            <td class="py-4 pr-5"><StateBadge :tone="loginTone(item.login_status)" shape="rounded" size="sm">{{ loginLabel(item.login_status) }}</StateBadge></td>
            <td class="py-4 pr-5">
              <StateBadge :tone="item.poll_error ? 'warning' : item.oauth_status === 'authorized' ? 'success' : item.oauth_status === 'pending' ? 'muted' : 'warning'" shape="rounded" size="sm">{{ oauthProgressText(item) }}</StateBadge>
              <p v-if="item.poll_error || item.oauth_message" class="mt-2 break-words text-xs text-amber-600">{{ item.poll_error || item.oauth_message }}</p>
              <p v-if="isCPAManaged(item) && item.cpa_sync?.status === 'synced'" class="mt-2 text-xs text-muted-foreground">由 CPA 负责续期</p>
              <p v-else-if="item.quality?.blocked" class="mt-2 text-xs text-rose-600">已拦截 CPA 交付</p>
              <p v-else-if="item.quality?.status === 'error'" class="mt-2 text-xs text-amber-600">检测失败，复测通过后再处理交付</p>
              <p v-else-if="item.has_refresh_token && !running(item)" class="mt-2 text-xs text-muted-foreground">授权已保存，可单独同步</p>
              <p v-if="item.account?.status === 'disabled'" class="mt-2 text-xs text-muted-foreground">CFM OAuth 已禁用</p>
            </td>
            <td class="py-4 pr-5">
              <StateBadge :tone="item.quality?.blocked ? 'danger' : item.quality?.status === 'normal' ? 'success' : item.quality ? 'warning' : 'muted'" shape="rounded" size="sm">{{ qualityStatusText(item) }}</StateBadge>
              <p v-if="item.quality_running" class="mt-2 text-xs text-muted-foreground">正在检测，请稍候</p>
              <p v-if="qualityFailureReason(item)" class="mt-2 break-words text-xs text-amber-600">{{ qualityFailureReason(item) }}</p>
              <p v-if="item.quality?.at" class="mt-2 text-xs text-muted-foreground">{{ formatGrokAccountDate(item.quality.at) }}</p>
              <button v-if="item.quality" class="mt-2 text-xs text-primary underline" @click="$emit('qualityDetails', item)">查看检测依据</button>
            </td>
            <td class="py-4 pr-5">
              <StateBadge :tone="item.quality?.blocked ? 'danger' : item.quality?.status === 'error' ? 'warning' : item.cpa_sync?.status === 'synced' ? 'success' : item.cpa_sync ? 'warning' : 'muted'" shape="rounded" size="sm">{{ cpaStatusText(item) }}</StateBadge>
              <p v-if="item.cpa_sync?.pool_name || item.job?.cpa_pool_name" class="mt-2 break-words text-xs text-muted-foreground">{{ item.job?.status === 'running' ? item.job.cpa_pool_name || item.cpa_sync?.pool_name : item.cpa_sync?.pool_name }}</p>
              <p v-if="item.quality?.removal?.error" class="mt-2 break-words text-xs text-rose-600">{{ item.quality.removal.error }}</p>
              <p v-if="item.cpa_sync?.status === 'quality_removed' && isCPAManaged(item)" class="mt-2 text-xs text-muted-foreground">重新授权后再复测</p>
              <p v-if="item.quality?.removal && item.quality.removal.status !== 'removed'" class="mt-2 break-all text-xs text-muted-foreground">{{ item.quality.removal.file_name }}</p>
              <p v-if="item.cpa_sync?.error && (!isCPAManaged(item) || item.cpa_sync.status === 'quality_blocked')" class="mt-2 break-words text-xs text-rose-600">{{ item.cpa_sync.error }}</p>
              <p v-else-if="isCPAManaged(item) && !item.quality?.blocked && item.cpa_sync?.status !== 'synced'" class="mt-2 text-xs text-muted-foreground">核对远端交付记录</p>
            </td>
            <td class="py-4 pr-5 text-xs text-muted-foreground">{{ formatGrokAccountDate(item.job?.updated_at || item.account?.updated_at || item.updated_at) }}</td>
            <td class="py-4 pl-4 align-middle text-right">
              <div class="flex items-center justify-end gap-2">
                <Button v-if="item.source_account_id" size="sm" variant="outline" class="shrink-0 whitespace-nowrap" :disabled="Boolean(busyRows[item.id]) || !targetSelected || !item.email || running(item) || unavailable(item.login_status)" @click="$emit('authorize', item)">{{ running(item) ? '处理中' : item.has_refresh_token ? '重新授权' : '开始授权' }}</Button>
                <Button v-if="cpaAction(item)" size="sm" variant="outline" class="shrink-0 whitespace-nowrap" :disabled="Boolean(busyRows[item.id]) || running(item) || !(item.cpa_sync?.pool_id || targetSelected)" @click="$emit('cpa', item)">{{ busyRows[item.id] === 'cpa' ? '处理中' : cpaAction(item) === 'check' ? '核对 CPA' : item.cpa_sync?.status === 'failed' ? '重试同步' : '同步 CPA' }}</Button>
              </div>
              <div v-if="item.account" class="mt-2 flex flex-wrap justify-end gap-2">
                <Button size="xs" variant="outline" :disabled="Boolean(busyRows[item.id]) || running(item) || item.quality_running || item.account.status === 'disabled' || (item.cpa_sync?.status === 'quality_removed' && isCPAManaged(item))" @click="$emit('quality', item)">{{ item.quality_running ? '检测中' : item.quality ? '复测 grok-4.7' : '降智检测' }}</Button>
                <Button v-if="item.quality?.blocked && item.quality.confirmed_at && item.quality.removal && item.quality.removal.status !== 'removed'" size="xs" variant="outline" :disabled="Boolean(busyRows[item.id]) || running(item) || item.quality_running" @click="$emit('cleanup', item)">重试移除 CPA</Button>
              </div>
              <details v-if="item.source_account_id || item.account" class="mt-2 text-xs">
                <summary class="ml-auto w-fit cursor-pointer text-muted-foreground">更多</summary>
                <div class="mt-2 flex flex-wrap justify-end gap-2">
                  <Button v-if="item.source_account_id" size="xs" variant="outline" :disabled="Boolean(busyRows[item.id]) || running(item) || !targetSelected || unavailable(item.login_status)" @click="$emit('device', item)">手动设备码</Button>
                  <Button v-if="item.account && !isCPAManaged(item)" size="xs" variant="outline" :disabled="Boolean(busyRows[item.id]) || running(item)" @click="$emit('details', item)">额度与测试</Button>
                  <Button v-if="item.account" size="xs" variant="outline" :disabled="Boolean(busyRows[item.id]) || running(item)" @click="$emit('toggle', item)">{{ item.account.status === 'disabled' ? '启用 OAuth' : '禁用 OAuth' }}</Button>
                  <Button v-if="item.account" size="xs" variant="outline" class="text-rose-600" :disabled="Boolean(busyRows[item.id]) || running(item)" @click="$emit('remove', item)">移除 OAuth</Button>
                </div>
              </details>
              <p v-if="!targetSelected && item.source_account_id && !running(item)" class="mt-2 text-xs text-muted-foreground">{{ item.has_refresh_token ? '重新授权需选择 CPA 目标' : '开始授权需选择 CPA 目标' }}</p>
              <p v-if="item.action_error" role="alert" class="mt-2 break-words text-xs text-rose-600">{{ item.action_error }}</p>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div v-if="filtered.length" class="flex flex-wrap items-center justify-end gap-2 text-xs text-muted-foreground"><span>共 {{ filtered.length }} 个 · 第 {{ page }} / {{ pages }} 页</span><Button size="xs" variant="outline" :disabled="page <= 1" @click="page--">上一页</Button><Button size="xs" variant="outline" :disabled="page >= pages" @click="page++">下一页</Button></div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Button, Input } from 'nanocat-ui'
import MetricStrip from '@/components/ai/MetricStrip.vue'
import ConsoleSegmentedTabs from '@/components/ai/ConsoleSegmentedTabs.vue'
import StateBadge from '@/components/ai/StateBadge.vue'
import { formatGrokAccountDate } from './grokAccountView'
import { cpaAction, cpaStatusText, filterGrokOAuthCandidates, isCPAManaged, oauthProgressText, qualityFailureReason, qualityStatusText, type GrokOAuthRow } from './grokOAuthCandidates'

const props = defineProps<{ items: GrokOAuthRow[]; loading: boolean; busyRows: Record<string, string>; targetSelected: boolean; error: string }>()
defineEmits<{ authorize: [account: GrokOAuthRow]; cpa: [account: GrokOAuthRow]; device: [account: GrokOAuthRow]; details: [account: GrokOAuthRow]; toggle: [account: GrokOAuthRow]; remove: [account: GrokOAuthRow]; quality: [account: GrokOAuthRow]; qualityDetails: [account: GrokOAuthRow]; cleanup: [account: GrokOAuthRow] }>()
const keyword = ref(''), filter = ref(''), page = ref(1)
const filters = [{ label: '全部', value: '' }, { label: '待授权', value: 'pending' }, { label: '已授权', value: 'authorized' }, { label: '授权中', value: 'running' }, { label: '授权失败', value: 'failed' }, { label: 'CPA 待处理', value: 'cpa_attention' }, { label: '降智风险', value: 'quality_risk' }, { label: '检测失败', value: 'quality_error' }]
const filtered = computed(() => filterGrokOAuthCandidates(props.items, keyword.value, filter.value))
const pages = computed(() => Math.max(1, Math.ceil(filtered.value.length / 20)))
const pageItems = computed(() => filtered.value.slice((page.value - 1) * 20, page.value * 20))
const metrics = computed(() => [
  { label: '账号', value: props.items.length },
  { label: '待授权', value: props.items.filter(i => i.oauth_status === 'pending').length },
  { label: '有 CPA 交付记录', value: props.items.filter(i => i.cpa_sync?.status === 'synced').length },
  { label: '需处理', value: props.items.filter(i => i.quality?.blocked || i.quality?.status === 'error' || ['failed', 'requires_renewal', 'manual_action_required'].includes(i.oauth_status) || (i.cpa_sync && i.cpa_sync.status !== 'synced')).length },
])
watch([keyword, filter], () => { page.value = 1 })
watch(pages, () => { page.value = Math.min(page.value, pages.value) })
function running(item: GrokOAuthRow) { return ['queued', 'running'].includes(item.oauth_status) }
function unavailable(status: string) { return ['invalid', 'expired', 'disabled', '异常'].includes(status) }
function loginLabel(status: string) { return ({ active: '正常', valid: '正常', invalid: '异常', disabled: '已禁用', expired: '已过期', unlinked: '未关联 SSO' } as Record<string, string>)[status] || status || '未验证' }
function loginTone(status: string): 'success' | 'danger' | 'muted' { return ['active', 'valid', '正常'].includes(status) ? 'success' : unavailable(status) ? 'danger' : 'muted' }
</script>
