<template>
  <div class="grid gap-1.5 text-xs" :title="grokMediaQuotaNote(item)">
    <div v-for="entry in grokMediaQuotaEntries(item)" :key="entry.key">
      <div class="flex items-baseline justify-between gap-3">
        <span class="text-muted-foreground">{{ entry.label }}</span>
        <span class="tabular-nums" :class="entry.exhausted ? 'text-amber-600' : 'text-foreground'">{{ entry.text }}</span>
      </div>
      <p v-if="entry.reset" class="mt-0.5 text-[11px] text-muted-foreground">恢复 {{ entry.reset }}</p>
    </div>
    <p class="text-[11px] text-muted-foreground">{{ grokMediaQuotaNote(item) }}</p>
    <p v-if="grokMediaQuotaCheckedAt(item)" class="text-[11px] text-muted-foreground">查询 {{ grokMediaQuotaCheckedAt(item) }}</p>
  </div>
</template>

<script setup lang="ts">
import type { GrokAccount } from '@/api/grokAccounts'
import { grokMediaQuotaCheckedAt, grokMediaQuotaEntries, grokMediaQuotaNote } from './grokMediaQuota'
defineProps<{ item: GrokAccount }>()
</script>
