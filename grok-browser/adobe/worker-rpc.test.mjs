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
async function rpcFixture(t,{submitted=false,saved=false,missing=false}={}) {
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
      constructor(store){this.store=store;this.opened=false;this.phase='awaiting_password';}
      async open(options){
        this.opened=true;
        const state=await this.store.load();
        await this.store.save({testOpenMode:options.resumeOnly});
        if(state.submission.started){this.phase='challenge';await this.store.setStage('needs_manual','human_verification_required');}
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
        const state=await this.store.load();await this.store.save({testCreates:(state.testCreates||0)+1});
        this.phase='awaiting_verification';
      }
      async verify(){await this.store.save({verificationSubmitted:true});this.phase='firefly';}
      async exportSession(){
        const state=await this.store.load();
        const result=state.result||{account:{email:state.email,account_id:'fixture-id',token:'fixture-token'}};
        await this.store.save({stage:'completed',result});return {stage:'completed'};
      }
      async close(){}
    }
  `,{mode:0o600});
  await writeFile(loader,`export async function resolve(specifier,context,next){
    if(specifier==='./browser.mjs'&&context.parentURL?.endsWith('/worker-rpc.mjs'))
      return {url:${JSON.stringify(pathToFileURL(mock).href)},shortCircuit:true};
    return next(specifier,context);
  }`,{mode:0o600});
  const child=spawn(process.execPath,['--experimental-loader',loader,fileURLToPath(new URL('./worker-rpc.mjs',import.meta.url))],{
    env:{...process.env,CFM_ADOBE_PROFILE_ROOT:root},stdio:['pipe','pipe','pipe'],
  });
  child.stderr.resume();
  const exited=once(child,'exit');
  const lines=createInterface({input:child.stdout})[Symbol.asyncIterator]();
  let serial=0;
  const command=async request=>{
    const id=String(++serial);child.stdin.write(JSON.stringify({...request,id})+'\n');
    let timer;
    try{
      const line=await Promise.race([lines.next(),new Promise((_,reject)=>{timer=setTimeout(()=>reject(new Error('RPC test timeout')),22000);})]);
      assert.equal(line.done,false);const response=JSON.parse(line.value);assert.equal(response.id,id);
      if(request.action==='close')child.stdin.end();
      return response;
    }finally{clearTimeout(timer);}
  };
  t.after(async()=>{
    if(child.exitCode===null){child.kill('SIGKILL');await exited;}
    await rm(root,{recursive:true,force:true});
  });
  const initialize=()=>command({action:'init',mode:'resume',profile_id:'original',email:'fixture@example.test',profile:{country:'US'},recovery_id:'child-1'});
  const journal=async()=>{
    const current=new PrivateRunStore(root,'original');
    await current.init({}, {requireExisting:true});
    const state=await current.load();await current.close();return state;
  };
  return {initialize,command,exited,journal};
}

test('RPC init opens a proven unsubmitted existing profile and resume creates at most once across verify',async t=>{
  const f=await rpcFixture(t);
  assert.equal((await f.initialize()).result.stage,'ready');
  const first=(await f.command({action:'resume'})).result;
  assert.equal(first.code,'verification_required');assert.equal(first.submission_started,true);
  assert.equal(first.recovery_mode,'continue_registration');
  const completed=(await f.command({action:'verify',code:'123456'})).result;
  assert.equal(completed.stage,'completed');assert.equal(completed.submission_started,true);
  assert.equal(completed.account.profile_id,'original');
  assert.equal((await f.command({action:'resume'})).result.stage,'completed');
  await f.command({action:'close'});await f.exited;
  const state=await f.journal();assert.equal(state.testCreates,1);assert.equal(state.testOpenMode,false);
  assert.equal(state.config.password,'fixture-password');assert.equal(state.submission.started,true);
});

test('RPC resume for a submitted journal keeps challenge evidence and never opens registration mode',async t=>{
  const f=await rpcFixture(t,{submitted:true});
  assert.equal((await f.initialize()).result.stage,'ready');
  const result=(await f.command({action:'resume'})).result;
  assert.equal(result.code,'human_verification_required');assert.equal(result.submission_started,true);
  assert.equal(result.diagnostics.verification_source,'visible_instruction');
  await f.command({action:'close'});await f.exited;
  const state=await f.journal();assert.equal(state.testCreates,undefined);assert.equal(state.testOpenMode,true);
  assert.equal(state.lastObservation.captured_at,result.diagnostics.captured_at);
  const timeline=state.lastLifecycle;
  assert.equal(timeline.diagnosticsCapturedAt,result.diagnostics.captured_at);
  assert.ok(Date.parse(timeline.diagnosticsCapturedAt)<=Date.parse(timeline.responsePreparedAt));
  assert.ok(Date.parse(timeline.responsePreparedAt)<=Date.parse(timeline.closeStartedAt));
  assert.ok(Date.parse(timeline.closeStartedAt)<=Date.parse(timeline.closeCompletedAt));
  assert.doesNotMatch(JSON.stringify({timeline,diagnostics:state.lastObservation}),/fixture-password|fixture-token|fixture@example/);
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
