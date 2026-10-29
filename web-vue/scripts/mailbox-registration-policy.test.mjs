import test from 'node:test'
import assert from 'node:assert/strict'
import { mailboxRegistrationDisabled } from '../src/views/mailboxRegistrationPolicy.ts'

test('legacy disabled assets disable both registration channels', () => {
  for (const platform of ['openai', 'grok']) assert.equal(mailboxRegistrationDisabled({ status: 'disabled' }, platform), true)
})
test('channel policy is independent of occupancy and of the other channel', () => {
  const box = { status: 'active', openai_disabled: true, grok_disabled: false, openai_claimed: true }
  assert.equal(mailboxRegistrationDisabled(box, 'openai'), true)
  assert.equal(mailboxRegistrationDisabled(box, 'grok'), false)
  assert.equal(mailboxRegistrationDisabled({ status: 'available', grok_disabled: true }, 'openai'), false)
  assert.equal(mailboxRegistrationDisabled({ status: 'available', grok_disabled: true }, 'grok'), true)
})
