import test from 'node:test';
import assert from 'node:assert/strict';
import { StartupProgress, startupExceptionType } from './startup-progress.mjs';

test('startup stream reports ordered steps with monotonic elapsed and per-step durations', async () => {
  let clock = 100;
  const frames = [], progress = new StartupProgress({ emit: frame => frames.push(frame), now: () => clock });
  assert.equal(await progress.run('starting_browser', async () => { clock += 25; return 'browser'; }), 'browser');
  await progress.run('adobe_loading_account_page', async () => { clock += 45; });
  assert.deepEqual(frames, [
    { sequence: 1, stage: 'starting_browser', status: 'started', elapsed_ms: 0, duration_ms: 0 },
    { sequence: 2, stage: 'starting_browser', status: 'completed', elapsed_ms: 25, duration_ms: 25 },
    { sequence: 3, stage: 'adobe_loading_account_page', status: 'started', elapsed_ms: 25, duration_ms: 0 },
    { sequence: 4, stage: 'adobe_loading_account_page', status: 'completed', elapsed_ms: 70, duration_ms: 45 },
  ]);
  clock = 90;
  await progress.run('adobe_waiting_create_entry', async () => {});
  assert.equal(frames.at(-1).elapsed_ms, 70);
  assert.equal(frames.at(-1).duration_ms, 0);
});

test('failed step preserves original error but publishes and retains only fixed diagnostics', async () => {
  let clock = 0;
  const frames = [], progress = new StartupProgress({ emit: frame => frames.push(frame), now: () => clock });
  const error = Object.assign(new Error('PRIVATE https://secret.invalid/?token=PRIVATE'), { name: 'TimeoutError' });
  await assert.rejects(progress.run('adobe_waiting_registration_form', async () => { clock = 20_000; throw error; }), actual => actual === error);
  assert.deepEqual(frames.at(-1), { sequence: 2, stage: 'adobe_waiting_registration_form', status: 'failed', elapsed_ms: 20_000, duration_ms: 20_000, exception_type: 'timeout' });
  clock = 21_000;
  assert.deepEqual(progress.failureDetails(error), { failure_action: 'adobe_waiting_registration_form', failure_phase: 'before_submission', exception_type: 'timeout', elapsed_ms: 20_000, duration_ms: 20_000 });
  assert.doesNotMatch(JSON.stringify({ frames, failure: progress.failureDetails(error) }), /PRIVATE|secret\.invalid/);
});

test('startup telemetry is bounded and never changes operation execution', async () => {
  let calls = 0, clock = 0;
  const frames = [], progress = new StartupProgress({ emit: frame => frames.push(frame), now: () => clock });
  for (let i = 0; i < 20; i++) await progress.run('starting_browser', async () => { calls++; clock++; });
  assert.equal(calls, 20); assert.equal(frames.length, 32);
  assert.deepEqual(frames.map(frame => frame.sequence), Array.from({ length: 32 }, (_, i) => i + 1));
  const silent = new StartupProgress({ emit: () => { throw new Error('telemetry failed'); } });
  assert.equal(await silent.run('starting_browser', async () => 'unchanged'), 'unchanged');
  await assert.rejects(progress.run('PRIVATE', async () => assert.fail('unknown stage ran')), /invalid_startup_stage/);
});

test('elapsed values outside the init deadline are omitted rather than clipped', async () => {
  let clock = 0;
  const frames = [], progress = new StartupProgress({ emit: frame => frames.push(frame), now: () => clock });
  const error = new Error('late');
  await assert.rejects(progress.run('starting_browser', async () => { clock = 150_001; throw error; }));
  assert.equal(frames.length, 1);
  assert.deepEqual(progress.failureDetails(error), { failure_action: 'starting_browser', failure_phase: 'before_submission', exception_type: 'unknown' });
});

test('exception categories are fixed even when browser errors contain private context', () => {
  for (const [error, expected] of [
    [{ code: 'browser_launch_timeout' }, 'timeout'],
    [{ code: 'stage_transition_timeout' }, 'timeout'],
    [new Error('PRIVATE Execution context was destroyed'), 'navigation'],
    [new Error('PRIVATE Target page, context or browser has been closed'), 'page_closed'],
    [{ code: 'browser_launch_license_denied', message: 'PRIVATE' }, 'registration'],
    [{ code: 'PRIVATE URL', message: 'PRIVATE' }, 'unknown'],
  ]) assert.equal(startupExceptionType(error), expected);
});
