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
