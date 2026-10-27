<template>
  <div class="space-y-6">
    <PagePanel class="space-y-5">
      <PanelHeader title="iCloud 邮箱" align="start">
        <template #copy>
          <p class="mt-1 text-xs text-muted-foreground">统一管理 iCloud 邮箱资产，以及 GPT 和 Grok 的占用、释放和历史操作。</p>
        </template>
        <template #actions>
          <StateBadge :tone="bridgeTone" shape="rounded">{{ bridgeLabel }}</StateBadge>
          <Button size="sm" variant="outline" :disabled="loading" @click="refreshAll">{{ loading ? '刷新中...' : '刷新' }}</Button>
          <Button size="sm" variant="primary" :disabled="reconciling || !bridge?.reachable" @click="reconcile">
            {{ reconciling ? '核对中...' : '核对现有邮箱' }}
          </Button>
        </template>
      </PanelHeader>

      <StateBlock v-if="pageError" compact dashed title="iCloud 邮箱模块暂不可用" :description="pageError" />

      <template v-if="bridge?.reachable">
        <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
          <div v-for="metric in metrics" :key="metric.label" class="rounded-lg border border-border bg-muted/20 px-3 py-3">
            <p class="text-xs text-muted-foreground">{{ metric.label }}</p>
            <p class="mt-1 text-xl font-semibold tabular-nums text-foreground">{{ metric.value }}</p>
          </div>
        </div>

        <div class="rounded-lg border border-border bg-muted/20 px-3 py-3 text-xs leading-5 text-muted-foreground">
          邮箱由独立的 iCloud 邮箱上游提供。一个邮箱可以同时被 GPT 和 Grok 使用；删除账号时只释放对应平台的占用，最后一个占用释放后才会删除上游邮箱。
          <span v-if="!bridge.hme_configured" class="text-amber-700 dark:text-amber-300">当前尚未配置 iCloud 邮箱上游。</span>
        </div>

        <FormSection title="邮箱来源" density="roomy">
          <template #actions>
            <span class="text-xs text-muted-foreground">注册流程只选择来源，配置在这里维护</span>
          </template>
          <div v-if="sources.length" class="space-y-3">
            <div v-for="source in sources" :key="source.id" class="rounded-lg border border-border bg-background p-3">
              <div class="flex flex-wrap items-start justify-between gap-3">
                <div class="min-w-0">
                  <p class="font-medium text-foreground">{{ source.label || source.id || '未命名来源' }}</p>
                  <p class="mt-1 text-xs text-muted-foreground">{{ sourceTypeLabel(source) }} · {{ sourceStatusLabel(source) }}</p>
                </div>
                <div class="flex items-center gap-2">
                  <MetaChip size="xs" :tone="sourceUsable(source) ? 'success' : 'muted'">{{ sourceUsable(source) ? '可用于注册' : '仅已迁移配置' }}</MetaChip>
                  <label class="flex items-center gap-1.5 text-xs text-muted-foreground">
                    <input v-model="sourceDraft(source).enable" type="checkbox" :disabled="sourceSaving === source.id" />
                    启用
                  </label>
                </div>
              </div>

              <div v-if="source.type === 'icloud_hme'" class="mt-3 grid gap-3 md:grid-cols-2">
                <label class="text-xs md:col-span-2">
                  <span class="ui-field-label">来源名称</span>
                  <Input v-model="sourceDraft(source).label" block :disabled="sourceSaving === source.id" />
                </label>
                <label class="text-xs">
                  <span class="ui-field-label">API Base</span>
                  <Input v-model="sourceDraft(source).config.api_base" block placeholder="http://hme:8081" :disabled="sourceSaving === source.id" />
                </label>
                <label class="text-xs">
                  <span class="ui-field-label">上游账号 ID</span>
                  <Input v-model="sourceDraft(source).config.account_id" block :disabled="sourceSaving === source.id" />
                </label>
                <label class="text-xs">
                  <span class="ui-field-label">管理员密码</span>
                  <Input v-model="sourceDraft(source).config.admin_password" type="password" block :placeholder="source.config?.admin_password_set ? '已保存，留空保持不变' : '请输入管理员密码'" :disabled="sourceSaving === source.id" />
                </label>
                <label class="flex items-center gap-2 self-end pb-2 text-xs text-muted-foreground">
                  <input v-model="sourceDraft(source).config.delete_hme_mailbox_on_account_delete" type="checkbox" :disabled="sourceSaving === source.id" />
                  删除账号时同步删除上游邮箱
                </label>
              </div>
              <p v-else class="mt-3 text-xs leading-5 text-muted-foreground">配置已迁移到邮箱资产管理模块，当前注册适配器尚未接入此来源。</p>
              <div class="mt-3 flex justify-end">
                <Button size="sm" variant="outline" :disabled="sourceSaving === source.id" @click="saveSource(source)">{{ sourceSaving === source.id ? '保存中...' : '保存来源' }}</Button>
              </div>
            </div>
          </div>
          <p v-else class="text-xs text-muted-foreground">暂无邮箱来源。请先完成来源迁移或添加 iCloud 邮箱上游。</p>
        </FormSection>

        <FormSection title="邮箱资产" density="roomy">
          <template #actions>
            <span class="text-xs text-muted-foreground">共 {{ mailboxTotal }} 个</span>
          </template>
          <div class="mb-4 flex flex-wrap items-end justify-between gap-3 border-b border-border pb-3">
            <label class="min-w-0 flex-1 text-xs sm:max-w-md">
              <span class="ui-field-label">搜索邮箱</span>
              <Input v-model="search" block placeholder="邮箱地址或来源" @keyup.enter="searchMailboxes" />
            </label>
            <div class="flex items-center gap-2">
              <Button size="sm" variant="outline" @click="searchMailboxes">搜索</Button>
              <span class="text-xs tabular-nums text-muted-foreground">第 {{ page }} / {{ totalPages }} 页</span>
              <Button size="xs" variant="outline" :disabled="page <= 1" @click="changePage(-1)">上一页</Button>
              <Button size="xs" variant="outline" :disabled="page >= totalPages" @click="changePage(1)">下一页</Button>
            </div>
          </div>

          <div v-if="mailboxes.length" class="grid min-w-0 gap-3 lg:grid-cols-2">
            <div v-for="mailbox in mailboxes" :key="mailbox.id" class="min-w-0 rounded-lg border border-border bg-background p-3">
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <p class="truncate font-mono text-sm font-medium text-foreground">{{ mailbox.email || '-' }}</p>
                  <p class="mt-1 truncate text-xs text-muted-foreground">来源：{{ mailbox.source_type || 'iCloud 邮箱上游' }} · {{ mailbox.status || 'unknown' }}</p>
                </div>
                <MetaChip size="xs" :tone="mailboxTone(mailbox)">{{ mailboxLabel(mailbox) }}</MetaChip>
              </div>
              <div class="mt-3 flex flex-wrap gap-1.5">
                <MetaChip size="xs" :tone="mailbox.openai_claimed ? 'success' : 'muted'">GPT {{ mailbox.openai_claimed ? '已占用' : '可用' }}</MetaChip>
                <MetaChip size="xs" :tone="mailbox.grok_claimed ? 'info' : 'muted'">Grok {{ mailbox.grok_claimed ? '已占用' : '可用' }}</MetaChip>
              </div>
              <p class="mt-3 break-all text-[11px] text-muted-foreground">资产 ID：{{ mailbox.id || '-' }}</p>
              <p v-if="mailbox.last_error" class="mt-1 break-words text-xs text-amber-700 dark:text-amber-300">{{ mailbox.last_error }}</p>
            </div>
          </div>
          <p v-else class="rounded-lg border border-dashed border-border px-3 py-5 text-center text-xs text-muted-foreground">暂无邮箱资产。点击“核对现有邮箱”导入上游邮箱并关联历史账号。</p>
        </FormSection>

        <FormSection title="操作日志" density="roomy">
          <template #actions>
            <Button size="xs" variant="outline" @click="loadEvents">刷新日志</Button>
          </template>
          <div v-if="events.length" class="max-h-80 space-y-2 overflow-auto">
            <div v-for="event in events" :key="event.id" class="rounded-md border border-border bg-muted/20 px-3 py-2 text-xs">
              <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
                <span class="font-medium text-foreground">{{ event.event_type || 'mailbox_event' }}</span>
                <MetaChip size="xs" :tone="event.platform === 'grok' ? 'info' : event.platform === 'openai' ? 'success' : 'muted'">{{ event.platform || '系统' }}</MetaChip>
                <span class="text-muted-foreground">{{ formatDate(event.created_at) }}</span>
              </div>
              <p class="mt-1 break-all text-muted-foreground">邮箱 {{ event.mailbox_id || '-' }} · 结果 {{ event.result || '-' }}<span v-if="event.error"> · {{ event.error }}</span></p>
            </div>
          </div>
          <p v-else class="text-xs text-muted-foreground">暂无操作日志。</p>
        </FormSection>

        <p v-if="notice" class="break-words rounded-lg border border-sky-200 bg-sky-50 px-3 py-2 text-xs text-sky-800 dark:border-sky-900/50 dark:bg-sky-950/30 dark:text-sky-200">{{ notice }}</p>
      </template>
      <p v-else class="rounded-lg border border-dashed border-border px-3 py-5 text-center text-xs text-muted-foreground">请先配置可访问的 PostgreSQL 邮箱资产数据库和 iCloud 邮箱上游。</p>
    </PagePanel>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { Button, Input } from 'nanocat-ui'
import { icloudApi, type ICloudBridgeStatus, type ICloudMailbox, type ICloudMailboxEvent, type ICloudMailboxSource } from '@/api/icloud'
import FormSection from '@/components/ai/FormSection.vue'
import MetaChip from '@/components/ai/MetaChip.vue'
import PagePanel from '@/components/ai/PagePanel.vue'
import PanelHeader from '@/components/ai/PanelHeader.vue'
import StateBadge from '@/components/ai/StateBadge.vue'
import StateBlock from '@/components/ai/StateBlock.vue'

defineOptions({ name: 'ICloudPrivacyMail' })

const loading = ref(false)
const reconciling = ref(false)
const pageError = ref('')
const notice = ref('')
const bridge = ref<ICloudBridgeStatus | null>(null)
const counts = ref<Record<string, number>>({})
const sources = ref<ICloudMailboxSource[]>([])
const sourceDrafts = ref<Record<string, ICloudMailboxSource>>({})
const sourceSaving = ref('')
const mailboxes = ref<ICloudMailbox[]>([])
const events = ref<ICloudMailboxEvent[]>([])
const search = ref('')
const page = ref(1)
const pageSize = 20
const mailboxTotal = ref(0)
const totalPages = ref(1)

function sourceTypeLabel(source: ICloudMailboxSource) {
  return source.type === 'icloud_hme' ? 'iCloud 邮箱上游' : String(source.type || '未接入来源')
}

function sourceStatusLabel(source: ICloudMailboxSource) {
  if (source.enable === false) return '已停用'
  return source.status === 'available' ? '可用' : '仅已迁移配置'
}

function sourceUsable(source: ICloudMailboxSource) {
  return source.enable !== false && source.capabilities?.openai === true && source.capabilities?.grok === true
}

function sourceDraft(source: ICloudMailboxSource) {
  const id = String(source.id || '')
  if (!sourceDrafts.value[id]) {
    sourceDrafts.value[id] = {
      ...source,
      config: { ...(source.config || {}) },
    }
  }
  return sourceDrafts.value[id]
}

async function loadSources() {
  const result = await icloudApi.listSources()
  sources.value = result.sources || []
  const validIDs = new Set(sources.value.map(source => String(source.id || '')))
  sourceDrafts.value = Object.fromEntries(Object.entries(sourceDrafts.value).filter(([id]) => validIDs.has(id)))
  for (const source of sources.value) sourceDraft(source)
}

async function saveSource(source: ICloudMailboxSource) {
  const id = String(source.id || '')
  if (!id) return
  sourceSaving.value = id
  notice.value = ''
  try {
    const result = await icloudApi.updateSource(sourceDraft(source))
    const updated = result.source
    if (!updated) throw new Error('来源保存失败')
    sources.value = sources.value.map(item => item.id === updated.id ? updated : item)
    sourceDrafts.value[id] = { ...updated, config: { ...(updated.config || {}) } }
    notice.value = `来源“${updated.label || id}”已保存。`
  } catch (error) {
    pageError.value = error instanceof Error ? error.message : String(error || '来源保存失败')
  } finally {
    sourceSaving.value = ''
  }
}

const bridgeTone = computed(() => bridge.value?.reachable ? 'success' : bridge.value ? 'danger' : 'muted')
const bridgeLabel = computed(() => bridge.value?.reachable ? '模块在线' : bridge.value ? '模块离线' : '检查中')
const metrics = computed(() => [
  { label: '邮箱总数', value: counts.value.mailboxes || 0 },
  { label: '可用邮箱', value: counts.value.available || 0 },
  { label: '使用中', value: counts.value.active || 0 },
  { label: '待清理', value: counts.value.cleanup_pending || 0 },
  { label: '操作事件', value: counts.value.events || 0 },
])

function formatDate(value?: string) {
  if (!value) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function mailboxLabel(mailbox: ICloudMailbox) {
  if (mailbox.status === 'cleanup_pending') return '待清理'
  if (mailbox.status === 'deleted') return '已删除'
  if (mailbox.openai_claimed && mailbox.grok_claimed) return '两平台均占用'
  if (mailbox.openai_claimed || mailbox.grok_claimed) return '部分占用'
  return '可用'
}

function mailboxTone(mailbox: ICloudMailbox): 'success' | 'info' | 'warning' | 'danger' | 'muted' {
  if (mailbox.status === 'cleanup_pending') return 'warning'
  if (mailbox.status === 'deleted') return 'danger'
  if (mailbox.openai_claimed && mailbox.grok_claimed) return 'warning'
  if (mailbox.openai_claimed || mailbox.grok_claimed) return 'info'
  return 'success'
}

async function loadMailboxes() {
  const result = await icloudApi.listMailboxes({ page: page.value, page_size: pageSize, search: search.value.trim() || undefined })
  mailboxes.value = result.mailboxes || []
  mailboxTotal.value = Number(result.pagination?.total_all ?? result.pagination?.total ?? mailboxes.value.length)
  totalPages.value = Math.max(1, Number(result.pagination?.total_pages || 1))
  if (page.value > totalPages.value) page.value = totalPages.value
}

async function loadEvents() {
  const result = await icloudApi.listEvents({ page: 1, page_size: 50 })
  events.value = result.events || []
}

async function refreshAll() {
  loading.value = true
  pageError.value = ''
  try {
    bridge.value = await icloudApi.bridgeStatus()
    if (!bridge.value.reachable) {
      pageError.value = bridge.value.error || '邮箱资产数据库不可用，请检查 GO_DATABASE_URL 和 PostgreSQL 服务。'
      return
    }
    const status = await icloudApi.status()
    counts.value = (status as { counts?: Record<string, number> }).counts || {}
    await Promise.all([loadSources(), loadMailboxes(), loadEvents()])
  } catch (error) {
    pageError.value = error instanceof Error ? error.message : String(error || '请求失败')
  } finally {
    loading.value = false
  }
}

async function reconcile() {
  reconciling.value = true
  notice.value = ''
  pageError.value = ''
  try {
    const result = await icloudApi.reconcile()
    if (!result.success) throw new Error((result.errors || []).join('；') || '邮箱核对失败')
    notice.value = `核对完成：发现 ${result.aliases || 0} 个上游邮箱，关联 GPT ${result.openai_claims || 0} 个、Grok ${result.grok_claims || 0} 个。`
    await refreshAll()
  } catch (error) {
    pageError.value = error instanceof Error ? error.message : String(error || '核对失败')
  } finally {
    reconciling.value = false
  }
}

async function searchMailboxes() {
  page.value = 1
  try {
    await loadMailboxes()
  } catch (error) {
    pageError.value = error instanceof Error ? error.message : String(error || '搜索失败')
  }
}

async function changePage(offset: number) {
  const next = Math.min(totalPages.value, Math.max(1, page.value + offset))
  if (next === page.value) return
  page.value = next
  await loadMailboxes()
}

onMounted(refreshAll)
</script>
