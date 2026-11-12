import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
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
async function rpcFixture(t,{submitted=false,saved=false,missing=false,refresh=false,uiFailed=false,rejected=false,closeFailed=false,launchFailed=false}={}) {
  const root=await mkdtemp(join(tmpdir(),'adobe-worker-rpc-'));
  const store=new PrivateRunStore(root,'original');
  if(!missing){
    await store.init({email:'fixture@example.test',config:{profile:{country:'US'},password:'fixture-password'}});
    if(submitted)await store.markSubmissionStarted();
    if(saved)await store.save({stage:'credentials_saved',result:{account:{email:'fixture@example.test',account_id:'fixture-id',token:'fixture-token'}}});
    await store.close();
  }
  const mock=join(root,'mock-browser.mjs'),loader=join(root,'loader.mjs');
  await writeFile(mock,`
    export class BrowserRegistration {
      constructor(store,options={}){this.store=store;this.startupProgress=options.startupProgress;this.opened=false;this.phase='awaiting_password';}
      async open(options){
        await this.startupProgress.run('starting_browser',async()=>{});
        this.opened=true;
        const state=await this.store.load();
        await this.store.save({testOpenMode:options.resumeOnly,testRequireExistingProfile:options.requireExistingProfile});
        if(state.submission.started){this.phase='challenge';await this.store.setStage('needs_manual','human_verification_required');}
        else if(${uiFailed}){this.phase='needs_manual';await this.store.setStage('needs_manual','login_ui_not_ready');}
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
      async approveCreate(){
        await this.store.markSubmissionStarted();
        const state=await this.store.load();await this.store.save({testCreates:(state.testCreates||0)+1,
          creationEvidence:{version:1,marker:state.submission.startedAt,requests:1,finished:1,rejected:${rejected}?1:0,unknown:0,accepted:!${rejected},complete:true,sealed:false}});
        this.phase=${rejected}?'challenge':'awaiting_verification';
        if(${rejected})await this.store.setStage('needs_manual','human_verification_required');
      }
      async verify(){await this.store.save({verificationSubmitted:true});this.phase='firefly';}
      async exportSession(){
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
    if(specifier==='./browser.mjs'&&context.parentURL?.endsWith('/worker-rpc.mjs'))
      return {url:${JSON.stringify(pathToFileURL(mock).href)},shortCircuit:true};
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
  return {initialize,command,exited,journal,startupFrames};
}

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
