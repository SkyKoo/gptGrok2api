<template>
  <section class="space-y-4" aria-label="Grok 账号授权列表">
    <div>
      <h3 class="text-base font-semibold text-foreground">Grok 账号授权</h3>
      <p class="mt-1 text-xs leading-5 text-muted-foreground">从账号管理读取 Grok 账号。SSO 登录态与 xAI OAuth 凭据分别记录；尚未取得 OAuth 凭据的账号显示为待授权。</p>
    </div>
    <MetricStrip :items="metrics" columns-class="grid-cols-2 md:grid-cols-4" density="compact" />
    <div class="flex flex-wrap items-center gap-3">
      <Input v-model="keyword" block placeholder="搜索邮箱、账号 ID 或状态" root-class="min-w-[14rem] flex-1" />
      <ConsoleSegmentedTabs :model-value="filter" :options="filters" aria-label="xAI OAuth 状态筛选" @update:model-value="filter = String($event)" />
    </div>
    <p v-if="error" role="alert" class="text-sm text-rose-600">{{ error }}</p>
    <p v-else-if="loading && !items.length" class="py-5 text-center text-sm text-muted-foreground">正在读取 Grok 账号…</p>
    <p v-else-if="!filtered.length" class="rounded-lg border border-dashed border-border px-4 py-6 text-center text-sm text-muted-foreground">{{ items.length ? '没有符合筛选条件的账号。' : '暂无 Grok 账号，请先注册或在账号管理中导入。' }}</p>
    <div v-else class="overflow-x-auto">
      <table class="w-full min-w-[700px] text-left text-sm">
        <thead class="text-xs text-muted-foreground"><tr><th class="py-3 pr-4">Grok 账号</th><th class="py-3 pr-4">SSO 登录状态</th><th class="py-3 pr-4">xAI OAuth</th><th class="py-3 text-right">操作</th></tr></thead>
        <tbody>
          <tr v-for="item in pageItems" :key="item.id" class="border-t border-border/60 align-top">
            <td class="py-4 pr-4"><p class="font-medium">{{ item.email || '未保存邮箱' }}</p><p class="mt-1 text-xs text-muted-foreground">{{ item.id }}</p></td>
            <td class="py-4 pr-4"><StateBadge :tone="loginTone(item.login_status)" size="sm">{{ loginLabel(item.login_status) }}</StateBadge></td>
            <td class="py-4 pr-4"><StateBadge :tone="item.oauth_status === 'authorized' ? 'success' : item.oauth_status === 'pending' ? 'muted' : 'warning'" size="sm">{{ xaiOAuthStatusLabels[item.oauth_status] }}</StateBadge><p v-if="item.oauth_message" class="mt-1 max-w-sm text-xs text-amber-600">{{ item.oauth_message }}</p><p v-if="item.has_refresh_token" class="mt-1 text-xs text-muted-foreground">已保存 Refresh Token</p></td>
            <td class="py-4 text-right"><Button size="xs" variant="outline" :disabled="busy || !item.email || ['queued','running'].includes(item.oauth_status)" @click="$emit('authorize', item)">{{ ['queued','running'].includes(item.oauth_status) ? '授权处理中' : item.oauth_status === 'authorized' ? '重新授权' : '后台授权' }}</Button><p v-if="!item.email" class="mt-1 text-xs text-muted-foreground">请先补充账号邮箱</p></td>
          </tr>
        </tbody>
      </table>
      <div class="mt-3 flex items-center justify-end gap-2 text-xs text-muted-foreground"><span>共 {{ filtered.length }} 个 · 第 {{ page }} / {{ pages }} 页</span><Button size="xs" variant="outline" :disabled="page <= 1" @click="page--">上一页</Button><Button size="xs" variant="outline" :disabled="page >= pages" @click="page++">下一页</Button></div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Button, Input } from 'nanocat-ui'
import type { GrokOAuthCandidate } from '@/api/grokOAuthAccounts'
import MetricStrip from '@/components/ai/MetricStrip.vue'
import ConsoleSegmentedTabs from '@/components/ai/ConsoleSegmentedTabs.vue'
import StateBadge from '@/components/ai/StateBadge.vue'
import { filterGrokOAuthCandidates, xaiOAuthStatusLabels } from './grokOAuthCandidates'

const props = defineProps<{ items: GrokOAuthCandidate[]; loading: boolean; busy: boolean; error: string }>()
defineEmits<{ authorize: [account: GrokOAuthCandidate] }>()
const keyword = ref('')
const filter = ref('')
const page = ref(1)
const filters = [{label:'全部',value:''}, ...Object.entries(xaiOAuthStatusLabels).map(([value,label])=>({value,label}))]
const filtered = computed(()=>filterGrokOAuthCandidates(props.items,keyword.value,filter.value))
const pages = computed(()=>Math.max(1,Math.ceil(filtered.value.length/20)))
const pageItems = computed(()=>filtered.value.slice((page.value-1)*20,page.value*20))
const metrics = computed(()=>[
  {label:'Grok 账号',value:props.items.length},
  {label:'待授权',value:props.items.filter(i=>i.oauth_status==='pending').length},
  {label:'已授权',value:props.items.filter(i=>i.oauth_status==='authorized').length},
  {label:'需处理',value:props.items.filter(i=>['failed','requires_renewal','manual_action_required'].includes(i.oauth_status)).length},
])
watch([keyword,filter],()=>{page.value=1})
watch(pages,()=>{page.value=Math.min(page.value,pages.value)})
function loginLabel(status:string) { return ({active:'正常',valid:'正常',invalid:'异常',disabled:'已禁用',expired:'已过期'} as Record<string,string>)[status] || status || '未验证' }
function loginTone(status:string): 'success'|'danger'|'muted' { return ['active','valid','正常'].includes(status)?'success':['invalid','异常','expired'].includes(status)?'danger':'muted' }
</script>
