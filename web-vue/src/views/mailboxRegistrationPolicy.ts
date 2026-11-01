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
  mailbox: { status?: string; openai_disabled?: boolean; grok_disabled?: boolean; openai_retired?: boolean; grok_retired?: boolean },
  platform: RegistrationChannel,
): string {
  if (mailbox.status === 'deleted') return '已删除'
  if (mailbox.status === 'cleanup_pending') return '待清理'
  if (mailboxRegistrationRetired(mailbox, platform)) return '已使用，待清理'
  return mailboxRegistrationDisabled(mailbox, platform) ? '已禁用注册' : '允许注册'
}
