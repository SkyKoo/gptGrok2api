import assert from 'node:assert/strict'
import test from 'node:test'
import { createReloginTracker, reloginElapsed, reloginErrorText, reloginStep } from '../src/lib/accountRelogin.ts'
const task = (ref, stage = 'wait_code', id = ref) => ({ progress_id: id, account_ref: ref, email: `${ref}@example.test`, stage, started_at: '2026-10-11T01:00:00Z', updated_at: '2026-10-11T01:00:01Z', done: stage === 'completed' || stage === 'failed' })
const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r }); return { promise, resolve } }

test('concurrent accounts remain independent and repeated start is suppressed', async () => {
  let state, completions = 0, posts = []
  const a = deferred(), b = deferred()
  let upstream = []
  const tracker = createReloginTracker({ start: ref => { posts.push(ref); return (ref === 'a' ? a : b).promise }, list: async () => upstream }, value => { state = value }, () => { completions++ })
  const first = tracker.start('a'), second = tracker.start('b')
  await tracker.start('a')
  assert.deepEqual(posts, ['a', 'b'])
  assert.ok(tracker.busy('a') && tracker.busy('b'))
  a.resolve(task('a')); b.resolve(task('b')); await Promise.all([first, second])
  upstream = [task('a', 'completed'), task('b')]
  await tracker.sync()
  assert.equal(completions, 1)
  assert.equal(tracker.busy('a'), false)
  assert.equal(tracker.busy('b'), true)
  upstream = [task('a', 'completed'), { ...task('b', 'failed'), failed_stage: 'validate_code', error: 'validate_code: unexpected_status' }]
  await tracker.sync()
  assert.equal(state.tasks.length, 2)
  assert.equal(state.tasks.find(t => t.account_ref === 'b').failed_stage, 'validate_code')
  assert.equal(completions, 2)
  assert.equal(tracker.busy('b'), false)
})

test('page reopening restores active tasks with GET only; disconnect preserves busy status', async () => {
  let state, fail = false, posts = 0
  const tracker = createReloginTracker({ start: async () => { posts++; return task('a') }, list: async () => { if (fail) throw Error('offline'); return [task('a'), task('b')] } }, value => { state = value }, () => {})
  await tracker.sync()
  assert.equal(posts, 0)
  assert.equal(state.tasks.length, 2)
  fail = true; await tracker.sync()
  assert.equal(state.pollError, true)
  assert.ok(tracker.busy('a') && tracker.busy('b'))
  fail = false; await tracker.sync()
  assert.equal(state.pollError, false)
  assert.equal(posts, 0)
})

test('late start response cannot overwrite completion seen by polling', async () => {
  const response = deferred()
  let state
  const tracker = createReloginTracker({ start: () => response.promise, list: async () => [{ ...task('a', 'completed'), updated_at: '2026-10-11T01:00:05Z' }] }, value => { state = value }, () => {})
  const start = tracker.start('a')
  await tracker.sync()
  response.resolve(task('a', 'queued')); await start
  assert.equal(state.tasks[0].stage, 'completed')
  assert.equal(tracker.busy('a'), false)
})

test('overlapping list/start does not lose the newly submitted task', async () => {
  const response = deferred()
  let state
  const tracker = createReloginTracker({ start: async ref => task(ref), list: () => response.promise }, value => { state = value }, () => {})
  const read = tracker.sync()
  await tracker.start('a')
  response.resolve([]); await read
  assert.equal(state.tasks[0].done, false)
  assert.equal(tracker.busy('a'), true)
})

test('missing active task after restart is shown as unknown, never retried automatically', async () => {
  let incoming = [task('a')], state
  const tracker = createReloginTracker({ start: async () => { throw Error('must not POST') }, list: async () => incoming }, value => { state = value }, () => {})
  await tracker.sync(); incoming = []; await tracker.sync()
  assert.equal(state.tasks[0].unavailable, true)
  assert.equal(state.tasks[0].error, undefined)
  assert.match(reloginErrorText(state.tasks[0]), /无法确认/)
})

test('explicit retry creates a new task while preserving the old failure', async () => {
  let state, count = 0
  const tracker = createReloginTracker({ start: async ref => { count++; return task(ref, 'queued', 'new') }, list: async () => [{ ...task('a', 'failed', 'old'), error: 'task_timeout' }] }, value => { state = value }, () => {})
  await tracker.sync(); await tracker.start('a'); await tracker.start('a')
  assert.equal(count, 1)
  assert.equal(state.tasks.length, 2)
  assert.equal(state.tasks.find(t => t.progress_id === 'old').error, 'task_timeout')
})

test('elapsed time freezes at completion and steps reflect the failed stage', () => {
  const failed = { ...task('a', 'failed'), failed_stage: 'validate_code', finished_at: '2026-10-11T01:01:05Z' }
  assert.equal(reloginElapsed(failed, Date.now()), '1 分 5 秒')
  assert.equal(reloginStep(failed), 3)
  assert.equal(reloginStep(task('b', 'completed')), 5)
})


test('automatically completed task between polls refreshes accounts without a new POST', async () => {
  let incoming = [], completions = 0
  const tracker = createReloginTracker({ start: async () => { throw Error('must not POST') }, list: async () => incoming }, () => {}, () => { completions++ })
  await tracker.sync()
  incoming = [{ ...task('a', 'completed'), automatic: true, session_updated: true, quotas_refreshed: true }]
  await tracker.sync()
  assert.equal(completions, 1)
  await tracker.sync()
  assert.equal(completions, 1)
})

test('partial recovery explains that new credentials are kept but quotas need refreshing', () => {
  const partial = { ...task('a', 'failed'), session_updated: true, failed_stage: 'refresh_account', error: 'account_refresh_failed' }
  assert.match(reloginErrorText(partial), /新登录凭据已保存/)
  assert.doesNotMatch(reloginErrorText(partial), /原账号凭据保持不变/)
  assert.match(reloginErrorText(partial), /异常.*暂停调度/)
  assert.equal(reloginStep(partial), 4)
})
