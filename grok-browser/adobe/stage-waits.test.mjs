import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runInNewContext } from 'node:vm';
import { BrowserRegistration, formBlockingCode, waitForFormStage } from './browser.mjs';
import { PrivateRunStore } from './core.mjs';

const form = patch => ({ fields: [], headings: [], notices: [], buttons: [], agreements: [], ...patch });
const emailForm = () => form({ fields: [{ tag: 'input', type: 'email', name: 'email', id: 'email', label: 'Email', autocomplete: 'email' }] });
const passwordForm = () => form({ headings: ['Create an account'], fields: [{ tag: 'input', type: 'password', name: 'password', id: 'password', label: 'Password', autocomplete: 'new-password' }] });
const profileForm = () => form({ fields: [{ tag: 'input', type: 'text', name: 'firstName', id: 'firstName', label: 'First name', autocomplete: '' }], buttons: [{ text: 'Create account', disabled: false }] });
const verificationForm = () => form({ headings: ['Verify your email'], fields: [{ tag: 'input', type: 'text', name: 'code', id: 'code', label: 'Verification code', autocomplete: 'one-time-code' }] });

function clock() {
  let milliseconds = 0;
  return { timeoutMs: 1000, pollIntervalMs: 100, now: () => milliseconds,
    pause: async duration => { milliseconds += duration; }, elapsed: () => milliseconds };
}

test('stage waits observe delayed forms through transient blank pages without treating navigation text as an error', async () => {
  const timing = clock();
  const observations = [emailForm(), form({ headings: ['Loading'] }), form({ headings: ['Sign up with email / Already have an account?'] }), passwordForm()];
  let reads = 0;
  const result = await waitForFormStage(async () => observations[reads++], { ...timing, fromStage: 'awaiting_email' });
  assert.equal(result.stage, 'awaiting_password');
  assert.equal(result.code, null);
  assert.equal(reads, 4);
  assert.equal(timing.elapsed(), 300);
});

test('visible challenges and submitted field errors stop promptly, while verification instructions are allowed', async () => {
  for (const [current, code] of [
    [form({ humanVerification: { required: true } }), 'human_verification_required'],
    [emailForm(), null],
    [form({ notices: ['Too many attempts. Try again later.'] }), 'upstream_rate_limited'],
    [form({ notices: ['This email is already registered.'] }), 'account_already_exists'],
    [form({ fields: [{ invalid: true }] }), 'form_rejected'],
    [form({ notices: ['Your verification code is incorrect.'] }), 'form_rejected'],
    [form({ notices: ['Check your email for the verification code.'] }), null],
  ]) assert.equal(formBlockingCode(current), code);
  const timing = clock();
  const result = await waitForFormStage(async () => form({ humanVerification: { required: true } }), timing);
  assert.equal(result.code, 'human_verification_required');
  assert.equal(timing.elapsed(), 0);
});

test('only transient navigation exceptions are retried, with a bounded deadline', async () => {
  let reads = 0;
  const timing = clock();
  const outcome = await waitForFormStage(async () => {
    if (reads++ === 0) throw new Error('Execution context was destroyed');
    return verificationForm();
  }, { ...timing, fromStage: 'awaiting_password' });
  assert.equal(outcome.stage, 'awaiting_verification');
  const closed = new Error('Target page, context or browser has been closed');
  await assert.rejects(waitForFormStage(async () => { throw closed; }, timing), error => error === closed);
  await assert.rejects(waitForFormStage(async () => emailForm(), { ...clock(), fromStage: 'awaiting_email' }), error => error.code === 'stage_transition_timeout');
});

async function fixture(t) {
  const directory = await mkdtemp(join(tmpdir(), 'adobe-stages-'));
  const store = new PrivateRunStore(join(directory, 'private'), 'one');
  await store.init({ email: 'fixture@example.invalid' });
  const browser = new BrowserRegistration(store);
  browser.stageWaitOptions = clock();
  browser.status = async () => {
    const state = await store.load();
    return { stage: state.stage, code: state.code };
  };
  t.after(async () => { await store.close(); await rm(directory, { recursive: true, force: true }); });
  return { store, browser };
}

test('email and password continuation timeouts preserve durable markers and never send a second submission', async t => {
  for (const [current, marker] of [[emailForm(), 'emailSubmitted'], [passwordForm(), 'passwordSubmitted']]) {
    await t.test(marker, async t => {
      const { store, browser } = await fixture(t);
      let clicks = 0;
      const button = { waitFor: async () => {}, count: async () => 1, click: async options => {
        if (options.trial) { assert.equal((await store.load())[marker], undefined); return; }
        assert.equal((await store.load())[marker], true);
        clicks++;
      } };
      const frame = { getByRole: role => role === 'link' ? { count: async () => 0 } : button };
      browser.authFrame = async () => frame;
      browser.form = async () => current;
      browser.field = async () => ({ fill: async () => {} });
      await assert.rejects(browser.advance(), error => error.code === 'continuation_result_unknown');
      const saved = await store.load();
      assert.equal(saved.stage, 'needs_manual');
      assert.equal(saved.code, 'continuation_result_unknown');
      assert.equal(saved[marker], true);
      assert.equal(clicks, 1);
      await assert.rejects(browser.advance(), error => error.code === (marker === 'emailSubmitted' ? 'email_already_submitted' : 'password_already_submitted'));
      assert.equal(clicks, 1);
    });
  }
});

test('creation waits past the old profile form and records unknown on timeout without replay', async t => {
  const { store, browser } = await fixture(t);
  await store.setStage('awaiting_approval');
  let clicks = 0;
  const button = { count: async () => 1, isEnabled: async () => true, click: async () => {
    assert.equal((await store.load()).submission.started, true);
    clicks++;
  } };
  browser.authFrame = async () => ({ getByRole: () => button });
  browser.form = async () => profileForm();
  await assert.rejects(browser.approveCreate(), error => error.code === 'submission_result_unknown');
  const saved = await store.load();
  assert.equal(saved.submission.outcome, 'unknown');
  assert.equal(saved.stage, 'outcome_unknown');
  assert.equal(clicks, 1);
  await assert.rejects(browser.approveCreate(), error => error.code === 'submission_already_started');
  assert.equal(clicks, 1);
});

test('creation accepts the account landing page without claiming identity confirmation or waiting for a timeout', async t => {
  const { store, browser } = await fixture(t);
  await store.setStage('awaiting_approval');
  let clicks = 0;
  browser.authFrame = async () => ({ getByRole: () => ({
    count: async () => 1, isEnabled: async () => true,
    click: async () => { clicks++; assert.equal((await store.load()).submission.started, true); },
  }) });
  browser.form = async () => form({ host: 'account.adobe.com', headings: ['Welcome to Adobe'] });
  assert.deepEqual(await browser.approveCreate(), { stage: 'needs_manual', code: 'firefly_identity_required' });
  const state = await store.load();
  assert.equal(clicks, 1);
  assert.equal(state.submission.started, true);
  assert.equal(state.submission.outcome, 'pending');
  assert.equal(state.result, undefined);
  await assert.rejects(browser.approveCreate(), error => error.code === 'submission_already_started');
  assert.equal(clicks, 1);
});

test('opening Firefly waits through its initial page until IMS is ready or the email verification form appears', async t => {
  for (const destination of ['ims', 'verification']) {
    await t.test(destination, async subtest => {
      const { store, browser } = await fixture(subtest);
      await store.markSubmissionStarted();
      let navigations = 0;
      let observations = 0;
      let readinessChecks = 0;
      const page = {
        goto: async url => { assert.equal(url, 'https://firefly.adobe.com/'); navigations++; },
        evaluate: async () => { readinessChecks++; return destination === 'ims' && observations >= 3; },
      };
      browser.context = { newPage: async () => page };
      browser.form = async () => {
        observations++;
        return destination === 'verification' && observations >= 3
          ? { ...verificationForm(), host: 'auth.services.adobe.com' }
          : form({ host: 'firefly.adobe.com', headings: ['Welcome'] });
      };
      await browser.advance({ action: 'open_firefly' });
      assert.equal(navigations, 1);
      assert.equal(observations, 3);
      assert.equal(readinessChecks, destination === 'ims' ? 3 : 2);
      assert.equal(browser.focusPageOnly, true);
      const state = await store.load();
      assert.equal(state.submission.started, true);
      assert.equal(state.submission.outcome, 'pending');
      assert.equal(state.verificationSubmitted, undefined);
      assert.equal(state.result, undefined);
    });
  }
});

test('a Firefly welcome page without IMS readiness times out without another navigation or a success claim', async t => {
  const { store, browser } = await fixture(t);
  await store.markSubmissionStarted();
  let navigations = 0;
  const page = { goto: async () => { navigations++; }, evaluate: async () => false };
  browser.context = { newPage: async () => page };
  browser.form = async () => form({ host: 'firefly.adobe.com', headings: ['Welcome'] });
  await assert.rejects(browser.advance({ action: 'open_firefly' }), error => error.code === 'stage_transition_timeout');
  assert.equal(navigations, 1);
  assert.equal((await store.load()).submission.outcome, 'pending');
  assert.equal((await store.load()).result, undefined);
});

test('profile completion waits for the create button to become enabled and stops before creation', async t => {
  const { store, browser } = await fixture(t);
  const fields = ['firstName', 'lastName', 'birthMonth', 'birthYear', 'country']
    .map(name => ({ tag: 'input', type: 'text', id: name, name, label: name, autocomplete: '' }));
  let reads = 0;
  let fills = 0;
  browser.form = async () => form({ fields, buttons: [{ text: 'Create account', disabled: ++reads < 3 }] });
  browser.authFrame = async () => ({ getByRole: () => ({ count: async () => 0, click: async () => assert.fail('Creation requires separate approval') }) });
  browser.field = async () => ({ fill: async () => { fills++; } });
  const result = await browser.advance({ profile: {
    firstName: 'Fixture', lastName: 'Test', birthMonth: 5, birthYear: 1985, country: 'United States',
  } });
  assert.equal(fills, 5);
  assert.equal(reads, 3);
  assert.deepEqual(result, { stage: 'awaiting_approval', code: 'account_agreement_required' });
  assert.equal((await store.load()).submission.started, false);
});

test('OTP auto submission waits for the resulting page and never adds a second Verify click', async t => {
  const { store, browser } = await fixture(t);
  await store.markSubmissionStarted();
  let fills = 0;
  let reads = 0;
  browser.authFrame = async () => ({ getByRole: () => assert.fail('Do not add a Verify click after fill') });
  browser.field = async () => ({ fill: async () => {
    assert.equal((await store.load()).verificationSubmitted, true);
    fills++;
  } });
  browser.form = async () => ++reads <= 2 ? verificationForm() : form({ host: 'firefly.adobe.com' });
  browser.page = { evaluate: async () => true };
  await browser.verify({ code: '123456' });
  assert.equal(fills, 1);
  assert.equal((await store.load()).verificationSubmitted, true);
  browser.form = async () => verificationForm();
  await assert.rejects(browser.verify({ code: '123456' }), error => error.code === 'verification_already_submitted');
  assert.equal(fills, 1);
});

test('a verification navigation timeout retains its marker and records an unknown result', async t => {
  const { store, browser } = await fixture(t);
  await store.markSubmissionStarted();
  let navigations = 0;
  browser.page = { goto: async () => { navigations++; throw new Error('simulated navigation timeout'); } };
  browser.form = async () => verificationForm();
  await assert.rejects(browser.verify({ verificationURL: 'https://account.adobe.com/' }), error => error.code === 'verification_result_unknown');
  const state = await store.load();
  assert.equal(state.verificationSubmitted, true);
  assert.equal(state.code, 'verification_result_unknown');
  await assert.rejects(browser.verify({ verificationURL: 'https://account.adobe.com/' }), error => error.code === 'verification_already_submitted');
  assert.equal(navigations, 1);
});

test('OTP fill navigation errors still observe matching Firefly identity without another fill or click', async t => {
  const { store, browser } = await fixture(t);
  await store.markSubmissionStarted();
  let fills = 0;
  let observations = 0;
  let identityReads = 0;
  browser.authFrame = async () => ({ getByRole: () => assert.fail('Never click Verify after filling the code') });
  browser.field = async () => ({ fill: async () => {
    assert.equal((await store.load()).verificationSubmitted, true);
    fills++;
    throw new Error('Execution context was destroyed during navigation');
  } });
  browser.form = async () => {
    if (fills === 0) return verificationForm();
    observations++;
    if (observations === 1) throw new Error('Execution context was destroyed');
    return form({ host: 'firefly.adobe.com', headings: ['Welcome'] });
  };
  browser.page = { evaluate: (fn, email) => runInNewContext(`(${fn.toString()})(expectedEmail)`, {
    expectedEmail: email,
    window: { adobeIMS: {
      isSignedInUser: async () => true,
      getProfile: async () => ++identityReads < 2 ? null : {
        email: 'fixture@example.invalid', userId: 'synthetic-user', emailVerified: true,
      },
      getAccessToken: () => assert.fail('Readiness observation must not collect tokens'),
    } },
  }) };
  const result = await browser.verify({ code: '123456' });
  assert.equal(fills, 1);
  assert.equal(identityReads, 2);
  assert.equal(observations, 3);
  assert.notEqual(result.code, 'verification_result_unknown');
  const state = await store.load();
  assert.equal(state.verificationSubmitted, true);
  assert.equal(state.submission.outcome, 'pending');
  assert.notEqual(state.stage, 'completed');
  assert.equal(state.result, undefined);
  browser.form = async () => verificationForm();
  await assert.rejects(browser.verify({ code: '654321' }), error => error.code === 'verification_already_submitted');
  assert.equal(fills, 1);
});

test('a fill error followed only by Firefly title without matching identity remains unknown and cannot be replayed', async t => {
  const { store, browser } = await fixture(t);
  await store.markSubmissionStarted();
  let fills = 0;
  browser.authFrame = async () => ({ getByRole: () => assert.fail('Do not click another verification control') });
  browser.field = async () => ({ fill: async () => { fills++; throw new Error('navigation interrupted'); } });
  browser.form = async () => fills ? form({ host: 'firefly.adobe.com', headings: ['Welcome'] }) : verificationForm();
  browser.page = { evaluate: (fn, email) => runInNewContext(`(${fn.toString()})(expectedEmail)`, {
    expectedEmail: email,
    window: { adobeIMS: {
      isSignedInUser: async () => true,
      getProfile: async () => ({ email: 'someone-else@example.invalid', userId: 'synthetic-user', emailVerified: true }),
      getAccessToken: () => assert.fail('Readiness observation must not collect tokens'),
    } },
  }) };
  await assert.rejects(browser.verify({ code: '123456' }), error => error.code === 'verification_result_unknown');
  assert.equal(fills, 1);
  const state = await store.load();
  assert.equal(state.verificationSubmitted, true);
  assert.equal(state.code, 'verification_result_unknown');
  assert.equal(state.result, undefined);
  browser.form = async () => verificationForm();
  await assert.rejects(browser.verify({ code: '654321' }), error => error.code === 'verification_already_submitted');
  assert.equal(fills, 1);
});
