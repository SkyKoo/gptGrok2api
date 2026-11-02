import test from 'node:test'
import assert from 'node:assert/strict'
import { formatGrokAccountDate, grokAccountDetailItems, grokLastUsedText, grokOAuthRemainingQuotaText, grokSuccessRate, grokUsageText } from '../src/views/accounts/grokAccountView.ts'

test('missing usage stays unknown while a recorded zero remains zero', () => {
 const account={sync_state:'synced'}
 assert.equal(grokLastUsedText(account),'暂无记录')
 assert.equal(grokSuccessRate(account),'-')
 assert.equal(grokUsageText(account),'暂无记录')
 assert.equal(grokUsageText({...account,use_count:0,fail_count:2}),'0 / 2')
 assert.equal(grokSuccessRate({...account,use_count:0,fail_count:2}),'0%')
})

test('creation uses the inventory timestamp and OAuth recovery cannot replace it',()=>{
 const account={created_at:'2026-10-01T00:00:00Z',updated_at:'2026-11-02T00:00:00Z',oauth:{recovery:{next_attempt_at:'2026-11-03T00:00:00Z'}}}
 const details=grokAccountDetailItems(account)
 assert.equal(details.find(item=>item.label==='创建时间（CFM 入库）').value,formatGrokAccountDate(account.created_at))
 assert.ok(!details.some(item=>['OAuth 额度','OAuth 恢复','恢复时间'].includes(item.label)))
})

test('unknown OAuth quota and unset timestamps do not appear exhausted or as 1970',()=>{
 for(const value of [undefined,null,'']) assert.equal(grokOAuthRemainingQuotaText({oauth:{quota:{remaining_percent:value}}}),'-')
 assert.equal(grokOAuthRemainingQuotaText({oauth:{quota:{remaining_percent:0}}}),'0%')
 for(const value of [undefined,null,0,'0','invalid']) assert.equal(formatGrokAccountDate(value),'-')
 assert.equal(formatGrokAccountDate(1793577600),formatGrokAccountDate(1793577600000))
})
