<template>
  <tr
    class="border-t border-border transition-colors"
    :class="grokAccountRowClass(item)"
    v-memo="[signature, selected, runtimeAvailable, busy, syncing, refreshing, testing, toggling, deleting, oauthAction]"
  >
    <td class="py-4 pr-4 align-middle">
      <Checkbox
        :model-value="selected"
        @update:model-value="emit('toggle-select', item.id, $event)"
      />
    </td>
    <td class="py-4 pr-5 align-middle">
      <p class="max-w-[11rem] truncate text-sm font-medium text-foreground">{{ item.email || '-' }}</p>
      <p class="mt-1 max-w-[11rem] truncate font-mono text-xs text-muted-foreground" :title="item.id">
        {{ grokAccountTokenPreview(item) }}
      </p>
    </td>
    <td class="py-4 pr-5 align-middle">
      <p class="text-xs font-medium text-foreground">{{ grokAccountPoolText(item) }}</p>
      <p class="mt-1 text-xs text-muted-foreground">{{ grokSyncStateText(item) }}</p>
    </td>
    <td class="py-4 pr-5 align-middle">
      <StatusPill
        :label="grokAccountStatusText(item)"
        :tone-class="`${grokAccountStatusClass(item)} border-border`"
      />
    </td>
    <td class="py-4 pr-5 align-middle">
      <div class="flex flex-col items-start gap-1">
        <StatusPill
          :label="grokRuntimeStatusText(item)"
          :tone-class="`${grokRuntimeStatusClass(item)} border-border`"
          :title="grokRefreshStatusTitle(item)"
        />
        <StatusPill
          v-if="item.probe_status"
          :label="grokProbeStatusText(item)"
          :tone-class="`${grokProbeStatusClass(item)} border-border`"
          :title="grokProbeStatusTitle(item)"
        />
      </div>
      <p v-if="grokRefreshFailed(item)" class="mt-1 max-w-[12rem] truncate text-xs text-amber-600" :title="grokRefreshStatusTitle(item)">
        {{ item.refresh_error || '上游未返回真实额度数据' }}
      </p>
    </td>
    <td class="max-w-[18rem] py-4 pr-5 align-middle font-mono text-xs text-muted-foreground">
      <span class="whitespace-normal leading-5">{{ grokQuotaText(item) }}</span>
    </td>
    <td class="py-4 pr-5 align-middle">
      <StatusPill
        :label="grokOAuthShortStatusText(item)"
        :tone-class="`${grokOAuthStatusClass(item)} border-border`"
        :title="grokOAuthStatusTitle(item)"
      />
      <template v-if="item.oauth">
        <p class="mt-1 max-w-[7.5rem] truncate font-mono text-[11px] text-muted-foreground">{{ item.oauth.probe?.model || item.oauth.models?.[0] || 'grok-4.5' }}</p>
        <p v-if="item.oauth.probe?.at" class="mt-1 whitespace-nowrap text-[11px] text-muted-foreground">
          {{ formatGrokAccountDate(item.oauth.probe.at) }}
        </p>
        <p
          v-if="grokOAuthRecoveryStatusText(item)"
          class="mt-1 text-[11px] font-medium"
          :class="grokOAuthRecoveryStatusClass(item)"
          :title="grokOAuthRecoveryStatusTitle(item)"
        >
          {{ grokOAuthRecoveryStatusText(item) }}
        </p>
      </template>
    </td>
    <td class="py-4 pr-5 align-middle">
      <template v-if="item.oauth">
        <div class="grid gap-1 font-mono text-xs tabular-nums">
          <p class="grid grid-cols-[3.5rem_1fr] gap-2">
            <span class="text-muted-foreground">剩余</span>
            <span class="text-foreground">{{ grokOAuthRemainingQuotaText(item) }}</span>
          </p>
          <p class="grid grid-cols-[3.5rem_1fr] gap-2">
            <span class="text-muted-foreground">重置</span>
            <span class="whitespace-nowrap text-foreground">{{ grokOAuthQuotaResetText(item) }}</span>
          </p>
        </div>
      </template>
      <span v-else class="text-xs text-muted-foreground">-</span>
    </td>
    <td
      class="py-4 pr-5 align-middle text-xs text-muted-foreground"
      :title="grokOAuthRecoveryStatusTitle(item) || grokRecoveryStatusTitle(item)"
    >
      <p class="whitespace-nowrap">{{ grokRecoveryTimeText(item) }}</p>
      <StatusPill
        v-if="item.recovery_status"
        class="mt-1"
        :label="grokRecoveryStatusText(item)"
        :tone-class="`${grokRecoveryStatusClass(item)} border-border`"
      />
    </td>
    <td class="py-4 pr-5 align-middle">
      <p class="font-mono text-sm tabular-nums">
        <span :class="item.sync_state === 'synced' ? 'text-emerald-600' : 'text-muted-foreground'">
          {{ item.sync_state === 'synced' ? (item.use_count || 0) : '-' }}
        </span>
        <span class="mx-1 text-muted-foreground/60">/</span>
        <span :class="item.sync_state === 'synced' ? 'text-rose-600' : 'text-muted-foreground'">
          {{ item.sync_state === 'synced' ? (item.fail_count || 0) : '-' }}
        </span>
      </p>
      <p class="mt-1 text-xs text-muted-foreground">成功率 {{ grokSuccessRate(item) }}</p>
    </td>
    <td class="py-4 pr-5 align-middle text-xs text-muted-foreground">
      {{ formatGrokAccountDate(item.last_used_at) }}
    </td>
    <td class="w-[16rem] py-4 text-right align-middle">
      <GrokAccountActionButtons
        :item="item"
        :runtime-available="runtimeAvailable"
        :busy="busy"
        :syncing="syncing"
        :refreshing="refreshing"
        :testing="testing"
        :toggling="toggling"
        :deleting="deleting"
        :authorizing="authorizing"
        :oauth-account="item.oauth"
        :oauth-action="oauthAction"
        align="end"
        @credentials="emit('credentials', item)"
        @sync="emit('sync', item)"
        @refresh="emit('refresh', item)"
        @test="emit('test', item)"
        @toggle-disabled="emit('toggle-disabled', item)"
        @remove="emit('remove', item)"
        @oauth-sync="emit('oauth-sync', item)"
        @oauth-authorize="emit('oauth-authorize', item)"
        @oauth-refresh="emit('oauth-refresh', item)"
        @oauth-toggle="emit('oauth-toggle', item)"
        @oauth-remove="emit('oauth-remove', item)"
      />
    </td>
  </tr>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { Checkbox, StatusPill } from 'nanocat-ui'

import GrokAccountActionButtons from '@/components/ai/GrokAccountActionButtons.vue'
import type { GrokAccount } from '@/api/grokAccounts'
import {
  formatGrokAccountDate,
  grokAccountPoolText,
  grokAccountRowClass,
  grokAccountRowSignature,
  grokAccountStatusClass,
  grokAccountStatusText,
  grokAccountTokenPreview,
  grokQuotaText,
  grokRecoveryStatusClass,
  grokRecoveryStatusText,
  grokRecoveryStatusTitle,
  grokRecoveryTimeText,
  grokRefreshFailed,
  grokRefreshStatusTitle,
  grokOAuthStatusClass,
  grokOAuthRemainingQuotaText,
  grokOAuthRecoveryStatusClass,
  grokOAuthRecoveryStatusText,
  grokOAuthRecoveryStatusTitle,
  grokOAuthShortStatusText,
  grokOAuthStatusTitle,
  grokOAuthQuotaResetText,
  grokProbeStatusClass,
  grokProbeStatusText,
  grokProbeStatusTitle,
  grokRuntimeStatusClass,
  grokRuntimeStatusText,
  grokSuccessRate,
  grokSyncStateText,
} from './grokAccountView'

const props = withDefaults(defineProps<{
  item: GrokAccount
  selected?: boolean
  runtimeAvailable?: boolean
  busy?: boolean
  syncing?: boolean
  refreshing?: boolean
  testing?: boolean
  toggling?: boolean
  deleting?: boolean
  authorizing?: boolean
  oauthAction?: string
}>(), {
  selected: false,
  runtimeAvailable: false,
  busy: false,
  syncing: false,
  refreshing: false,
  testing: false,
  toggling: false,
  deleting: false,
  authorizing: false,
  oauthAction: '',
})

const signature = computed(() => grokAccountRowSignature(props.item))

const emit = defineEmits<{
  (e: 'toggle-select', id: string, checked: unknown): void
  (e: 'credentials', item: GrokAccount): void
  (e: 'sync', item: GrokAccount): void
  (e: 'refresh', item: GrokAccount): void
  (e: 'test', item: GrokAccount): void
  (e: 'toggle-disabled', item: GrokAccount): void
  (e: 'remove', item: GrokAccount): void
  (e: 'oauth-sync', item: GrokAccount): void
  (e: 'oauth-authorize', item: GrokAccount): void
  (e: 'oauth-refresh', item: GrokAccount): void
  (e: 'oauth-toggle', item: GrokAccount): void
  (e: 'oauth-remove', item: GrokAccount): void
}>()
</script>
