import test from 'node:test'
import assert from 'node:assert/strict'
import { grokMediaQuotaEntries, grokMediaQuotaNote, grokMediaSchedulingText, grokMediaLastSuccess, grokMediaHistory } from '../src/views/accounts/grokMediaQuota.ts'
const item = (quotas, status = 'success') => ({ media_quota: { status, checked_at: '2026-11-01T12:00:00Z', quotas } })

test('missing media categories remain unknown; explicit zero with recovery is exhausted', () => {
 const rows = grokMediaQuotaEntries(item({imagePro:{available:true, remaining:0, next_available_at:'2026-11-02T11:59:19Z'},video720p:{available:true,remaining:1}}))
 assert.deepEqual(rows.map(x=>x.key), ['imagePro','video720p'])
 assert.match(rows[0].label,/共用/)
 assert.equal(rows.find(x=>x.key==='imagePro').text, '0 次')
 assert.ok(rows.find(x=>x.key==='imagePro').reset)
 assert.equal(rows.find(x=>x.key==='video720p').text, '1 次')
})
test('available zero without recovery is unspecified, not empty or unlimited', () => {
 for(const remaining of [undefined,0,null]){
  const row=grokMediaQuotaEntries(item({imagePro:{available:true,remaining}}))[0]
  assert.equal(row.text,'可用 · 次数未知');assert.equal(row.exhausted,false)
 }
 assert.equal(grokMediaQuotaEntries(item({imagePro:{available:false}}))[0].text,'暂不可用')
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

test('missing shared quota is clearly distinguished from failed query',()=>{
 assert.ok(grokMediaQuotaEntries(item({})).every(x=>x.text==='上游未提供额度'))
})

test('shared reservations and settlement are visible instead of promising stale balance',()=>{
 const account=item({imagePro:{available:true,remaining:4}})
 account.media_quota_pending={image_gen:1}
 assert.equal(grokMediaQuotaEntries(account)[0].text,'4 次 · 已预占 1')
 account.media_quota_refresh_needed=true
 assert.equal(grokMediaQuotaEntries(account)[0].text,'等待结算刷新')
 assert.match(grokMediaQuotaNote(account),/暂停/)
})

test('failed checks retain history without advertising usable quota', () => {
 const account = item({}, 'failed')
 account.media_quota_last_success = item({ imagePro: { remaining: 4 }, video720p: { remaining: 2 } }).media_quota
 account.media_scheduling = { image: { eligible: false, reason: 'quota_unknown' } }
 assert.ok(grokMediaQuotaEntries(account).every(x => x.text === '未知'))
 assert.match(grokMediaLastSuccess(account), /图片 4 次/)
 assert.match(grokMediaLastSuccess(account), /不用于调度/)
 assert.match(grokMediaSchedulingText(account, 'image'), /暂停调度/)
})
test('image exhaustion and video recovery have independent displays', () => {
 const account = item({ imagePro: { available: false }, video720p: { remaining: 5 } })
 account.media_scheduling = { image: { eligible: false, reason: 'exhausted' }, video: { eligible: true, reason: 'ready' } }
 account.media_quota_history = [{ at: '2026-11-09T12:00:00Z', changes: { video_gen: { before: 0, after: 5 } } }]
 assert.equal(grokMediaSchedulingText(account, 'image'), '额度耗尽')
 assert.equal(grokMediaSchedulingText(account, 'video'), '可调度')
 assert.equal(grokMediaHistory(account)[0].text, '视频 0 → 5')
})
