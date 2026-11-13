import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, lstat, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runInNewContext } from 'node:vm';
import { BrowserRegistration } from './browser.mjs';
import { PrivateRunStore, RegistrationError } from './core.mjs';
import { runAutomaticRegistration } from './auto-runner.mjs';

const directories = [];
after(async () => { await Promise.all(directories.map(dir => rm(dir, { recursive: true, force: true }))); });
const codeIs = code => error => error instanceof RegistrationError && error.code === code;

async function setup(t, { submitted = true, profile = {}, signedIn = true, token = 'synthetic-token', cookies, url = 'https://firefly.adobe.com/' } = {}) {
  const temporary = await mkdtemp(join(tmpdir(), 'adobe-export-test-'));
  directories.push(temporary);
  const store = new PrivateRunStore(join(temporary, 'private'), 'one');
  await store.init({ email: 'person@example.test' });
  if (submitted) await store.markSubmissionStarted();
  t.after(() => store.close());
  const calls = [];
  const browser = Object.create(BrowserRegistration.prototype);
  browser.store = store;
  browser.page = {
    url: () => url,
    evaluate: fn => {
      calls.push(fn.toString().includes('const profile =') ? 'identity' : 'ready');
      return runInNewContext(`(${fn.toString()})()`, {
        window: { adobeIMS: {
          isSignedInUser: async () => signedIn,
          getProfile: async () => ({ email: 'person@example.test', userId: 'synthetic-id', emailVerified: true, ...profile }),
          getAccessToken: async () => ({ token }),
        } },
      });
    },
  };
  browser.context = { cookies: async origin => {
    if(origin === 'https://adobeid-na1.services.adobe.com/ims/check/v6/token') { calls.push('refresh_cookies'); return [{name:'ims_refresh',value:'synthetic-refresh-cookie'}]; }
    assert.equal(origin, 'https://firefly.adobe.com/');
    calls.push('cookies');
    return cookies ?? [{ name: 'ims_sid', value: 'synthetic-cookie' }];
  } };
  return { browser, store, calls, file: join(store.runDir, 'adobe2api-import.json') };
}

test('export awaits IMS identity, persists private credentials, and completes only after the file exists', async t => {
  const { browser, store, calls, file } = await setup(t);
  const originalSave = store.save.bind(store);
  const stages = [];
  store.save = async patch => {
    stages.push(patch.stage);
    if (patch.stage === 'completed') assert.equal((await lstat(file)).mode & 0o777, 0o600);
    return originalSave(patch);
  };
  const result = await browser.exportSession();
  assert.deepEqual(calls, ['identity', 'cookies', 'refresh_cookies']);
  assert.deepEqual(stages, ['credentials_saved', 'completed']);
  assert.equal(result.emailVerified, true);
  assert.equal(result.generationVerified, false);
  assert.equal(JSON.stringify(result).includes('synthetic-token'), false);
  const state = await store.load();
  assert.equal(state.submission.outcome, 'confirmed');
  assert.equal(state.result.exportComplete, true);
  assert.equal(state.result.account.refresh_cookie,'ims_refresh=synthetic-refresh-cookie');
  assert.equal(state.result.account.cookie.includes('synthetic-refresh-cookie'),false);
  assert.equal(state.result.credentialFile, file);
  assert.deepEqual(JSON.parse(await readFile(file, 'utf8')).accounts, [state.result.account]);
});

test('a completed export reuses saved bytes offline without reading or changing the browser identity', async t => {
  const { browser, store, file } = await setup(t);
  await browser.exportSession();
  const inode = (await lstat(file)).ino;
  browser.page = null;
  browser.context = null;
  store.save = async () => { throw new Error('completed export must not downgrade or resave state'); };
  assert.equal((await browser.exportSession()).stage, 'completed');
  assert.equal((await lstat(file)).ino, inode);
});

test('failure after file publication resumes the saved credential snapshot without recollecting tokens', async t => {
  const { browser, store, file } = await setup(t);
  const originalSave = store.save.bind(store);
  store.save = async patch => {
    if (patch.stage === 'completed') throw new RegistrationError('synthetic_save_failure');
    return originalSave(patch);
  };
  await assert.rejects(browser.exportSession(), codeIs('synthetic_save_failure'));
  assert.equal((await store.load()).stage, 'credentials_saved');
  const bytes = await readFile(file);
  store.save = originalSave;
  await store.markOutcomeUnknown();
  browser.page = null;
  browser.context = null;
  assert.equal((await browser.exportSession()).stage, 'completed');
  assert.deepEqual(await readFile(file), bytes);
  assert.equal((await store.load()).result.account.token, 'synthetic-token');
});

test('conflicting exports preserve the existing file and never report completion', async t => {
  const { browser, store, file } = await setup(t);
  const original = '{"other":"synthetic-original-secret"}\n';
  await writeFile(file, original, { mode: 0o600 });
  // The helper comes from a distinct ESM module: its errors must be rewrapped for the CLI.
  await assert.rejects(browser.exportSession(), codeIs('private_file_conflict'));
  assert.equal(await readFile(file, 'utf8'), original);
  assert.equal((await store.load()).stage, 'credentials_saved');
  assert.equal((await store.load()).submission.outcome, 'pending');
});

test('unsubmitted, mismatched, unsigned, unverified, or incomplete identities cannot complete', async t => {
  const cases = [
    [{ submitted: false }, 'registration_not_submitted'],
    [{ profile: { email: 'other@example.test' } }, 'identity_not_verified'],
    [{ profile: { userId: '' } }, 'identity_not_verified'],
    [{ token: '' }, 'identity_not_verified'],
    [{ signedIn: false }, 'identity_not_verified'],
    [{ profile: { emailVerified: false } }, 'email_verification_required'],
    [{ cookies: [] }, 'firefly_cookie_missing'],
    [{ url: 'http://firefly.adobe.com/' }, 'firefly_session_not_ready'],
  ];
  for (const [options, expected] of cases) {
    const { browser, store, file } = await setup(t, options);
    await assert.rejects(browser.exportSession(), codeIs(expected));
    assert.notEqual((await store.load()).stage, 'completed');
    await assert.rejects(lstat(file), { code: 'ENOENT' });
  }
});

test('saved identity mismatch is rejected without replacing evidence, and unknown email verification stays unknown', async t => {
  const first = await setup(t);
  await first.store.save({ stage: 'credentials_saved', result: {
    fireflySessionVerified: true,
    account: { email: 'other@example.test', account_id: 'id', token: 'token', cookie: 'cookie=value' },
  } });
  await assert.rejects(first.browser.exportSession(), codeIs('saved_identity_not_verified'));
  assert.deepEqual(first.calls, []);
  assert.equal((await first.store.load()).result.account.email, 'other@example.test');
  const second = await setup(t, { profile: { emailVerified: undefined } });
  assert.equal((await second.browser.exportSession()).emailVerified, null);
  assert.equal((await second.store.load()).result.emailVerified, null);
});

test('account landing uses the existing Firefly navigation and only completes after the real export identity check', async t => {
  for (const matches of [true, false]) {
    await t.test(matches ? 'matching identity' : 'mismatched identity', async subtest => {
      const fixture = await setup(subtest, { profile: matches ? {} : { email: 'someone-else@example.test' } });
      const { browser, store, calls, file } = fixture;
      const firefly = browser.page;
      let navigations = 0;
      firefly.goto = async destination => { assert.equal(destination, 'https://firefly.adobe.com/'); navigations++; };
      browser.page = { url: () => 'https://account.adobe.com/' };
      browser.context.newPage = async () => firefly;
      browser.form = async () => ({ host: new URL(browser.page.url()).hostname, fields: [], headings: [], notices: [],
        buttons: [], agreements: [], humanVerification: { required: false } });
      browser.status = async () => ({
        stage: (await store.load()).stage, detected: 'needs_manual', submissionStarted: true,
        form: { host: new URL(browser.page.url()).hostname, fields: [], humanVerification: { required: false } },
      });
      browser.approveCreate = async () => assert.fail('Never create another account after the landing page');
      browser.verify = async () => assert.fail('Never send verification without an observed verification form');
      const result = await runAutomaticRegistration(browser, store, {
        readInput: async () => assert.fail('Account landing needs no replacement signup or verification input'),
      });
      assert.equal(navigations, 1);
      assert.equal(browser.focusPageOnly, true);
      assert.equal((await store.load()).submission.started, true);
      if (matches) {
        assert.equal(result.stage, 'completed');
        assert.equal(result.automatic.completed, true);
        assert.equal(result.fireflySessionVerified, true);
        assert.deepEqual(calls, ['ready', 'identity', 'cookies', 'refresh_cookies']);
        assert.equal((await lstat(file)).mode & 0o777, 0o600);
      } else {
        assert.equal(result.code, 'identity_not_verified');
        assert.equal(result.automatic.paused, true);
        assert.notEqual((await store.load()).stage, 'completed');
        assert.deepEqual(calls, ['ready', 'identity']);
        await assert.rejects(lstat(file), { code: 'ENOENT' });
      }
    });
  }
});


async function workerExportFixture(t,{mismatched=false,invalid=false,changed=false}={}) {
 const fixture=await setup(t,{cookies:[]});
 const {browser,calls}=fixture;browser.workerMode=true;let url='https://firefly.adobe.com/',identities=0;
 browser.page={url:()=>url,goto:async next=>{assert.equal(next,'https://firefly.adobe.com/generate/image');url=next;calls.push('editor')},
  evaluate:async (fn,args)=>{
   if(fn.name==='identityInPage'){
    identities++;calls.push('identity');return {email:'person@example.test',account_id:mismatched&&identities>1?'different-id':'synthetic-id',token:'synthetic-token',user_agent:'fixture-agent',emailVerified:true};
   }
   if(fn.name==='mediaSessionMatchesInPage'){
    calls.push('verify');assert.equal(args.token,'runtime-token');assert.equal(args.identity.account_id,'synthetic-id');return !changed;
   }
   assert.equal(fn.name,'runtimeARPInPage');calls.push('arp');
   return invalid?{state:'invalid',source:'spl_runtime'}:{state:'ready',token:'runtime-token',source:'spl_runtime'};
  }};
 return fixture;
}
test('worker registration verifies the final media snapshot and resumes saved credentials offline',async t=>{
 const {browser,store,calls}=await workerExportFixture(t);
 assert.equal((await browser.exportSession()).stage,'completed');
 assert.deepEqual(calls,['identity','editor','arp','cookies','identity','verify','refresh_cookies']);
 const account=(await store.load()).result.account;
 assert.equal(account.cookie,'sherlockToken=runtime-token');assert.equal(account.media_session_arp_mode,'present');assert.equal(account.media_session_arp_source,'spl_runtime');
 browser.page=null;browser.context=null;assert.equal((await browser.exportSession()).stage,'completed');
});
test('an already saved omitted record remains resumable without collecting a new media session',async t=>{
 const {browser,store}=await workerExportFixture(t);await browser.exportSession();
 const state=await store.load();
 await store.save({result:{...state.result,account:{...state.result.account,cookie:'',media_session_arp_mode:'omitted'}}});
 browser.page=null;browser.context=null;assert.equal((await browser.exportSession()).stage,'completed');
});
test('worker registration refuses identity changes and invalid runtime values without saving credentials',async t=>{
 for(const [options,code] of [[{mismatched:true},'identity_not_verified'],[{invalid:true},'media_session_incomplete'],[{changed:true},'media_session_incomplete']]){
  const {browser,store}=await workerExportFixture(t,options);
  await assert.rejects(browser.exportSession(),codeIs(code));assert.equal((await store.load()).result?.account,undefined);
 }
});

test('deferred media export persists only a verified identity checkpoint and returns pending',async t=>{
 const {browser,store,calls}=await workerExportFixture(t);browser.deferMedia=true;
 browser.creationTracker={accepted:async()=>{
  const state=await store.load();await store.save({creationEvidence:{version:1,marker:state.submission.startedAt,requests:1,finished:1,rejected:0,unknown:0,accepted:true,complete:true,sealed:false}});
 }};
 const result=await browser.exportSession();
 assert.equal(result.stage,'media_pending');assert.equal(result.emailVerified,true);assert.deepEqual(calls,['identity']);
 const state=await store.load();assert.equal(state.stage,'media_pending');assert.equal(state.result,undefined);
 assert.equal(state.mediaCheckpoint.account_id,'synthetic-id');assert.equal(state.mediaCheckpoint.emailVerified,true);
 assert.doesNotMatch(JSON.stringify(state.mediaCheckpoint),/synthetic-token|cookie|user_agent|password/);
 browser.page=null;browser.context=null;
 assert.equal((await browser.exportSession()).stage,'media_pending');assert.deepEqual(calls,['identity']);
});

test('deferred export requires explicit verified email and cannot save an unknown identity',async t=>{
 for(const verified of [false,null,undefined]){
  const {browser,store,calls}=await workerExportFixture(t);browser.deferMedia=true;
  const evaluate=browser.page.evaluate;browser.page.evaluate=async(fn,args)=>{
   const value=await evaluate(fn,args);if(fn.name==='identityInPage')return {...value,emailVerified:verified};return value;
  };
  browser.creationTracker={accepted:async()=>{
   const state=await store.load();await store.save({creationEvidence:{version:1,marker:state.submission.startedAt,requests:0,finished:0,rejected:0,unknown:0,accepted:true,complete:true,sealed:false}});
  }};
  await assert.rejects(browser.exportSession(),error=>['email_verification_required','media_email_unverified'].includes(error.code));
  const state=await store.load();assert.equal(state.mediaCheckpoint,undefined);assert.equal(state.result,undefined);assert.deepEqual(calls,['identity']);
 }
});
