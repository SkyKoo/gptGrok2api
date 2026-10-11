import assert from 'node:assert/strict'
import test from 'node:test'
import { accountQuotaItems } from '../src/lib/accountQuotas.ts'

test('all quota categories are visible, with independent remaining and reset values', () => {
  const items = accountQuotaItems({ quota: 99, capability_quotas: {
    image_gen: { remaining: 0, reset_at: '2026-10-12T00:00:00Z' },
    reason: { remaining: 8, reset_at: '2026-10-11T12:00:00Z' },
    file_upload: { remaining: 3 }, paste_text_to_file: { remaining: 2 }, deep_research: { remaining: 5 },
  } })
  assert.deepEqual(items.map((q) => [q.label, q.text]), [['图片', '0'], ['推理', '8'], ['文件上传', '3'], ['长文本转文件', '2'], ['深度研究', '5']])
  assert.equal(items[0].tone, 'danger')
  assert.equal(items[1].tone, 'success')
  assert.notEqual(items[0].resetAt, items[1].resetAt)
})

test('unknown is not zero and an explicit snapshot never falls back to the legacy count', () => {
  assert.equal(accountQuotaItems({ quota: 25, capability_quotas: { image_gen: { remaining: null } } })[0].text, '未知')
  assert.equal(accountQuotaItems({ quota: 25, image_quota_unknown: true })[0].text, '未知')
  assert.equal(accountQuotaItems({ quota: 25 })[0].text, '25')
  assert.ok(accountQuotaItems({}).every((q) => q.text === '未知' && q.tone === 'muted'))
  for (const remaining of [-1, NaN, Infinity]) {
    assert.equal(accountQuotaItems({ capability_quotas: { reason: { remaining } } })[1].text, '未知')
  }
})

test('new upstream categories and pending use remain visible without changing observed remaining', () => {
  const items = accountQuotaItems({ capability_quotas: { reason: { remaining: 8 }, future_feature: { remaining: 7 } }, quota_pending: { reason: 2, unseen_feature: 1 } })
  assert.equal(items.find((q) => q.key === 'reason').text, '8')
  assert.equal(items.find((q) => q.key === 'reason').pending, 2)
  assert.equal(items.find((q) => q.key === 'future_feature').text, '7')
  assert.equal(items.find((q) => q.key === 'unseen_feature').text, '未知')
  assert.equal(items.find((q) => q.key === 'unseen_feature').pending, 1)
})
