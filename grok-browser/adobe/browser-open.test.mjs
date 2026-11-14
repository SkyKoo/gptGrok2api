import test from 'node:test';
import assert from 'node:assert/strict';
import { lstat, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { BrowserRegistration } from './browser.mjs';
import { PrivateRunStore, RegistrationError } from './core.mjs';
import { creationOutcome } from './creation-outcome.mjs';
import { StartupProgress } from './startup-progress.mjs';
import { registrationDiagnostics } from './registration-diagnostics.mjs';

test('recovery refuses a missing or aliased browser directory before launch without replacing it', async t => {
  for (const kind of ['missing','symlink','file']) {
    await t.test(kind, async subtest => {
      const directory=await mkdtemp(join(tmpdir(),'adobe-isolation-'));
      const store=new PrivateRunStore(directory,'original');
      await store.init({email:'fixture@example.test'});
      await store.markSubmissionStarted();
      subtest.after(async()=>{await store.close();await rm(directory,{recursive:true,force:true});});
      const profile=join(store.runDir,'browser');
      const other=join(directory,'other-account');
      if(kind==='symlink'){await mkdir(other,{mode:0o700});await symlink(other,profile);}
      if(kind==='file')await writeFile(profile,'unmodified',{mode:0o600});
      const before=await readFile(store.statePath);
      const browser=new BrowserRegistration(store,{}, {launchBrowser:async()=>assert.fail('invalid profile must not launch')});
      await assert.rejects(browser.open({resumeOnly:true,requireExistingProfile:true}),error=>
        error.code===(kind==='missing'?'browser_profile_missing':'browser_profile_unavailable'));
      assert.deepEqual(await readFile(store.statePath),before);
      if(kind==='missing')await assert.rejects(lstat(profile),{code:'ENOENT'});
      if(kind==='symlink')assert.equal((await lstat(profile)).isSymbolicLink(),true);
      if(kind==='file')assert.equal(await readFile(profile,'utf8'),'unmodified');
    });
  }
});

test('open preserves restored Adobe pages after any durable signup submission', async t => {
  for (const marker of ['emailSubmitted', 'passwordSubmitted', 'submission.started']) {
    await t.test(marker, async subtest => {
      const directory = await mkdtemp(join(tmpdir(), 'adobe-browser-open-'));
      const store = new PrivateRunStore(directory, 'fixture');
      await store.init({ email: 'fixture@example.invalid' });
      if (marker === 'submission.started') await store.markSubmissionStarted();
      else await store.save({ [marker]: true, stage: 'awaiting_profile' });
      subtest.after(async () => {
        await store.close();
        await rm(directory, { recursive: true, force: true });
      });
      const stateBefore = await store.load();
      let navigations = 0;
      const fakePage = url => ({
        url: () => url,
        goto: async () => { navigations++; throw new Error('restored pages must not navigate'); },
        getByRole: () => { throw new Error('restored pages must not reopen signup'); },
      });
      const blank = fakePage('about:blank');
      const restored = fakePage('https://auth.services.adobe.com/signup');
      const unrelated = fakePage('https://unrelated.example.invalid/');
      const context = {
        pages: () => [blank, restored, unrelated],
        on: () => {},
        newPage: async () => { throw new Error('restored context must not create another page'); },
      };
      let launches = 0;
      const browser = new BrowserRegistration(store, {}, {
        launchBrowser: async (profileDir, options) => {
          launches++;
          assert.equal(profileDir, join(store.runDir, 'browser'));
          assert.deepEqual(options, { headless: false });
          return context;
        },
      });
      browser.status = async () => ({ restored: browser.page === restored });
      assert.deepEqual(await browser.open(), { restored: true });
      assert.equal(launches, 1);
      assert.equal(navigations, 0);
      assert.deepEqual(await store.load(), stateBefore);
    });
  }
});

async function restoredFixture(t, readStatus, { timeoutMs = 1000 } = {}) {
  const directory = await mkdtemp(join(tmpdir(), 'adobe-restored-page-'));
  const store = new PrivateRunStore(directory, 'fixture');
  await store.init({ email: 'fixture@example.test' });
  await store.markSubmissionStarted();
  const state = await store.load();
  await store.save({ creationEvidence: { version: 1, marker: state.submission.startedAt,
    requests: 1, finished: 1, rejected: 0, unknown: 0, accepted: true, complete: true, sealed: true } });
  t.after(async () => { await store.close(); await rm(directory, { recursive: true, force: true }); });
  let clock = 0, reads = 0, url = 'https://auth.services.adobe.com/verify';
  const page = { url: () => url, goto: () => assert.fail('recovery observation must not navigate'),
    getByRole: () => assert.fail('recovery observation must not click or resend') };
  const context = { pages: () => [page], on: () => {}, off: () => {} };
  const frames = [], startupProgress = new StartupProgress({ emit: frame => frames.push(frame), now: () => clock });
  const browser = new BrowserRegistration(store, { startupProgress }, { launchBrowser: async () => context });
  browser.restorationWaitOptions = { timeoutMs, pollIntervalMs: 250, now: () => clock, pause: async ms => { clock += ms; } };
  browser.status = async () => readStatus({ reads: ++reads, setURL: value => { url = value; } });
  return { store, browser, frames, readCount: () => reads };
}

test('restored page navigation is re-observed without reopening registration or losing accepted evidence', async t => {
  const f = await restoredFixture(t, ({ reads, setURL }) => {
    if (reads === 1) throw new Error('PRIVATE Execution context was destroyed');
    if (reads === 2) { setURL('about:blank'); throw new RegistrationError('untrusted_page'); }
    setURL('https://auth.services.adobe.com/verify');
    return { stage: 'awaiting_verification' };
  });
  assert.deepEqual(await f.browser.open({ resumeOnly: true }), { stage: 'awaiting_verification' });
  assert.equal(f.readCount(), 3);
  const state = await f.store.load();
  assert.deepEqual(creationOutcome(state), { creation_outcome: 'accepted' });
  assert.equal(state.verificationSubmitted, undefined);
  assert.equal(state.recoveryAttempts, undefined);
  assert.equal(state.startupFailure, undefined);
  assert.deepEqual(f.frames.filter(frame => frame.stage === 'adobe_restoring_registration_page').map(frame => frame.status), ['started', 'completed']);
  assert.doesNotMatch(JSON.stringify(f.frames), /PRIVATE|fixture@/);
});

test('restoration calls the real status reader using one outer navigation window without replaying signup', async t => {
  const f = await restoredFixture(t, () => assert.fail('Use the real status reader'));
  f.browser.status = BrowserRegistration.prototype.status.bind(f.browser);
  f.browser.workerMode = true;
  f.browser.statusWaitOptions = { timeoutMs: 1, now: () => 0,
    pause: () => assert.fail('Restoration must own its single observation window') };
  let reads = 0;
  f.browser.form = async () => {
    if (++reads <= 2) throw new Error('Execution context was destroyed');
    return { host: 'auth.services.adobe.com', fields: [{ autocomplete: 'one-time-code' }],
      headings: [], notices: [], buttons: [], agreements: [], humanVerification: { required: false } };
  };
  const before = await f.store.load();
  const status = await f.browser.open({ resumeOnly: true });
  assert.equal(status.detected, 'awaiting_verification');
  assert.equal(status.submissionStarted, true);
  assert.equal(reads, 3);
  assert.deepEqual(await f.store.load(), before);
  assert.deepEqual(creationOutcome(await f.store.load()), { creation_outcome: 'accepted' });
  assert.deepEqual(f.frames.filter(frame => frame.stage === 'adobe_restoring_registration_page')
    .map(frame => [frame.status, frame.duration_ms]), [['started', 0], ['completed', 500]]);
});

test('restored navigation can settle after fifteen seconds while retaining the original accepted session', async t => {
  let elapsed = 0;
  const f = await restoredFixture(t, () => {
    if (elapsed < 20_000) throw new Error('Execution context was destroyed');
    return { stage: 'awaiting_verification' };
  });
  f.browser.restorationWaitOptions = { pollIntervalMs: 1000, now: () => elapsed,
    pause: async ms => { elapsed += ms; } };
  const before = await f.store.load();
  assert.equal((await f.browser.open({ resumeOnly: true })).stage, 'awaiting_verification');
  assert.equal(elapsed, 20_000);
  assert.deepEqual(await f.store.load(), before);
});

test('continuous restored navigation and a stalled status read both have one bounded observation window', async t => {
  for (const stalled of [false, true]) await t.test(String(stalled), async sub => {
    const f = await restoredFixture(sub, () => {
      if (stalled) return new Promise(() => {});
      throw new Error('PRIVATE Frame was detached');
    }, { timeoutMs: stalled ? 20 : 1000 });
    await assert.rejects(f.browser.open({ resumeOnly: true }), error => error.code === 'recovery_page_observation_timeout');
    assert.equal(f.readCount(), stalled ? 1 : 4);
    const state = await f.store.load();
    assert.deepEqual(creationOutcome(state), { creation_outcome: 'accepted' });
    assert.equal(state.startupFailure.failure_action, 'adobe_restoring_registration_page');
    assert.equal(state.startupFailure.failure_phase, 'submission_started');
    assert.equal(state.startupFailure.exception_type, 'timeout');
    assert.equal(f.frames.at(-1).status, 'failed');
    assert.doesNotMatch(JSON.stringify({ failure: state.startupFailure, frames: f.frames }), /PRIVATE|fixture@/);
  });
});

test('closed, unknown and untrusted restored pages stop immediately with fixed diagnostics', async t => {
  for (const [kind, code, type] of [
    ['closed', 'recovery_page_closed', 'page_closed'],
    ['unknown', 'recovery_page_observation_failed', 'unknown'],
    ['untrusted', 'recovery_page_untrusted', 'registration'],
  ]) await t.test(kind, async sub => {
    const f = await restoredFixture(sub, ({ setURL }) => {
      if (kind === 'closed') throw new Error('PRIVATE Target page, context or browser has been closed');
      if (kind === 'unknown') throw new Error('PRIVATE unexpected browser error');
      setURL('https://unrelated.example.invalid/?token=PRIVATE');
      return { stage: 'awaiting_verification' };
    });
    await assert.rejects(f.browser.open({ resumeOnly: true }), error => error.code === code && !error.message.includes('PRIVATE'));
    assert.equal(f.readCount(), 1);
    const state = await f.store.load();
    assert.deepEqual(creationOutcome(state), { creation_outcome: 'accepted' });
    assert.equal(state.startupFailure.exception_type, type);
    assert.equal(state.startupFailure.failure_action, 'adobe_restoring_registration_page');
    assert.doesNotMatch(JSON.stringify(state.startupFailure), /PRIVATE|unrelated|fixture@/);
  });
});

async function startupFixture(t, failureStage, { blockedFont = false } = {}) {
  const directory = await mkdtemp(join(tmpdir(), 'adobe-startup-phases-'));
  const store = new PrivateRunStore(directory, 'fixture');
  await store.init({ email: 'fixture@example.test' });
  t.after(async () => { await store.close(); await rm(directory, { recursive: true, force: true }); });
  let clock = 0, entryWaits = 0;
  const frames = [], calls = [];
  const failure = Object.assign(new Error('PRIVATE URL credential timeout'), { name: 'TimeoutError' });
  const action = async (stage, options, elapsed) => {
    calls.push({ stage, options }); clock += elapsed;
    if (stage === failureStage) throw failure;
  };
  const entry = {
    count: async () => 1,
    waitFor: async options => {
      entryWaits++;
      if (blockedFont && entryWaits === 1) { clock += 20_000; throw failure; }
      await action('adobe_waiting_create_entry', options, 13);
    },
    click: async options => action('adobe_opening_registration', options, 17),
  };
  const page = {
    url: () => 'https://auth.services.adobe.com/',
    goto: async (_url, options) => action('adobe_loading_account_page', options, 11),
    getByRole: () => entry,
    evaluate: async () => blockedFont,
    route: async () => {},
    reload: async options => action('adobe_reloading_account_page', options, 29),
    locator: selector => selector === '[data-id="EmailPage-CreateAccountLink"]:visible'
      ? { or: other => { assert.equal(other, entry); return entry; } }
      : { waitFor: async options => action('adobe_waiting_registration_form', options, 19) },
  };
  const context = { pages: () => [page], on: () => {}, off: () => {}, close: async () => {} };
  const startupProgress = new StartupProgress({ emit: frame => frames.push(frame), now: () => clock });
  const browser = new BrowserRegistration(store, { startupProgress }, { launchBrowser: async (_profile, options) => {
    await action('starting_browser', options, 7); return context;
  } });
  browser.workerMode = true;
  browser.form = async () => ({ fields: [{ type: 'email' }], headings: [], notices: [], buttons: [], humanVerification: { required: false } });
  browser.networkStatus = async () => ({ code: null });
  browser.waitForStage = async () => { await action('adobe_checking_registration_form', undefined, 23); return { code: null }; };
  return { store, browser, frames, calls, failure };
}

test('initialization distinguishes browser launch, page load, entry and registration form without repeating actions', async t => {
  const f = await startupFixture(t);
  assert.equal((await f.browser.open()).stage, 'form_ready');
  const stages = ['starting_browser', 'adobe_loading_account_page', 'adobe_waiting_create_entry',
    'adobe_opening_registration', 'adobe_waiting_registration_form', 'adobe_checking_registration_form'];
  assert.deepEqual(f.frames.filter(frame => frame.status === 'started').map(frame => frame.stage), stages);
  assert.deepEqual(f.frames.filter(frame => frame.status === 'completed').map(frame => frame.stage), stages);
  assert.deepEqual(f.calls.map(call => call.stage), stages);
  assert.equal(f.calls.find(call => call.stage === 'adobe_loading_account_page').options.timeout, 90000);
  assert.equal(f.calls.find(call => call.stage === 'adobe_waiting_create_entry').options.timeout, 40000);
  assert.equal(f.calls.find(call => call.stage === 'adobe_opening_registration').options.timeout, 30000);
  assert.equal(f.calls.find(call => call.stage === 'adobe_waiting_registration_form').options.timeout, 40000);
  assert.equal((await f.store.load()).submission.started, false);
});

test('each form startup failure keeps login_ui_not_ready and its exact sanitized failure step', async t => {
  for (const stage of ['adobe_waiting_create_entry', 'adobe_opening_registration', 'adobe_waiting_registration_form', 'adobe_checking_registration_form']) {
    await t.test(stage, async sub => {
      const f = await startupFixture(sub, stage);
      const result = await f.browser.open(), state = await f.store.load();
      assert.equal(result.code, 'login_ui_not_ready');
      assert.equal(state.submission.started, false);
      assert.equal(state.startupFailure.failure_action, stage);
      assert.equal(state.startupFailure.failure_phase, 'before_submission');
      assert.equal(state.startupFailure.exception_type, 'timeout');
      const diagnostic = registrationDiagnostics(result, state);
      assert.equal(diagnostic.failure_action, stage);
      assert.equal(diagnostic.duration_ms, f.frames.at(-1).duration_ms);
      assert.doesNotMatch(JSON.stringify({ frames: f.frames, diagnostic, saved: state.startupFailure }), /PRIVATE|credential/);
      assert.equal(f.calls.filter(call => call.stage === stage).length, 1);
    });
  }
});

test('launch and page-load failures publish their failed step without changing the thrown error', async t => {
  for (const stage of ['starting_browser', 'adobe_loading_account_page']) await t.test(stage, async sub => {
    const f = await startupFixture(sub, stage);
    await assert.rejects(f.browser.open(), error => error === f.failure);
    assert.equal(f.frames.at(-1).stage, stage); assert.equal(f.frames.at(-1).status, 'failed');
    assert.equal(f.frames.at(-1).exception_type, 'timeout');
    if (stage === 'adobe_loading_account_page') assert.equal((await f.store.load()).startupFailure.failure_action, stage);
    assert.equal((await f.store.load()).submission.started, false);
  });
});

test('existing optional font reload has separate progress and does not duplicate the signup click', async t => {
  const f = await startupFixture(t, undefined, { blockedFont: true });
  assert.equal((await f.browser.open()).stage, 'form_ready');
  assert.equal(f.frames.filter(frame => frame.stage === 'adobe_waiting_create_entry' && frame.status === 'failed').length, 1);
  assert.equal(f.frames.filter(frame => frame.stage === 'adobe_reloading_account_page' && frame.status === 'completed').length, 1);
  assert.equal(f.calls.filter(call => call.stage === 'adobe_opening_registration').length, 1);
  assert.equal(f.calls.find(call => call.stage === 'adobe_reloading_account_page').options.timeout, 60000);
  assert.equal((await f.store.load()).startupFailure, undefined);
});
