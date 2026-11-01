import test from 'node:test'
import assert from 'node:assert/strict'
import { grokMediaQuotaEntries, grokMediaQuotaNote } from '../src/views/accounts/grokMediaQuota.ts'
const item = (quotas, status = 'success') => ({ media_quota: { status, checked_at: '2026-11-01T12:00:00Z', quotas } })

test('missing media categories remain unknown; explicit zero with recovery is exhausted', () => {
 const rows = grokMediaQuotaEntries(item({imagePro:{available:true, remaining:0, next_available_at:'2026-11-02T11:59:19Z'},video720p:{available:true,remaining:1}}))
 assert.equal(rows.find(x=>x.key==='image').text, '未知')
 assert.equal(rows.find(x=>x.key==='imagePro').text, '0 次')
 assert.ok(rows.find(x=>x.key==='imagePro').reset)
 assert.equal(rows.find(x=>x.key==='video720p').text, '1 次')
})
test('available zero without recovery is unspecified, not empty or unlimited', () => {
 for(const remaining of [undefined,0,null]){
  const row=grokMediaQuotaEntries(item({image:{available:true,remaining}}))[0]
  assert.equal(row.text,'可用 · 次数未知');assert.equal(row.exhausted,false)
 }
 assert.equal(grokMediaQuotaEntries(item({image:{available:false}}))[0].text,'暂不可用')
})
test('failed refresh hides stale counters and untrusted error text', () => {
 const account=item({imagePro:{remaining:4}},'failed')
 account.media_quota.error='HTTP 403'
 assert.ok(grokMediaQuotaEntries(account).every(x=>x.text==='未知'))
 assert.equal(grokMediaQuotaNote(account),'查询失败 · HTTP 403')
 account.media_quota.error='secret-cookie'
 assert.ok(!grokMediaQuotaNote(account).includes('secret'))
 assert.equal(grokMediaQuotaNote({}),'尚未查询')
})
