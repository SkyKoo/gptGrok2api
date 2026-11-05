<template>
  <div class="min-w-[15rem]">
    <dl class="grid grid-cols-2 gap-x-3 gap-y-1.5">
      <div v-for="quota in quotas" :key="quota.key" class="flex items-center justify-between gap-2" :title="quotaDetails(quota)">
        <dt class="text-xs text-muted-foreground">{{ quota.label }}</dt>
        <dd>
          <MetaChip :tone="quota.tone" size="xs" strong chip-class="font-mono tabular-nums">
            {{ quota.text }}
          </MetaChip>
        </dd>
      </div>
    </dl>
    <p class="mt-2 text-[11px] text-muted-foreground">{{ observedText }}</p>
    <p v-if="account.quota_last_attempt_at && account.quota_refresh_error" class="mt-1 text-[11px] text-muted-foreground">最近尝试：{{ formatTime(account.quota_last_attempt_at) }}</p>
    <p class="mt-1 text-[11px] text-muted-foreground">{{ quotaCheckReason(account.quota_check_reason) }}</p>
    <p v-if="account.quota_next_check_at" class="mt-1 text-[11px] text-muted-foreground" title="计划开始核对的时间，可能因在途任务或排队延后；不代表额度一定恢复。">计划核对：{{ formatTime(account.quota_next_check_at) }}</p>
    <details v-if="history.length" class="mt-2 text-[11px] text-muted-foreground">
      <summary class="cursor-pointer">最近额度变化（{{ history.length }}）</summary>
      <ol class="mt-1 max-h-48 space-y-2 overflow-y-auto">
        <li v-for="(event, index) in history" :key="`${event.at}-${index}`">
          <div>{{ formatTime(event.at) }} · {{ event.source === 'survival' ? '存活巡检' : '额度核对' }}</div>
          <div>{{ quotaHistoryText(event) }}</div>
        </li>
      </ol>
    </details>
    <p v-if="pending" class="mt-1 text-xs text-muted-foreground" :title="pendingDetails">{{ pending }} 项待核对</p>
    <p v-if="account.quota_refresh_error" class="mt-1 text-xs text-amber-600">刷新失败，显示上次记录</p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { Account } from '@/api/accounts'
import { accountQuotaItems, quotaCheckReason, quotaHistoryText } from '@/lib/accountQuotas'
import MetaChip from './MetaChip.vue'

const props = defineProps<{ account: Account }>()
const history = computed(() => [...(props.account.quota_history || [])].slice(-20).reverse())
function formatTime(value?: string | null) {
  if (!value) return '未安排'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '时间未知' : date.toLocaleString()
}
const quotas = computed(() => accountQuotaItems(props.account))
const pending = computed(() => quotas.value.reduce((sum, quota) => sum + quota.pending, 0))
const pendingDetails = computed(() => quotas.value.filter((q) => q.pending).map((q) => `${q.label}：${q.pending} 项待核对`).join('\n'))
const observedText = computed(() => props.account.quota_observed_at
  ? `最近核对：${new Date(props.account.quota_observed_at).toLocaleString()}`
  : '额度尚未重新核对')

function quotaDetails(quota: ReturnType<typeof accountQuotaItems>[number]) {
  const lines = [`${quota.label}（${quota.key}）：${quota.text}`, observedText.value]
  if (quota.resetAt) lines.push(`预计恢复：${new Date(quota.resetAt).toLocaleString()}`)
  if (quota.pending) lines.push(`${quota.pending} 项待核对；显示值尚未扣除这些待确认用量`)
  if (quota.key === 'reason') lines.push('推理额度；普通文本是否可用由上游决定')
  return lines.join('\n')
}
</script>
