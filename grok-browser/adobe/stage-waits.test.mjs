import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runInNewContext } from 'node:vm';
import { BrowserRegistration, formBlockingCode, formFailureDiagnostics, waitForFormStage } from './browser.mjs';
import { PrivateRunStore } from './core.mjs';
import { NetworkDiagnostics } from './network-diagnostics.mjs';
import { runAutomaticRegistration } from './auto-runner.mjs';

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

test('normal stage progress may take longer than thirty seconds but still has a sixty-second bound', async () => {
  for (const resolves of [true, false]) {
    let elapsed = 0;
    const pending = waitForFormStage(async () => resolves && elapsed >= 40_000 ? verificationForm() : profileForm(), {
      fromStage: 'awaiting_profile', pollIntervalMs: 1000,
      now: () => elapsed, pause: async ms => { elapsed += ms; },
    });
    if (resolves) {
      assert.equal((await pending).stage, 'awaiting_verification');
      assert.equal(elapsed, 40_000);
    } else {
      await assert.rejects(pending, { code: 'stage_transition_timeout' });
      assert.equal(elapsed, 60_000);
    }
  }
});

test('visible challenges and submitted field errors stop promptly, while verification instructions are allowed', async () => {
  for (const [current, code] of [
    [form({ humanVerification: { required: true } }), 'human_verification_required'],
    [emailForm(), null],
    [form({ notices: ['Too many attempts. Try again later.'] }), 'upstream_rate_limited'],
    [form({ notices: ['This email is already registered.'] }), 'account_already_exists'],
    [form({ fields: [{ invalid: true }] }), 'form_rejected'],
    [form({ notices: ['Your verification code is incorrect.'] }), 'form_rejected'],
    [form({ notices: ['Account does not exist.'] }), 'form_rejected'],
    [form({ notices: ['No account found.'] }), 'form_rejected'],
    [form({ notices: ['Wrong password.'] }), 'form_rejected'],
    [form({ notices: ['Check your email for the verification code.'] }), null],
    [form({ notices: ['Sign up with email / Already have an account?'] }), null],
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

test('rejection diagnostics classify explicit visible errors without treating normal headings as a rejection', () => {
  for (const [notice, expected] of [
    ["We couldn't find an account with this email.", 'account_not_found'],
    ['Please enter a valid email address.', 'invalid_email'],
    ['The password is incorrect.', 'invalid_password'],
    ['Your verification code is incorrect.', 'invalid_verification_code'],
    ['An unexpected error occurred.', 'generic_notice'],
  ]) {
    assert.equal(formFailureDiagnostics(form({notices:[notice]})).form_error, expected);
    assert.deepEqual(formFailureDiagnostics(form({headings:[notice]})), {});
  }
  assert.deepEqual(formFailureDiagnostics(form({notices:['Sign up with email / Already have an account?']})), {});
  assert.deepEqual(formFailureDiagnostics(form({fields:[
    {type:'email', invalid:true}, {type:'password', invalid:true},
    {autocomplete:'one-time-code',invalid:true}, {name:'birthYear',invalid:true},
    {name:'SECRET',invalid:true}, {type:'email',invalid:true},
  ]})), {form_error:'invalid_field',invalid_fields:['email','password','verification_code','birth_year','other']});
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

test('a Create navigation interruption observes its result without losing the submission fence or clicking again', async t => {
  for (const destination of ['account', 'verification', 'stalled']) await t.test(destination, async subtest => {
    const { store, browser } = await fixture(subtest);
    await store.setStage('awaiting_approval');
    let clicks = 0, reads = 0;
    browser.page = { url: () => 'https://auth.services.adobe.com/' };
    browser.authFrame = async () => ({ getByRole: () => ({
      count: async () => 1, isEnabled: async () => true,
      click: async () => {
        clicks++;
        assert.equal((await store.load()).submission.started, true);
        throw new Error('PRIVATE Frame was detached');
      },
    }) });
    browser.form = async () => {
      if (!clicks || destination === 'stalled') return profileForm();
      if (++reads === 1) throw new Error('PRIVATE Execution context was destroyed');
      return destination === 'verification' ? verificationForm() : form({ host: 'account.adobe.com' });
    };
    if (destination === 'stalled') await assert.rejects(browser.approveCreate(), { code: 'submission_result_unknown' });
    else {
      const result = await browser.approveCreate();
      assert.equal(result.code, destination === 'account' ? 'firefly_identity_required' : 'creation_needs_verification');
      assert.equal(reads, 2);
    }
    const state = await store.load();
    assert.equal(state.submission.started, true);
    assert.equal(state.result, undefined);
    assert.equal(state.submission.outcome, destination === 'stalled' ? 'unknown' : 'pending');
    await assert.rejects(browser.approveCreate(), { code: 'submission_already_started' });
    assert.equal(clicks, 1);
    assert.doesNotMatch(JSON.stringify(state), /PRIVATE/);
  });
});

test('unknown, closed and external-page Create errors stop without observing a speculative result', async t => {
  for (const kind of ['unknown', 'closed', 'external']) await t.test(kind, async subtest => {
    const { store, browser } = await fixture(subtest);
    await store.setStage('awaiting_approval');
    let clicks = 0;
    browser.page = { url: () => kind === 'external' ? 'https://external.example.invalid/' : 'https://auth.services.adobe.com/' };
    browser.authFrame = async () => ({ getByRole: () => ({
      count: async () => 1, isEnabled: async () => true,
      click: async () => {
        clicks++;
        throw new Error(kind === 'closed' ? 'Target page, context or browser has been closed' :
          kind === 'external' ? 'Execution context was destroyed' : 'PRIVATE unexpected error');
      },
    }) });
    browser.form = async () => profileForm();
    browser.waitForStage = () => assert.fail('Only recognized Adobe navigation interruptions may be re-observed');
    await assert.rejects(browser.approveCreate(), { code: 'submission_result_unknown' });
    assert.equal((await store.load()).submission.outcome, 'unknown');
    assert.equal(clicks, 1);
  });
});

test('status re-reads a navigating snapshot and retains the original submission state', async t => {
  const { store, browser } = await fixture(t);
  await store.markSubmissionStarted();
  browser.status = BrowserRegistration.prototype.status.bind(browser);
  browser.workerMode = true;
  browser.statusWaitOptions = clock();
  let reads = 0, url = 'https://auth.services.adobe.com/';
  browser.page = { url: () => url };
  browser.context = { pages: () => [browser.page] };
  browser.networkStatus = async () => ({ code: null });
  browser.form = async () => {
    if (++reads === 1) throw new Error('Execution context was destroyed');
    if (reads === 2) { url = 'about:blank'; throw new Error('Frame was detached'); }
    url = 'https://account.adobe.com/';
    return form({ host: 'account.adobe.com' });
  };
  const status = await browser.status();
  assert.equal(reads, 3);
  assert.equal(status.form.host, 'account.adobe.com');
  assert.equal(status.submissionStarted, true);
  assert.equal((await store.load()).result, undefined);
  assert.equal(browser.statusWaitOptions.elapsed(), 200);
});

test('status navigation observation is bounded and never hides closed, unknown or external-page errors', async t => {
  for (const kind of ['stalled', 'never_resolves', 'closed', 'unknown', 'external']) await t.test(kind, async subtest => {
    const { browser } = await fixture(subtest);
    browser.status = BrowserRegistration.prototype.status.bind(browser);
    browser.statusWaitOptions = { ...clock(), ...(kind === 'never_resolves' ? { timeoutMs: 20, pollIntervalMs: 1 } : {}) };
    browser.page = { url: () => kind === 'external' ? 'https://external.example.invalid/' : 'https://auth.services.adobe.com/' };
    let reads = 0;
    const error = new Error(kind === 'closed' ? 'Target page, context or browser has been closed' :
      kind === 'unknown' ? 'PRIVATE unexpected error' : 'Execution context was destroyed');
    browser.readStatus = async () => { reads++; if (kind === 'never_resolves' && reads > 1) return new Promise(() => {}); throw error; };
    await assert.rejects(browser.status(), failure => ['stalled', 'never_resolves'].includes(kind)
      ? failure.code === 'stage_transition_timeout' : failure === error);
    assert.equal(reads, kind === 'stalled' ? 10 : kind === 'never_resolves' ? 2 : 1);
  });
});

test('normal status reads and CLI screenshots retain their original timeouts outside navigation observation', async t => {
  for (const navigation of [false, true]) await t.test(String(navigation), async subtest => {
    const { browser } = await fixture(subtest);
    browser.status = BrowserRegistration.prototype.status.bind(browser);
    browser.statusWaitOptions = { timeoutMs: 20, pollIntervalMs: 1 };
    let reads = 0, screenshots = 0;
    browser.page = { url: () => 'https://account.adobe.com/', screenshot: async options => {
      assert.equal(options.timeout, 10000);
      screenshots++;
      await new Promise(resolve => setTimeout(resolve, 40));
      throw new Error('Optional screenshot failed');
    } };
    browser.context = { pages: () => [browser.page] };
    browser.networkStatus = async () => ({ code: null });
    browser.form = async () => {
      if (++reads === 1) {
        if (navigation) throw new Error('Execution context was destroyed');
        await new Promise(resolve => setTimeout(resolve, 40));
      }
      return form({ host: 'account.adobe.com' });
    };
    const status = await browser.status();
    assert.equal(status.form.host, 'account.adobe.com');
    assert.equal(reads, navigation ? 2 : 1);
    assert.equal(screenshots, 1);
    assert.match(status.screenshot, /current\.png$/);
  });
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
  let readyReads = 0;
  const values = {};
  browser.form = async () => {
    reads++;
    if (fills === 5) readyReads++;
    return form({ fields, buttons: [{ text: 'Create account', disabled: readyReads < 3 }] });
  };
  browser.authFrame = async () => ({ getByRole: () => ({ count: async () => 0, click: async () => assert.fail('Creation requires separate approval') }) });
  browser.field = async (_frame, descriptor) => ({
    fill: async value => { fills++; values[descriptor.name] = value; },
    inputValue: async () => values[descriptor.name] || '',
  });
  const result = await browser.advance({ profile: {
    firstName: 'Fixture', lastName: 'Test', birthMonth: 5, birthYear: 1985, country: 'United States',
  } });
  assert.equal(fills, 5);
  assert.equal(readyReads, 3);
  assert.ok(reads > readyReads, 'Country transition is observed before filling other controls');
  assert.deepEqual(result, { stage: 'awaiting_approval', code: 'account_agreement_required' });
  assert.equal((await store.load()).submission.started, false);
});

test('profile keeps the generated day and fills it only when the form requests it', async t => {
  for (const hasDay of [false, true]) {
    const { store, browser } = await fixture(t);
    const names = ['firstName', 'lastName', 'birthMonth', 'birthYear', 'country', ...(hasDay ? ['birthDay'] : [])];
    const fields = names.map(name => ({ tag: 'input', type: 'text', id: name, name, label: name === 'birthMonth' ? 'Birthday month' : name, autocomplete: '' }));
    const filled = {};
    browser.form = async () => form({fields, buttons:[{text:'Create account', disabled:false}]});
    browser.authFrame = async () => ({getByRole:()=>({count:async()=>0})});
    browser.field = async (_frame, descriptor) => ({fill:async value => { filled[descriptor.name] = value; }, inputValue:async()=>filled[descriptor.name] || ''});
    const profile = {firstName:'Fixture',lastName:'Test',birthYear:1984,birthMonth:2,birthDay:29,country:'US'};
    await browser.advance({profile});
    assert.equal(filled.birthMonth, '2');
    assert.equal(filled.birthDay, hasDay ? '29' : undefined);
    assert.deepEqual((await store.load()).config.profile, profile);
    assert.equal((await store.load()).submission.started, false);
  }
});

test('country changes asynchronously replace profile fields and add a day before a single fill pass', async t => {
  const { store, browser } = await fixture(t);
  const timing = clock();
  browser.stageWaitOptions = timing;
  const values = { country: 'US' };
  const writes = [];
  let changedAt;
  browser.form = async () => {
    const regional = changedAt !== undefined && timing.elapsed() - changedAt >= 200;
    const names = ['firstName', 'lastName', 'month', 'year', 'country', ...(regional ? ['day'] : [])];
    return form({ fields: names.map(name => ({
      tag: 'input', type: 'text', id: `${name}-${regional ? 'kr' : 'us'}`, name, label: name,
      required: true, filled: !!values[name], invalid: regional && name !== 'country' && !values[name],
      validity: { valueMissing: !values[name] },
    })), buttons: [{ text: 'Create account', disabled: false }] });
  };
  browser.authFrame = async () => ({ getByRole: () => ({ count: async () => 0, click: async () => assert.fail('No creation') }) });
  browser.field = async (_frame, descriptor) => ({
    fill: async value => {
      if (descriptor.name === 'country') changedAt = timing.elapsed();
      else assert.ok(descriptor.id.endsWith('-kr'), 'Never fill controls captured before country changed');
      writes.push({ name: descriptor.name, value });
      values[descriptor.name] = value;
    },
    inputValue: async () => values[descriptor.name] || '',
  });
  const result = await browser.advance({ profile: { firstName: 'Fixture', lastName: 'Test', birthMonth: 2, birthYear: 1984, birthDay: 29, country: 'KR' } });
  assert.equal(result.stage, 'awaiting_approval');
  assert.deepEqual(writes.map(row => row.name), ['country', 'firstName', 'lastName', 'month', 'year', 'day']);
  assert.equal(values.day, '29');
  assert.ok(timing.elapsed() >= 450);
  assert.equal((await store.load()).submission.started, false);
});

test('country changes wait past a country-only form or disabled core controls before filling once', async t => {
  for (const transition of ['country-only', 'disabled']) {
    await t.test(transition, async t => {
      const { store, browser } = await fixture(t);
      const timing = clock();
      browser.stageWaitOptions = timing;
      const values = { country: 'US' };
      const writes = [];
      let changing = false;
      browser.form = async () => {
        const pending = changing && timing.elapsed() < 600;
        const names = pending && transition === 'country-only' ? ['country']
          : ['country', 'firstName', 'lastName', 'birthMonth', 'birthYear'];
        return form({ fields: names.map(name => ({
          tag: 'input', type: 'text', id: name, name, label: name,
          disabled: pending && transition === 'disabled' && name !== 'country',
          required: true, filled: !!values[name],
        })), buttons: [{ text: 'Create account', disabled: false }] });
      };
      browser.authFrame = async () => ({ getByRole: () => ({ count: async () => 0, click: async () => assert.fail('No creation') }) });
      browser.field = async (_frame, descriptor) => ({
        fill: async value => {
          if (descriptor.name === 'country') changing = true;
          else {
            assert.ok(timing.elapsed() >= 850, 'Wait for complete enabled controls and their stability window');
            assert.notEqual(descriptor.disabled, true);
          }
          writes.push(descriptor.name);
          values[descriptor.name] = value;
        },
        inputValue: async () => values[descriptor.name] || '',
      });
      const result = await browser.advance({ profile: {
        firstName: 'Fixture', lastName: 'Test', birthMonth: 2, birthYear: 1984, country: 'KR',
      } });
      assert.equal(result.stage, 'awaiting_approval');
      assert.deepEqual(writes, ['country', 'firstName', 'lastName', 'birthMonth', 'birthYear']);
      assert.equal(timing.elapsed(), 900);
      assert.equal((await store.load()).submission.started, false);
    });
  }
});

test('a required control appearing after the country stability window stops before creation', async t => {
  const { store, browser } = await fixture(t);
  const values = {};
  let fills = 0;
  const names = ['firstName', 'lastName', 'birthMonth', 'birthYear', 'country'];
  browser.form = async () => form({ fields: [
    ...names.map(name => ({ tag: 'input', type: 'text', id: name, name, label: name, required: true, filled: !!values[name] })),
    ...(fills === 5 ? [{ tag: 'input', type: 'text', id: 'new-required', name: 'new-required', label: 'Required detail', required: true, filled: false, validity: { valueMissing: true } }] : []),
  ], buttons: [{ text: 'Create account', disabled: false }] });
  browser.authFrame = async () => ({ getByRole: () => ({ count: async () => 0, click: async () => assert.fail('No creation') }) });
  browser.field = async (_frame, descriptor) => ({ fill: async value => { fills++; values[descriptor.name] = value; }, inputValue: async () => values[descriptor.name] || '' });
  browser.status = async () => { throw Error('Execution context was destroyed after the rejection snapshot'); };
  const result = await browser.advance({ profile: { firstName: 'Fixture', lastName: 'Test', birthMonth: 2, birthYear: 1984, birthDay: 29, country: 'KR' } });
  assert.equal(result.code, 'form_rejected');
  assert.equal(result.form.fields.find(field => field.id === 'new-required').invalid, true);
  assert.equal(fills, 5, 'Unknown controls are not filled by guessing');
  assert.equal((await store.load()).submission.started, false);
});

test('a required unchecked agreement is reported without accepting it implicitly', async t => {
  const { store, browser } = await fixture(t);
  const values = {};
  const names = ['firstName', 'lastName', 'birthMonth', 'birthYear', 'country'];
  browser.form = async () => form({ fields: [
    ...names.map(name => ({ tag: 'input', type: 'text', id: name, name, label: name, required: true, filled: !!values[name] })),
    { tag: 'input', type: 'checkbox', id: 'required-consent', name: 'required-consent', label: 'Terms agreement', required: true, filled: true, checked: false },
    { tag: 'input', type: 'checkbox', id: 'optional', name: 'optional', label: 'Optional offers', required: false, filled: true, checked: false },
  ], buttons: [{ text: 'Create account', disabled: false }] });
  browser.authFrame = async () => ({ getByRole: () => ({ count: async () => 0, click: async () => assert.fail('No creation') }) });
  browser.field = async (_frame, descriptor) => ({ fill: async value => { assert.notEqual(descriptor.type, 'checkbox'); values[descriptor.name] = value; }, inputValue: async () => values[descriptor.name] || '' });
  const result = await browser.advance({ profile: { firstName: 'Fixture', lastName: 'Test', birthMonth: 2, birthYear: 1984, country: 'KR' } });
  assert.equal(result.code, 'form_rejected');
  assert.equal(result.form.fields.find(field => field.id === 'required-consent').invalid, true);
  assert.notEqual(result.form.fields.find(field => field.id === 'optional').invalid, true);
  assert.equal((await store.load()).submission.started, false);
});

test('profile readiness rejects a country that changed while the other fields were filled', async t => {
  const { store, browser } = await fixture(t);
  const values = {};
  const names = ['firstName', 'lastName', 'birthMonth', 'birthYear', 'country'];
  browser.form = async () => form({ fields: names.map(name => ({ tag: 'input', type: 'text', id: name, name, label: name, filled: !!values[name] })), buttons: [{ text: 'Create account', disabled: false }] });
  browser.authFrame = async () => ({ getByRole: () => ({ count: async () => 0 }) });
  browser.field = async (_frame, descriptor) => ({
    fill: async value => { values[descriptor.name] = value; if (descriptor.name === 'birthYear') values.country = 'US'; },
    inputValue: async () => values[descriptor.name] || '',
  });
  const result = await browser.advance({ profile: { firstName: 'Fixture', lastName: 'Test', birthMonth: 2, birthYear: 1984, country: 'KR' } });
  assert.equal(result.code, 'form_rejected');
  assert.equal(result.form.fields.find(field => field.name === 'country').invalid, true);
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

test('a transient verification frame can settle into email verification without another submission', async () => {
  const timing = clock();
  let reads = 0;
  const transitional = form({humanVerification:{required:true,source:'visible_challenge_frame'}});
  const result = await waitForFormStage(async () => ++reads <= 3 ? transitional : verificationForm(),
    {...timing,fromStage:'awaiting_profile'});
  assert.equal(result.stage,'awaiting_verification');
  assert.equal(result.code,null);
  assert.equal(reads,4);
  assert.equal(timing.elapsed(),300);
});

test('an upstream captcha response can settle into an account landing page during read-only observation', async () => {
  const timing = clock();
  let reads = 0;
  const result = await waitForFormStage(async () => ++reads <= 2
    ? form({humanVerification:{required:true,source:'upstream_captcha_response'}})
    : form({host:'account.adobe.com'}), {...timing,accept:(_stage,current) => current.host === 'account.adobe.com'});
  assert.equal(result.form.host,'account.adobe.com');
  assert.equal(result.code,null);
  assert.equal(reads,3);
});

test('instructions, frames and upstream evidence share the same bounded read-only observation window', async () => {
  for (const source of ['visible_challenge_frame','upstream_captcha_response','visible_instruction']) {
    const timing=clock();
    const result=await waitForFormStage(async()=>form({humanVerification:{required:true,source}}),
      {...timing,verificationGraceMs:400,accept:()=>true});
    assert.equal(result.code,'human_verification_required');
    assert.equal(result.stage,'needs_manual');
    assert.equal(timing.elapsed(),400);
  }
});

test('a transient visible instruction proceeds to OTP without touching verification controls',async()=>{
  const timing=clock();let reads=0;
  const result=await waitForFormStage(async()=>++reads<=3
    ?form({humanVerification:{required:true,source:'visible_instruction'}}):verificationForm(),
    {...timing,fromStage:'awaiting_profile'});
  assert.equal(result.stage,'awaiting_verification');assert.equal(result.code,null);
  assert.equal(reads,4);assert.equal(timing.elapsed(),300);
});

test('default verification window stops at fifteen seconds even when frame evidence changes to an instruction',async()=>{
  const timing=clock();
  const result=await waitForFormStage(async()=>form({humanVerification:{required:true,
    source:timing.elapsed()<5000?'visible_challenge_frame':'visible_instruction'}}),{...timing,timeoutMs:30000,accept:()=>true});
  assert.equal(result.code,'human_verification_required');assert.equal(timing.elapsed(),15000);
});

test('verification diagnostics report the actual remaining step window without extending or repeating it',async()=>{
  const timing=clock(), observations=[];
  const result=await waitForFormStage(async()=>timing.elapsed()<800 ? profileForm()
    :form({humanVerification:{required:true,source:'visible_instruction'}}),
    {...timing,accept:()=>false,onVerificationWait:value=>observations.push(value)});
  assert.equal(result.code,'human_verification_required');
  assert.equal(timing.elapsed(),1000);
  assert.deepEqual(observations,[{elapsed_ms:200,outcome:'still_required'}]);
});

test('verification diagnostics distinguish progress from observation failure without leaking errors',async()=>{
  for(const fail of [false,true]){
    const timing=clock(),observations=[];let reads=0;
    const pending=waitForFormStage(async()=>{
      if(reads++===0)return form({humanVerification:{required:true,source:'visible_instruction'}});
      if(fail)throw Error('PRIVATE page error');
      return verificationForm();
    },{...timing,onVerificationWait:value=>observations.push(value)});
    if(fail)await assert.rejects(pending,/PRIVATE page error/);else assert.equal((await pending).stage,'awaiting_verification');
    assert.deepEqual(observations,[{elapsed_ms:100,outcome:fail?'observation_error':'resolved'}]);
  }
  const timing=clock(),observations=[];
  await waitForFormStage(verificationForm,{...timing,onVerificationWait:value=>observations.push(value)});
  assert.deepEqual(observations,[]);
  await waitForFormStage(async()=>form({humanVerification:{required:true,source:'visible_instruction'}}),
    {...timing,onVerificationWait:()=>{throw Error('diagnostic unavailable');}});
  assert.equal(timing.elapsed(),1000);
});

test('a completed creation-stage observation is not followed by a second runner wait or another Create',async t=>{
  const {store,browser}=await fixture(t);const timing=clock();let clicks=0;
  browser.stageWaitOptions={...timing,timeoutMs:30000};
  await store.setStage('awaiting_approval');
  browser.authFrame=async()=>({getByRole:()=>({count:async()=>1,isEnabled:async()=>true,click:async()=>{clicks++;}})});
  browser.form=async()=>clicks ? form({humanVerification:{required:true,source:'visible_instruction'}}) : profileForm();
  await browser.approveCreate();assert.equal(timing.elapsed(),15000);
  browser.status=async()=>{const state=await store.load();return {stage:state.stage,code:state.code,detected:'needs_manual',form:await browser.form()};};
  const result=await runAutomaticRegistration(browser,store,{approveCreate:true,verificationWait:{...timing,timeoutMs:15000}});
  assert.equal(result.code,'human_verification_required');assert.equal(timing.elapsed(),15000);
  assert.deepEqual(result.verificationWait,{elapsed_ms:15000,outcome:'still_required'});
  assert.equal(clicks,1);assert.equal((await store.load()).submission.started,true);
});

test('bounded challenge observation retains the original creation fence and never clicks again', async t => {
  const {store,browser}=await fixture(t);
  await store.setStage('awaiting_approval');
  let clicks=0;
  browser.authFrame=async()=>({getByRole:()=>({count:async()=>1,isEnabled:async()=>true,click:async()=>{clicks++;}})});
  browser.form=async()=>clicks
    ? form({humanVerification:{required:true,source:'visible_challenge_frame'}})
    : profileForm();
  await browser.approveCreate();
  const state=await store.load();
  assert.equal(state.submission.started,true);
  assert.equal(state.submission.outcome,'pending');
  assert.equal(state.stage,'needs_manual');
  assert.equal(state.code,'human_verification_required');
  assert.equal(clicks,1);
  await assert.rejects(browser.approveCreate(),error=>error.code==='submission_already_started');
  assert.equal(clicks,1);
});

test('visible manual instructions take precedence over a provider iframe marker', async () => {
  const browser=new BrowserRegistration({});
  const root={url:()=> 'https://auth.services.adobe.com/',parentFrame:()=>null,getByText:()=>({all:async()=>[]})};
  const frame={url:()=> 'https://arks-client.adobe.com/fc/fixture',parentFrame:()=>root,
    frameElement:async()=>({boundingBox:async()=>({width:300,height:300}),isVisible:async()=>true,dispose:async()=>{}}),
    getByText:()=>({all:async()=>[{isVisible:async()=>true}]})};
  browser.page={url:root.url,frames:()=>[root,frame]};
  browser.networkStatus=async()=>{browser.network=[];return {code:null};};
  assert.equal((await browser.detectHumanVerification()).source,'visible_instruction');
  frame.getByText=()=>({all:async()=>[]});
  assert.equal((await browser.detectHumanVerification()).source,'visible_challenge_frame');
});

function observedBrowserFixture() {
  let phase='otp', challenge=false;
  const requests=[];
  const store={load:async()=>({email:'fixture@example.test',config:{},stage:'awaiting_verification',code:null,submission:{started:true}})};
  const browser=new BrowserRegistration(store);
  browser.workerMode=true;
  const root={url:()=>phase==='firefly'?'https://firefly.adobe.com/':'https://auth.services.adobe.com/verify',
    parentFrame:()=>null,getByText:()=>({all:async()=>[]}),locator:selector=>({evaluateAll:async()=>{
      if (selector==='input:visible,select:visible' && phase==='otp') return [{tag:'input',type:'text',id:'otp',name:'code',label:'Verification code',autocomplete:'one-time-code'}];
      if (selector==='h1:visible,h2:visible,h3:visible') return phase==='otp'?['Verify your email']:['Firefly'];
      return [];
    }})};
  const challengeFrame={url:()=> 'https://arks-client.adobe.com/fc/fixture',parentFrame:()=>root,
    frameElement:async()=>({boundingBox:async()=>({width:300,height:300}),isVisible:async()=>true,dispose:async()=>{}}),
    getByText:()=>({all:async()=>[]})};
  const page={url:root.url,frames:()=>challenge?[root,challengeFrame]:[root],requests:async()=>requests};
  browser.page=page;
  browser.authFrame=async()=>root;
  browser.context={pages:()=>[page]};
  browser.networkDiagnostics=new NetworkDiagnostics();
  const addRequest=(error=null)=>requests.push({
    url:()=>error?'https://arks-client.adobe.com/fc/a':'https://auth.services.adobe.com/signin/v2/accounts',
    method:()=> 'POST',timing:()=>({startTime:1794272400000,responseEnd:100}),
    failure:()=>error?{errorText:error}:null,
    response:async()=>({status:()=>400,headers:()=>({'content-type':'application/json'}),
      body:async()=>Buffer.from('{"errorCode":"captcha_required"}')}),
  });
  return {browser,addRequest,setPhase:value=>{phase=value;},setChallenge:value=>{challenge=value;}};
}

test('only visible OTP progress without a visible challenge records creation acceptance',async()=>{
  for(const kind of ['otp','challenge','firefly']){
    const f=observedBrowserFixture();let accepted=0;
    f.browser.creationTracker={accepted:async()=>{accepted++;}};
    if(kind==='challenge')f.setChallenge(true);
    if(kind==='firefly')f.setPhase('firefly');
    await f.browser.status();await f.browser.status();
    assert.equal(accepted,kind==='otp'?1:0);
  }
});

test('real form/status keep an old captcha superseded through OTP to Firefly on the same page, but a new event blocks', async () => {
  const f=observedBrowserFixture();
  f.addRequest();
  let status=await f.browser.status();
  assert.equal(status.detected,'awaiting_verification');
  assert.equal(status.form.humanVerification.required,false);
  assert.equal(status.networkDiagnosis.code,null);
  assert.equal(status.network[0].errorCode,'captcha_required');
  f.setPhase('firefly');
  status=await f.browser.status();
  assert.equal(status.form.host,'firefly.adobe.com');
  assert.equal(status.form.humanVerification.required,false);
  assert.equal(status.networkDiagnosis.code,null);
  // Same timestamp and metadata, different completed request: never suppressed.
  f.addRequest();
  status=await f.browser.status();
  assert.equal(status.form.humanVerification.required,true);
  assert.equal(status.networkDiagnosis.code,'captcha_required');
  assert.equal(status.network.length,2);
});

test('real form/status never retire a new captcha merely because the old OTP field remains visible', async () => {
  const f=observedBrowserFixture();
  f.addRequest();
  assert.equal((await f.browser.status()).networkDiagnosis.code,null);
  f.addRequest();
  const status=await f.browser.status();
  assert.equal(status.form.humanVerification.required,true);
  assert.equal(status.networkDiagnosis.code,'captcha_required');
});

test('real form/status preserve DNS and timeout failures despite OTP progress', async () => {
  for (const [error,code] of [['net::ERR_NAME_NOT_RESOLVED','verification_dns_failed'],
    ['net::ERR_TIMED_OUT','verification_request_timeout']]) {
    const f=observedBrowserFixture();
    f.addRequest();
    f.addRequest(error);
    let status=await f.browser.status();
    assert.equal(status.form.humanVerification.required,false);
    assert.equal(status.networkDiagnosis.code,code);
    assert.equal(status.code,code);
    assert.equal(status.stage,'needs_manual');
    f.setPhase('firefly');
    status=await f.browser.status();
    assert.equal(status.networkDiagnosis.code,code);
    assert.equal(status.network.length,2);
  }
});

test('first OTP progress retires an old aborted request but a later abort still blocks',async()=>{
  const f=observedBrowserFixture();
  f.addRequest();f.addRequest('net::ERR_ABORTED');
  let status=await f.browser.status();
  assert.equal(status.detected,'awaiting_verification');
  assert.equal(status.networkDiagnosis.code,null);
  assert.equal(status.network.length,2);
  f.addRequest('net::ERR_ABORTED');
  status=await f.browser.status();
  assert.equal(status.networkDiagnosis.code,'verification_request_aborted');
  assert.equal(status.network.length,3);
});

test('a real visible challenge is not hidden by an aborted request or an OTP field',async()=>{
  const f=observedBrowserFixture();
  f.addRequest('net::ERR_ABORTED');f.setChallenge(true);
  const status=await f.browser.status();
  assert.equal(status.code,'human_verification_required');
  assert.equal(status.form.humanVerification.required,true);
  assert.equal(status.networkDiagnosis.code,'verification_request_aborted');
});

test('real form/status retain visible challenges even when an OTP field or previous superseded captcha exists', async () => {
  const f=observedBrowserFixture();
  f.addRequest();
  f.setChallenge(true);
  let status=await f.browser.status();
  assert.equal(status.form.humanVerification.source,'visible_challenge_frame');
  assert.equal(status.networkDiagnosis.code,'captcha_required');
  f.setChallenge(false);
  assert.equal((await f.browser.status()).networkDiagnosis.code,null);
  f.setPhase('firefly');
  f.setChallenge(true);
  status=await f.browser.status();
  assert.equal(status.form.humanVerification.source,'visible_challenge_frame');
  assert.equal(status.detected,'needs_manual');
});
