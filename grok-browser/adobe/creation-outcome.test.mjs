import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { mkdtemp, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { PrivateRunStore } from './core.mjs';
import { BrowserRegistration } from './browser.mjs';
import { CreationTracker, creationOutcome } from './creation-outcome.mjs';
import { NetworkDiagnostics } from './network-diagnostics.mjs';

async function fixture(t) {
  const directory = await mkdtemp(join(tmpdir(), 'adobe-creation-'));
  let store = new PrivateRunStore(directory, 'attempt');
  await store.init({email:'fixture@example.test'});
  t.after(async () => { await store.close(); await rm(directory, {recursive:true,force:true}); });
  const context = new EventEmitter();
  const page = new EventEmitter();page.mainFrame=() => frame;
  const frame = {page:() => page};
  context.pages = () => [page];
  context.close = async () => {};
  const tracker = new CreationTracker(store, context);
  const browser = new BrowserRegistration(store);
  browser.context = context; browser.creationTracker = tracker;
  return {store,context,page,frame,tracker,browser,
    begin:async () => {await store.markSubmissionStarted(); await tracker.begin(page);},
    settle:async () => {await Promise.all([...tracker.pending]);await tracker.writes;},
    outcome:async () => creationOutcome(await store.load()),
    restart:async () => {await store.close();store=new PrivateRunStore(directory,'attempt');await store.init({}, {requireExisting:true});return creationOutcome(await store.load());},
  };
}

function request(page, {status=400,body={errorCode:'captcha_required'},failure='',readBody,headers={'content-type':'application/json'},url='https://auth.services.adobe.com/signin/v2/accounts',method='POST'}={}) {
  return {url:()=>url,method:()=>method,frame:()=>({page:()=>page}),failure:()=>failure?{errorText:failure}:null,
    timing:()=>({startTime:Date.now(),responseEnd:10}),response:async()=>({status:()=>status,headers:()=>headers,
      body:readBody||(async()=>Buffer.from(JSON.stringify(body)))})};
}

test('only a journal proving no Create yields not_submitted; missing or legacy submitted evidence stays unknown',async t=>{
  const f=await fixture(t);
  assert.deepEqual(await f.outcome(),{creation_outcome:'not_submitted'});
  assert.deepEqual(creationOutcome(null),{creation_outcome:'unknown'});
  await f.store.markSubmissionStarted();
  assert.deepEqual(await f.outcome(),{creation_outcome:'unknown'});
  assert.deepEqual(await f.restart(),{creation_outcome:'unknown'});
});

test('an incomplete or differently bound persisted ledger cannot authorize registration',()=>{
  const base={submission:{started:true,startedAt:'2026-11-12T00:00:00.000Z'},creationEvidence:{version:1,
    marker:'2026-11-12T00:00:00.000Z',requests:1,finished:1,rejected:1,unknown:0,accepted:false,complete:true,sealed:true}};
  for(const patch of [{version:2},{marker:'different'},{finished:2},{accepted:undefined},{unknown:1},{sealed:false},{complete:false}]){
    assert.equal(creationOutcome({...base,creationEvidence:{...base.creationEvidence,...patch}}).creation_outcome,'unknown');
  }
});

test('explicit rejection is unavailable until confirmed close, and survives restart without clearing the original marker',async t=>{
  const f=await fixture(t);await f.begin();
  const original=(await f.store.load()).submission.startedAt;
  const r=request(f.page);f.context.emit('request',r);f.context.emit('requestfinished',r);await f.settle();
  f.page.emit('framenavigated',f.frame);
  assert.equal((await f.outcome()).creation_outcome,'unknown');
  await f.browser.close();
  assert.deepEqual(await f.outcome(),{creation_outcome:'rejected',creation_error_code:'captcha_required',creation_http_status:400});
  assert.equal((await f.store.load()).submission.startedAt,original);
  assert.equal((await f.restart()).creation_outcome,'rejected');
});

test('in-flight, lost or failed response evidence never certifies rejection',async t=>{
  for(const variant of ['pending','failed','body_throw','body_timeout','invalid_json','http_500','http_403','non_json'])await t.test(variant,async sub=>{
    const f=await fixture(sub);await f.begin();
    const options=variant==='failed'?{failure:'net::ERR_ABORTED'}:variant==='body_throw'?{readBody:async()=>{throw new Error('PRIVATE')}}:
      variant==='body_timeout'?{readBody:()=>new Promise(()=>{})}:variant==='invalid_json'?{readBody:async()=>Buffer.from('PRIVATE')}:
      variant==='http_500'?{status:500}:variant==='http_403'?{status:403}:variant==='non_json'?{headers:{'content-type':'text/html'}}:{};
    const r=request(f.page,options);f.context.emit('request',r);
    if(variant!=='pending')f.context.emit(variant==='failed'?'requestfailed':'requestfinished',r);
    await f.browser.close();
    assert.deepEqual(await f.outcome(),{creation_outcome:'unknown'});
    assert.doesNotMatch(JSON.stringify((await f.store.load()).creationEvidence),/PRIVATE|ERR_ABORTED/);
  });
});

test('all requests in an attempt must finish with explicit rejection; successful creation is monotonic',async t=>{
  for(const second of ['rejected','pending','unknown','accepted'])await t.test(second,async sub=>{
    const f=await fixture(sub);await f.begin();
    for(const [index,kind] of ['rejected',second].entries()){
      const r=request(f.page,{status:kind==='accepted'?201:kind==='unknown'?500:400});
      f.context.emit('request',r);
      if(kind!=='pending')f.context.emit('requestfinished',r);
      if(index===0)await f.settle();
    }
    await f.browser.close();
    assert.equal((await f.outcome()).creation_outcome,second==='accepted'?'accepted':second==='rejected'?'rejected':'unknown');
  });
});

test('foreign and untracked requests cannot claim this attempt was created or rejected',async t=>{
  for(const mode of ['foreign','untracked'])for(const status of [201,400])await t.test(`${mode}/${status}`,async sub=>{
    const f=await fixture(sub);await f.begin();
    const r=request(mode==='foreign'?{}:f.page,{status});
    if(mode==='foreign')f.context.emit('request',r);
    f.context.emit('requestfinished',r);await f.browser.close();
    assert.equal((await f.outcome()).creation_outcome,'unknown');
  });
});

test('unrelated verification traffic, navigation and display truncation cannot replace creation evidence',async t=>{
  const f=await fixture(t),diagnostics=new NetworkDiagnostics();diagnostics.attach(f.context);await f.begin();
  const r=request(f.page);f.context.emit('request',r);f.context.emit('requestfinished',r);await f.settle();
  for(let i=0;i<60;i++){
    const noise=request(f.page,{url:'https://arks-client.adobe.com/fc/a',method:'GET',status:200});
    f.context.emit('request',noise);f.context.emit('requestfinished',noise);
  }
  await f.browser.close();
  const ledger=(await f.store.load()).creationEvidence;
  assert.equal(ledger.requests,1);assert.equal(ledger.rejected,1);
  assert.equal((await f.outcome()).creation_outcome,'rejected');
});

test('closing observes a final late request and refuses rejection if browser closure is unconfirmed',async t=>{
  for(const failure of [false,true])await t.test(String(failure),async sub=>{
    const f=await fixture(sub);await f.begin();
    const r=request(f.page);f.context.emit('request',r);f.context.emit('requestfinished',r);await f.settle();
    f.context.close=async()=>{if(failure)throw new Error('close failed');f.context.emit('request',request(f.page));};
    if(failure)await assert.rejects(f.browser.close());else await f.browser.close();
    assert.equal((await f.outcome()).creation_outcome,'unknown');
    assert.equal((await f.restart()).creation_outcome,'unknown');
  });
});

test('email verification progress or captured account overrides rejection and stays accepted after restart',async t=>{
  const f=await fixture(t);await f.begin();
  const r=request(f.page);f.context.emit('request',r);f.context.emit('requestfinished',r);await f.settle();
  await f.tracker.accepted();await f.browser.close();
  assert.equal((await f.restart()).creation_outcome,'accepted');
  assert.equal(creationOutcome({email:'fixture@example.test',submission:{started:true},result:{account:{email:'fixture@example.test',account_id:'id',token:'PRIVATE'}}}).creation_outcome,'accepted');
});

test('drain persistence failure never seals rejected evidence',async t=>{
  const f=await fixture(t);await f.begin();
  const r=request(f.page);f.context.emit('request',r);f.context.emit('requestfinished',r);await f.settle();
  const save=f.store.save.bind(f.store);f.store.save=async()=>{throw new Error('disk unavailable')};
  await assert.rejects(f.browser.close(),error=>error.code==='creation_evidence_write_failed');
  f.store.save=save;
  assert.equal((await f.outcome()).creation_outcome,'unknown');
});
