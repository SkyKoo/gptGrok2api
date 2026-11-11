<template>
  <section v-if="tasks.length || requests.length || pollError" class="overflow-hidden rounded-xl border border-border" aria-label="Adobe 重新登录进度">
    <button type="button" class="flex w-full flex-wrap items-center justify-between gap-3 bg-muted/10 px-4 py-3 text-left" :aria-expanded="expanded" @click="emit('update:expanded', !expanded)">
      <span class="text-sm font-medium">重新登录进度 <span class="ml-2 text-xs font-normal text-muted-foreground">{{ activeCount }} 个进行中 · {{ tasks.filter(task => task.done).length }} 个已结束</span></span>
      <span class="text-xs text-muted-foreground">{{ expanded ? '收起' : '展开' }}</span>
    </button>
    <div v-if="expanded" class="space-y-3 border-t border-border p-4">
      <p class="text-xs text-muted-foreground">浏览器在后台排队执行；离开或刷新页面后可继续查看进度。</p>
      <p v-if="pollError" role="status" class="text-xs text-amber-600">暂时无法获取最新进度，正在重新连接；保留上次状态，不代表登录失败。</p>
      <p v-for="request in requests" :key="request.request_id" role="status" class="text-xs text-amber-600">{{ request.uncertain ? `启动结果待确认（${request.ids.length} 个账号），正在查询原请求，不会重复提交。` : `正在提交 ${request.ids.length} 个登录任务…` }}</p>
      <div class="max-h-96 divide-y divide-border overflow-y-auto">
        <article v-for="task in tasks" :key="task.progress_id" class="space-y-2 py-3" :aria-label="`${task.email || task.account_ref} 的重新登录进度`">
          <div class="flex flex-wrap items-start justify-between gap-2">
            <div class="min-w-0"><p class="break-all text-sm">{{ task.email || task.account_ref }}</p><p class="mt-1 text-xs text-muted-foreground">{{ stageLabel(task) }} · {{ reloginElapsed(task, now) }}</p></div>
            <button v-if="task.done" type="button" class="text-xs text-muted-foreground" :aria-label="`收起 ${task.email || task.account_ref} 的登录记录`" @click="emit('dismiss', task.progress_id)">收起记录</button>
          </div>
          <p v-if="task.result_unknown" class="text-xs text-amber-600">本次登录结果无法确认，请刷新账号列表核对；没有自动重试。</p>
          <p v-else-if="task.error" role="alert" class="break-words text-xs text-rose-500">{{ adobeReloginError(task.error) }}</p>
          <p v-else-if="task.done && task.session_updated" role="status" class="text-xs text-emerald-600">重新登录完成，媒体会话已确认。</p>
          <p v-if="task.submission_unknown" class="text-xs text-muted-foreground">待核对任务仍保留，未重新提交。</p>
        </article>
      </div>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { reloginElapsed } from '@/lib/accountRelogin'
import type { AdobeReloginTask, AdobeReloginRequest } from '@/lib/adobeRelogin'
import { adobeReloginError } from './adobeAccountView'
const props = defineProps<{ tasks: AdobeReloginTask[]; requests: AdobeReloginRequest[]; pollError: boolean; expanded: boolean; now: number }>()
const activeCount = computed(() => new Set([...props.requests.flatMap(request => request.ids), ...props.tasks.filter(task => !task.done).map(task => task.account_ref)]).size)
function stageLabel(task: AdobeReloginTask) {
  if (task.result_unknown) return '结果待核对'
  return ({ queued: '等待浏览器', running: '浏览器登录中', completed: '媒体会话已更新', failed: '重新登录未完成', interrupted: '登录任务已中断' } as Record<string, string>)[task.stage] || '正在处理'
}
const emit = defineEmits<{ (event: 'update:expanded', value: boolean): void; (event: 'dismiss', id: string): void }>()
</script>
