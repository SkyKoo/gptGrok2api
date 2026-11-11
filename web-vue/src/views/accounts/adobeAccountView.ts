import type { AdobeAccount, AdobeMutationResponse } from '../../api/adobeAccounts'

export function adobeSubmissionUnknown(account: AdobeAccount): boolean {
  return account.submission_unknown === true || account.status === 'submission_unknown'
}

export function adobeAccountBusy(account: AdobeAccount): boolean {
  return Boolean(account.pending || account.in_flight)
}

export function adobeReloginBlocked(account: AdobeAccount): boolean {
  return adobeAccountBusy(account) && !adobeSubmissionUnknown(account)
}

export function adobeActivityLabel(account: AdobeAccount): string {
  if (adobeSubmissionUnknown(account)) return '提交结果未知，原任务保留；重新登录仅更新会话。'
  return account.in_flight ? '生成中' : account.pending ? '等待结算' : ''
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
    message: succeeded.length ? `重新登录完成 ${succeeded.length}/${rows.length} 个账号，媒体会话已确认。${unknown ? '待核对任务仍保留，未重新提交。' : ''}` : '',
    error: result.error ? adobeReloginError(result.error) : failures.map(row => `${accounts.find(account => account.id === row.id)?.email || row.id}：${adobeReloginError(row.error)}`).join('；') || (rows.length ? '' : adobeReloginError('adobe_relogin_result_unknown')),
  }
}
