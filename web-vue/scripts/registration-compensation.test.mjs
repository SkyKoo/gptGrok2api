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
  const cooldown = registrationProxyFailureText('task: mailbox_registration_cooldown retry_after="2026-10-14T00:05:00Z"')
  assert.match(cooldown, /邮箱.*注册冷却.*本地时间/)
  assert.ok(cooldown.includes(new Date('2026-10-14T00:05:00Z').toLocaleString()))
  assert.doesNotMatch(cooldown, /retry_after|task:|限流/)
  assert.match(registrationProxyFailureText('task: mailbox_registration_cooldown retry_after="invalid"'), /等待后再重试/)
  assert.match(registrationProxyFailureText('task: mailbox_already_has_saved_account'), /已有保存的账号.*账号管理.*没有启动/)
  assert.match(registrationProxyFailureText('storage: account_lookup_failed'), /无法核对.*没有启动/)
})

test('Adobe failed attempts restart registration while accepted creations continue verification', async () => {
  const { registrationCompensationHelp, registrationJobStatusText } = await import('../src/lib/registrationCompensation.ts')
  assert.match(registrationCompensationHelp('adobe'), /最多自动补偿 3 次.*原邮箱.*独立浏览器环境.*共用次数和冷却.*手动重新注册.*新的注册流程.*旧失败记录保留.*未知时停止自动重试.*安全验证/)
  const text = registrationCompensationText(waiting, false, now, 'adobe', 'resume_session')
  assert.match(text, /等待自动继续账号验证.*1\/3.*5 分钟后/)
  assert.match(registrationCompensationText({ ...waiting, state: 'running', attempts: 2 }, true, now, 'adobe', 'resume_session'), /正在自动继续账号验证.*2\/3/)
  assert.match(registrationCompensationText(waiting, false, now, 'adobe', 'retry_registration'), /等待自动重新注册.*1\/3/)
  assert.match(registrationCompensationText(waiting, true, now + 300000, 'adobe', 'resume_session'), /等待当前注册任务结束/)
  assert.equal(registrationJobStatusText('registration_pending', waiting, undefined, 'adobe', 'resume_session'), '账号待验证')
  assert.equal(registrationJobStatusText('registration_pending', waiting, undefined, 'adobe', 'retry_registration'), '注册失败')
  assert.match(registrationJobStatusText('registration_pending', undefined, undefined, 'adobe', 'resume_session'), /账号待验证/)
  assert.doesNotMatch(registrationJobStatusText('registration_pending', undefined, undefined, 'adobe'), /人工处理/)
})

test('Adobe exhaustion and safety verification never promise more automatic attempts', () => {
  const exhausted = registrationCompensationText({ ...waiting, state: 'exhausted', attempts: 3 }, false, now, 'adobe', 'resume_session')
  assert.match(exhausted, /次数已用完（3\/3）.*已停止自动重试/)
  assert.doesNotMatch(exhausted, /等待自动|可手动重试/)
  assert.equal(registrationCompensationText({ ...waiting, state: 'manual', reason: 'human_verification_required' }, false, now, 'adobe', 'resume_session'), '当前错误不支持自动重试。')
  assert.equal(registrationCompensationText(waiting, false, now, 'adobe', 'saved_result'), '')
})

test('Adobe actions require both the server capability and matching creation outcome', async () => {
  const { registrationRecoveryAction } = await import('../src/lib/registrationCompensation.ts')
  assert.equal(registrationRecoveryAction({ recovery_mode: 'resume_session' }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_resume_registration: true, recovery_mode: 'resume_session' }, 'adobe'), '继续验证')
  assert.equal(registrationRecoveryAction({ can_retry_registration: true, recovery_mode: 'retry_registration' }, 'adobe'), '重新注册')
  assert.equal(registrationRecoveryAction({ can_retry_registration: true, recovery_mode: 'resume_session' }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_resume_registration: true, recovery_mode: 'retry_registration' }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_retry: true, can_retry_registration: true }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_resume_registration: true, recovery_mode: 'saved_result' }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_retry_registration: false }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ can_retry_registration: true }, 'grok'), '复用此邮箱重试注册')
  assert.equal(registrationRecoveryAction({ can_retry_registration: true }, 'openai'), '复用此邮箱重试注册')
})

test('Adobe uncertain results require the new explicit capability rather than legacy retry flags', async () => {
  const { registrationRecoveryAction, registrationJobStatusText } = await import('../src/lib/registrationCompensation.ts')
  assert.equal(registrationRecoveryAction({ recovery_mode: 'result_uncertain', can_retry_registration: true, can_resume_registration: true }, 'adobe'), '')
  for (const state of [undefined, waiting, { ...waiting, state: 'running' }, { ...waiting, state: 'manual', reason: 'registration_result_uncertain' }]) {
    const message = registrationCompensationText(state, false, now, 'adobe', 'result_uncertain')
    assert.match(message, /未收到 Adobe 创建结果.*已停止自动重试/)
    assert.doesNotMatch(message, /等待自动|正在自动|继续验证|恢复原会话/)
    assert.equal(registrationJobStatusText('registration_pending', state, undefined, 'adobe', 'result_uncertain'), '注册失败')
  }
  assert.equal(registrationJobStatusText('failed', undefined, undefined, 'adobe', 'result_uncertain'), '注册失败')
  assert.equal(registrationJobStatusText('registration_pending', waiting, 'child', 'adobe', 'result_uncertain'), '已发起后续重试')
  assert.match(registrationCompensationText({ ...waiting, state: 'manual', reason: 'registration_result_uncertain' }, false, now, 'adobe'), /未收到 Adobe 创建结果.*已停止自动重试/)
  const { registrationProxyFailureText } = await import('../src/lib/registrationProxy.ts')
  assert.match(registrationProxyFailureText('task: registration_result_uncertain'), /尚未确认.*已暂停.*不会自动转入登录/)
})

test('Adobe manual restart is explicit and takes priority over the old registration action', async () => {
  const { adobeManualReregistration, registrationRecoveryAction } = await import('../src/lib/registrationCompensation.ts')
  const job = { status: 'registration_pending', can_reregister: true, can_retry_registration: true, recovery_mode: 'result_uncertain', creation_outcome: 'unknown' }
  assert.equal(adobeManualReregistration(job, 'adobe'), true)
  assert.equal(registrationRecoveryAction(job, 'adobe'), '手动重新注册')
  assert.equal(adobeManualReregistration({ ...job, can_reregister: false }, 'adobe'), false)
  for (const target of ['grok', 'openai', undefined]) assert.equal(adobeManualReregistration(job, target), false)
})

test('Adobe manual restart cannot replace accepted, saved, active or superseded work', async () => {
  const { adobeManualReregistration, registrationRecoveryAction } = await import('../src/lib/registrationCompensation.ts')
  const job = { status: 'failed', can_reregister: true, can_retry_registration: true, recovery_mode: 'retry_registration', creation_outcome: 'rejected' }
  for (const change of [
    { creation_outcome: 'accepted' }, { imported: true }, { can_retry: true },
    { recovery_mode: 'saved_result' }, { recovery_mode: 'resume_session' },
    { status: 'running' }, { status: 'queued' }, { status: 'completed' },
    { retry_job_id: 'child' }, { auto_retry: { ...waiting, state: 'superseded' } },
  ]) assert.equal(adobeManualReregistration({ ...job, ...change }, 'adobe'), false, JSON.stringify(change))
  assert.equal(registrationRecoveryAction({ ...job, retry_job_id: 'child' }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ ...job, auto_retry: { ...waiting, state: 'superseded' } }, 'adobe'), '')
  assert.equal(registrationRecoveryAction({ ...job, creation_outcome: 'accepted', recovery_mode: 'resume_session', can_resume_registration: true }, 'adobe'), '继续验证')
})

test('Adobe mailbox cooldown exposes local deadline without calling ordinary waiting an upstream limit', async () => {
  const { adobeReregistrationCooldown } = await import('../src/lib/registrationCompensation.ts')
  const due = '2026-10-14T00:05:00Z'
  const result = adobeReregistrationCooldown({ reregister_not_before: due }, now)
  assert.equal(result.blocked, true)
  assert.ok(result.text.includes(new Date(due).toLocaleString()))
  assert.match(result.text, /邮箱注册冷却.*本地时间.*剩余约 5 分钟/)
  assert.doesNotMatch(result.text, /Adobe 限流|上游限流/)
  assert.match(adobeReregistrationCooldown({ reregister_not_before: due }, now + 299000).text, /剩余1 秒/)
  assert.deepEqual(adobeReregistrationCooldown({ reregister_not_before: due }, now + 300000), { blocked: false, text: '' })
  assert.deepEqual(adobeReregistrationCooldown({ auto_retry: waiting }, now), { blocked: false, text: '' })
  assert.deepEqual(adobeReregistrationCooldown({ reregister_not_before: '' }, now), { blocked: false, text: '' })
  assert.equal(adobeReregistrationCooldown({ reregister_not_before: 'invalid' }, now).blocked, true)
})

test('Adobe in-flight attempts do not display the uncertainty of a failed attempt', async () => {
  const { registrationJobStatusText } = await import('../src/lib/registrationCompensation.ts')
  for (const [status, expected] of [['running', '进行中'], ['queued', '等待执行'], ['completed', '已完成']]) {
    assert.equal(registrationJobStatusText(status, undefined, undefined, 'adobe', 'result_uncertain'), expected)
    assert.equal(registrationCompensationText(undefined, true, now, 'adobe', 'result_uncertain', status), '')
  }
  for (const state of ['superseded', 'completed', 'saved_result']) {
    assert.equal(registrationCompensationText({ ...waiting, state }, false, now, 'adobe', 'result_uncertain', 'registration_pending'), '')
  }
})


test('Adobe final failure labels preserve accepted accounts and expose the actual cause', async () => {
  const { registrationJobStatusText, registrationFailureText } = await import('../src/lib/registrationCompensation.ts')
  for (const status of ['registration_pending', 'failed', 'interrupted']) {
    assert.equal(registrationJobStatusText(status, undefined, undefined, 'adobe', 'retry_registration'), '注册失败')
    assert.equal(registrationJobStatusText(status, undefined, undefined, 'adobe', 'resume_session'), '账号待验证')
  }
  assert.equal(registrationJobStatusText('cancelled', undefined, undefined, 'adobe', 'result_uncertain'), '已取消')
  assert.equal(registrationJobStatusText('cancelled', undefined, undefined, 'adobe', 'resume_session'), '已取消')
  assert.equal(registrationJobStatusText('import_pending', undefined, undefined, 'adobe', 'saved_result'), '等待入库')
  assert.equal(registrationJobStatusText('verification_pending', undefined, undefined, 'adobe', 'saved_result'), '等待校验')
  assert.equal(registrationJobStatusText('registration_pending', { ...waiting, state: 'superseded' }, undefined, 'adobe', 'result_uncertain'), '已发起后续重试')
  assert.equal(registrationFailureText('adobe: human_verification_required', 'adobe'), 'Adobe 人机验证未完成。')
  assert.match(registrationFailureText('adobe: verification_dns_failed', 'adobe'), /域名解析失败/)
  assert.match(registrationFailureText('adobe: verification_request_timeout', 'adobe'), /请求超时/)
  assert.equal(registrationFailureText('adobe: verification_request_aborted', 'adobe'), '验证流程未完成，观察到组件请求取消。')
  assert.equal(registrationFailureText('adobe: submission_result_unknown', 'adobe'), '未取得创建账户的确认结果。')
  assert.equal(registrationFailureText('adobe: registration_country_preparation_failed', 'adobe'), '无法准备并确认本次注册的国家/地区。')
  assert.equal(registrationFailureText('adobe: registration_country_invalid', 'adobe'), '浏览器返回的注册国家/地区无效。')
  assert.equal(registrationFailureText('adobe: saved_registration_country_changed', 'adobe'), '本次注册的国家/地区无法与已保存资料保持一致，已停止执行。')
  assert.equal(registrationFailureText('adobe: future_error', 'adobe'), 'adobe: future_error')
  assert.equal(registrationFailureText('task: registration_result_uncertain', 'adobe'), '')
  assert.equal(registrationFailureText('original OpenAI failure', 'openai'), 'original OpenAI failure')
})

test('Adobe recovery-page failures describe page restoration without claiming account creation failed', async () => {
  const { registrationFailureText, registrationJobStatusText, registrationRecoveryAction } = await import('../src/lib/registrationCompensation.ts')
  for (const code of ['recovery_page_observation_timeout', 'recovery_page_observation_failed', 'recovery_page_closed', 'recovery_page_untrusted']) {
    const text = registrationFailureText(`adobe: ${code}`, 'adobe')
    assert.match(text, /恢复原浏览器页面/)
    assert.doesNotMatch(text, /创建失败|注册失败|重新注册|adobe:/)
  }
  assert.equal(registrationJobStatusText('registration_pending', undefined, undefined, 'adobe', 'resume_session'), '账号待验证')
  assert.equal(registrationRecoveryAction({ status: 'registration_pending', can_resume_registration: true, recovery_mode: 'resume_session', creation_outcome: 'accepted' }, 'adobe'), '继续验证')
  assert.equal(registrationFailureText('mail: timeout', 'adobe'), '未在规定时间内收到 Adobe 邮件验证码。')
})
