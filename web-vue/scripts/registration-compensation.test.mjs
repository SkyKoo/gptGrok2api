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
