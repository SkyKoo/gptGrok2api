<template>
  <div class="grid gap-1.5 text-xs" :title="grokMediaQuotaNote(item)">
    <div v-for="entry in grokMediaQuotaEntries(item)" :key="entry.key">
      <div class="flex items-baseline justify-between gap-3">
        <span class="text-muted-foreground">{{ entry.label }}</span>
        <span class="tabular-nums" :class="entry.exhausted ? 'text-amber-600' : 'text-foreground'">{{ entry.text }}</span>
      </div>
      <p v-if="entry.reset" class="mt-0.5 text-[11px] text-muted-foreground">预计恢复 {{ entry.reset }}</p>
    </div>
    <p class="text-[11px] text-muted-foreground">{{ grokMediaQuotaNote(item) }}</p>
    <p v-if="item.media_scheduling" class="text-[11px] text-muted-foreground">图片：{{ grokMediaSchedulingText(item, 'image') }}；视频：{{ grokMediaSchedulingText(item, 'video') }}</p>
    <p v-if="item.probe_at" class="text-[11px] text-muted-foreground">登录检测 {{ grokQuotaDisplayDate(String(item.probe_at)) }} · {{ ({ valid: '有效', invalid: '失效', unknown: '未确认' } as Record<string, string>)[item.probe_status || 'unknown'] || '未确认' }}</p>
    <p v-if="item.credential_next_check_at" class="text-[11px] text-muted-foreground">下次登录检测 {{ grokQuotaDisplayDate(item.credential_next_check_at) }}</p>
    <p v-if="item.media_quota_next_check_at" class="text-[11px] text-muted-foreground">下次额度检查 {{ grokQuotaDisplayDate(item.media_quota_next_check_at) }}</p>
    <p v-if="grokMediaLastSuccess(item)" class="text-[11px] text-muted-foreground">{{ grokMediaLastSuccess(item) }}</p>
    <details v-if="item.media_quota_history?.length" class="text-[11px] text-muted-foreground">
      <summary class="cursor-pointer">最近额度变化（{{ item.media_quota_history.length }}）</summary>
      <p v-for="(event, index) in grokMediaHistory(item)" :key="index">{{ event.at }} · {{ event.text }}</p>
    </details>
    <p v-if="grokMediaQuotaCheckedAt(item)" class="text-[11px] text-muted-foreground">查询 {{ grokMediaQuotaCheckedAt(item) }}</p>
  </div>
</template>

<script setup lang="ts">
import type { GrokAccount } from '@/api/grokAccounts'
import { grokMediaQuotaCheckedAt, grokMediaQuotaEntries, grokMediaQuotaNote, grokMediaSchedulingText, grokMediaLastSuccess, grokMediaHistory, grokQuotaDisplayDate } from './grokMediaQuota'
defineProps<{ item: GrokAccount }>()
</script>
