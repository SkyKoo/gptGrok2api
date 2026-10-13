<template>
  <section v-if="tasks.length || starting.length || pollError" id="account-relogin-progress" class="overflow-hidden rounded-2xl border border-border" aria-label="重新登录进度">
    <button type="button" class="flex w-full flex-wrap items-center justify-between gap-3 bg-muted/10 px-5 py-4 text-left" :aria-expanded="expanded" aria-controls="account-relogin-tasks" @click="emit('update:expanded', !expanded)">
      <span class="text-sm font-medium">重新登录进度 <span class="ml-2 text-xs font-normal text-muted-foreground">{{ running }} 个进行中 · {{ tasks.filter(t => t.done).length }} 个已结束</span></span>
      <span class="text-xs text-muted-foreground">{{ expanded ? '收起' : '展开' }}</span>
    </button>
    <div v-if="expanded" id="account-relogin-tasks" class="border-t border-border">
      <p class="px-5 pt-4 text-xs text-muted-foreground">各账号独立执行；离开或刷新页面后可继续查看。结束后可手动收起记录。</p>
      <p v-if="pollError" role="status" class="mx-5 mt-3 rounded-lg bg-amber-500/10 px-3 py-2 text-xs text-amber-600">暂时无法获取最新进度，正在重新连接。下方保留上次状态，不代表登录失败。</p>
      <p v-if="starting.length" role="status" class="px-5 pt-3 text-sm text-muted-foreground">正在提交 {{ starting.length }} 个登录任务…</p>
      <div class="max-h-[32rem] overflow-y-auto divide-y divide-border px-5">
        <article v-for="task in tasks" :key="task.progress_id" class="space-y-3 py-4" :aria-label="task.email || task.account_ref">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div class="min-w-0 space-y-1">
              <p class="break-all text-sm font-medium">{{ task.email || task.account_ref }}<span v-if="task.validation_only" class="ml-2 text-xs font-normal text-muted-foreground">会话校验重试 {{ task.validation_attempt || 0 }}/3</span><span v-if="task.automatic" class="ml-2 text-xs font-normal text-muted-foreground">自动恢复</span></p>
              <p class="text-xs text-muted-foreground">{{ reloginStageLabel(task.failed_stage || task.stage) }} · {{ task.done ? '耗时' : '已用时' }} {{ reloginElapsed(task, now) }}</p>
            </div>
            <div class="flex shrink-0 items-center gap-3">
              <span class="rounded-full border px-2.5 py-1 text-xs" :class="task.unavailable ? 'border-amber-500/30 text-amber-600' : task.error ? 'border-rose-500/30 text-rose-500' : task.done ? 'border-emerald-500/30 text-emerald-600' : 'border-sky-500/30 text-sky-500'">{{ task.unavailable ? '结果待核对' : task.error ? (task.next_validation_at ? '等待校验重试' : task.session_updated ? '校验未通过' : '失败') : task.done ? '成功' : '进行中' }}</span>
              <button v-if="task.done && task.error && !task.unavailable && !task.session_updated" type="button" class="text-xs text-foreground underline underline-offset-4 disabled:opacity-40" :disabled="busyIds.has(task.account_ref)" @click="emit('retry', task.account_ref)">{{ busyIds.has(task.account_ref) ? '重试中' : '重试' }}</button>
              <button v-if="task.done" type="button" class="text-xs text-muted-foreground hover:text-foreground" :aria-label="`收起 ${task.email || task.account_ref} 的登录记录`" @click="emit('dismiss', task.progress_id)">收起记录</button>
            </div>
          </div>
          <ol v-if="!task.unavailable && !task.validation_only" class="flex flex-wrap gap-x-4 gap-y-2" aria-label="登录步骤">
            <li v-for="(step, index) in reloginSteps" :key="step" class="flex items-center gap-1.5 text-xs" :class="index === reloginStep(task) ? (task.error ? 'text-rose-500' : 'text-sky-500') : 'text-muted-foreground'" :aria-current="index === reloginStep(task) ? 'step' : undefined">
              <span class="h-1.5 w-1.5 rounded-full" :class="index < reloginStep(task) ? 'bg-emerald-500' : index === reloginStep(task) ? (task.error ? 'bg-rose-500' : 'bg-sky-500 animate-pulse') : 'bg-muted-foreground/30'"></span>{{ step }}
            </li>
          </ol>
          <div v-if="task.error || task.unavailable" class="space-y-2 rounded-lg bg-rose-500/5 px-3 py-2 text-xs">
            <p :class="task.unavailable ? 'text-muted-foreground' : 'text-rose-500'">{{ reloginErrorText(task) }}</p>
            <details v-if="task.error && !task.unavailable" class="text-muted-foreground"><summary class="cursor-pointer">错误详情</summary><p class="mt-2 break-words font-mono">{{ task.error }}</p><p v-if="task.diagnostic" class="mt-1 break-words">阶段：{{ task.diagnostic.stage }} · 接口：{{ task.diagnostic.endpoint || '—' }} · HTTP：{{ task.diagnostic.status || '未收到响应' }} · 类型：{{ task.diagnostic.kind }}</p></details>
          </div>
          <p v-if="task.next_validation_at" role="status" class="text-xs text-amber-600">下一次校验：{{ formatTime(task.next_validation_at) }} · {{ validationCountdown(task.next_validation_at, now) }}；最多自动重试 3 次。</p>
          <p v-if="task.done && !task.error && !task.unavailable" class="text-xs text-emerald-600">会话已更新<span v-if="task.quotas_refreshed">，用户信息和额度已刷新</span><span v-if="task.token_expires_at"> · 新凭据到期时间：{{ formatTime(task.token_expires_at) }}</span></p>
          <p v-else-if="task.stage === 'wait_code'" class="text-xs text-muted-foreground">正在自动读取邮箱验证码，无需在此页面输入。</p>
        </article>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { reloginSteps, reloginStep, reloginStageLabel, reloginElapsed, reloginErrorText, type AccountReloginTask } from '@/lib/accountRelogin'
const props = defineProps<{
  tasks: AccountReloginTask[]
  starting: string[]
  busyIds: Set<string>
  pollError: boolean
  expanded: boolean
  now: number
}>()
const running = computed(() => props.busyIds.size)
const formatTime = (value: string) => new Date(value).toLocaleString()
const validationCountdown = (value: string, now: number) => {
  const seconds = Math.ceil((Date.parse(value) - now) / 1000)
  return seconds > 0 ? `约 ${seconds} 秒后` : '等待后台执行'
}
const emit = defineEmits<{
  (event: 'update:expanded', value: boolean): void
  (event: 'retry', account: string): void
  (event: 'dismiss', id: string): void
}>()
</script>
