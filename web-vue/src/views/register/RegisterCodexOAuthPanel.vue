<template>
  <ModalShell :open="Boolean(job)" max-width="42rem" :close-on-backdrop="false">
    <ModalHeader title="补跑 Codex OAuth" subtitle="Web Free 注册完成后，可单独为该账号获取 Codex Token" compact @close="close" />
    <ModalBody density="compact" class="space-y-3">
      <p class="text-sm">账号：{{ job?.email || '已注册账号' }}</p>
      <p class="text-xs text-muted-foreground">授权页面中按上游提示手动完成手机验证码；只有页面要求时才填写 2FA。CFM 暂不接入自动短信平台，也不会保存手机号或验证码。</p>
      <div class="rounded border p-3 text-sm" role="status">
        <p>阶段：{{ stageLabel }}</p>
        <p v-if="message" class="mt-1 text-xs text-muted-foreground break-words">{{ message }}</p>
        <p v-if="error" class="mt-1 text-xs text-red-500 break-words">{{ error }}</p>
      </div>
      <div v-if="authorizeUrl" class="space-y-2">
        <a class="text-xs text-sky-600 break-all" :href="authorizeUrl" target="_blank" rel="noreferrer">打开 Codex 授权页面</a>
        <Button size="xs" variant="outline" :disabled="busy" @click="openAuthorize">重新打开授权页面</Button>
      </div>
      <label class="block text-xs">
        <span class="ui-field-label">Callback URL / Code</span>
        <textarea v-model.trim="callback" rows="4" class="ui-textarea-sm font-mono w-full" placeholder="粘贴最终跳转的 http://localhost:1455/auth/callback?..." :disabled="busy || !sessionId"></textarea>
      </label>
    </ModalBody>
    <ModalFooter align="between" compact>
      <Button size="sm" variant="ghost" :disabled="busy" @click="close">关闭</Button>
      <div class="flex gap-2">
        <Button size="sm" variant="outline" :disabled="busy || !job" @click="start">重新生成授权链接</Button>
        <Button size="sm" variant="primary" :disabled="busy || !sessionId || !callback" @click="finish">完成并同步 CPA</Button>
      </div>
    </ModalFooter>
  </ModalShell>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { Button } from 'nanocat-ui'
import ModalBody from '@/components/ai/ModalBody.vue'
import ModalFooter from '@/components/ai/ModalFooter.vue'
import ModalHeader from '@/components/ai/ModalHeader.vue'
import ModalShell from '@/components/ai/ModalShell.vue'
import { registerApi, type LegacyRegisterConfig } from '@/api/register'

type Job = NonNullable<LegacyRegisterConfig['jobs']>[number]
const props = defineProps<{ job: Job | null }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'changed'): void }>()
const sessionId = ref(''), authorizeUrl = ref(''), callback = ref(''), stage = ref(''), message = ref(''), error = ref('')
const busy = ref(false)
let timer: ReturnType<typeof setInterval> | null = null
const stageLabel = computed(() => ({ waiting_authorization: '等待授权（请在页面内完成手机验证或可选 2FA）', exchanging_token: '交换 Token', syncing_cpa: '同步 CPA', cpa_sync_failed: 'CPA 同步失败（Token 已保存）', done: '已完成', failed: '失败' } as Record<string, string>)[stage.value] || stage.value || '准备中')

watch(() => props.job, (job) => { if (job) { resetFromJob(job); void start() } else stopPolling() }, { immediate: true })
onBeforeUnmount(stopPolling)

function resetFromJob(job: Job) {
  const task = job.codex_oauth
  sessionId.value = ''; authorizeUrl.value = ''; callback.value = ''; stage.value = task?.stage || ''; message.value = task?.message || ''; error.value = task?.error || ''
}
async function start() {
  if (!props.job) return
  busy.value = true; error.value = ''
  try {
    const result = await registerApi.startCodexOAuth(props.job.id)
    sessionId.value = String(result.session_id || '')
    authorizeUrl.value = String(result.authorize_url || '')
    stage.value = String(result.stage || 'waiting_authorization')
    message.value = String(result.message || '')
    if (!sessionId.value || !authorizeUrl.value) throw new Error('后端没有返回完整的 Codex OAuth 授权会话')
    startPolling()
    openAuthorize()
    emit('changed')
  } catch (e: any) { error.value = e?.message || '生成授权链接失败' } finally { busy.value = false }
}
async function finish() {
  if (!props.job || !sessionId.value || !callback.value) return
  busy.value = true; error.value = ''; stopPolling()
  try {
    const result = await registerApi.finishCodexOAuth({ session_id: sessionId.value, callback: callback.value, registration_job_id: props.job.id })
    stage.value = result.ok === false ? 'cpa_sync_failed' : 'done'; message.value = result.message || ''; emit('changed')
  } catch (e: any) { error.value = e?.message || 'Codex OAuth 失败' } finally { busy.value = false }
}
function openAuthorize() { if (authorizeUrl.value) window.open(authorizeUrl.value, '_blank', 'noopener,noreferrer') }
function startPolling() { stopPolling(); timer = setInterval(() => void poll(), 1200); void poll() }
function stopPolling() { if (timer) clearInterval(timer); timer = null }
async function poll() {
  if (!sessionId.value) return
  try { const result = await registerApi.getCodexOAuthProgress(sessionId.value); stage.value = result.stage || stage.value; message.value = result.message || message.value; error.value = result.error || error.value; if (result.status === 'completed' || result.status === 'failed') stopPolling() } catch { /* callback can still be submitted while polling is delayed */ }
}
function close() { if (!busy.value) { stopPolling(); emit('close') } }
</script>
