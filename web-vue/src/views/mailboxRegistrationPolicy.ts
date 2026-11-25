import type { ICloudMailboxReconcileResult } from '../api/icloud'

export type RegistrationChannel = 'openai' | 'grok' | 'adobe'
export const registrationChannels: { value: RegistrationChannel; label: string }[] = [
  { value: 'openai', label: 'GPT' },
  { value: 'grok', label: 'Grok' },
  { value: 'adobe', label: 'Adobe' },
]

export function mailboxRegistrationDisabled(
  mailbox: { status?: string; openai_disabled?: boolean; grok_disabled?: boolean; adobe_disabled?: boolean },
  platform: RegistrationChannel,
): boolean {
  return mailbox.status === 'disabled' || mailbox[`${platform}_disabled`] === true
}

export function mailboxRegistrationRetired(
  mailbox: { openai_retired?: boolean; grok_retired?: boolean; adobe_retired?: boolean },
  platform: RegistrationChannel,
): boolean {
  return mailbox[`${platform}_retired`] === true
}

export function mailboxRegistrationLabel(
  mailbox: { status?: string; openai_disabled?: boolean; grok_disabled?: boolean; adobe_disabled?: boolean; openai_retired?: boolean; grok_retired?: boolean; adobe_retired?: boolean } & MailboxClaimState,
  platform: RegistrationChannel,
): string {
  if (mailbox.status === 'deleted') return '已删除'
  if (mailbox.status === 'cleanup_pending') return '待清理'
  if (mailboxRegistrationRetired(mailbox, platform)) return '已使用，待清理'
  if (mailboxRegistrationDisabled(mailbox, platform)) return '已禁用注册'
  if (mailbox[`${platform}_reserved`]) return '注册预留中'
  return '允许注册'
}

export type MailboxClaimState = {
  openai_claimed?: boolean; grok_claimed?: boolean; adobe_claimed?: boolean
  openai_reserved?: boolean; grok_reserved?: boolean; adobe_reserved?: boolean
}

export function mailboxOccupancyLabel(mailbox: MailboxClaimState, platform: RegistrationChannel): string {
  if (mailbox[`${platform}_claimed`]) return '已占用'
  if (mailbox[`${platform}_reserved`]) return '注册预留，未占用'
  return '未占用'
}

export function mailboxHasReservation(mailbox: MailboxClaimState): boolean {
  return Boolean(mailbox.openai_reserved || mailbox.grok_reserved || mailbox.adobe_reserved)
}

export function mailboxDeletionBlocked(mailbox: MailboxClaimState): boolean {
  return Boolean(mailbox.openai_claimed || mailbox.grok_claimed || mailbox.adobe_claimed || mailboxHasReservation(mailbox))
}

export type MailboxReconcileFeedback = {
  notice: string
  warning: string
  error: string
}

export function formatMailboxReconcileFeedback(result: ICloudMailboxReconcileResult): MailboxReconcileFeedback {
  const claimCounts: Record<RegistrationChannel, number> = {
    openai: Number(result.openai_claims || 0),
    grok: Number(result.grok_claims || 0),
    adobe: Number(result.adobe_claims || 0),
  }
  const claimsSummary = registrationChannels
    .map(channel => `${channel.label} ${claimCounts[channel.value]} 个`)
    .join('、')
  const aliases = Number(result.aliases || 0)
  const releasedClaims = Number(result.released_claims || 0)
  const summary = `发现 ${aliases} 个上游邮箱，关联 ${claimsSummary}，修复历史占用 ${releasedClaims} 个。`
  const errors = (result.errors || []).map(item => String(item || '').trim()).filter(Boolean)
  // Older servers return success, counts and errors without an explicit outcome.
  const hasProgress = aliases > 0 || releasedClaims > 0 || Object.values(claimCounts).some(count => count > 0)
  const outcome = result.outcome ?? (result.success === true && errors.length === 0
    ? 'success'
    : hasProgress ? 'partial' : 'failed')
  if (outcome === 'failed') {
    return {
      notice: '',
      warning: '',
      error: errors.length ? `邮箱核对失败：${errors.join('；')}` : '邮箱核对失败',
    }
  }
  if (outcome === 'partial') {
    return {
      notice: `核对部分完成：${summary}`,
      warning: errors.join('；') || '部分项目未通过核对',
      error: '',
    }
  }
  return {
    notice: `核对完成：${summary}`,
    warning: '',
    error: '',
  }
}

export function mailboxStateBlockTitle(bridge: { reachable?: boolean } | null | undefined): string {
  return bridge?.reachable ? '操作未完成' : '邮箱管理模块暂不可用'
}
