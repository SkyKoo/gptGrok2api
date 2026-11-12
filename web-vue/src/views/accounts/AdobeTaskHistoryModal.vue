<template>
  <ModalShell :open="Boolean(account)" max-width="38rem" :z-index="150">
    <ModalHeader title="Adobe 任务处理记录" @close="emit('close')" />
    <ModalBody class="max-h-[65vh] space-y-4 overflow-y-auto">
      <p class="break-all text-sm font-medium">{{ account?.email }}</p>
      <div v-if="account && processing" class="space-y-2 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3 text-sm leading-relaxed">
        <p class="font-medium">系统处理中，无需手动确认。</p>
        <p>{{ adobeActivityLabel(account) }}</p>
        <p v-if="adobeReconciliationTime(account)" class="text-xs text-muted-foreground">{{ adobeReconciliationTime(account) }}</p>
      </div>
      <p class="text-xs leading-relaxed text-muted-foreground">系统查询原任务并处理本地占用，不会重新生成。结束本地占用不代表 Adobe 已取消任务、退回额度或生成成功；无法确认的结果仍记为未知。</p>
      <p v-if="error" class="text-sm text-rose-500" role="alert">{{ error }}</p>
      <p v-else-if="loading && !records.length" class="text-sm text-muted-foreground">正在读取处理记录…</p>
      <p v-else-if="!records.length" class="text-sm text-muted-foreground">暂无已结束的处理记录，系统处理进度会自动更新。</p>
      <ol v-else class="space-y-3" aria-label="Adobe 任务处理历史">
        <li v-for="record in records" :key="`${record.task_key}:${record.resolved_at}`" class="space-y-2 rounded-lg border border-border p-3 text-sm">
          <div class="flex flex-wrap items-center justify-between gap-2"><span class="font-medium">{{ record.reviewed_by === 'system' ? '系统自动处理' : '管理员处理' }}</span><span class="text-xs text-muted-foreground">{{ time(record.resolved_at) }}</span></div>
          <p>{{ outcome(record.outcome) }}</p>
          <p class="break-words text-xs leading-relaxed text-muted-foreground">{{ record.reason }}</p>
          <p v-if="record.quota_status" class="text-xs text-muted-foreground">额度查询：{{ record.quota_status === 'known' ? '已更新' : record.quota_status === 'error' ? '未完成，后续继续刷新' : '未查询' }}</p>
          <p class="break-all font-mono text-xs text-muted-foreground">任务：{{ record.task_key }}</p>
        </li>
      </ol>
    </ModalBody>
    <ModalFooter>
      <Button size="sm" variant="outline" :disabled="loading" @click="load">刷新记录</Button>
      <Button size="sm" @click="emit('close')">关闭</Button>
    </ModalFooter>
  </ModalShell>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { Button } from 'nanocat-ui'
import { adobeAccountsApi, type AdobeAccount, type AdobeSubmissionResolution } from '@/api/adobeAccounts'
import ModalShell from '@/components/ai/ModalShell.vue'
import ModalHeader from '@/components/ai/ModalHeader.vue'
import ModalBody from '@/components/ai/ModalBody.vue'
import ModalFooter from '@/components/ai/ModalFooter.vue'
import { adobeActivityLabel, adobeReconciliationTime, adobeSubmissionUnknown } from './adobeAccountView'

const props = defineProps<{ account: AdobeAccount | null }>()
const emit = defineEmits<{ close: [] }>()
const records = ref<AdobeSubmissionResolution[]>([]), loading = ref(false), error = ref('')
let sequence = 0
const processing = computed(() => props.account && adobeSubmissionUnknown(props.account) && !records.value.some(record => record.task_key === props.account?.task_key))
function time(value: string) { const date = new Date(value); return Number.isFinite(date.getTime()) ? date.toLocaleString() : '时间未知' }
function outcome(value?: string) {
  return value === 'not_submitted' ? '任务在提交前已停止，本地占用已结束。'
    : value === 'completed' ? '已确认原任务完成，本地占用已结束。'
    : value === 'failed' ? '已确认原任务结束，本地占用已解除。'
      : '本地占用已结束；原任务生成及扣费结果仍未知。'
}
async function load() {
  const account = props.account
  if (!account) return
  const current = ++sequence
  loading.value = true; error.value = ''
  try {
    const response = await adobeAccountsApi.submissionResolutions(account.id)
    if (current === sequence) records.value = response.items
  } catch {
    if (current === sequence) error.value = '处理记录暂时读取失败，系统后台处理仍会继续。'
  } finally { if (current === sequence) loading.value = false }
}
watch(() => props.account, (account, previous) => {
  sequence++
  if (account?.id !== previous?.id) { records.value = []; error.value = ''; loading.value = false }
  if (account) void load()
})
onBeforeUnmount(() => { sequence++ })
</script>
