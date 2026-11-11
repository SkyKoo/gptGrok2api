import test from 'node:test'
import assert from 'node:assert/strict'
import { adobeSubmissionUnknown, adobeReloginBlocked, adobeActivityLabel, adobeReloginFeedback } from '../src/views/accounts/adobeAccountView.ts'

const unknown = { id:'adobe:unknown', email:'unknown@example.test', status:'submission_unknown', in_flight:true, pending:true }
test('unknown submissions remain held while browser authentication is allowed',()=>{
 assert.equal(adobeReloginBlocked(unknown),false)
 assert.equal(adobeSubmissionUnknown({...unknown,status:'disabled',submission_unknown:true}),true)
 assert.equal(adobeReloginBlocked({...unknown,status:'busy'}),true)
 assert.equal(adobeReloginBlocked({...unknown,status:'active',in_flight:false}),true)
 assert.ok(adobeActivityLabel(unknown).includes('原任务保留'))
 assert.ok(!adobeActivityLabel(unknown).includes('生成中'))
 assert.equal(adobeActivityLabel({...unknown,status:'busy'}),'生成中')
})
test('successful relogin only confirms authentication and retains unknown warning',()=>{
 const feedback=adobeReloginFeedback({results:[{id:unknown.id,ok:true,account:unknown}]},[unknown])
 assert.ok(feedback.message.includes('媒体会话已确认'))
 assert.ok(feedback.message.includes('待核对任务仍保留，未重新提交'))
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
