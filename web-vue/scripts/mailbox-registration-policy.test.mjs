import test from 'node:test'
import assert from 'node:assert/strict'
import {
  mailboxOccupancyLabel,
  mailboxHasReservation,
  mailboxDeletionBlocked,
  mailboxRegistrationDisabled,
  mailboxRegistrationRetired,
  mailboxRegistrationLabel,
  formatMailboxReconcileFeedback,
  mailboxStateBlockTitle,
} from '../src/views/mailboxRegistrationPolicy.ts'

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

test('reconcile feedback separates partial warnings from module unavailability', () => {
  const clean = formatMailboxReconcileFeedback({
    success: true,
    outcome: 'success',
    aliases: 603,
    openai_claims: 460,
    grok_claims: 227,
    adobe_claims: 132,
    released_claims: 0,
    errors: [],
  })
  assert.equal(clean.warning, '')
  assert.equal(clean.error, '')
  assert.equal(clean.notice, '核对完成：发现 603 个上游邮箱，关联 GPT 460 个、Grok 227 个、Adobe 132 个，修复历史占用 0 个。')

  const partial = formatMailboxReconcileFeedback({
    success: false,
    outcome: 'partial',
    aliases: 603,
    openai_claims: 460,
    grok_claims: 227,
    adobe_claims: 91,
    released_claims: 0,
    errors: ['adobe:u1：Adobe 账号邮箱来源与现有资产不一致，未改写', 'adobe:u2：Adobe 邮箱资产未匹配，请先核对该邮箱来源，再关联账号'],
  })
  assert.equal(partial.notice, '核对部分完成：发现 603 个上游邮箱，关联 GPT 460 个、Grok 227 个、Adobe 91 个，修复历史占用 0 个。')
  assert.equal(partial.warning, 'adobe:u1：Adobe 账号邮箱来源与现有资产不一致，未改写；adobe:u2：Adobe 邮箱资产未匹配，请先核对该邮箱来源，再关联账号')
  assert.equal(partial.error, '')

  assert.equal(mailboxStateBlockTitle(null), '邮箱管理模块暂不可用')
  assert.equal(mailboxStateBlockTitle({ reachable: false }), '邮箱管理模块暂不可用')
  assert.equal(mailboxStateBlockTitle({ reachable: true }), '操作未完成')
})

test('failed reconcile displays no completion notice for both API versions', () => {
  const failed = { success: false, aliases: 0, openai_claims: 0, grok_claims: 0, adobe_claims: 0,
    released_claims: 0, errors: ['context deadline exceeded'] }
  const expected = { notice: '', warning: '', error: '邮箱核对失败：context deadline exceeded' }
  assert.deepEqual(formatMailboxReconcileFeedback({ ...failed, outcome: 'failed' }), expected)
  assert.deepEqual(formatMailboxReconcileFeedback(failed), expected)
  const legacyPartial = formatMailboxReconcileFeedback({ ...failed, adobe_claims: 1 })
  assert.match(legacyPartial.notice, /^核对部分完成/)
  assert.equal(legacyPartial.error, '')
  assert.match(formatMailboxReconcileFeedback({ success: true }).notice, /^核对完成/)
})
