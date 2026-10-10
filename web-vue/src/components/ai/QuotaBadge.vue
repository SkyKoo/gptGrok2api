<template>
  <div :title="quotaDetails">
  <MetaChip
    :tone="quotaTone"
    size="xs"
    strong
    chip-class="min-w-[2.75rem] font-mono tabular-nums"
  >
    图片 {{ quotaText }}
  </MetaChip>
  <p class="mt-1 text-xs text-muted-foreground">对话 {{ reasonText }}</p>
  <p v-if="pending" class="mt-1 text-xs text-muted-foreground">{{ pending }} 项待核对</p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { Account } from '@/api/accounts'
import MetaChip from './MetaChip.vue'

const props = defineProps<{
  account: Account
}>()

const reasonText = computed(() => props.account.capability_quotas?.reason?.remaining ?? '未知')
const pending = computed(() => Object.values(props.account.quota_pending || {}).reduce((a, b) => a + b, 0))
const quotaDetails = computed(() => {
 const a = props.account
 const lines = [a.quota_observed_at ? `最近核对：${new Date(a.quota_observed_at).toLocaleString()}` : '额度尚未重新核对']
 for (const [key, q] of Object.entries(a.capability_quotas || {})) {
  lines.push(`${key}：${q.remaining ?? '未知'}${q.reset_at ? `，重置：${new Date(q.reset_at).toLocaleString()}` : ''}`)
 }
 if (a.quota_refresh_error) lines.push('刷新失败，显示上次已知额度')
 if (a.capability_quotas?.image_gen?.remaining === 0 && Number(a.capability_quotas?.reason?.remaining) > 0) lines.push('图片额度耗尽；对话额度仍可用（还需账号及模型可用）')
 return lines.join('\n')
})
const quotaValue = computed(() => Number(props.account.quota || 0))

const quotaText = computed(() => {
  if (props.account.image_quota_unknown) return '未知'
  return String(Math.max(0, Math.trunc(quotaValue.value)))
})

const quotaTone = computed(() => {
  if (props.account.image_quota_unknown) {
    return 'muted'
  }
  if (quotaValue.value <= 0) {
    return 'danger'
  }
  if (quotaValue.value <= 3) {
    return 'warning'
  }
  return 'success'
})
</script>
