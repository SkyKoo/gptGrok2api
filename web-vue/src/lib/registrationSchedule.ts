export type RegistrationSchedule = {
  enabled: boolean
  interval_minutes: number
  next_run_at: string
  last_attempt_at: string
  last_started_at: string
  last_result: string
  last_error: string
}

export function validRegistrationInterval(value: number) {
  return Number.isInteger(value) && value >= 1 && value <= 10080
}

export function registrationScheduleResult(schedule: RegistrationSchedule) {
  const error = schedule.last_error
  if (error.includes('schedule_state_')) return '定时状态无法读取或保存，请检查服务器存储。'
  if (error.includes('history_full_archive_completed_jobs_first')) return '注册历史已满，请先清理已完成记录；待恢复任务会保留。'
  if (error.includes('already_running')) return '上次触发时已有注册任务，已跳过，等待下次定时。'
  if (error.includes('maintenance')) return '上次触发时服务正在维护，已跳过，等待下次定时。'
  if (error.includes('openai_target_required')) return '当前注册目标不是 ChatGPT，已跳过本次定时。'
  if (error.startsWith('config:')) return '批次配置未通过检查，请检查任务参数和邮箱来源。'
  if (error) return '上次定时启动失败，请检查注册配置和服务器日志。'
  if (schedule.last_result === 'started') return '上次定时已启动一批注册，结果请查看注册任务记录。'
  if (schedule.last_result === 'pending') return '上次触发结果尚未确认，不会重复补跑。'
  return ''
}
