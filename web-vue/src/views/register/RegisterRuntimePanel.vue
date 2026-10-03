<template>
  <aside class="register-runtime-column">
    <FormSection title="执行控制" density="roomy" class="register-runtime-section">
      <MetricStrip
        :items="metricItems"
        columns-class="grid-cols-2 md:grid-cols-4"
        density="compact"
      />

      <div class="register-runtime-actions" :class="{ 'register-runtime-actions--grok': target === 'grok' }">
        <Button
          block
          variant="primary"
          :disabled="actionDisabled"
          @click="emit('toggle-task')"
        >
          {{ enabled ? '停止' : '启动' }}
        </Button>
        <Button
          block
          variant="outline"
          :disabled="resetDisabled"
          @click="emit('reset-stats')"
        >
          {{ target === 'openai' ? '清理历史记录' : '重置' }}
        </Button>
        <FloatingActionMenu
          v-if="target === 'grok'"
          class="register-runtime-export-menu"
          :label="exportBusy ? '导出中' : '导出账号'"
          :items="grokExportItems"
          :disabled="exportBusy"
          align="right"
          placement="auto"
          @select="format => emit('export-grok', format === 'cpa' ? 'cpa' : 'sub2api')"
        />
      </div>

      <SurfaceBox tone="muted" density="compact">
        {{ runtimeHint }}
      </SurfaceBox>

      <SurfaceBox v-if="target === 'openai'" tone="muted" density="compact" class="register-runtime-tips">
        <p>Cloudflare 拦截：可在系统设置启用 FlareSolverr 清障，并确认相关容器已启动。</p>
        <p>HTTP 400 等注册错误通常与邮箱域名风控有关，建议更换新的域名邮箱后重试。</p>
      </SurfaceBox>
    </FormSection>

    <RuntimeLogPanel
      class="register-runtime-log"
      :title="target === 'grok' ? '注册进度' : '实时日志'"
      :lines="activeLogLines"
      :empty-title="activeLogEmptyTitle"
      min-height="20rem"
      max-height="min(58vh, 38rem)"
    >
      <template v-if="showLogTabs" #actions>
        <ConsoleSegmentedTabs
          :model-value="activeLog"
          :options="logTabs"
          aria-label="实时日志类型"
          fit="content"
          @update:model-value="setActiveLog"
        />
      </template>
    </RuntimeLogPanel>

    <div v-if="$slots.default" class="register-runtime-extension">
      <slot />
    </div>
  </aside>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Button } from 'nanocat-ui'

import ConsoleSegmentedTabs from '@/components/ai/ConsoleSegmentedTabs.vue'
import FloatingActionMenu from '@/components/ai/FloatingActionMenu.vue'
import FormSection from '@/components/ai/FormSection.vue'
import MetricStrip from '@/components/ai/MetricStrip.vue'
import RuntimeLogPanel from '@/components/ai/RuntimeLogPanel.vue'
import SurfaceBox from '@/components/ai/SurfaceBox.vue'
import type { RegisterTarget } from '@/api/register'
import type { RegisterMetricItem, RegisterRuntimeLogLine } from '@/views/register/registerProviderView'

const props = defineProps<{
  target: RegisterTarget
  enabled: boolean
  saving: boolean
  exportBusy: boolean
  actionDisabled: boolean
  runtimeHint: string
  metricItems: RegisterMetricItem[]
  runtimeLogLines: RegisterRuntimeLogLine[]
  grokOauthLogLines: RegisterRuntimeLogLine[]
  checkoutLogLines: RegisterRuntimeLogLine[]
}>()

const emit = defineEmits<{
  (e: 'toggle-task'): void
  (e: 'reset-stats'): void
  (e: 'export-grok', format: 'cpa' | 'sub2api'): void
}>()

const resetDisabled = computed(() => props.saving || props.enabled)
const grokExportItems = [
  { key: 'sub2api', label: 'Sub2API 格式 (.json)' },
  { key: 'cpa', label: 'CPA 格式 (.zip)' },
]
type RuntimeLogView = 'register' | 'oauth' | 'checkout'
const activeLog = ref<RuntimeLogView>('register')
const showCheckoutLogTab = computed(() => props.target === 'openai' || props.checkoutLogLines.length > 0)
const showOauthLogTab = computed(() => props.target === 'grok' || props.grokOauthLogLines.length > 0)
const showLogTabs = computed(() => showCheckoutLogTab.value || showOauthLogTab.value)
const logTabs = computed(() => [
  { value: 'register', label: '注册日志' },
  ...(showOauthLogTab.value ? [{ value: 'oauth', label: 'OAuth 日志' }] : []),
  ...(showCheckoutLogTab.value ? [{ value: 'checkout', label: '提链日志' }] : []),
])
const activeLogLines = computed(() => (
  activeLog.value === 'oauth'
    ? props.grokOauthLogLines
    : activeLog.value === 'checkout'
      ? props.checkoutLogLines
      : props.runtimeLogLines
))
const activeLogEmptyTitle = computed(() => {
  if (activeLog.value === 'oauth') return '暂无 OAuth 日志'
  if (activeLog.value === 'checkout') return '暂无提链日志'
  return props.target === 'grok' ? '暂无注册进度' : '暂无注册日志'
})

function setActiveLog(value: string | number) {
  activeLog.value = value === 'oauth' ? 'oauth' : value === 'checkout' ? 'checkout' : 'register'
}

watch([showCheckoutLogTab, showOauthLogTab], ([checkoutVisible, oauthVisible]) => {
  if (activeLog.value === 'checkout' && !checkoutVisible) activeLog.value = 'register'
  if (activeLog.value === 'oauth' && !oauthVisible) activeLog.value = 'register'
})
</script>

<style scoped>
.register-runtime-column {
  display: flex;
  flex-direction: column;
  align-self: stretch;
  min-height: 100%;
  min-width: 0;
  gap: 16px;
}

.register-runtime-section {
  display: grid;
  gap: 12px;
}

.register-runtime-log {
  min-width: 0;
}

.register-runtime-log :deep(.console-segmented-tabs) {
  min-width: 11rem;
}

.register-runtime-log :deep(.ui-segmented-btn) {
  min-height: 28px;
  padding-inline: 10px;
}

.register-runtime-extension {
  display: flex;
  flex: 1;
  min-width: 0;
  min-height: 20rem;
}

.register-runtime-extension :deep(.register-link-tasks) {
  flex: 1;
  width: 100%;
}

.register-runtime-tips {
  display: grid;
  gap: 4px;
  color: hsl(var(--muted-foreground));
  line-height: 1.6;
}

.register-runtime-tips p {
  margin: 0;
}

.register-runtime-actions {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px;
}

.register-runtime-actions--grok {
  grid-template-columns: repeat(3, minmax(0, 1fr));
}

.register-runtime-export-menu {
  width: 100%;
  min-width: 0;
}

.register-runtime-export-menu :deep(.floating-action-menu-trigger) {
  width: 100%;
  min-height: 32px;
}

@media (max-width: 640px) {
  .register-runtime-extension {
    min-height: 16rem;
  }

  .register-runtime-actions,
  .register-runtime-actions--grok {
    grid-template-columns: 1fr;
    justify-content: flex-start;
  }
}
</style>
