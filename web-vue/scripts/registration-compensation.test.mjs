import test from 'node:test'
import assert from 'node:assert/strict'
import { registrationCompensationText } from '../src/lib/registrationCompensation.ts'
const now = Date.parse('2026-10-14T00:00:00Z')
const waiting = { state: 'waiting', attempts: 1, max_attempts: 3, next_attempt_at: '2026-10-14T00:05:00Z' }
test('waiting shows cooldown and attempt budget', () => {
  const text = registrationCompensationText(waiting, false, now)
  assert.match(text, /1\/3/)
  assert.match(text, /5 分钟后/)
})
test('due tasks distinguish engine busy from waiting for scheduler', () => {
  assert.match(registrationCompensationText(waiting, true, now + 300000), /当前注册任务/)
  assert.match(registrationCompensationText(waiting, false, now + 300000), /后台执行/)
})
test('running and exhaustion do not claim registration succeeded', () => {
  assert.match(registrationCompensationText({ ...waiting, state: 'running' }, true, now), /正在自动补偿/)
  assert.match(registrationCompensationText({ ...waiting, state: 'exhausted', attempts: 3 }, false, now), /上限.*3\/3/)
  assert.match(registrationCompensationText({ ...waiting, state: 'manual', reason: 'cancelled' }, false, now), /已停止/)
})
test('completed and saved sessions hide registration retry notice', () => {
  for (const state of ['completed', 'saved_result']) assert.equal(registrationCompensationText({ ...waiting, state }, false, now), '')
  assert.equal(registrationCompensationText(null, false, now), '')
})
test('missing environment and invalid timestamps are explicit', () => {
  assert.match(registrationCompensationText({ ...waiting, state: 'manual', reason: 'saved_environment_missing' }, false, now), /缺少/)
  assert.match(registrationCompensationText({ ...waiting, next_attempt_at: 'broken' }, false, now), /待核对/)
})

test('manual and legacy jobs never claim compensation is scheduled', async () => {
  const { registrationJobStatusText } = await import('../src/lib/registrationCompensation.ts')
  for (const state of [undefined, {state:'manual'}, {state:'completed'}]) {
    assert.match(registrationJobStatusText('registration_pending', state), /人工处理/)
    assert.doesNotMatch(registrationJobStatusText('registration_pending', state), /等待.*补偿/)
  }
  assert.match(registrationJobStatusText('registration_pending', waiting), /等待自动补偿/)
  assert.match(registrationJobStatusText('registration_pending', {...waiting,state:'running'}), /正在自动补偿/)
  assert.match(registrationCompensationText({...waiting,state:'manual',reason:'upstream_signup_refused'},false,now), /上游拒绝.*不自动补偿/)
})

test('legacy HTTP 504 refusals and challenge errors explain the actual failure', async () => {
  const { registrationProxyFailureText } = await import('../src/lib/registrationProxy.ts')
  assert.match(registrationProxyFailureText('send_code: browser_worker_rejected (HTTP 504) operation=worker_error=signup_unavailable;upstream_code=email_signup_unavailable;upstream_status=400'), /xAI 拒绝.*未进入收取验证码/)
  assert.match(registrationProxyFailureText('signup: browser_worker_rejected (HTTP 504) operation=worker_error=turnstile_incomplete;network_failures=brunhild.challenges.cloudflare.com|proxy_tunnel_failed|0'), /尚未点击.*网络失败/)
  assert.equal(registrationProxyFailureText('unrecognized failure'), 'unrecognized failure')
})
