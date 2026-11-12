import test from 'node:test';
import assert from 'node:assert/strict';
import { lstat, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { BrowserRegistration } from './browser.mjs';
import { PrivateRunStore } from './core.mjs';
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
    locator: () => ({ waitFor: async options => action('adobe_waiting_registration_form', options, 19) }),
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
  assert.equal(f.calls.find(call => call.stage === 'adobe_loading_account_page').options.timeout, 45000);
  assert.equal(f.calls.find(call => call.stage === 'adobe_waiting_create_entry').options.timeout, 20000);
  assert.equal(f.calls.find(call => call.stage === 'adobe_opening_registration').options, undefined);
  assert.equal(f.calls.find(call => call.stage === 'adobe_waiting_registration_form').options.timeout, 20000);
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
  assert.equal(f.calls.find(call => call.stage === 'adobe_reloading_account_page').options.timeout, 30000);
  assert.equal((await f.store.load()).startupFailure, undefined);
});
