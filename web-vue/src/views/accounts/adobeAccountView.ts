import type { AdobeAccount, AdobeMutationResponse } from '../../api/adobeAccounts'

export function adobeSubmissionUnknown(account: AdobeAccount): boolean {
  return account.task_status === 'submission_unknown' || account.submission_unknown === true || account.status === 'submission_unknown'
}

export function adobeAccountBusy(account: AdobeAccount): boolean {
  return Boolean(account.pending || account.in_flight)
}

export function adobeReloginBlocked(account: AdobeAccount): boolean {
  return adobeAccountBusy(account) && !adobeSubmissionUnknown(account)
}

export function adobeQuotaQueryBlocked(account: AdobeAccount): boolean {
  if (typeof account.can_query_quota === 'boolean') return !account.can_query_quota
  return adobeAccountBusy(account) && !adobeSubmissionUnknown(account)
}

export function adobeCanResolveSubmission(account: AdobeAccount): boolean {
  return account.can_resolve_submission === true && adobeSubmissionUnknown(account)
    && Boolean(account.task_key) && Number.isInteger(account.credential_revision)
}

export function adobeLoginLabel(account: AdobeAccount): string {
  let status = account.login_status
  if (!status) {
    if (account.status === 'invalid') status = 'invalid'
    else if (account.expires_at && account.expires_at * 1000 <= Date.now()) status = 'expired'
  }
  return ({ valid: '登录态有效', expired: '登录态已过期', invalid: '登录态异常', missing: '缺少登录凭据' } as Record<string, string>)[status || ''] || '登录态待确认'
}

export function adobeTaskLabel(account: AdobeAccount): string {
  if (adobeSubmissionUnknown(account)) return '系统处理中'
  if (account.task_status === 'running' || account.in_flight) return '生成中'
  if (account.task_status === 'settling' || account.pending) return '等待结算'
  return '无任务占用'
}

export function adobeActivityLabel(account: AdobeAccount): string {
  if (adobeSubmissionUnknown(account)) {
    return ({
      checking: '系统正在查询任务和额度，原请求不会重新提交。',
      polling: '系统正在查询已提交任务的结果，原请求不会重新提交。',
      retry_wait: '本次查询未完成，系统将稍后继续处理，原请求不会重新提交。',
    } as Record<string, string>)[account.task_reconciliation?.state || ''] || '系统将自动查询并处理占用，原任务和结果记录保留，不重新生成。'
  }
  return account.in_flight ? '生成中' : account.pending ? '等待结算' : ''
}

export function adobeAutomaticProcessing(account: AdobeAccount): boolean {
  return adobeSubmissionUnknown(account) || Boolean(account.pending || account.task_reconciliation)
}

export function adobeReconciliationTime(account: AdobeAccount): string {
  const pending = account.task_reconciliation
  const value = pending?.state === 'retry_wait' ? pending.next_check_at : pending?.release_after
  if (!value) return ''
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return ''
  return pending?.state === 'retry_wait'
    ? `下次处理：${date.toLocaleString()}`
    : `无可查询任务时，${date.toLocaleString()} 起自动结束本地占用。`
}

export function adobeRecoveryError(code?: string): string {
  return ({
    adobe_submission_state_changed: '账号、额度或任务状态已变化，本次结果未覆盖新状态。系统将稍后继续处理，可刷新列表查看。',
    adobe_submission_review_required: '此任务由系统自动处理，无需填写核对说明，可刷新列表查看处理进度。',
    adobe_submission_task_not_reviewable: '原任务仍在运行，系统将在任务结束后继续处理，可刷新列表查看。',
    adobe_submission_already_resolved: '该占用已经处理，请刷新列表查看。',
  } as Record<string, string>)[code || ''] || adobeReloginError(code)
}

export function adobeReloginError(code?: string): string {
  return ({
    adobe_account_busy: '账号正在生成、结算或维护，请稍后再试。',
    adobe_account_not_found: '账号已不存在，请刷新列表。',
    adobe_credentials_changed: '账号已被其他操作更新，请刷新列表确认。',
    adobe_account_identity_mismatch: '浏览器登录身份不匹配，未替换原凭据。',
    adobe_relogin_profile_required: '缺少保存的浏览器资料，无法重新登录。',
    adobe_relogin_login_required: 'Adobe 要求登录，当前保存的凭据未能完成登录。',
    adobe_relogin_email_verification_required: 'Adobe 要求邮件验证码，本次重新登录未完成，原凭据保持。',
    adobe_relogin_verification_required: 'Adobe 要求额外身份验证，本次重新登录未完成，原凭据保持。',
    adobe_relogin_login_entry_unrecognized: '未找到 Adobe 登录入口，本次重新登录未完成。',
    adobe_relogin_login_form_unavailable: 'Adobe 登录表单尚未出现，本次重新登录未完成。',
    adobe_relogin_password_form_unavailable: 'Adobe 密码表单尚未出现，本次重新登录未完成。',
    adobe_relogin_login_pending: 'Adobe 尚未确认登录完成，原凭据保持。',
    adobe_relogin_login_form_unrecognized: 'Adobe 登录页面未能识别，未替换原凭据。',
    adobe_relogin_media_session_incomplete: '媒体会话尚未就绪，未替换原凭据。',
    adobe_relogin_token_expired: '浏览器返回的登录凭据已过期，未替换原凭据。',
    adobe_relogin_token_invalid: '浏览器返回的登录凭据无效，未替换原凭据。',
    adobe_relogin_browser_unavailable: '浏览器服务不可用或排队超时，请稍后再试。',
    adobe_relogin_proxy_binding_unavailable: '账号绑定的代理不可用，未更换出口。',
    adobe_relogin_result_unknown: '未收到重新登录结果，请刷新列表确认；原任务仍保留。',
    adobe_relogin_browser_failed: '浏览器未完成重新登录，原凭据保持。',
    adobe_relogin_cancelled: '重新登录已取消或超时，原凭据保持。',
  } as Record<string, string>)[code || ''] || code || '重新登录未完成。'
}

export function adobeReloginFeedback(result: AdobeMutationResponse, accounts: AdobeAccount[]): { message: string; error: string } {
  const rows = result.results || []
  const succeeded = rows.filter(row => row.ok)
  const unknown = succeeded.some(row => adobeSubmissionUnknown(row.account || accounts.find(account => account.id === row.id) || {} as AdobeAccount))
  const failures = rows.filter(row => !row.ok)
  return {
    message: succeeded.length ? `重新登录完成 ${succeeded.length}/${rows.length} 个账号，媒体会话已确认。${unknown ? '原生成任务由系统继续处理，不会重新提交。' : ''}` : '',
    error: result.error ? adobeReloginError(result.error) : failures.map(row => `${accounts.find(account => account.id === row.id)?.email || row.id}：${adobeReloginError(row.error)}`).join('；') || (rows.length ? '' : adobeReloginError('adobe_relogin_result_unknown')),
  }
}
