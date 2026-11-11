<template>
  <div>
    <p v-if="adobeGenerationQuotaNotice(quota)" class="mb-1 text-xs text-muted-foreground">{{ adobeGenerationQuotaNotice(quota) }}</p>
    <div v-for="item in adobeGenerationQuotaItems(quota)" :key="item.key" class="flex items-baseline justify-between gap-3 text-xs leading-6" :title="item.models.join('、')">
      <span class="whitespace-nowrap">{{ item.label }}</span>
      <span class="whitespace-nowrap font-medium" :class="item.tone === 'danger' ? 'text-rose-500' : item.tone === 'muted' ? 'text-muted-foreground' : ''">{{ item.text }}</span>
    </div>
    <div class="mt-1 text-xs text-muted-foreground">{{ adobeCreditLabel(quota) }}</div>
    <p class="mt-1 text-xs text-muted-foreground">查询：{{ time(quota?.generation?.checked_at) }}</p>
    <p v-if="quota?.generation?.error" class="mt-1 text-xs text-rose-500">{{ quota.generation.error }}</p>
  </div>
</template>
<script setup lang="ts">
import type { AdobeQuota } from '@/api/adobeAccounts'
import { adobeGenerationQuotaItems, adobeGenerationQuotaNotice, adobeCreditLabel } from '@/lib/adobeQuotas'
defineProps<{ quota?: AdobeQuota }>()
function time(value?: string | number) { if (!value) return '未知'; const date = new Date(value); return Number.isFinite(date.getTime()) ? date.toLocaleString() : '未知' }
</script>
