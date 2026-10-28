<template>
  <div class="space-y-6 oauth-page">
    <PagePanel class="space-y-5">
      <PanelHeader title="OAuth 授权" align="start">
        <template #copy>
          <p class="mt-1 text-sm text-muted-foreground">
            统一管理不同上游的 OAuth 凭据。每个提供商保持独立授权、刷新、模型探测和外部同步流程。
          </p>
        </template>
        <template #actions>
          <span class="text-xs text-muted-foreground">当前：{{ activeDefinition.label }}</span>
        </template>
      </PanelHeader>

      <div class="flex flex-wrap items-center justify-between gap-3">
        <ConsoleSegmentedTabs
          :model-value="activeProvider"
          :options="providerOptions"
          fit="content"
          aria-label="OAuth 提供商"
          @update:model-value="setActiveProvider(String($event))"
        />
        <p class="text-xs text-muted-foreground">{{ activeDefinition.description }}</p>
      </div>
    </PagePanel>

    <CodexOAuth v-if="activeProvider === 'codex'" />

    <PagePanel v-else class="oauth-xai-panel" flush>
      <GrokOAuthAccountsPanel :open="true" embedded @changed="handleXaiChanged" />
    </PagePanel>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import ConsoleSegmentedTabs from '@/components/ai/ConsoleSegmentedTabs.vue'
import PagePanel from '@/components/ai/PagePanel.vue'
import PanelHeader from '@/components/ai/PanelHeader.vue'
import GrokOAuthAccountsPanel from '@/views/accounts/GrokOAuthAccountsPanel.vue'
import CodexOAuth from '@/views/CodexOAuth.vue'
import {
  normalizeOAuthProvider,
  oauthProviderDefinitions,
  type OAuthProviderKey,
} from '@/views/oauth/providerRegistry'

defineOptions({ name: 'OAuth' })

const route = useRoute()
const router = useRouter()
const providerOptions = oauthProviderDefinitions.map(({ key, label }) => ({ label, value: key }))
const activeProvider = computed<OAuthProviderKey>(() => normalizeOAuthProvider(route.query.tab))
const activeDefinition = computed(() => oauthProviderDefinitions.find(({ key }) => key === activeProvider.value) || oauthProviderDefinitions[0])

function setActiveProvider(value: string) {
  const tab = normalizeOAuthProvider(value)
  void router.replace({ query: { ...route.query, tab } })
}

function handleXaiChanged() {
  // The xAI panel owns its account refresh. The page hook is kept as a stable
  // provider boundary for future shared OAuth notifications and metrics.
}
</script>

<style scoped>
.oauth-xai-panel :deep(.modal-shell__panel--inline) {
  border: 0;
  border-radius: 0;
}
</style>
