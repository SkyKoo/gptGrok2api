import test from 'node:test'
import assert from 'node:assert/strict'
import { mailboxOccupancyLabel, mailboxHasReservation, mailboxDeletionBlocked, mailboxRegistrationDisabled, mailboxRegistrationRetired, mailboxRegistrationLabel } from '../src/views/mailboxRegistrationPolicy.ts'

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

test('consumed platform remains distinct from manual disable and other platform', () => {
 const box = {status: 'available', openai_retired: true}
 assert.equal(mailboxRegistrationRetired(box, 'openai'), true)
 assert.equal(mailboxRegistrationRetired(box, 'grok'), false)
 assert.equal(mailboxRegistrationLabel(box, 'openai'), '已使用，待清理')
 assert.equal(mailboxRegistrationLabel(box, 'grok'), '允许注册')
 assert.equal(mailboxRegistrationLabel({...box, status:'deleted'}, 'grok'), '已删除')
 assert.equal(mailboxRegistrationLabel({...box, status:'cleanup_pending'}, 'grok'), '待清理')
})


test('registration reservation is separate from an account and protects deletion', () => {
 const box = {grok_reserved: true}
 assert.equal(mailboxOccupancyLabel(box, 'grok'), '注册预留，未占用')
 assert.equal(mailboxRegistrationLabel(box, 'grok'), '注册预留中')
 assert.equal(mailboxRegistrationLabel(box, 'openai'), '允许注册')
 assert.equal(mailboxOccupancyLabel(box, 'openai'), '未占用')
 assert.equal(mailboxHasReservation(box), true)
 assert.equal(mailboxDeletionBlocked(box), true)
 assert.equal(mailboxOccupancyLabel({grok_claimed:true}, 'grok'), '已占用')
 assert.equal(mailboxDeletionBlocked({grok_claimed:true}), true)
 assert.equal(mailboxDeletionBlocked({grok_reserved:false, grok_claimed:false}), false)
 assert.equal(mailboxDeletionBlocked({openai_claimed:true, grok_reserved:false}), true)
})
