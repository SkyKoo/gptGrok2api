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
      grok_manual_retry: 'Grok 当前需要手动重试；请先检查错误原因，原邮箱会保留。',
      upstream_signup_refused: '上游拒绝本次邮箱注册，不自动补偿；请先核实上游可用性和账号资格。',
      human_verification_required: '安全验证未完成，不自动补偿；请先人工核查页面和网络。',
      email_already_registered: '上游提示邮箱已注册，请核对已有账号。',
      email_invalid: '上游提示邮箱无效，请核对邮箱。',
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

export function registrationJobStatusText(status: string, state?: RegistrationCompensation | null): string {
  if (status === 'registration_pending') {
    if (state?.state === 'waiting') return '邮箱已保留，等待自动补偿'
    if (state?.state === 'running') return '邮箱已保留，正在自动补偿'
    if (state?.state === 'exhausted') return '邮箱已保留，补偿次数已用完'
    return '邮箱已保留，需要人工处理'
  }
  const labels: Record<string, string> = { queued: '等待执行', running: '进行中', completed: '已完成', failed: '失败', cancelled: '已取消', interrupted: '已中断，需核查', import_pending: '等待入库', verification_pending: '等待校验' }
  return labels[status] || status
}
