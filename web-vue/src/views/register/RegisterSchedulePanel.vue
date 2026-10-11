<template>
  <form v-if="schedule" class="register-schedule" aria-label="定时启动注册" @submit.prevent="save">
    <Checkbox class="schedule-toggle" :model-value="enabled" :disabled="saving" @update:model-value="enabled = Boolean($event)">
      定时启动
    </Checkbox>
    <div class="schedule-controls">
      <label class="schedule-field">
        <span>间隔时间（分钟）</span>
        <Input v-model.number="minutes" type="number" min="1" max="10080" step="1" :disabled="saving" block />
      </label>
      <Button type="submit" variant="outline" :disabled="!dirty || !valid || saving || configSaving">
        {{ saving ? '保存中…' : '保存定时设置' }}
      </Button>
    </div>
    <p>开启方式：勾选“定时启动”，设置间隔时间，再点击“保存定时设置”。</p>
    <p v-if="dirty && !saving" class="schedule-draft" role="status">{{ schedule.enabled ? '修改尚未保存，定时器仍按已保存的设置运行。' : '修改尚未保存，定时器尚未开启。' }}</p>
    <div class="schedule-status" :class="{ 'is-enabled': schedule.enabled }">
      <strong role="status">{{ schedule.enabled ? '定时启动已开启' : '定时启动未开启' }}</strong>
      <template v-if="schedule.enabled">
        <div v-if="remainingSeconds > 0" class="schedule-countdown" role="timer" aria-live="off" aria-label="距离下次定时触发">
          <span>距离下次触发</span>
          <b>{{ countdown }}</b>
        </div>
        <p v-else>{{ Number.isFinite(nextRunAt) ? '已到触发时间，等待服务器更新状态…' : '等待服务器更新下次触发时间…' }}</p>
        <p v-if="Number.isFinite(nextRunAt)">下次触发：{{ nextRun }}</p>
      </template>
    </div>
    <p>沿用当前批次的注册总数、线程数和邮箱配置；启用后等待一个完整间隔再启动，关闭页面后仍会定时触发。</p>
    <p>已有注册任务或服务维护时跳过本次。关闭定时不停止当前批次；手动“停止”不关闭定时。</p>
    <p v-if="!valid" class="schedule-error" role="alert">请输入 1～10080 的整数分钟。</p>
    <p v-if="error" class="schedule-error" role="alert">{{ error }}</p>
    <p v-if="lastResult" :class="{ 'schedule-error': schedule.last_result === 'failed' || schedule.last_error.startsWith('storage:') }">{{ lastResult }}</p>
    <p v-if="saved" role="status">定时设置已保存</p>
  </form>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Button, Input, Checkbox } from 'nanocat-ui'
import { registerApi } from '@/api/register'
import { usePageRuntime } from '@/composables/usePageRuntime'
import { registrationScheduleResult, validRegistrationInterval, type RegistrationSchedule } from '@/lib/registrationSchedule'

const props = defineProps<{
  schedule?: RegistrationSchedule
  prepare: () => Promise<boolean>
  configSaving: boolean
}>()
const emit = defineEmits<{ (e: 'saved', schedule: RegistrationSchedule): void }>()
const enabled = ref(false)
const minutes = ref(60)
const saving = ref(false)
const error = ref('')
const saved = ref(false)
const valid = computed(() => validRegistrationInterval(minutes.value))
const dirty = computed(() => enabled.value !== props.schedule?.enabled || minutes.value !== props.schedule?.interval_minutes)
const lastResult = computed(() => props.schedule ? registrationScheduleResult(props.schedule) : '')
const runtime = usePageRuntime('registration-schedule-countdown')
const now = ref(Date.now())
const nextRunAt = computed(() => Date.parse(props.schedule?.next_run_at || ''))
const nextRun = computed(() => new Date(nextRunAt.value).toLocaleString('zh-CN', { hour12: false }))
const remainingSeconds = computed(() => Number.isFinite(nextRunAt.value) ? Math.max(0, Math.ceil((nextRunAt.value - now.value) / 1000)) : 0)
const countdown = computed(() => {
  const seconds = remainingSeconds.value
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor(seconds % 3600 / 60)
  return [hours, minutes, seconds % 60].map(value => String(value).padStart(2, '0')).join(':')
})
// 只展示服务器已保存的触发时间；回到页面时按当前时间重算，不在前端启动任务。
watch([runtime.canRun, () => props.schedule?.enabled, nextRunAt], () => {
  runtime.clearInterval('countdown')
  now.value = Date.now()
  if (!runtime.canRun.value || !props.schedule?.enabled || remainingSeconds.value <= 0) return
  runtime.setInterval('countdown', 1000, () => {
    now.value = Date.now()
    if (remainingSeconds.value <= 0) runtime.clearInterval('countdown')
  })
}, { immediate: true })
// Polls update status without discarding an unsaved timing edit.
watch(() => [props.schedule?.enabled, props.schedule?.interval_minutes] as const, (current, previous) => {
  if (current[0] === undefined || current[1] === undefined) return
  if (!previous || previous[0] === undefined || (enabled.value === previous[0] && minutes.value === previous[1])) {
    enabled.value = current[0]
    minutes.value = current[1]
  }
}, { immediate: true })
watch([enabled, minutes], () => { saved.value = false })

async function save() {
  if (!valid.value || saving.value || props.configSaving) return
  saving.value = true
  error.value = ''
  try {
    // Disabling must remain possible even if there are invalid unsaved batch edits.
    if (enabled.value && !await props.prepare()) {
      error.value = '批次配置尚未保存，请先处理自动保存错误。'
      return
    }
    const result = await registerApi.updateSchedule({ enabled: enabled.value, interval_minutes: minutes.value })
    enabled.value = result.schedule.enabled
    minutes.value = result.schedule.interval_minutes
    emit('saved', result.schedule)
    saved.value = true
  } catch (cause: any) {
    error.value = cause?.message || '保存定时设置失败'
  } finally {
    saving.value = false
  }
}
</script>

<style scoped>
.register-schedule { display: grid; gap: 10px; padding: 14px; border: 1px solid hsl(var(--border)); border-radius: 10px; }
.schedule-toggle { display: flex; align-items: center; gap: 9px; font-size: 13px; }
.schedule-controls { display: flex; align-items: end; gap: 12px; flex-wrap: wrap; }
.schedule-field { display: grid; gap: 6px; flex: 1; min-width: 160px; font-size: 12px; }
p { margin: 0; font-size: 12px; line-height: 1.6; color: hsl(var(--muted-foreground)); }
.schedule-status { display: grid; gap: 8px; padding: 12px; border-radius: 8px; background: hsl(var(--muted) / 0.35); border: 1px solid hsl(var(--border)); }
.schedule-status strong { font-size: 13px; }
.schedule-status.is-enabled { border-color: hsl(var(--primary) / 0.35); }
.schedule-countdown { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; }
.schedule-countdown span { font-size: 12px; color: hsl(var(--muted-foreground)); }
.schedule-countdown b { font-size: 24px; line-height: 1.25; font-variant-numeric: tabular-nums; letter-spacing: 0.04em; }
p.schedule-draft { color: hsl(var(--foreground)); }
p.schedule-error { color: hsl(var(--destructive)); }
</style>
