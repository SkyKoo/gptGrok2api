<template>
  <ModalShell :open="Boolean(job)" max-width="42rem" :close-on-backdrop="false">
    <ModalHeader title="补跑 Codex OAuth" subtitle="Web Free 注册完成后，可单独为该账号获取 Codex Token" compact @close="close" />
    <ModalBody density="compact" class="space-y-3">
      <p class="text-sm">账号：{{ job?.email || '已注册账号' }}</p>
      <p class="text-xs text-muted-foreground">CFM 会自动推进授权流程；只有上游要求时才填写手机号、短信验证码或可选 2FA。手机号和验证码只保留在本次会话中。</p>
      <div class="rounded border p-3 text-sm" role="status">
        <p>阶段：{{ stageLabel }}</p>
        <p v-if="message" class="mt-1 text-xs text-muted-foreground break-words">{{ message }}</p>
        <p v-if="error" class="mt-1 text-xs text-red-500 break-words">{{ error }}</p>
      </div>
      <div v-if="authorizeUrl" class="space-y-2">
        <a class="text-xs text-sky-600 break-all" :href="authorizeUrl" target="_blank" rel="noreferrer">打开 Codex 授权页面</a>
        <Button size="xs" variant="outline" :disabled="busy" @click="openAuthorize">重新打开授权页面</Button>
      </div>
      <label v-if="stage === 'waiting_phone'" class="block text-xs">
        <span class="ui-field-label">手机号</span>
        <input v-model.trim="phone" type="tel" class="ui-input-sm w-full" placeholder="例如 +8613800138000" :disabled="busy || !sessionId" @keyup.enter="submitPhone" />
      </label>
      <label v-else-if="stage === 'waiting_sms_code'" class="block text-xs">
        <span class="ui-field-label">短信验证码</span>
        <input v-model.trim="smsCode" inputmode="numeric" type="text" class="ui-input-sm w-full" placeholder="输入短信验证码" :disabled="busy || !sessionId" @keyup.enter="submitSMS" />
      </label>
      <label v-else-if="stage === 'waiting_2fa'" class="block text-xs">
        <span class="ui-field-label">2FA 验证码</span>
        <input v-model.trim="twoFactorCode" inputmode="numeric" type="text" class="ui-input-sm w-full" placeholder="输入一次性验证码" :disabled="busy || !sessionId" @keyup.enter="submit2FA" />
      </label>
      <label v-else-if="stage === 'waiting_authorization' && authorizeUrl" class="block text-xs">
        <span class="ui-field-label">Callback URL / Code（兼容兜底）</span>
        <textarea v-model.trim="callback" rows="3" class="ui-textarea-sm font-mono w-full" placeholder="粘贴最终跳转的 http://localhost:1455/auth/callback?..." :disabled="busy || !sessionId"></textarea>
      </label>
    </ModalBody>
    <ModalFooter align="between" compact>
      <Button size="sm" variant="ghost" :disabled="busy" @click="close">关闭</Button>
      <div class="flex gap-2">
        <Button size="sm" variant="outline" :disabled="busy || !job" @click="start">重新生成授权链接</Button>
        <Button v-if="stage === 'waiting_phone'" size="sm" variant="primary" :disabled="busy || !sessionId || !phone" @click="submitPhone">提交手机号</Button>
        <Button v-else-if="stage === 'waiting_sms_code'" size="sm" variant="primary" :disabled="busy || !sessionId || !smsCode" @click="submitSMS">验证短信验证码</Button>
        <Button v-else-if="stage === 'waiting_2fa'" size="sm" variant="primary" :disabled="busy || !sessionId || !twoFactorCode" @click="submit2FA">提交 2FA</Button>
        <Button v-else size="sm" variant="primary" :disabled="busy || !sessionId || !callback" @click="finish">提交回调并保存</Button>
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
const sessionId = ref(''), authorizeUrl = ref(''), callback = ref(''), phone = ref(''), smsCode = ref(''), twoFactorCode = ref(''), stage = ref(''), message = ref(''), error = ref('')
const busy = ref(false)
let timer: ReturnType<typeof setInterval> | null = null
const stageLabel = computed(() => ({ starting: '正在进入授权', waiting_authorization: '等待上游授权', waiting_phone: '等待输入手机号', waiting_sms_code: '等待输入短信验证码', waiting_2fa: '等待输入 2FA', waiting_email_code: '等待邮箱验证码（暂未接入）', exchanging_token: '交换 Token', syncing_cpa: '同步 CPA', cpa_sync_failed: 'CPA 同步失败（Token 已保存）', done: '已完成', failed: '失败' } as Record<string, string>)[stage.value] || stage.value || '准备中')

watch(() => props.job, (job) => { if (job) { resetFromJob(job); void start() } else stopPolling() }, { immediate: true })
onBeforeUnmount(stopPolling)

function resetFromJob(job: Job) {
  const task = job.codex_oauth
  sessionId.value = ''; authorizeUrl.value = ''; callback.value = ''; phone.value = ''; smsCode.value = ''; twoFactorCode.value = ''; stage.value = task?.stage || ''; message.value = task?.message || ''; error.value = task?.error || ''
}
async function submitInput(kind: 'phone' | 'sms-code' | '2fa', value: string) {
  if (!sessionId.value || !value.trim()) return
  busy.value = true; error.value = ''; stopPolling()
  try {
    const payload = { session_id: sessionId.value, value: value.trim(), registration_job_id: props.job?.id }
    const result = kind === 'phone' ? await registerApi.submitCodexPhone(payload) : kind === 'sms-code' ? await registerApi.submitCodexSMSCode(payload) : await registerApi.submitCodex2FA(payload)
    stage.value = result.stage || stage.value; message.value = result.message || ''; if (stage.value !== 'done') startPolling()
    if (kind === 'phone') phone.value = ''; if (kind === 'sms-code') smsCode.value = ''; if (kind === '2fa') twoFactorCode.value = ''
    emit('changed')
  } catch (e: any) { error.value = e?.message || '提交验证信息失败' } finally { busy.value = false }
}
function submitPhone() { return submitInput('phone', phone.value) }
function submitSMS() { return submitInput('sms-code', smsCode.value) }
function submit2FA() { return submitInput('2fa', twoFactorCode.value) }
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
    if (stage.value === 'waiting_authorization') openAuthorize()
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
