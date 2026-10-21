<template>
  <section class="register-free-jobs">
    <p class="text-xs text-muted-foreground mb-3">已有邮箱遇到超时或临时网络错误后，会在注册空闲时自动补偿，最多 3 次；无需保持页面打开。</p>
    <p v-if="compensationError" class="text-xs text-amber-600 mb-3">自动补偿暂缓：请检查已保存的批次和邮箱来源配置。</p>
    <EmptyState v-if="!visibleJobs.length" plain title="暂无注册任务" description="启动注册后会在这里显示任务记录。" />
    <div v-for="job in visibleJobs" :key="job.id" class="border-b py-3 space-y-1 text-sm break-words">
      <p>{{ job.email || '等待分配邮箱' }} · {{ labels[job.status] || job.status }}</p>
      <p class="text-xs text-muted-foreground">{{ stages[job.stage] || job.stage }} · 入库{{ job.imported ? '完成' : '未完成' }} · 校验{{ job.verified ? '通过' : '未通过' }}</p>
      <p v-if="job.twofa" class="text-xs" :class="job.twofa.status === 'completed' ? 'text-teal-600' : 'text-amber-600'" role="status">
        2FA · {{ twoFALabels[job.twofa.status] || job.twofa.status }} · {{ job.twofa.message }}
        <span v-if="job.twofa.pending">（待激活密钥已保留）</span>
        <span v-if="job.twofa.error"> · {{ job.twofa.error }}</span>
      </p>
      <p v-if="job.error" class="text-xs break-all">{{ job.error }}</p>
      <p v-if="registrationCompensationText(job.auto_retry, running, now)" class="text-xs text-amber-600" role="status">{{ registrationCompensationText(job.auto_retry, running, now) }}</p>
      <Button v-if="job.can_retry_registration" size="sm" variant="outline" :disabled="running || busy" @click="retryRegistration(job.id)">复用此邮箱重试注册</Button>
      <Button v-if="job.can_retry" size="sm" variant="outline" :disabled="running || busy" @click="retry(job.id)">重试入库 / 校验</Button>
    </div>
    <div v-if="totalCount > pageSize" class="flex flex-wrap items-center justify-between gap-3 pt-4 text-sm">
      <span class="text-muted-foreground">每页 {{ pageSize }} 条 · 共 {{ totalCount }} 条</span>
      <div class="flex items-center gap-2">
        <Button size="sm" variant="outline" :disabled="loading || page <= 1" @click="loadPage(page - 1)">上一页</Button>
        <span class="tabular-nums">{{ page }} / {{ pageCount }}</span>
        <Button size="sm" variant="outline" :disabled="loading || page >= pageCount" @click="loadPage(page + 1)">下一页</Button>
      </div>
    </div>
    <p v-if="loadError" class="text-sm text-red-500">{{ loadError }}</p>
    <p v-if="error" class="text-sm text-red-500">{{ error }}</p>
  </section>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { Button, EmptyState } from 'nanocat-ui'
import { registerApi, type LegacyRegisterConfig } from '@/api/register'
import { registrationCompensationText } from '@/lib/registrationCompensation'
const props = defineProps<{ jobs: LegacyRegisterConfig['jobs']; total?: number; pageSize?: number; running: boolean; compensationError?: string }>()
const emit = defineEmits<{ (e: 'changed'): void }>()
const now = ref(Date.now())
const clock = setInterval(() => { now.value = Date.now() }, 1000)
const busy = ref(false), error = ref('')
const page = ref(1), loading = ref(false), loadError = ref('')
const historyJobs = ref<NonNullable<LegacyRegisterConfig['jobs']>>([])
const pageSize = computed(() => props.pageSize || 50)
const totalCount = computed(() => props.total ?? props.jobs?.length ?? 0)
const pageCount = computed(() => Math.max(1, Math.ceil(totalCount.value / pageSize.value)))
const visibleJobs = computed(() => [...(page.value === 1 ? (props.jobs || []) : historyJobs.value)].reverse())
let loadSequence = 0
async function loadPage(next: number) {
  const sequence = ++loadSequence
  loadError.value = ''
  if (next <= 1) {
    page.value = 1; historyJobs.value = []; loading.value = false
    return
  }
  loading.value = true
  try {
    const result = await registerApi.getFreeJobs(next)
    if (sequence !== loadSequence) return
    historyJobs.value = result.jobs
    page.value = result.jobs_page
  } catch (e: any) {
    if (sequence === loadSequence) loadError.value = e?.message || '历史记录加载失败'
  } finally {
    if (sequence === loadSequence) loading.value = false
  }
}
// SSE carries only the newest page. Refresh a viewed older page separately so
// retry progress remains visible without downloading the full history.
watch(() => props.jobs, () => {
  if (page.value > 1 && !loading.value) void loadPage(Math.min(page.value, pageCount.value))
})
onBeforeUnmount(() => { loadSequence++; clearInterval(clock) })
const labels: Record<string, string> = { queued: '等待执行', running: '进行中', completed: '已完成', failed: '失败', cancelled: '已取消', interrupted: '已中断，需核查', registration_pending: '邮箱已创建，等待补偿注册', import_pending: '等待入库', verification_pending: '等待校验' }
const twoFALabels: Record<string, string> = { running: '进行中', completed: '已开启', failed: '失败，需补充开启', interrupted: '已中断，需核对', cancelled: '已取消' }
const stages: Record<string, string> = { twofa_reauth: '2FA 邮箱重认证', twofa_waiting_email_code: '2FA 等待邮箱验证码', twofa_verifying_email_code: '2FA 验证邮箱', twofa_enroll: '2FA 申请密钥', twofa_activate: '2FA 激活', twofa_completed: '2FA 已开启', queued: '等待执行', mailbox: '创建邮箱别名', compensation: '准备复用邮箱', authorize: '初始化会话', submit_email: '提交邮箱', send_code: '发送验证码', wait_code: '等待验证码', validate_code: '验证邮箱', create_profile: '创建账号资料', session: '获取会话凭据', import: '导入账号', verify: '校验账号', completed: '账号可用' }
async function retry(id: string) {
  busy.value = true; error.value = ''
  try { await registerApi.retryFreeResult(id); emit('changed'); if (page.value > 1) await loadPage(page.value) }
  catch (e: any) { error.value = e?.message || '重试失败' }
  finally { busy.value = false }
}
async function retryRegistration(id: string) {
  busy.value = true; error.value = ''
  try { await registerApi.retryFreeRegistration(id); emit('changed'); if (page.value > 1) await loadPage(page.value) }
  catch (e: any) { error.value = e?.message || '补偿注册失败' }
  finally { busy.value = false }
}
</script>
