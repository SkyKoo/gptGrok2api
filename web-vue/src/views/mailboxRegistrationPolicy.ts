export type RegistrationChannel = 'openai' | 'grok'
export const registrationChannels: { value: RegistrationChannel; label: string }[] = [
  { value: 'openai', label: 'GPT' },
  { value: 'grok', label: 'Grok' },
]

export function mailboxRegistrationDisabled(
  mailbox: { status?: string; openai_disabled?: boolean; grok_disabled?: boolean },
  platform: RegistrationChannel,
): boolean {
  return mailbox.status === 'disabled' || mailbox[`${platform}_disabled`] === true
}

export function mailboxRegistrationRetired(
  mailbox: { openai_retired?: boolean; grok_retired?: boolean },
  platform: RegistrationChannel,
): boolean {
  return mailbox[`${platform}_retired`] === true
}

export function mailboxRegistrationLabel(
  mailbox: { status?: string; openai_disabled?: boolean; grok_disabled?: boolean; openai_retired?: boolean; grok_retired?: boolean } & MailboxClaimState,
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
  openai_claimed?: boolean; grok_claimed?: boolean
  openai_reserved?: boolean; grok_reserved?: boolean
}

export function mailboxOccupancyLabel(mailbox: MailboxClaimState, platform: RegistrationChannel): string {
  if (mailbox[`${platform}_claimed`]) return '已占用'
  if (mailbox[`${platform}_reserved`]) return '注册预留，未占用'
  return '未占用'
}

export function mailboxHasReservation(mailbox: MailboxClaimState): boolean {
  return Boolean(mailbox.openai_reserved || mailbox.grok_reserved)
}

export function mailboxDeletionBlocked(mailbox: MailboxClaimState): boolean {
  return Boolean(mailbox.openai_claimed || mailbox.grok_claimed || mailboxHasReservation(mailbox))
}
