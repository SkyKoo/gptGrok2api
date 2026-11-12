import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { BrowserRegistration } from './browser.mjs';
import { PrivateRunStore } from './core.mjs';
import { creationOutcome } from './creation-outcome.mjs';
import { runAutomaticRegistration } from './auto-runner.mjs';
import { registrationDiagnostics } from './registration-diagnostics.mjs';

const profile = { firstName: 'Fixture', lastName: 'Test', birthMonth: 2, birthYear: 1984, birthDay: 29, country: 'KR' };
const fieldNames = ['firstName', 'lastName', 'birthMonth', 'birthYear', 'country'];
const options = { approveCreate: true, resumeBeforeSubmission: true, readInput: async () => ({ profile }) };

// 使用真实状态仓库、资料驱动和自动调度；仅替换页面/控件 I/O，避免真实注册。
async function fixture(t, decorate = (_state, fields) => fields) {
  const directory = await mkdtemp(join(tmpdir(), 'adobe-runner-profile-'));
  const store = new PrivateRunStore(join(directory, 'private'), 'one');
  await store.init({ email: 'fixture@example.invalid' });
  await store.setStage('form_ready');
  const browser = new BrowserRegistration(store);
  let elapsed = 0;
  browser.stageWaitOptions = { timeoutMs: 1000, pollIntervalMs: 100, now: () => elapsed,
    pause: async duration => { elapsed += duration; } };
  browser.workerMode = true;
  browser.page = { url: () => 'https://auth.services.adobe.com/' };
  browser.context = { pages: () => [browser.page] };
  browser.networkStatus = async () => ({ code: null });
  const values = {};
  const writes = [];
  let clicks = 0;
  const button = { count: async () => 1, isEnabled: async () => true, click: async () => {
    assert.equal((await store.load()).submission.started, true);
    clicks++;
  } };
  browser.authFrame = async () => ({ getByRole: role => role === 'button' ? button : { count: async () => 0 } });
  browser.field = async (_frame, descriptor) => ({
    fill: async value => { writes.push(descriptor.name); values[descriptor.name] = value; },
    inputValue: async () => values[descriptor.name] || '',
  });
  browser.form = async () => ({ host: 'auth.services.adobe.com', headings: [], notices: [], agreements: [],
    buttons: [{ text: 'Create account', disabled: false }],
    fields: decorate({ stored: await store.load(), writes, clicks }, fieldNames.map(name => ({
      tag: 'input', type: 'text', id: name, name, label: name, required: true,
      filled: !!values[name], invalid: false,
    }))),
  });
  t.after(async () => { await store.close(); await rm(directory, { recursive: true, force: true }); });
  return { store, browser, writes, clicks: () => clicks };
}

test('option timeout keeps its field diagnosis through the real driver and runner without submission', async t => {
  const f = await fixture(t, (_state, fields) => fields.map(field => field.name === 'country'
    ? {...field, tag:'select', type:'select-one'} : field));
  f.browser.field = async () => ({ locator: () => ({ evaluateAll: async () => [{value:'',label:'PRIVATE'}] }) });
  const result = await runAutomaticRegistration(f.browser, f.store, options);
  assert.equal(result.code, 'profile_option_invalid');
  const diagnostic = registrationDiagnostics(result, await f.store.load());
  assert.equal(diagnostic.profile_field, 'country');
  assert.equal(diagnostic.profile_option_reason, 'missing');
  assert.equal(diagnostic.option_count, 1);
  assert.equal(diagnostic.submission_started, false);
  assert.doesNotMatch(JSON.stringify(diagnostic), /PRIVATE|options|label/);
  assert.equal(f.clicks(), 0);
  assert.deepEqual(f.writes, []);
});

test('runner retains a late required profile field rejection without refilling or submitting', async t => {
  const f = await fixture(t, ({ writes }, fields) => [...fields, ...(writes.length === 5 ? [{
    tag: 'input', type: 'text', id: 'day', name: 'day', label: 'Day',
    required: true, filled: false, invalid: false, validity: { valueMissing: true },
  }] : [])]);
  const result = await runAutomaticRegistration(f.browser, f.store, options);
  assert.equal(result.code, 'form_rejected');
  assert.equal(result.form.fields.find(field => field.name === 'day').invalid, true);
  assert.deepEqual(result.failureDiagnostics, { form_error: 'invalid_field', invalid_fields: ['birth_day'],
    invalid_field_reasons: [{ field: 'birth_day', reason: 'required' }] });
  assert.deepEqual(result.automatic, { paused: true, steps: 1 });
  assert.equal(result.submissionStarted, false);
  assert.equal((await f.store.load()).submission.started, false);
  assert.equal((await f.store.load()).code, 'form_rejected');
  assert.deepEqual(f.writes, ['country', 'firstName', 'lastName', 'birthMonth', 'birthYear']);
  assert.equal(f.clicks(), 0);
});

test('runner retains the immediate creation preflight rejection and leaves submission unstarted', async t => {
  const f = await fixture(t, ({ stored }, fields) => [...fields, ...(stored.stage === 'awaiting_approval' || stored.code === 'form_rejected' ? [{
    tag: 'input', type: 'checkbox', id: 'terms', name: 'terms', label: 'Terms agreement',
    required: true, filled: true, checked: false, invalid: false,
  }] : [])]);
  const result = await runAutomaticRegistration(f.browser, f.store, options);
  assert.equal(result.code, 'form_rejected');
  assert.equal(result.form.fields.find(field => field.name === 'terms').invalid, true);
  assert.deepEqual(result.failureDiagnostics, { form_error: 'invalid_field', invalid_fields: ['terms'],
    invalid_field_reasons: [{ field: 'terms', reason: 'unchecked' }] });
  assert.deepEqual(result.automatic, { paused: true, steps: 2 });
  assert.equal(result.submissionStarted, false);
  assert.equal((await f.store.load()).submission.started, false);
  assert.deepEqual(f.writes, ['country', 'firstName', 'lastName', 'birthMonth', 'birthYear']);
  assert.equal(f.clicks(), 0);
});

test('an explicit rejection after Create retains its observed form and durable submission fence', async t => {
  let rejectionSeen = false;
  const f = await fixture(t, ({ clicks }, fields) => {
    if (!clicks || rejectionSeen) return fields;
    rejectionSeen = true;
    return fields.map(field => field.name === 'birthMonth'
      ? { ...field, invalid: true, validity: { rangeOverflow: true } } : field);
  });
  const result = await runAutomaticRegistration(f.browser, f.store, options);
  assert.equal(result.code, 'form_rejected');
  assert.equal(result.form.fields.find(field => field.name === 'birthMonth').invalid, true);
  assert.deepEqual(result.failureDiagnostics, { form_error: 'invalid_field', invalid_fields: ['birth_month'],
    invalid_field_reasons: [{ field: 'birth_month', reason: 'range' }] });
  assert.equal(result.submissionStarted, true);
  assert.deepEqual(result.automatic, { paused: true, steps: 2 });
  const state = await f.store.load();
  assert.equal(state.submission.started, true);
  assert.equal(state.code, 'form_rejected');
  assert.equal(creationOutcome(state).creation_outcome, 'unknown', 'A UI rejection alone does not certify an upstream rejection');
  assert.equal(f.clicks(), 1);
  const retry = await runAutomaticRegistration(f.browser, f.store, options);
  assert.equal(retry.code, 'form_rejected');
  assert.equal(retry.submissionStarted, true);
  assert.equal(retry.automatic.steps, 0);
  assert.equal(f.clicks(), 1);
  assert.deepEqual(f.writes, ['country', 'firstName', 'lastName', 'birthMonth', 'birthYear']);
});
