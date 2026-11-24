<template>
  <section v-if="rows.length || starting.size || pollError" class="overflow-hidden rounded-2xl border border-border" aria-label="Grok 重新登录进度">
    <button type="button" class="flex w-full flex-wrap items-center justify-between gap-3 bg-muted/10 px-5 py-4 text-left" :aria-expanded="expanded" aria-controls="grok-relogin-tasks" @click="expanded = !expanded">
      <span class="text-sm font-medium">重新登录进度 <span class="ml-2 text-xs font-normal text-muted-foreground">{{ running }} 个进行中 · {{ ended }} 个已结束</span></span>
      <span class="text-xs text-muted-foreground">{{ expanded ? '收起' : '展开' }}</span>
    </button>
    <div v-if="expanded" id="grok-relogin-tasks" class="border-t border-border">
      <p v-if="pollError" role="status" class="px-5 pt-3 text-xs text-amber-600">进度暂时无法更新，正在重新连接。</p>
      <p v-if="starting.size" role="status" class="px-5 pt-3 text-xs text-muted-foreground">正在确认 {{ starting.size }} 个账号的重登任务…</p>
      <div class="max-h-[28rem] divide-y divide-border overflow-y-auto px-5">
        <article v-for="row in rows" :key="row.job.id" class="space-y-3 py-4" :aria-label="row.job.email || row.job.payload.account_id">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div class="min-w-0 space-y-1">
              <p class="break-all text-sm font-medium">{{ row.job.email || row.job.payload.account_id }}</p>
              <p class="text-xs text-muted-foreground">{{ row.stageLabel }} · {{ row.active ? '已用时' : '耗时' }} {{ elapsed(row.job) }}</p>
            </div>
            <div class="flex shrink-0 items-center gap-3">
              <span class="rounded-full border px-2.5 py-1 text-xs" :class="row.badge.tone">{{ row.badge.label }}</span>
              <button v-if="!row.active" type="button" class="text-xs text-muted-foreground" :aria-label="`收起 ${row.job.email || row.job.payload.account_id} 的登录记录`" @click="dismiss(row.job.id)">收起记录</button>
            </div>
          </div>
          <ol v-if="row.step >= 0" class="flex flex-wrap gap-x-4 gap-y-2" aria-label="登录步骤">
            <li v-for="(step, i) in grokReloginSteps" :key="step.key" class="flex items-center gap-1.5 text-xs text-muted-foreground" :aria-current="row.step === i ? 'step' : undefined">
              <span class="h-1.5 w-1.5 rounded-full" :class="i < row.step ? 'bg-emerald-500' : i === row.step ? row.badge.dot : 'bg-muted-foreground/30'"></span>{{ step.label }}
            </li>
          </ol>
          <p v-if="row.detail" class="break-words text-xs" :class="row.badge.tone">{{ row.detail }}</p>
        </article>
      </div>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed, ref, watchEffect } from 'vue'
import type { GrokReloginJob } from '@/api/grokAccounts'
import { grokReloginActive, grokReloginDetail, grokReloginLabel, grokReloginSteps } from '@/lib/grokReloginProgress'

const props = defineProps<{ jobs: GrokReloginJob[]; starting: Set<string>; pollError: boolean }>()
const expanded = ref(false)
const storageKey = 'chatgpt2api.grok.relogin.dismissed'
const dismissed = ref<string[]>([])
try {
  const saved: unknown = JSON.parse(localStorage.getItem(storageKey) || '[]')
  if (Array.isArray(saved)) dismissed.value = saved.filter(id => typeof id === 'string').slice(-300)
} catch { /* Record visibility is an optional browser preference. */ }

function dismiss(id: string) {
  dismissed.value = [...dismissed.value, id].slice(-300)
  try { localStorage.setItem(storageKey, JSON.stringify(dismissed.value)) } catch { /* In-memory preference still applies. */ }
}

const badges = {
  queued: { label: '排队中', tone: 'text-sky-500', dot: 'bg-sky-500' },
  running: { label: '进行中', tone: 'text-sky-500', dot: 'bg-sky-500' },
  completed: { label: '已结束', tone: 'text-muted-foreground', dot: 'bg-muted-foreground' },
  success: { label: '成功', tone: 'text-emerald-600', dot: 'bg-emerald-500' },
  failed: { label: '失败', tone: 'text-rose-500', dot: 'bg-rose-500' },
  cancelled: { label: '已取消', tone: 'text-muted-foreground', dot: 'bg-muted-foreground' },
}

const rows = computed(() => props.jobs.filter(job => grokReloginActive(job) || !dismissed.value.includes(job.id)).map(job => {
  const active = grokReloginActive(job)
  const success = job.status === 'completed' && job.result?.session_updated === true
  const stage = job.status === 'queued' ? 'waiting_browser' : job.result?.stage
  const step = success ? grokReloginSteps.length : grokReloginSteps.findIndex(s => s.key === stage)
  const fallback = active ? '等待阶段更新' : '任务已结束'
  const stageLabel = success ? '会话已更新' : grokReloginSteps[step]?.label || fallback
  const badge = job.status === 'failed' ? { ...badges.failed, label: grokReloginLabel(job) }
    : success ? badges.success : badges[job.status]
  return { job, active, step, badge, stageLabel, detail: grokReloginDetail(job) }
}))
const ended = computed(() => rows.value.filter(row => !row.active).length)
const running = computed(() => {
  const active = rows.value.filter(row => row.active)
  const accounts = new Set(active.map(row => row.job.payload.account_id))
  return active.length + [...props.starting].filter(id => !accounts.has(id)).length
})
const now = ref(Date.now())
watchEffect(cleanup => {
  if (!expanded.value || !running.value) return
  now.value = Date.now()
  const clock = window.setInterval(() => { now.value = Date.now() }, 1000)
  cleanup(() => window.clearInterval(clock))
})

function elapsed(job: GrokReloginJob) {
  const end = grokReloginActive(job) ? now.value / 1000 : job.updated_at
  if (!job.created_at || !end) return '—'
  const seconds = Math.max(0, Math.floor(end - job.created_at))
  return seconds < 60 ? `${seconds} 秒` : `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`
}
</script>
