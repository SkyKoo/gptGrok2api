import test from 'node:test'
import assert from 'node:assert/strict'
import { adobeSubmissionUnknown, adobeReloginBlocked, adobeActivityLabel, adobeReloginFeedback, adobeQuotaQueryBlocked, adobeLoginLabel, adobeTaskLabel, adobeAutomaticProcessing, adobeReconciliationTime, adobeRecoveryError } from '../src/views/accounts/adobeAccountView.ts'

const unknown = { id:'adobe:unknown', email:'unknown@example.test', status:'submission_unknown', in_flight:true, pending:true }
test('unknown submissions remain held while browser authentication is allowed',()=>{
 assert.equal(adobeReloginBlocked(unknown),false)
 assert.equal(adobeSubmissionUnknown({...unknown,status:'disabled',submission_unknown:true}),true)
 assert.equal(adobeReloginBlocked({...unknown,status:'busy'}),true)
 assert.equal(adobeReloginBlocked({...unknown,status:'active',in_flight:false}),true)
 assert.ok(adobeActivityLabel(unknown).includes('原任务和结果记录保留'))
 assert.ok(!adobeActivityLabel(unknown).includes('生成中'))
 assert.equal(adobeActivityLabel({...unknown,status:'busy'}),'生成中')
})
test('successful relogin only confirms authentication and retains unknown warning',()=>{
 const feedback=adobeReloginFeedback({results:[{id:unknown.id,ok:true,account:unknown}]},[unknown])
 assert.ok(feedback.message.includes('媒体会话已确认'))
 assert.ok(feedback.message.includes('原生成任务由系统继续处理，不会重新提交'))
 assert.equal(feedback.error,'')
})
test('bulk result distinguishes successful login from verification or readiness failure',()=>{
 const result={results:[{id:unknown.id,ok:true,account:unknown},{id:'otp',ok:false,error:'adobe_relogin_email_verification_required'},{id:'media',ok:false,error:'adobe_relogin_media_session_incomplete'}]}
 const feedback=adobeReloginFeedback(result,[unknown,{id:'otp',email:'otp@example.test'}])
 assert.ok(feedback.message.includes('1/3'))
 assert.ok(feedback.error.includes('otp@example.test：Adobe 要求邮件验证码'))
 assert.ok(feedback.error.includes('媒体会话尚未就绪'))
 const empty=adobeReloginFeedback({},[])
 assert.equal(empty.message,'')
 assert.ok(empty.error.includes('未收到重新登录结果'))
})
test('login validity and generation reservation remain independent after relogin',()=>{
 const relogged={...unknown,login_status:'valid',login_updated_at:1762860000}
 assert.equal(adobeLoginLabel(relogged),'登录态有效')
 assert.equal(adobeTaskLabel(relogged),'系统处理中')
 assert.equal(adobeLoginLabel({...relogged,login_status:'expired'}),'登录态已过期')
 assert.equal(adobeTaskLabel({...relogged,disabled:true}),'系统处理中')
})
test('quota queries can run for unknown reservations but not locked maintenance',()=>{
 assert.equal(adobeQuotaQueryBlocked(unknown),false)
 assert.equal(adobeQuotaQueryBlocked({...unknown,can_query_quota:false}),true)
 assert.equal(adobeQuotaQueryBlocked({...unknown,status:'busy',submission_unknown:false}),true)
 assert.equal(adobeQuotaQueryBlocked({...unknown,can_query_quota:true}),false)
})
test('automatic task processing describes observed state without asserting generation or refund',()=>{
 assert.equal(adobeAutomaticProcessing(unknown),true)
 assert.equal(adobeAutomaticProcessing({status:'active',pending:false,in_flight:false}),false)
 assert.equal(adobeAutomaticProcessing({status:'active',pending:true}),true)
 for(const state of ['checking','polling','retry_wait']) {
  const account={...unknown,task_reconciliation:{state}}
  assert.ok(adobeActivityLabel(account).includes('不会重新提交'))
  assert.ok(!adobeActivityLabel(account).includes('成功'))
 }
 assert.ok(adobeReconciliationTime({...unknown,task_reconciliation:{state:'waiting',release_after:'2026-11-12T02:00:00Z'}}).includes('起自动结束本地占用'))
 assert.ok(adobeReconciliationTime({...unknown,task_reconciliation:{state:'retry_wait',next_check_at:'2026-11-12T02:00:00Z'}}).startsWith('下次处理：'))
 assert.equal(adobeReconciliationTime({...unknown,task_reconciliation:{state:'waiting',release_after:'invalid'}}),'')
})
test('concurrent task and quota changes direct users to automatic progress rather than manual review',()=>{
 for(const code of ['adobe_submission_state_changed','adobe_submission_review_required','adobe_submission_task_not_reviewable']) {
  const message=adobeRecoveryError(code)
  assert.ok(message.includes('系统'))
  assert.ok(message.includes('刷新列表'))
  assert.ok(!message.includes('8–500'))
  assert.ok(!message.includes('重新核对'))
 }
})
