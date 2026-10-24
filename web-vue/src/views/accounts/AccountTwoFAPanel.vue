<template>
  <section v-if="tasks.length || starting.length || pollError" id="account-twofa-progress" class="overflow-hidden rounded-2xl border border-border" aria-label="2FA 开启进度">
    <button type="button" class="flex w-full items-center justify-between gap-3 bg-muted/10 px-5 py-4 text-left" :aria-expanded="expanded" aria-controls="account-twofa-tasks" @click="emit('update:expanded', !expanded)">
      <span class="text-sm font-medium">2FA 开启进度 <span class="ml-2 text-xs text-muted-foreground">{{ tasks.filter(t => t.status === 'running').length }} 个进行中</span></span>
      <span class="text-xs text-muted-foreground">{{ expanded ? '收起' : '展开' }}</span>
    </button>
    <div v-if="expanded" id="account-twofa-tasks" class="max-h-[28rem] overflow-y-auto border-t border-border px-5">
      <p class="py-3 text-xs text-muted-foreground">HME 邮箱尝试自动取码，也可手动输入邮箱验证码。完成后密钥保存在账号中，可通过“编辑”查看。</p>
      <p v-if="pollError" role="status" class="py-2 text-xs text-amber-600">暂时无法读取进度，正在重连；保留的状态不代表任务失败。</p>
      <p v-if="starting.length" role="status" class="py-2 text-xs text-muted-foreground">正在提交开启任务…</p>
      <article v-for="task in tasks" :key="task.progress_id" class="space-y-3 border-t border-border py-4">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div class="min-w-0">
            <p class="break-all text-sm font-medium">{{ task.email || task.account_ref }}</p>
            <p class="mt-1 text-xs text-muted-foreground">{{ twoFAStageLabels[task.stage] || task.stage }} · {{ statusLabels[task.status] || task.status }}</p>
          </div>
          <div class="flex gap-3 text-xs">
            <button v-if="task.status === 'running'" type="button" class="underline disabled:opacity-40" :disabled="submitting.includes(task.progress_id)" @click="emit('cancel', task)">取消任务</button>
            <button v-else-if="task.status !== 'completed'" type="button" class="underline disabled:opacity-40" :disabled="busyIds.has(task.account_ref)" @click="emit('retry', task.account_ref)">{{ task.pending ? '续跑激活' : '重试' }}</button>
            <button v-if="task.status !== 'running'" type="button" class="text-muted-foreground" @click="emit('dismiss', task.progress_id)">收起记录</button>
          </div>
        </div>
        <p role="status" class="text-xs" :class="task.status === 'completed' ? 'text-emerald-600' : 'text-muted-foreground'">{{ task.message }}</p>
        <p v-if="task.pending && task.status !== 'running'" class="text-xs text-amber-600">待激活密钥已保留。上游结果可能需要核对；续跑会使用原密钥，不重新申请。需要人工处理时请从账号编辑中复制待激活密钥并妥善保存。</p>
        <p v-if="task.error" class="break-all font-mono text-xs text-rose-500">{{ task.error }}</p>
        <form v-if="task.status === 'running' && task.stage === 'waiting_email_code'" class="flex max-w-sm gap-2" @submit.prevent="submitCode(task)">
          <input v-model="codes[task.progress_id]" class="ui-input min-w-0 flex-1" inputmode="numeric" autocomplete="one-time-code" maxlength="6" aria-label="邮箱验证码" placeholder="6 位邮箱验证码" />
          <button type="submit" class="shrink-0 text-xs underline disabled:opacity-40" :disabled="submitting.includes(task.progress_id) || !/^\d{6}$/.test(codes[task.progress_id] || '')">提交验证码</button>
        </form>
      </article>
    </div>
  </section>
</template>

<script setup lang="ts">
import { reactive, watch } from 'vue'
import type { AccountTwoFATask } from '@/api/accounts'
import { twoFAStageLabels } from './accountTwoFARuntime'
const props = defineProps<{ tasks: AccountTwoFATask[]; starting: string[]; expanded: boolean; pollError: boolean; busyIds: Set<string>; submitting: string[] }>()
const codes = reactive<Record<string, string>>({})
const statusLabels: Record<string, string> = { running: '进行中', completed: '成功', failed: '失败', cancelled: '已取消', interrupted: '已中断' }
const emit = defineEmits<{
  (e: 'update:expanded', value: boolean): void
  (e: 'retry', accountId: string): void
  (e: 'dismiss', id: string): void
  (e: 'cancel', task: AccountTwoFATask): void
  (e: 'code', task: AccountTwoFATask, code: string): void
}>()
function submitCode(task: AccountTwoFATask) {
  const code = codes[task.progress_id] || ''
  if (!/^\d{6}$/.test(code)) return
  emit('code', task, code)
  delete codes[task.progress_id]
}
watch(() => props.tasks, tasks => {
  for (const id of Object.keys(codes)) if (!tasks.some(t => t.progress_id === id && t.stage === 'waiting_email_code')) delete codes[id]
})
</script>
