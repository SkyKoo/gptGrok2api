<template>
  <div class="min-w-[10rem] space-y-2 text-xs" :aria-label="`${item.email || '账号'} 2FA`">
    <div class="flex flex-wrap items-center gap-2">
      <StatusPill :label="item.has_two_factor_secret ? '已配置' : item.twofa_pending_activation ? '待激活 / 待核对' : '未配置'" :tone-class="item.has_two_factor_secret ? 'border-teal-500/30 bg-teal-500/10 text-teal-600' : 'border-border text-muted-foreground'" />
      <Button v-if="item.has_two_factor_secret" size="xs" variant="outline" :disabled="copying || item.is_demo" @click="copySecret">复制密钥</Button>
    </div>
    <template v-if="item.has_two_factor_secret">
      <div v-if="revealed" class="flex flex-wrap items-center gap-2">
        <span class="rounded-full border border-blue-500/30 bg-blue-500/10 px-3 py-1 font-mono text-base font-semibold tracking-widest text-blue-600" aria-label="当前 2FA 验证码">{{ code || '------' }}</span>
        <span class="tabular-nums text-muted-foreground">{{ remaining }} 秒</span>
      </div>
      <div class="flex flex-wrap gap-2">
        <Button size="xs" variant="outline" :disabled="loading || item.is_demo" @click="refresh">{{ loading ? '加载中…' : revealed ? '刷新' : '看验证码' }}</Button>
        <Button v-if="revealed" size="xs" variant="outline" :disabled="loading || copying || !code" @click="copyCode">复制验证码</Button>
        <Button v-if="revealed" size="xs" variant="ghost" @click="hide">隐藏</Button>
      </div>
    </template>
    <p v-if="error" role="alert" class="max-w-56 text-rose-600">{{ error }}</p>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { Button, StatusPill } from 'nanocat-ui'
import { accountsApi, type Account } from '@/api/accounts'
import { useToast } from '@/composables/useToast'

const props = defineProps<{ item: Account }>()
const toast = useToast()
const revealed = ref(false)
const code = ref('')
const remaining = ref(0)
const loading = ref(false)
const copying = ref(false)
const error = ref('')
let deadline = 0
let revision = 0
let timer: ReturnType<typeof setInterval> | undefined

function hide() {
  revision++
  clearInterval(timer)
  timer = undefined
  code.value = ''
  revealed.value = false
  remaining.value = 0
  loading.value = false
  error.value = ''
}

async function refresh() {
  if (loading.value) return
  const request = ++revision
  loading.value = true
  error.value = ''
  clearInterval(timer)
  code.value = ''
  const started = performance.now()
  try {
    const result = await accountsApi.getTwoFACode(props.item.id)
    if (request !== revision) return false
    // Subtract the round trip so slow responses never extend a code's lifetime.
    deadline = started + result.expires_at - result.server_time
    if (deadline <= performance.now()) throw new Error('验证码已过期，请重新刷新')
    code.value = result.code
    revealed.value = true
    tick()
    timer = setInterval(tick, 250)
    return true
  } catch (e) {
    if (request !== revision) return false
    revealed.value = false
    error.value = e instanceof Error ? e.message : '验证码加载失败'
    return false
  } finally {
    if (request === revision) loading.value = false
  }
}

function tick() {
  remaining.value = Math.max(0, Math.ceil((deadline - performance.now()) / 1000))
  if (!remaining.value) {
    code.value = ''
    clearInterval(timer)
    void refresh()
  }
}

async function copyText(value: string) {
  if (navigator.clipboard?.writeText) {
    try { await navigator.clipboard.writeText(value); return } catch { /* Use the HTTP fallback below. */ }
  }
  const input = document.createElement('textarea')
  input.value = value
  input.style.position = 'fixed'
  input.style.opacity = '0'
  document.body.appendChild(input)
  input.select()
  const copied = document.execCommand('copy')
  input.remove()
  if (!copied) throw new Error('复制失败，请重试')
}

async function copyCode() {
  copying.value = true
  try {
    if (!await refresh()) return
    const request = revision
    const value = code.value
    if (deadline <= performance.now() || request !== revision || !value) return
    await copyText(value)
    toast.success('2FA 验证码已复制')
  } catch { toast.error('验证码复制失败，请重试') }
  finally { copying.value = false }
}

async function copySecret() {
  copying.value = true
  const accountId = props.item.id
  try {
    const credentials = await accountsApi.getAccountSecrets(accountId)
    if (accountId !== props.item.id) return
    if (!credentials.two_factor_secret) throw new Error('账号未配置已确认的 2FA 密钥')
    await copyText(credentials.two_factor_secret)
    toast.success('2FA 密钥已复制')
  } catch (e) { toast.error(e instanceof Error ? e.message : '密钥复制失败') }
  finally { copying.value = false }
}

watch(() => [props.item.id, props.item.has_two_factor_secret, props.item.token_preview], hide)
onBeforeUnmount(hide)
</script>
