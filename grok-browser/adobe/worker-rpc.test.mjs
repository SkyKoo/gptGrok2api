import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, mkdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL, fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { once } from 'node:events';
import { PrivateRunStore } from './core.mjs';

// Run the actual line-oriented Worker protocol and real durable journal. Only
// the browser dependency is replaced, so tests never launch Chromium or make
// registration requests on the developer's host.
async function rpcFixture(t,{submitted=false,saved=false,missing=false,refresh=false,uiFailed=false,rejected=false,closeFailed=false,launchFailed=false,countryProbe=false,mediaFailed=false,
  checkpoint=false,mediaMode=false,missingBrowser=false,checkpointValue}={}) {
  const root=await mkdtemp(join(tmpdir(),'adobe-worker-rpc-'));
  const store=new PrivateRunStore(root,'original');
  if(!missing){
    await store.init({email:'fixture@example.test',config:{profile:{country:'US'},password:'fixture-password'}});
    if(submitted)await store.markSubmissionStarted();
    if(submitted&&(mediaFailed||checkpoint))await store.save({creationEvidence:{version:1,marker:(await store.load()).submission.startedAt,
      requests:1,finished:1,rejected:0,unknown:0,accepted:true,complete:true,sealed:true}});
    if(checkpoint)await store.save({stage:'media_pending',mediaCheckpoint:checkpointValue===undefined?{version:1,email:'fixture@example.test',account_id:'fixture-id',emailVerified:true,createdAt:new Date().toISOString()}:checkpointValue});
    if(mediaMode&&!missingBrowser)await mkdir(join(store.runDir,'browser'),{mode:0o700});
    if(saved)await store.save({stage:'credentials_saved',result:{account:{email:'fixture@example.test',account_id:'fixture-id',token:'fixture-token'}}});
    await store.close();
  }
  const mock=join(root,'mock-browser.mjs'),loader=join(root,'loader.mjs');
  await writeFile(mock,`
    import {RegistrationError} from ${JSON.stringify(new URL('./core.mjs',import.meta.url).href)};
    export class BrowserRegistration {
      constructor(store,options={}){this.store=store;this.startupProgress=options.startupProgress;this.opened=false;this.phase='awaiting_password';}
      async open(options){
        await this.startupProgress.run('starting_browser',async()=>{});
        this.opened=true;
        const state=await this.store.load();
        await this.store.save({testOpenMode:options.resumeOnly,testRequireExistingProfile:options.requireExistingProfile});
        if(state.submission.started&&${mediaFailed}){this.phase='firefly';await this.store.setStage('outcome_unknown','submission_result_unknown');}
        else if(state.submission.started){this.phase='challenge';await this.store.setStage('needs_manual','human_verification_required');}
        else if(${uiFailed}){this.phase='needs_manual';await this.store.setStage('needs_manual','login_ui_not_ready');}
        else if(${countryProbe}){this.phase='awaiting_profile';await this.store.setStage('awaiting_profile');}
        else await this.store.setStage('awaiting_approval');
      }
      async status(){
        if(!this.opened)throw new Error('browser not opened');
        const state=await this.store.load();
        return {stage:state.stage,code:state.code,detected:this.phase,
          form:{host:this.phase==='firefly'?'firefly.adobe.com':'auth.services.adobe.com',
            fields:[],headings:[],notices:[],buttons:[],agreements:[],
            humanVerification:this.phase==='challenge'?{required:true,source:'visible_instruction'}:{required:false}},networkDiagnosis:{code:null}};
      }
      async advance(input){await this.store.save({testFilledProfile:input.profile});await this.store.setStage('awaiting_approval');}
      async approveCreate(){
        await this.store.markSubmissionStarted();
        const state=await this.store.load();await this.store.save({testCreates:(state.testCreates||0)+1,
          creationEvidence:{version:1,marker:state.submission.startedAt,requests:1,finished:1,rejected:${rejected}?1:0,unknown:0,accepted:!${rejected},complete:true,sealed:false}});
        this.phase=${rejected}?'challenge':'awaiting_verification';
        if(${rejected})await this.store.setStage('needs_manual','human_verification_required');
      }
      async verify(){await this.store.save({verificationSubmitted:true});this.phase='firefly';}
      async exportSession(){
        if(this.deferMedia){
          const {saveMediaCheckpoint}=await import(${JSON.stringify(new URL('./registration-media.mjs',import.meta.url).href)});
          await saveMediaCheckpoint(this.store,{email:'fixture@example.test',account_id:'fixture-id',token:'fixture-token',emailVerified:true});
          return {stage:'media_pending'};
        }
        if(${mediaFailed}){
          const error=new RegistrationError('media_session_incomplete');
          error.diagnostics={media_runtime:{state:'pending',readiness_reason:'vendors_pending',vendors_started:3,vendors_complete:2,token:'PRIVATE'},
            media_page:{route:'editor',matching_host_count:1,authentication_count:1,arp_observer_count:1,scan_complete:true,url:'PRIVATE'},
            media_timing:{emissions:3,first_source_ms:0,raw:'PRIVATE'},cookie:'PRIVATE'};
          throw error;
        }
        const state=await this.store.load();
        const result=state.result||{account:{email:state.email,account_id:'fixture-id',token:'fixture-token'}};
        await this.store.save({stage:'completed',result});return {stage:'completed'};
      }
      async close(){
        if(${closeFailed})throw new Error('PRIVATE close failure');
        const state=await this.store.load();
        if(state.creationEvidence)await this.store.save({creationEvidence:{...state.creationEvidence,sealed:true}});
      }
    }
  `,{mode:0o600});
  const mediaMock=join(root,'mock-media-runtime.mjs');
  if(mediaMode)await writeFile(mediaMock,`
    import assert from 'node:assert/strict';
    export {validateMediaState} from ${JSON.stringify(new URL('./registration-media.mjs',import.meta.url).href)};
    export async function launchPersistentBrowser(dir,options){
      assert.equal(dir,${JSON.stringify(join(root,'original','browser'))});
      assert.equal(options.restoreLastSession,false);
      return {close:async()=>{}};
    }
    export async function prepareRegisteredMedia(context,store,options){
      const state=await store.load();assert.ok(context);assert.equal(options.email,state.email);
      await store.save({testMediaCalls:(state.testMediaCalls||0)+1});
      if(${mediaFailed})return {stage:'media_pending',code:'media_session_incomplete'};
      const account={email:state.email,account_id:'fixture-id',token:'fresh-token',cookie:'sherlockToken=fresh-arp',media_session_arp_mode:'present',media_session_arp_source:'spl_runtime'};
      await store.save({stage:'completed',result:{account,fireflySessionVerified:true,emailVerified:true,exportComplete:true,generationVerified:false}});
      return {stage:'completed',account:{...account,profile_id:store.runId,password:state.config.password}};
    }
  `,{mode:0o600});
  const countryMock=join(root,'mock-country.mjs');
  if(countryProbe)await writeFile(countryMock,`
    import {prepareRegistrationCountry as prepare} from ${JSON.stringify(new URL('./exit-country.mjs',import.meta.url).href)};
    export const prepareRegistrationCountry=(store,options)=>prepare(store,options,async proxy=>{
      const state=await store.load();await store.save({testCountryProbes:(state.testCountryProbes||0)+1});
      return {country:'JP',source:'ipwho_is'};
    });
  `,{mode:0o600});
  const refreshMock=join(root,'mock-refresh.mjs');
  if(refresh)await writeFile(refreshMock,`
    let owned;
    const fixed=code=>Object.assign(new Error(code),{code});
    export async function launchPersistentBrowser(){
      if(${launchFailed})throw Object.assign(new Error('PRIVATE browser failure'),{code:'browser_launch_timeout'});
      if(owned)throw fixed('test_duplicate_context');
      owned={phase:'fresh',close:async()=>{owned.phase='closed'}};return owned;
    }
    export async function refreshSession(context,expected,options){
      if(context!==owned||options.workerMode!==true)throw fixed('test_wrong_context');
      if(context.phase==='fresh'&&options.code===''){
        context.phase='pending';return {stage:'awaiting_verification',code:'refresh_email_otp_required',verification_requested_at:'2026-11-12T00:00:00Z'};
      }
      if(context.phase!=='pending')throw fixed('refresh_otp_already_submitted');
      context.phase='submitted';
      if(options.code!=='123456')throw fixed('refresh_otp_invalid');
      return {email:expected.email,account_id:expected.account_id,profile_id:expected.profile_id,token:'fixture-token',media_session_refreshed:true};
    }
  `,{mode:0o600});
  await writeFile(loader,`export async function resolve(specifier,context,next){
    if(${mediaMode}&&specifier==='./browser-runtime.mjs'&&context.parentURL?.endsWith('/worker-rpc.mjs'))
      return {url:${JSON.stringify(pathToFileURL(mediaMock).href)},shortCircuit:true};
    if(${mediaMode}&&specifier==='./registration-media.mjs'&&context.parentURL?.endsWith('/worker-rpc.mjs'))
      return {url:${JSON.stringify(pathToFileURL(mediaMock).href)},shortCircuit:true};
    if(specifier==='./browser.mjs'&&context.parentURL?.endsWith('/worker-rpc.mjs'))
      return {url:${JSON.stringify(pathToFileURL(mock).href)},shortCircuit:true};
    if(${countryProbe}&&specifier==='./exit-country.mjs'&&context.parentURL?.endsWith('/worker-rpc.mjs'))
      return {url:${JSON.stringify(pathToFileURL(countryMock).href)},shortCircuit:true};
    if(${refresh}&&['./session-refresh.mjs','./browser-runtime.mjs'].includes(specifier)&&context.parentURL?.endsWith('/worker-rpc.mjs'))
      return {url:${JSON.stringify(pathToFileURL(refreshMock).href)},shortCircuit:true};
    return next(specifier,context);
  }`,{mode:0o600});
  const child=spawn(process.execPath,['--experimental-loader',loader,fileURLToPath(new URL('./worker-rpc.mjs',import.meta.url))],{
    env:{...process.env,CFM_ADOBE_PROFILE_ROOT:root},stdio:['pipe','pipe','pipe'],
  });
  child.stderr.resume();
  const exited=once(child,'exit');
  const lines=createInterface({input:child.stdout})[Symbol.asyncIterator]();
  let serial=0;
  const startupFrames=[];
  const command=async request=>{
    const id=String(++serial);child.stdin.write(JSON.stringify({...request,id})+'\n');
    let timer;
    try{
      const deadline=new Promise((_,reject)=>{timer=setTimeout(()=>reject(new Error('RPC test timeout')),22000);});
      let count=0;
      while(true){
        const line=await Promise.race([lines.next(),deadline]);
        assert.equal(line.done,false);const response=JSON.parse(line.value);assert.equal(response.id,id);
        if(response.event==='startup_progress'){
          assert.equal(request.action,'init');assert.ok(++count<=32);
          assert.equal(response.progress.sequence,count);startupFrames.push(response);continue;
        }
        if(request.action==='close')child.stdin.end();
        return response;
      }
    }finally{clearTimeout(timer);}
  };
  t.after(async()=>{
    if(child.exitCode===null){child.kill('SIGKILL');await exited;}
    await rm(root,{recursive:true,force:true});
  });
  const initialize=(extra={})=>command({action:'init',mode:'resume',profile_id:'original',email:'fixture@example.test',profile:{country:'US'},recovery_id:'child-1',...extra});
  const journal=async()=>{
    const current=new PrivateRunStore(root,'original');
    await current.init({}, {requireExisting:true});
    const state=await current.load();await current.close();return state;
  };
  return {initialize,command,exited,journal,startupFrames,root};
}

test('real RPC persists safe media export failures for both signup and accepted recovery',async t=>{
  for(const mode of ['registration','resume'])await t.test(mode,async sub=>{
    const f=await rpcFixture(sub,{submitted:mode==='resume',mediaFailed:true});
    assert.equal((await f.initialize({mode})).result.stage,'ready');
    let response;
    if(mode==='registration'){
      assert.equal((await f.command({action:'run'})).result.code,'verification_required');
      response=await f.command({action:'verify',code:'123456'});
    }else response=await f.command({action:'resume'});
    assert.equal(response.ok,true);assert.equal(response.result.code,'media_session_incomplete');
    assert.equal(response.result.creation_outcome,'accepted');assert.equal(response.result.account,undefined);
    const diagnostics=response.result.diagnostics;
    assert.deepEqual(diagnostics.media_runtime,{state:'pending',readiness_reason:'vendors_pending',vendors_started:3,vendors_complete:2});
    assert.equal(diagnostics.media_page.route,'editor');assert.deepEqual(diagnostics.media_timing,{emissions:3,first_source_ms:0});
    assert.doesNotMatch(JSON.stringify(diagnostics),/PRIVATE|token|cookie|url|raw/);
    await f.command({action:'close'});await f.exited;
    const state=await f.journal();
    assert.deepEqual(state.lastObservation,diagnostics);assert.equal(state.creationEvidence.requests,1);
    assert.equal(state.creationEvidence.accepted,true);assert.equal(state.result,undefined);
    assert.equal(state.testCreates,mode==='registration'?1:undefined);
  });
});

test('RPC prepares one country before creating, and passes the saved selection into form filling',async t=>{
  const f=await rpcFixture(t,{missing:true,countryProbe:true});
  assert.equal((await f.initialize({mode:'registration',require_new:true,profile:{country:'US',birthYear:1988,birthMonth:6}})).result.stage,'ready');
  for(let i=0;i<2;i++)assert.deepEqual((await f.command({action:'prepare'})).result,
    {stage:'prepared',registration_country:{country:'JP',source:'ipwho_is'}});
  const result=(await f.command({action:'run'})).result;
  assert.equal(result.code,'verification_required');
  await f.command({action:'close'});await f.exited;
  const state=await f.journal();
  assert.equal(state.testCountryProbes,1);assert.equal(state.testCreates,1);
  assert.equal(state.testFilledProfile.country,'JP');assert.equal(state.testFilledProfile.birthYear,1988);
  assert.equal(state.config.profile.countrySource,'ipwho_is');
});

test('RPC country preparation preserves a submitted original profile without querying again',async t=>{
  const f=await rpcFixture(t,{submitted:true,countryProbe:true});
  assert.equal((await f.initialize()).result.stage,'ready');
  assert.deepEqual((await f.command({action:'prepare'})).result,
    {stage:'prepared',registration_country:{country:'US',source:'preserved'}});
  await f.command({action:'close'});await f.exited;
  const state=await f.journal();assert.equal(state.testCountryProbes,undefined);assert.equal(state.config.profile.country,'US');
});

test('init progress frames share the request id and do not consume the final RPC response',async t=>{
  const f=await rpcFixture(t,{uiFailed:true});
  const initialized=await f.initialize({mode:'registration'});
  assert.equal(initialized.ok,true);assert.equal(initialized.result.stage,'ready');
  assert.equal(f.startupFrames.length,2);
  assert.deepEqual(f.startupFrames.map(frame=>frame.id),[initialized.id,initialized.id]);
  assert.deepEqual(f.startupFrames.map(frame=>frame.progress.status),['started','completed']);
  assert.ok(f.startupFrames.every(frame=>frame.progress.stage==='starting_browser'));
  const before=f.startupFrames.length;
  assert.equal((await f.command({action:'run'})).result.code,'login_ui_not_ready');
  assert.equal(f.startupFrames.length,before);
  await f.command({action:'close'});await f.exited;
});

test('refresh initialization emits only launch progress and preserves launch failures',async t=>{
  for(const launchFailed of [false,true])await t.test(String(launchFailed),async sub=>{
    const f=await rpcFixture(sub,{refresh:true,launchFailed});
    const initialized=await f.initialize({mode:'refresh'});
    assert.equal(initialized.ok,!launchFailed);
    if(launchFailed)assert.equal(initialized.code,'browser_launch_timeout');
    assert.equal(f.startupFrames.length,2);
    assert.deepEqual(f.startupFrames.map(frame=>frame.progress.stage),['starting_browser','starting_browser']);
    assert.deepEqual(f.startupFrames.map(frame=>frame.progress.status),['started',launchFailed?'failed':'completed']);
    if(launchFailed)assert.equal(f.startupFrames[1].progress.exception_type,'timeout');
    assert.doesNotMatch(JSON.stringify(f.startupFrames),/PRIVATE|fixture@|fixture-password/);
    await f.command({action:'close'});await f.exited;
  });
});

test('RPC init opens a proven unsubmitted existing profile and resume creates at most once across verify',async t=>{
  const f=await rpcFixture(t);
  assert.equal((await f.initialize()).result.stage,'ready');
  const first=(await f.command({action:'resume'})).result;
  assert.equal(first.code,'verification_required');assert.equal(first.submission_started,true);
  assert.equal(first.creation_outcome,'accepted');
  assert.equal(first.recovery_mode,'continue_registration');
  const completed=(await f.command({action:'verify',code:'123456'})).result;
  assert.equal(completed.stage,'completed');assert.equal(completed.submission_started,true);
  assert.equal(completed.creation_outcome,'accepted');
  assert.equal(completed.account.profile_id,'original');
  assert.equal((await f.command({action:'resume'})).result.stage,'completed');
  await f.command({action:'close'});await f.exited;
  const state=await f.journal();assert.equal(state.testCreates,1);assert.equal(state.testOpenMode,false);
  assert.equal(state.config.password,'fixture-password');assert.equal(state.submission.started,true);
});

test('RPC initial UI failure returns not_submitted and seals the browser before allowing another attempt',async t=>{
  const f=await rpcFixture(t,{uiFailed:true});await f.initialize({mode:'registration'});
  const result=(await f.command({action:'run'})).result;
  assert.equal(result.code,'login_ui_not_ready');assert.equal(result.creation_outcome,'not_submitted');
  const ended=await f.command({action:'run'});assert.equal(ended.ok,false);assert.equal(ended.code,'registration_attempt_ended');
  await f.command({action:'close'});await f.exited;
  const state=await f.journal();assert.equal(state.submission.started,false);assert.ok(state.lastLifecycle.closeCompletedAt);
});

test('RPC rejected creation is sent only after closure; a failed close preserves unknown',async t=>{
  for(const closeFailed of [false,true])await t.test(String(closeFailed),async sub=>{
    const f=await rpcFixture(sub,{rejected:true,closeFailed});await f.initialize({mode:'registration'});
    const response=await f.command({action:'run'});
    const result=response.ok?response.result:response;
    assert.equal(result.creation_outcome,closeFailed?'unknown':'rejected');
    assert.equal(result.submission_started,true);
    if(!closeFailed){
      assert.equal(result.creation_error_code,'captcha_required');assert.equal(result.creation_http_status,400);
      await f.command({action:'close'});await f.exited;
      assert.ok((await f.journal()).lastLifecycle.closeCompletedAt);
    }
  });
});

test('RPC resume for a legacy submitted journal remains unknown without opening login or registration',async t=>{
  const f=await rpcFixture(t,{submitted:true});
  assert.equal((await f.initialize()).result.stage,'ready');
  const result=(await f.command({action:'resume'})).result;
  assert.equal(result.code,'creation_result_unknown');assert.equal(result.submission_started,true);
  assert.equal(result.creation_outcome,'unknown');
  await f.command({action:'close'});await f.exited;
  const state=await f.journal();assert.equal(state.testCreates,undefined);assert.equal(state.testOpenMode,true);
  assert.equal(state.testRequireExistingProfile,true);
  assert.equal(state.lastObservation.captured_at,result.diagnostics.captured_at);
  const timeline=state.lastLifecycle;
  assert.equal(timeline.diagnosticsCapturedAt,result.diagnostics.captured_at);
  assert.ok(Date.parse(timeline.diagnosticsCapturedAt)<=Date.parse(timeline.responsePreparedAt));
  assert.ok(Date.parse(timeline.responsePreparedAt)<=Date.parse(timeline.closeStartedAt));
  assert.ok(Date.parse(timeline.closeStartedAt)<=Date.parse(timeline.closeCompletedAt));
  assert.doesNotMatch(JSON.stringify({timeline,diagnostics:state.lastObservation}),/fixture-password|fixture-token|fixture@example/);
});

test('RPC new-registration flag cannot reuse a prior profile, while recovery remains explicit',async t=>{
  const f=await rpcFixture(t);
  const result=await f.initialize({mode:'registration',require_new:true});
  assert.equal(result.ok,false);
  assert.equal(result.code,'run_already_exists');
  await f.command({action:'close'});await f.exited;
  assert.equal((await f.journal()).testOpenMode,undefined);
});

test('RPC saved export is offline and missing profile cannot become a fresh registration',async t=>{
  const saved=await rpcFixture(t,{submitted:true,saved:true});
  assert.equal((await saved.initialize()).result.stage,'ready');
  assert.equal((await saved.command({action:'resume'})).result.stage,'completed');
  await saved.command({action:'close'});await saved.exited;
  assert.equal((await saved.journal()).testOpenMode,undefined);
  const missing=await rpcFixture(t,{missing:true});
  const rejected=await missing.initialize();assert.equal(rejected.ok,false);assert.equal(rejected.code,'state_missing');
  assert.equal(rejected.submission_started,undefined);
  await missing.command({action:'close'});await missing.exited;
});

test('RPC refresh preserves the waiting context and forwards one OTP without wrapping pending as a completed account',async t=>{
  const f=await rpcFixture(t,{refresh:true});
  assert.equal((await f.initialize({mode:'refresh',account_id:'fixture-id'})).result.stage,'ready');
  const pending=await f.command({action:'refresh'});
  assert.equal(pending.ok,true);assert.equal(pending.result.stage,'awaiting_verification');
  assert.equal(pending.result.code,'refresh_email_otp_required');assert.equal(pending.result.account,undefined);
  assert.equal(pending.result.verification_requested_at,'2026-11-12T00:00:00Z');
  const completed=await f.command({action:'refresh',code:'123456'});
  assert.equal(completed.result.stage,'completed');assert.equal(completed.result.account.account_id,'fixture-id');
  assert.equal(completed.result.account.profile_id,'original');assert.equal(completed.result.media_session_refreshed,true);
  const duplicate=await f.command({action:'refresh',code:'123456'});
  assert.equal(duplicate.ok,false);assert.equal(duplicate.code,'refresh_otp_already_submitted');
  await f.command({action:'close'});await f.exited;
});

test('RPC deferred registration returns accepted media pending without credentials',async t=>{
 const f=await rpcFixture(t);
 assert.equal((await f.initialize({mode:'registration',defer_media:true})).result.stage,'ready');
 assert.equal((await f.command({action:'run'})).result.code,'verification_required');
 const result=(await f.command({action:'verify',code:'123456'})).result;
 assert.equal(result.stage,'media_pending');assert.equal(result.code,'');assert.equal(result.account,undefined);assert.equal(result.creation_outcome,'accepted');
 await f.command({action:'close'});await f.exited;
 const state=await f.journal();assert.equal(state.stage,'media_pending');assert.equal(state.result,undefined);assert.equal(state.testCreates,1);
 assert.equal(state.mediaCheckpoint.account_id,'fixture-id');assert.doesNotMatch(JSON.stringify(state.mediaCheckpoint),/token|cookie|password/);
});

test('RPC resumes an existing media checkpoint without opening registration again',async t=>{
 const f=await rpcFixture(t,{submitted:true,checkpoint:true});
 assert.equal((await f.initialize({defer_media:true})).result.stage,'ready');
 const result=(await f.command({action:'resume'})).result;
 assert.equal(result.stage,'media_pending');assert.equal(result.code,'');assert.equal(result.account,undefined);assert.equal(result.creation_outcome,'accepted');
 await f.command({action:'close'});await f.exited;
 const state=await f.journal();assert.equal(state.testOpenMode,undefined);assert.equal(state.testCreates,undefined);assert.equal(state.result,undefined);
});

test('RPC media uses the existing profile without restored tabs and only exposes the media action',async t=>{
 const f=await rpcFixture(t,{submitted:true,checkpoint:true,mediaMode:true});
 assert.equal((await f.initialize({mode:'media',proxy:'direct'})).result.stage,'ready');
 for(const action of ['prepare','run','verify','resume','refresh']){
  const result=await f.command({action});assert.equal(result.ok,false);
 }
 const result=(await f.command({action:'media'})).result;
 assert.equal(result.stage,'completed');assert.equal(result.creation_outcome,'accepted');assert.equal(result.account.profile_id,'original');assert.equal(result.account.password,'fixture-password');
 await f.command({action:'close'});await f.exited;
 const state=await f.journal();assert.equal(state.testOpenMode,undefined);assert.equal(state.testCreates,undefined);assert.equal(state.testMediaCalls,1);
});

test('RPC media refuses missing journals, unknown creation, mismatched email and missing browser profiles before launch',async t=>{
 for(const [options,extra,expected] of [
  [{missing:true},{},'state_missing'],
  [{submitted:true},{legacy_media:true},'media_creation_not_accepted'],
  [{submitted:true,checkpoint:true},{email:'other@example.test'},'run_email_mismatch'],
  [{submitted:true,checkpoint:true,missingBrowser:true},{},'browser_profile_missing'],
 ])await t.test(expected,async sub=>{
  const f=await rpcFixture(sub,{...options,mediaMode:true});const result=await f.initialize({mode:'media',...extra});
  assert.equal(result.ok,false);assert.equal(result.code,expected);assert.equal(f.startupFrames.length,0);
  await f.command({action:'close'});await f.exited;
 });
});

test('RPC media failure leaves checkpoint pending without creating or returning an account',async t=>{
 const f=await rpcFixture(t,{submitted:true,checkpoint:true,mediaMode:true,mediaFailed:true});
 assert.equal((await f.initialize({mode:'media'})).result.stage,'ready');
 const result=(await f.command({action:'media'})).result;
 assert.equal(result.stage,'media_pending');assert.equal(result.code,'media_session_incomplete');assert.equal(result.account,undefined);assert.equal(result.creation_outcome,'accepted');
 await f.command({action:'close'});await f.exited;
 const state=await f.journal();assert.equal(state.mediaCheckpoint.account_id,'fixture-id');assert.equal(state.result,undefined);assert.equal(state.testCreates,undefined);
});

test('RPC deferred resume rejects every malformed checkpoint before opening a browser or login',async t=>{
 for(const checkpointValue of [null,false,'',{},[]])await t.test(JSON.stringify(checkpointValue),async sub=>{
  const f=await rpcFixture(sub,{submitted:true,checkpoint:true,checkpointValue});
  const response=await f.initialize({defer_media:true});
  assert.equal(response.ok,false);assert.equal(response.code,'media_checkpoint_invalid');assert.equal(f.startupFrames.length,0);
  await f.command({action:'close'});await f.exited;
  const state=await f.journal();assert.equal(state.testOpenMode,undefined);assert.equal(state.testCreates,undefined);assert.equal(state.result,undefined);
 });
});
