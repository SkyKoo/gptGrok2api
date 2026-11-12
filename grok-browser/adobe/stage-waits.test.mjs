import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runInNewContext } from 'node:vm';
import { BrowserRegistration, formBlockingCode, waitForFormStage } from './browser.mjs';
import { PrivateRunStore } from './core.mjs';
import { NetworkDiagnostics } from './network-diagnostics.mjs';

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

test('profile keeps the generated day and fills it only when the form requests it', async t => {
  for (const hasDay of [false, true]) {
    const { store, browser } = await fixture(t);
    const names = ['firstName', 'lastName', 'birthMonth', 'birthYear', 'country', ...(hasDay ? ['birthDay'] : [])];
    const fields = names.map(name => ({ tag: 'input', type: 'text', id: name, name, label: name === 'birthMonth' ? 'Birthday month' : name, autocomplete: '' }));
    const filled = {};
    browser.form = async () => form({fields, buttons:[{text:'Create account', disabled:false}]});
    browser.authFrame = async () => ({getByRole:()=>({count:async()=>0})});
    browser.field = async (_frame, descriptor) => ({fill:async value => { filled[descriptor.name] = value; }});
    const profile = {firstName:'Fixture',lastName:'Test',birthYear:1984,birthMonth:2,birthDay:29,country:'US'};
    await browser.advance({profile});
    assert.equal(filled.birthMonth, '2');
    assert.equal(filled.birthDay, hasDay ? '29' : undefined);
    assert.deepEqual((await store.load()).config.profile, profile);
    assert.equal((await store.load()).submission.started, false);
  }
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

test('explicit puzzle instructions stop immediately while persistent frames have a bounded observation window', async () => {
  for (const source of ['visible_challenge_frame','upstream_captcha_response','visible_instruction']) {
    const timing=clock();
    const result=await waitForFormStage(async()=>form({humanVerification:{required:true,source}}),
      {...timing,verificationGraceMs:400,accept:()=>true});
    assert.equal(result.code,'human_verification_required');
    assert.equal(result.stage,'needs_manual');
    assert.equal(timing.elapsed(),source === 'visible_instruction' ? 0 : 400);
  }
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

test('real form/status preserve DNS, timeout and aborted errors while superseding only captcha evidence', async () => {
  for (const [error,code] of [['net::ERR_NAME_NOT_RESOLVED','verification_dns_failed'],
    ['net::ERR_TIMED_OUT','verification_request_timeout'],['net::ERR_ABORTED','verification_request_aborted']]) {
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
