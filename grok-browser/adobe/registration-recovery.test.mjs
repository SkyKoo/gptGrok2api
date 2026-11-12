import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { PrivateRunStore, RegistrationError } from './core.mjs';
import { BrowserRegistration } from './browser.mjs';
import { resumeRegistration, requestRecoveryCode, submitRecoveryLogin, openRecoveryLogin } from './registration-recovery.mjs';

const form = patch => ({fields:[],headings:[],notices:[],buttons:[],agreements:[],host:'auth.services.adobe.com',humanVerification:{required:false},...patch});
const field = (type,autocomplete=type) => ({tag:'input',type,id:type,name:type,label:type,autocomplete});
const status = (stage,patch={}) => ({detected:stage,form:form(patch),networkDiagnosis:{code:null}});

async function fixture(t,{phase='awaiting_verification',submitted=true}={}) {
  const root=await mkdtemp(join(tmpdir(),'adobe-recovery-'));
  const store=new PrivateRunStore(root,'profile');
  t.after(async()=>{await store.close();await rm(root,{recursive:true,force:true});});
  await store.init({email:'fixture@example.test',config:{password:'fixture-password'}});
  if(submitted)await store.markSubmissionStarted();
  const calls=[];
  let challenge=false,network=null,resend=true;
  const control=(kind)=>({count:async()=>kind==='link'?0:kind==='resend'&&!resend?0:1,
    click:async options=>{
      if(options?.trial)return;
      calls.push(kind);
      if(kind==='login')phase=phase==='awaiting_email'?'awaiting_password':'awaiting_verification';
    }});
  const frame={getByRole:(role,{name})=>{
    assert.equal(name.test('Create account'),false);
    return control(role==='link'?'link':name.test('Resend code')?'resend':'login');
  }};
  const browser={page:{url:()=>phase==='firefly'?'https://firefly.adobe.com/':'https://auth.services.adobe.com/signin',goto:async url=>{
    calls.push('navigate');phase=url==='https://firefly.adobe.com/'?'firefly':'awaiting_email';
  }},authFrame:async()=>frame,field:async()=>({fill:async()=>calls.push('fill')}),
    status:async()=>({...status(phase,{host:phase==='firefly'?'firefly.adobe.com':phase==='account'?'account.adobe.com':'auth.services.adobe.com',
      fields:phase==='awaiting_email'?[field('email')]:phase==='awaiting_password'?[field('password','current-password')]:phase==='awaiting_verification'?[field('text','one-time-code')]:[],
      humanVerification:challenge?{required:true,source:'visible_instruction'}:{required:false}}),networkDiagnosis:{code:network}}),
    waitForStage:async()=>({stage:phase,form:form(),code:null}),
    verify:async(input,{recoveryId})=>{calls.push('verify');assert.equal(input.code,'123456');
      const state=await store.load(),attempt=state.recoveryAttempts[recoveryId];
      assert.equal(attempt.verificationSubmitted,undefined);
      await store.save({recoveryAttempts:{...state.recoveryAttempts,[recoveryId]:{...attempt,verificationSubmitted:true}}});phase='account';},
    exportSession:async()=>{calls.push('export');return {stage:'completed'}},
    approveCreate:()=>assert.fail('Recovery must never create an account'),
    advance:()=>assert.fail('Recovery must not use the signup advance driver')};
  return {store,browser,calls,setPhase:value=>{phase=value},challenge:value=>{challenge=value},network:value=>{network=value},resend:value=>{resend=value}};
}

test('recovery reuses saved credentials without inspecting or reopening the browser',async t=>{
  const f=await fixture(t);
  await f.store.save({result:{account:{email:'fixture@example.test'}}});
  f.browser.status=()=>assert.fail('Saved export is offline');
  assert.equal((await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'})).stage,'completed');
  assert.deepEqual(f.calls,['export']);
});

test('resume requires a bounded recovery identity before changing any journal',async t=>{
  const f=await fixture(t,{submitted:false});
  assert.equal((await resumeRegistration(f.browser,f.store,{recoveryId:'../secret'})).code,'recovery_id_required');
  assert.deepEqual(f.calls,[]);
  assert.equal((await f.store.load()).submission.started,false);
});

test('an authoritative unsubmitted journal continues its original registration and creates only once across commands',async t=>{
  const f=await fixture(t,{submitted:false,phase:'awaiting_password'});
  await f.store.setStage('awaiting_approval');
  let created=0,verified=0,phase='awaiting_password';
  f.browser.status=async()=>{const state=await f.store.load();return {...status(phase),stage:state.stage,code:state.code};};
  f.browser.approveCreate=async()=>{created++;await f.store.markSubmissionStarted();phase='awaiting_verification';};
  f.browser.verify=async input=>{assert.equal(input.code,'123456');verified++;await f.store.save({verificationSubmitted:true});phase='firefly';};
  const statusBefore=f.browser.status;
  f.browser.status=async()=>{const current=await statusBefore();if(phase==='firefly')current.form.host='firefly.adobe.com';return current;};
  let result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'});
  assert.equal(result.code,'verification_required');assert.equal(result.recovery_mode,'continue_registration');assert.equal(created,1);
  const started=(await f.store.load()).submission.startedAt;
  result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1',code:'123456'});
  assert.equal(result.stage,'completed');assert.equal(created,1);assert.equal(verified,1);
  assert.equal((await f.store.load()).submission.startedAt,started);
  // A different child attempt with the same now-submitted journal can only
  // recover that account; returning to signup does not authorize Create again.
  phase='awaiting_approval';
  f.browser.page.goto=async()=>{phase='needs_manual';};
  let now=0;
  await resumeRegistration(f.browser,f.store,{recoveryId:'attempt2',loginOptions:{timeoutMs:10,now:()=>now,wait:async ms=>{now+=ms;}}});
  assert.equal(created,1);assert.equal((await f.store.load()).submission.startedAt,started);
});

test('a visible challenge remains blocked even when a cancelled verification request also exists',async t=>{
  const f=await fixture(t);f.challenge(true);f.network('verification_request_aborted');
  // The old signup's terminal marker does not consume the new recovery ID's
  // observation budget. Only a marker written by this attempt can do that.
  await f.store.setStage('needs_manual','human_verification_required');
  const before=(await f.store.load()).submission;
  let now=0;
  const verificationWait={now:()=>now,pause:async ms=>{now+=ms;}};
  const result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1',verificationWait});
  assert.equal(result.code,'human_verification_required');
  assert.equal(result.form.humanVerification.source,'visible_instruction');
  assert.deepEqual(f.calls,[]);
  assert.deepEqual((await f.store.load()).submission,before);
  assert.equal(now,15000);
  await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1',verificationWait});
  assert.equal(now,15000);
  assert.ok((await f.store.load()).recoveryAttempts.attempt1.verificationObservationStartedAt);
  await resumeRegistration(f.browser,f.store,{recoveryId:'attempt2',verificationWait});
  assert.equal(now,30000);
  assert.ok((await f.store.load()).recoveryAttempts.attempt2.verificationObservationStartedAt);
  assert.deepEqual(f.calls,[]);assert.deepEqual((await f.store.load()).submission,before);
});

test('same-attempt verification reentry cannot restart its persisted observation window',async t=>{
  const f=await fixture(t);f.challenge(true);
  await f.store.save({recoveryAttempts:{attempt1:{verificationObservationStartedAt:new Date().toISOString(),
    verificationRequestedAt:new Date().toISOString()}}});
  const before=(await f.store.load()).submission;
  const result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1',code:'123456',
    verificationWait:{pause:async()=>assert.fail('Do not append a second observation window')}});
  assert.equal(result.code,'human_verification_required');assert.deepEqual(f.calls,[]);
  assert.deepEqual((await f.store.load()).submission,before);
});

test('recovery observes temporary instruction progress and cancellation leaves all submission fences unchanged',async t=>{
  const f=await fixture(t);f.challenge(true);let now=0;
  const verificationWait={now:()=>now,pause:async ms=>{now+=ms;f.challenge(false);}};
  const result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1',verificationWait});
  assert.equal(result.stage,'awaiting_verification');assert.equal(now,250);assert.deepEqual(f.calls,['resend']);
  f.challenge(true);let stop=false;
  const before=(await f.store.load()).submission;
  const cancelled=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt2',shouldStop:()=>stop,
    verificationWait:{now:()=>now,pause:async ms=>{now+=ms;stop=true;}}});
  assert.equal(cancelled.code,'automatic_interrupted');assert.deepEqual(f.calls,['resend']);
  assert.deepEqual((await f.store.load()).submission,before);
});

test('an old OTP form resends once per attempt and verify never resends or clears original markers',async t=>{
  const f=await fixture(t);
  await f.store.save({verificationSubmitted:true});
  const before=(await f.store.load()).submission;
  const first=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'});
  assert.equal(first.stage,'awaiting_verification');
  assert.ok(Date.parse(first.verification_requested_at)>0);
  const repeat=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'});
  assert.equal(repeat.verification_requested_at,first.verification_requested_at);
  assert.deepEqual(f.calls,['resend']);
  const result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1',code:'123456'});
  assert.equal(result.stage,'completed');
  assert.deepEqual(f.calls,['resend','verify','navigate','export']);
  const state=await f.store.load();
  assert.equal(state.verificationSubmitted,true);
  assert.equal(state.recoveryAttempts.attempt1.verificationSubmitted,true);
  assert.deepEqual(state.submission,before);
});

test('a later compensation attempt can request a fresh OTP while the earlier attempt remains immutable',async t=>{
  const f=await fixture(t);
  await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'});
  const previous=(await f.store.load()).recoveryAttempts.attempt1;
  await resumeRegistration(f.browser,f.store,{recoveryId:'attempt2'});
  assert.deepEqual(f.calls,['resend','resend']);
  assert.deepEqual((await f.store.load()).recoveryAttempts.attempt1,previous);
});

test('missing or failed resend cannot cause a speculative verification timestamp or duplicate send',async t=>{
  const f=await fixture(t);f.resend(false);
  assert.equal((await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'})).code,'recovery_verification_unavailable');
  assert.deepEqual(f.calls,[]);
  f.browser.authFrame=async()=>({getByRole:(role)=>({count:async()=>role==='button'?1:0,click:async options=>{
    if(!options?.trial){f.calls.push('resend');throw new Error('private transport error');}
  }})});
  assert.equal((await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'})).code,'recovery_verification_result_unknown');
  assert.equal((await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'})).code,'recovery_verification_result_unknown');
  assert.deepEqual(f.calls,['resend']);
  const attempt=(await f.store.load()).recoveryAttempts.attempt1;
  assert.ok(attempt.resendStartedAt);assert.equal(attempt.verificationRequestedAt,undefined);
});

test('normal existing-account login continues email/password then waits for its new OTP without Resend',async t=>{
  const f=await fixture(t,{phase:'awaiting_email'});
  const before=(await f.store.load()).submission;
  const result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'});
  assert.equal(result.stage,'awaiting_verification');
  assert.deepEqual(f.calls,['fill','login','fill','login']);
  const state=await f.store.load(),attempt=state.recoveryAttempts.attempt1;
  assert.ok(attempt.emailSubmittedAt);assert.ok(attempt.passwordSubmittedAt);
  assert.equal(result.verification_requested_at,attempt.passwordSubmittedAt);
  assert.deepEqual(state.submission,before);
});

test('a failed login click is persisted before submission and cannot replay within one recovery attempt',async t=>{
  const f=await fixture(t,{phase:'awaiting_email'});
  f.browser.authFrame=async()=>({getByRole:()=>({count:async()=>1,click:async options=>{
    if(!options?.trial){f.calls.push('login');throw new Error('private failure');}
  }})});
  for(let i=0;i<2;i++)assert.equal((await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'})).code,'recovery_login_result_unknown');
  assert.deepEqual(f.calls,['fill','login']);
  assert.ok((await f.store.load()).recoveryAttempts.attempt1.emailSubmittedAt);
});

test('a login click aborted by normal navigation proceeds only after current OTP evidence, while explicit errors stay explicit',async t=>{
  const f=await fixture(t,{phase:'awaiting_password'});
  f.browser.authFrame=async()=>({getByRole:()=>({count:async()=>1,click:async options=>{
    if(!options?.trial){f.calls.push('login');f.setPhase('awaiting_verification');throw new Error('navigation aborted');}
  }})});
  let result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1'});
  assert.equal(result.stage,'awaiting_verification');assert.deepEqual(f.calls,['fill','login']);
  f.setPhase('awaiting_password');
  f.browser.waitForStage=async()=>({code:'form_rejected'});
  result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt2'});
  assert.equal(result.code,'form_rejected');
});

test('recovery login rejects signup forms and never fills new passwords',async t=>{
  const f=await fixture(t,{phase:'awaiting_password'});
  const current=status('awaiting_password',{headings:['Create an account'],fields:[field('password','new-password')]});
  await assert.rejects(submitRecoveryLogin(f.browser,f.store,'attempt1',current),error=>error.code==='recovery_login_form_unrecognized');
  assert.deepEqual(f.calls,[]);
});

test('only normal Sign in entry is clicked and a challenge after navigation is retained',async t=>{
  const f=await fixture(t,{phase:'account'});let now=0,entered=false;
  f.browser.page.goto=async()=>{};
  f.browser.status=async()=>entered?status('awaiting_email',{fields:[field('email')]}):status('needs_manual',{host:'account.adobe.com'});
  f.browser.authFrame=async()=>({getByRole:(role,{name})=>{
    assert.equal(name.test('Create an account'),false);assert.equal(name.test('Sign in'),true);
    return {count:async()=>role==='link'?1:0,click:async()=>{entered=true;f.calls.push('sign-in')}};
  }});
  assert.equal((await openRecoveryLogin(f.browser,{now:()=>now,wait:async ms=>{now+=ms;}})).detected,'awaiting_email');
  assert.deepEqual(f.calls,['sign-in']);
  f.browser.status=async()=>status('needs_manual',{humanVerification:{required:true,source:'visible_instruction'}});
  await assert.rejects(openRecoveryLogin(f.browser),error=>error.code==='human_verification_required');
});

test('real verification keeps the original signup flag and fences a separate recovery OTP before filling',async t=>{
  const f=await fixture(t);
  await f.store.save({verificationSubmitted:true,recoveryAttempts:{attempt1:{verificationRequestedAt:new Date().toISOString()}}});
  const browser=new BrowserRegistration(f.store);
  browser.authFrame=async()=>({});browser.form=async()=>form({fields:[field('text','one-time-code')]});
  browser.field=async()=>({fill:async()=>{
    const state=await f.store.load();assert.equal(state.verificationSubmitted,true);
    assert.equal(state.recoveryAttempts.attempt1.verificationSubmitted,true);f.calls.push('fill');
  }});
  browser.waitForStage=async()=>({code:null});browser.status=async()=>({stage:'session'});
  await browser.verify({code:'123456'},{recoveryId:'attempt1'});
  await assert.rejects(browser.verify({code:'123456'},{recoveryId:'attempt1'}),error=>error.code==='verification_already_submitted');
  assert.deepEqual(f.calls,['fill']);assert.equal((await f.store.load()).submission.started,true);
});

test('an already submitted recovery OTP returns unknown instead of prompting another submit',async t=>{
  const f=await fixture(t);
  await f.store.save({recoveryAttempts:{attempt1:{verificationRequestedAt:new Date().toISOString(),verificationSubmitted:true}}});
  const result=await resumeRegistration(f.browser,f.store,{recoveryId:'attempt1',code:'123456'});
  assert.equal(result.code,'verification_result_unknown');assert.deepEqual(f.calls,[]);
});

test('resume-only browser startup never enters the account-creation link even without restored pages',async t=>{
  const f=await fixture(t);const calls=[];
  const page={url:()=> 'about:blank',goto:async url=>calls.push(url),getByRole:()=>assert.fail('No Create account entry')};
  const browser=new BrowserRegistration(f.store,{}, {launchBrowser:async()=>({pages:()=>[page]})});
  browser.status=async()=>({stage:'outcome_unknown'});
  await browser.open({resumeOnly:true});
  assert.deepEqual(calls,['https://account.adobe.com/']);assert.equal((await f.store.load()).submission.started,true);
});
