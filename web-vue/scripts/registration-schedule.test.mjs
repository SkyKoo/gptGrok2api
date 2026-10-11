import test from 'node:test'
import assert from 'node:assert/strict'
import { registrationScheduleResult, validRegistrationInterval } from '../src/lib/registrationSchedule.ts'

test('only whole minute intervals within bounds are accepted', () => {
  for (const value of [1, 60, 10080]) assert.equal(validRegistrationInterval(value), true)
  for (const value of [0, -1, 1.5, 10081, NaN, Infinity, '60']) assert.equal(validRegistrationInterval(value), false)
})
test('schedule outcomes distinguish started, skipped and failed without claiming completion', () => {
  const state = { last_error: '', last_result: 'started' }
  assert.match(registrationScheduleResult(state), /已启动一批/)
  assert.match(registrationScheduleResult({ ...state, last_error: 'task: already_running' }), /跳过/)
  assert.match(registrationScheduleResult({ ...state, last_error: 'task: maintenance' }), /维护/)
  assert.match(registrationScheduleResult({ ...state, last_error: 'task: history_full_archive_completed_jobs_first' }), /待恢复任务会保留/)
  assert.match(registrationScheduleResult({ ...state, last_error: 'storage: schedule_state_write_failed' }), /存储/)
  assert.match(registrationScheduleResult({ ...state, last_error: 'config: use_total_mode' }), /批次配置/)
  assert.match(registrationScheduleResult({ ...state, last_result: 'pending' }), /不会重复补跑/)
  assert.equal(registrationScheduleResult({ ...state, last_result: '' }), '')
})
