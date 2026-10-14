export type RegistrationCompensation = {
  state: string
  attempts: number
  max_attempts: number
  next_attempt_at?: string
  last_attempt_at?: string
  reason?: string
}

export function registrationCompensationText(state: RegistrationCompensation | null | undefined, running: boolean, now: number): string {
  if (!state) return ''
  const attempts = `${state.attempts}/${state.max_attempts}`
  if (state.state === 'running') return `正在自动补偿注册 · 第 ${attempts} 次 · 复用原邮箱和登录环境`
  if (state.state === 'exhausted') return `自动补偿已达上限（${attempts}），可手动重试。邮箱仍保留。`
  if (state.state === 'manual') {
    const reasons: Record<string, string> = {
      cancelled: '已停止自动补偿，可手动重试。',
      saved_environment_missing: '缺少原邮箱或登录环境，需手动处理。',
      saved_account_exists: '该邮箱已有保存的账号，请检查账号或待入库记录。',
      invalid_retry_state: '自动补偿记录需要核查，请手动处理。',
    }
    return reasons[state.reason || ''] || '此错误需要人工检查，暂不自动补偿。'
  }
  if (state.state !== 'waiting') return ''
  const due = Date.parse(state.next_attempt_at || '')
  if (!Number.isFinite(due)) return '自动补偿时间待核对。'
  if (due <= now) return `自动补偿已尝试 ${attempts} · ${running ? '等待当前注册任务结束' : '等待后台执行'}`
  const seconds = Math.ceil((due - now) / 1000)
  const remaining = seconds >= 60 ? `约 ${Math.ceil(seconds / 60)} 分钟后` : `${seconds} 秒后`
  return `自动补偿已尝试 ${attempts} · ${remaining}重试（${new Date(due).toLocaleString()}），有注册任务时顺延。`
}
