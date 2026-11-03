export type RegistrationCompensation = {
  state: string
  attempts: number
  max_attempts: number
  next_attempt_at?: string
  last_attempt_at?: string
  reason?: string
}

export function registrationCompensationHelp(target?: string): string {
  return target === 'grok'
    ? 'Grok 临时失败最多自动补偿 3 次，同一邮箱共用次数和冷却；失败后释放预留，新任务会跳过已有注册记录的邮箱。明确拒绝、需人工核查或次数耗尽时，不自动重试。'
    : '只有可重试的超时或临时网络错误才会自动补偿，最多 3 次。上游明确拒绝、邮箱无效或安全验证失败需人工处理；原邮箱会保留。'
}

export function registrationCompensationText(state: RegistrationCompensation | null | undefined, running: boolean, now: number, target?: string): string {
  if (!state) return ''
  const attempts = `${state.attempts}/${state.max_attempts}`
  const scope = target === 'grok' ? '同邮箱累计 · ' : ''
  const retained = target === 'grok' ? '失败预留已释放，重试前重新取得。' : '邮箱仍保留。'
  if (state.state === 'running') return `${scope}正在自动补偿注册 · 第 ${attempts} 次 · 复用原邮箱和登录环境`
  if (state.state === 'exhausted') return `${scope}自动补偿已达上限（${attempts}），可手动重试。${retained}`
  if (state.state === 'manual') {
    const reasons: Record<string, string> = {
      cancelled: '已停止自动补偿，可手动重试。',
      duplicate_mailbox_retry: '同邮箱的重复补偿计划已合并，请查看该邮箱的有效任务；历史记录和次数均保留。',
      mailbox_requires_review: '该邮箱已有需要人工处理的注册记录，不会通过新任务重复自动注册。',
      registration_result_uncertain: '已进入最终注册提交阶段，结果尚未确认；请先核对账号是否已创建，不自动重复注册。',
      grok_manual_retry: '旧版 Grok 待处理记录；后台将重新检查是否符合自动补偿条件，原邮箱会保留。',
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
  if (due <= now) return `${scope}自动补偿已尝试 ${attempts} · ${running ? '等待当前注册任务结束' : '等待后台执行'}`
  const seconds = Math.ceil((due - now) / 1000)
  const remaining = seconds >= 60 ? `约 ${Math.ceil(seconds / 60)} 分钟后` : `${seconds} 秒后`
  return `${scope}自动补偿已尝试 ${attempts} · ${remaining}重试（${new Date(due).toLocaleString()}），有注册任务时顺延。`
}

export function registrationJobStatusText(status: string, state?: RegistrationCompensation | null, retryJobId?: string, target?: string): string {
  if (retryJobId) return '已创建补偿任务，原记录已保留'
  if (status === 'registration_pending' && target === 'grok') {
    if (state?.reason === 'duplicate_mailbox_retry') return '同邮箱补偿已合并，原记录保留'
    if (state?.state === 'waiting') return '注册未成功，等待自动补偿'
    if (state?.state === 'running') return '正在自动补偿'
    if (state?.state === 'exhausted') return '注册未成功，补偿次数已用完'
    return '注册未成功，需要人工处理'
  }
  if (status === 'registration_pending') {
    if (state?.state === 'waiting') return '邮箱已保留，等待自动补偿'
    if (state?.state === 'running') return '邮箱已保留，正在自动补偿'
    if (state?.state === 'exhausted') return '邮箱已保留，补偿次数已用完'
    return '邮箱已保留，需要人工处理'
  }
  const labels: Record<string, string> = { queued: '等待执行', running: '进行中', completed: '已完成', failed: '失败', cancelled: '已取消', interrupted: '已中断，需核查', import_pending: '等待入库', verification_pending: '等待校验' }
  return labels[status] || status
}
