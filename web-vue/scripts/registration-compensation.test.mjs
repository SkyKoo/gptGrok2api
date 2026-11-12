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

test('superseded attempt points to a child and no longer advertises waiting', async () => {
  const { registrationJobStatusText } = await import('../src/lib/registrationCompensation.ts')
  assert.match(registrationJobStatusText('registration_pending', waiting, 'child-id'), /补偿任务.*原记录/)
  assert.doesNotMatch(registrationJobStatusText('registration_pending', waiting, 'child-id'), /等待自动/)
  assert.equal(registrationCompensationText({...waiting,state:'superseded'}, false, now), '')
})

test('uncertain final signup explains why registration is not replayed', () => {
  assert.match(registrationCompensationText({ ...waiting, state: 'manual', reason: 'registration_result_uncertain' }, false, now), /最终注册提交.*核对账号.*不自动重复注册/)
})

test('Grok explains shared retries without claiming a failed mailbox is reserved', async () => {
  const { registrationCompensationHelp, registrationJobStatusText } = await import('../src/lib/registrationCompensation.ts')
  const help = registrationCompensationHelp('grok')
  assert.match(help, /同一邮箱共用次数和冷却/)
  assert.match(help, /失败后释放预留/)
  assert.doesNotMatch(help, /不会自动补偿/)
  assert.match(registrationCompensationText(waiting, false, now, 'grok'), /同邮箱累计.*1\/3/)
  assert.doesNotMatch(registrationJobStatusText('registration_pending', waiting, undefined, 'grok'), /邮箱已保留/)
  assert.match(registrationJobStatusText('registration_pending', { ...waiting, state: 'manual', reason: 'duplicate_mailbox_retry' }, undefined, 'grok'), /已合并.*记录保留/)
  assert.match(registrationCompensationText({ ...waiting, state: 'manual', reason: 'duplicate_mailbox_retry' }, false, now, 'grok'), /重复补偿计划已合并/)
  assert.match(registrationCompensationText({ ...waiting, state: 'exhausted', attempts: 3 }, false, now, 'grok'), /同邮箱累计.*3\/3.*预留已释放/)
  assert.match(registrationJobStatusText('registration_pending', waiting), /邮箱已保留/)
})

test('shared mailbox cooldown and merged retries have actionable errors', async () => {
  const { registrationProxyFailureText } = await import('../src/lib/registrationProxy.ts')
  assert.match(registrationProxyFailureText('task: mailbox_registration_cooldown'), /冷却.*等待/)
  assert.match(registrationProxyFailureText('task: mailbox_retry_already_scheduled'), /已合并.*有效任务/)
})

test('Adobe waiting and running distinguish session recovery from a new registration attempt', async () => {
  const { registrationCompensationHelp, registrationJobStatusText } = await import('../src/lib/registrationCompensation.ts')
  assert.match(registrationCompensationHelp('adobe'), /最多自动补偿 3 次.*未知时只恢复原会话.*安全验证/)
  const text = registrationCompensationText(waiting, false, now, 'adobe', 'resume_session')
  assert.match(text, /等待自动恢复原注册会话.*1\/3.*5 分钟后/)
  assert.match(text, /不再次创建账号/)
  assert.match(registrationCompensationText({ ...waiting, state: 'running', attempts: 2 }, true, now, 'adobe', 'resume_session'), /正在自动恢复原注册会话.*2\/3/)
  assert.match(registrationCompensationText(waiting, false, now, 'adobe', 'retry_registration'), /等待自动补偿注册/)
  assert.match(registrationCompensationText(waiting, true, now + 300000, 'adobe', 'resume_session'), /等待当前注册任务结束/)
  assert.match(registrationJobStatusText('registration_pending', waiting, undefined, 'adobe', 'resume_session'), /等待自动恢复原会话/)
  assert.doesNotMatch(registrationJobStatusText('registration_pending', undefined, undefined, 'adobe'), /人工处理/)
})

test('Adobe exhaustion and safety verification never promise more automatic attempts', () => {
  const exhausted = registrationCompensationText({ ...waiting, state: 'exhausted', attempts: 3 }, false, now, 'adobe', 'resume_session')
  assert.match(exhausted, /已达上限（3\/3）.*不再次创建账号/)
  assert.doesNotMatch(exhausted, /等待自动|可手动重试/)
  assert.match(registrationCompensationText({ ...waiting, state: 'manual', reason: 'human_verification_required' }, false, now, 'adobe', 'resume_session'), /验证尚未完成.*已暂停.*须通过 Adobe 校验/)
  assert.equal(registrationCompensationText(waiting, false, now, 'adobe', 'saved_result'), '')
})

test('Adobe recovery actions require server capability and saved results only use result recovery', async () => {
  const { registrationRecoveryAction } = await import('../src/lib/registrationCompensation.ts')
  assert.equal(registrationRecoveryAction({ recovery_mode: 'resume_session' }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_resume_registration: true, recovery_mode: 'resume_session' }, 'adobe'), '恢复注册')
  assert.equal(registrationRecoveryAction({ can_retry_registration: true, recovery_mode: 'retry_registration' }, 'adobe'), '补偿注册')
  assert.equal(registrationRecoveryAction({ can_retry: true, can_retry_registration: true }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_resume_registration: true, recovery_mode: 'saved_result' }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_retry_registration: false }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_retry_registration: true }, 'grok'), '复用此邮箱重试注册')
})
