import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {PrivateRunStore,RegistrationError} from './core.mjs';
import {validateMediaState,saveMediaCheckpoint,prepareRegisteredMedia} from './registration-media.mjs';

const email='fixture@example.test',stamp='2026-11-13T09:00:00.000Z';
const fresh=()=>({email,account_id:'fixture-account',token:'PRIVATE-TOKEN',emailVerified:true,user_agent:'fixture-agent'});
const checkpoint=()=>({version:1,email,account_id:'fixture-account',emailVerified:true,createdAt:stamp});
function stateFixture({legacy=false}={}) {
 return {schemaVersion:1,runId:'fixture-profile',email,config:{password:'PRIVATE-PASSWORD'},stage:'media_pending',code:null,
  submission:{started:true,startedAt:stamp,outcome:'pending'},
  creationEvidence:{version:1,marker:stamp,requests:1,finished:1,rejected:0,unknown:0,accepted:true,complete:true,sealed:true},
  ...(!legacy?{mediaCheckpoint:checkpoint()}:{})};
}
function memoryStore(state=stateFixture()) {
 let current=structuredClone(state);const writes=[];
 return {runId:state.runId,writes,load:async()=>structuredClone(current),save:async patch=>{
  writes.push(structuredClone(patch));current={...current,...structuredClone(patch)};
  if(current.stage==='completed')current.submission.outcome='confirmed';return structuredClone(current);
 }};
}
const readyMedia=()=>({cookie:'other=fixture; sherlockToken=PRIVATE-ARP',media_session_arp_mode:'present',media_session_arp_source:'spl_runtime',runtime_proof:{private:'proof'}});
function browserFixture({oldPages=2}={}) {
 const events=[];let pages=[];
 const makePage=label=>({label,url:()=> 'https://firefly.adobe.com/',
  close:async options=>{assert.deepEqual(options,{runBeforeUnload:false});events.push(`close:${label}`);pages=pages.filter(value=>value!==pageByLabel[label]);},
  goto:async(url,options)=>{assert.equal(url,'https://firefly.adobe.com/');assert.equal(options.waitUntil,'domcontentloaded');assert.ok(options.timeout<=30000);events.push('home');},
  locator:()=>assert.fail('Media preparation must not fill a login or signup form'),
  getByRole:()=>assert.fail('Media preparation must not send OTP, log in or create an account')});
 const pageByLabel={};
 for(let i=0;i<oldPages;i++){const name=`old${i}`;pageByLabel[name]=makePage(name);pages.push(pageByLabel[name]);}
 const context={pages:()=>[...pages],newPage:async()=>{events.push('new');pageByLabel.fresh=makePage('fresh');pages.push(pageByLabel.fresh);return pageByLabel.fresh;},
  cookies:async url=>{assert.equal(url,'https://adobeid-na1.services.adobe.com/ims/check/v6/token');events.push('cookies');return[{name:'ims',value:'PRIVATE-COOKIE'}]},
  close:async()=>assert.fail('RPC owns context closure')};
 return {context,events,pages:pageByLabel,options:{email,waitIdentity:async()=>{events.push('identity');return fresh()},
  ensureMedia:async(page,actualContext,options)=>{assert.equal(actualContext,context);assert.equal(options.identity.token,'PRIVATE-TOKEN');assert.ok(options.signal);events.push('media');return readyMedia()},
  verifyMedia:async(page,media,identity)=>{assert.equal(media.runtime_proof.private,'proof');assert.equal(identity.email,email);events.push('verify')}}};
}

test('only the submitted, accepted journal and matching requested email can enter media preparation',()=>{
 const state=stateFixture();assert.equal(validateMediaState(state,{email}),state);
 assert.equal(validateMediaState(state,{email:email.toUpperCase()}),state);
 for(const change of [s=>{s.submission.started=false},s=>{s.creationEvidence.accepted=false},s=>{delete s.creationEvidence},s=>{s.creationEvidence.marker='other'}]){
  const wrong=structuredClone(state);change(wrong);assert.throws(()=>validateMediaState(wrong,{email}),RegistrationError);
 }
 for(const other of ['other@example.test',' '+email,email+' ',undefined])assert.throws(()=>validateMediaState(state,{email:other}),{code:'run_email_mismatch'});
 const legacy=stateFixture({legacy:true});assert.throws(()=>validateMediaState(legacy,{email}),{code:'media_checkpoint_missing'});
 assert.equal(validateMediaState(legacy,{email,legacyMedia:true}),legacy);
 legacy.creationEvidence.accepted=false;assert.throws(()=>validateMediaState(legacy,{email,legacyMedia:true}),{code:'media_creation_not_accepted'});
});

test('checkpoint schema rejects extra credentials, corrupt identity, unverified email and invalid timestamps',()=>{
 for(const change of [
  c=>{c.token='PRIVATE'},c=>{c.cookie='PRIVATE'},c=>{c.password='PRIVATE'},c=>{c.version=2},c=>{delete c.emailVerified},
  c=>{c.email='other@example.test'},c=>{c.account_id=''},c=>{c.emailVerified=false},c=>{c.createdAt='2026-02-30T00:00:00Z'},
  c=>{c.createdAt='today'},c=>{c.extra=true},
 ]){
  const state=stateFixture();change(state.mediaCheckpoint);
  assert.throws(()=>validateMediaState(state,{email,legacyMedia:true}),{code:'media_checkpoint_invalid'});
 }
});

test('checkpoint save retains only confirmed identity and never publishes a partial account',async()=>{
 const store=memoryStore(stateFixture({legacy:true}));
 const saved=await saveMediaCheckpoint(store,{...fresh(),email:email.toUpperCase(),cookie:'PRIVATE-COOKIE',refresh_cookie:'PRIVATE-REFRESH'},{now:()=>Date.parse(stamp)});
 assert.deepEqual(saved.mediaCheckpoint,checkpoint());assert.equal(saved.stage,'media_pending');assert.equal(saved.code,null);assert.equal(saved.result,undefined);
 assert.doesNotMatch(JSON.stringify(store.writes),/PRIVATE|token|cookie|password/);
 const second=await saveMediaCheckpoint(store,fresh(),{now:()=>Date.parse(stamp)+5000});assert.equal(second.mediaCheckpoint.createdAt,stamp);
 await assert.rejects(saveMediaCheckpoint(store,{...fresh(),account_id:'other'}),{code:'media_identity_mismatch'});
 for(const emailVerified of [false,null,undefined])await assert.rejects(saveMediaCheckpoint(store,{...fresh(),emailVerified}),{code:'media_email_unverified'});
});

test('media completion uses one fresh page, closes restored pages and exports only after final verification',async()=>{
 const store=memoryStore(),f=browserFixture();
 const result=await prepareRegisteredMedia(f.context,store,f.options);
 assert.equal(result.stage,'completed');assert.equal(result.account.profile_id,store.runId);assert.equal(result.account.password,'PRIVATE-PASSWORD');
 assert.deepEqual(f.events,['new','close:old0','close:old1','home','identity','media','identity','verify','cookies','close:fresh']);
 const saved=await store.load();assert.equal(saved.stage,'completed');assert.equal(saved.result.exportComplete,true);assert.equal(saved.result.emailVerified,true);assert.equal(saved.result.fireflySessionVerified,true);assert.equal(saved.result.generationVerified,false);
 assert.equal(saved.result.account.token,'PRIVATE-TOKEN');assert.equal(saved.result.account.refresh_cookie,'ims=PRIVATE-COOKIE');
 assert.equal(saved.result.account.runtime_proof,undefined);assert.equal(saved.result.account.profile_id,undefined);assert.equal(saved.result.account.password,undefined);
 assert.equal(f.context.pages().length,0);
 assert.equal(store.writes.filter(write=>write.result?.account).length,1);
});

test('accepted legacy media recovery first saves the identity checkpoint before collecting any media',async()=>{
 const store=memoryStore(stateFixture({legacy:true})),f=browserFixture();
 const original=f.options.ensureMedia;f.options.ensureMedia=async(...args)=>{assert.deepEqual((await store.load()).mediaCheckpoint.account_id,'fixture-account');assert.equal((await store.load()).result,undefined);return original(...args)};
 assert.equal((await prepareRegisteredMedia(f.context,store,{...f.options,legacyMedia:true})).stage,'completed');
 assert.equal(store.writes[0].stage,'media_pending');assert.equal(store.writes[0].mediaCheckpoint.emailVerified,true);
});

test('an invalid initial journal fails before touching the browser or changing state',async()=>{
 for(const change of [s=>{delete s.mediaCheckpoint},s=>{s.creationEvidence.accepted=false},s=>{s.mediaCheckpoint.token='PRIVATE'}]){
  const state=stateFixture();change(state);const store=memoryStore(state);
  await assert.rejects(prepareRegisteredMedia({pages:()=>assert.fail('no context use')},store,{email}),RegistrationError);assert.equal(store.writes.length,0);
 }
});

test('missing login, another identity or an unverified mailbox never starts login or media preparation',async()=>{
 for(const [identity,code] of [[null,'media_login_required'],[{...fresh(),account_id:'other'},'media_identity_mismatch'],[{...fresh(),email:'other@example.test'},'media_identity_mismatch'],[{...fresh(),emailVerified:null},'media_email_unverified']]){
  const store=memoryStore(),f=browserFixture();
  const result=await prepareRegisteredMedia(f.context,store,{...f.options,waitIdentity:async()=>identity});
  assert.equal(result.stage,'media_pending');assert.equal(result.code,code);assert.equal(f.events.includes('media'),false);assert.equal(f.context.pages().length,0);
  const saved=await store.load();assert.deepEqual(saved.mediaCheckpoint,checkpoint());assert.equal(saved.result,undefined);assert.equal(saved.code,code);
 }
});

test('media failure persists only fixed diagnostics while preserving the identity checkpoint',async()=>{
 const store=memoryStore(),f=browserFixture(),error=new RegistrationError('media_session_incomplete');
 error.diagnostics={media_runtime:{state:'pending',readiness_reason:'instance_missing',value_kind:'null',vendors_started:0,vendors_complete:0,token:'PRIVATE'},
  media_page:{route:'editor',matching_host_count:1,authentication_count:1,arp_observer_count:1,scan_complete:true},
  media_timing:{emissions:1,first_source_ms:2,raw:'PRIVATE'},cookie:'PRIVATE',url:'PRIVATE',runtime_proof:{private:'PRIVATE'}};
 const result=await prepareRegisteredMedia(f.context,store,{...f.options,ensureMedia:async()=>{throw error}});
 assert.equal(result.code,'media_session_incomplete');assert.doesNotMatch(JSON.stringify(result),/PRIVATE|token|cookie|runtime_proof|raw/);
 const saved=await store.load();assert.equal(saved.lastObservation.media_runtime.readiness_reason,'instance_missing');assert.equal(saved.lastObservation.submission_started,true);
 assert.doesNotMatch(JSON.stringify(saved.lastObservation),/PRIVATE|token|cookie|runtime_proof|raw/);assert.equal(saved.result,undefined);assert.deepEqual(saved.mediaCheckpoint,checkpoint());
 assert.equal(f.events.includes('verify'),false);assert.equal(f.context.pages().length,0);
});

test('final identity changes and failed proof validation cannot publish the collected credentials',async()=>{
 for(const mode of ['identity','proof','omitted']){
  const store=memoryStore(),f=browserFixture();let reads=0;
  const result=await prepareRegisteredMedia(f.context,store,{...f.options,
   waitIdentity:async()=>({...fresh(),account_id:mode==='identity'&&++reads>1?'other':'fixture-account'}),
   ensureMedia:async()=>mode==='omitted'?{cookie:'',media_session_arp_mode:'omitted',media_session_arp_source:'spl_runtime'}:readyMedia(),
   verifyMedia:async()=>{if(mode==='proof')throw new RegistrationError('media_session_incomplete')},
  });
  assert.equal(result.stage,'media_pending');assert.equal((await store.load()).result,undefined);assert.equal(f.context.pages().length,0);
  assert.equal(f.events.includes('cookies'),false);
 }
});

test('completed exports are returned offline without changing bytes or opening the context',async()=>{
 const store=memoryStore(),f=browserFixture();const first=await prepareRegisteredMedia(f.context,store,f.options);
 const before=JSON.stringify(await store.load()),writes=store.writes.length;
 const second=await prepareRegisteredMedia({pages:()=>assert.fail('offline completion')},store,{email});
 assert.deepEqual(second,first);assert.equal(JSON.stringify(await store.load()),before);assert.equal(store.writes.length,writes);
 const corrupt=await store.load();corrupt.result.account.cookie='';
 await assert.rejects(prepareRegisteredMedia({},memoryStore(corrupt),{email}),{code:'media_result_invalid'});
});

test('unknown exceptions use a fixed failure code and do not leak upstream details',async()=>{
 const store=memoryStore(),f=browserFixture();
 const result=await prepareRegisteredMedia(f.context,store,{...f.options,ensureMedia:async()=>{throw new Error('PRIVATE https://signed.example/token')}});
 assert.deepEqual(result,{stage:'media_pending',code:'media_prepare_failed',diagnostics:{}});
 assert.doesNotMatch(JSON.stringify((await store.load()).lastObservation),/PRIVATE|signed/);
});

test('cancellation during media collection retains pending identity and closes the page',async()=>{
 const store=memoryStore(),f=browserFixture(),abort=new AbortController();
 const result=await prepareRegisteredMedia(f.context,store,{...f.options,signal:abort.signal,ensureMedia:async()=>{abort.abort();return readyMedia()}});
 assert.equal(result.code,'media_session_cancelled');assert.equal((await store.load()).result,undefined);assert.equal(f.context.pages().length,0);
 const cancelled=new AbortController();cancelled.abort();const untouched=memoryStore();
 await assert.rejects(prepareRegisteredMedia({pages:()=>assert.fail('cancelled')},untouched,{email,signal:cancelled.signal}),{code:'media_session_cancelled'});assert.equal(untouched.writes.length,0);
});

test('stalled navigation and media work are bounded by the single action deadline',async()=>{
 for(const stage of ['home','media']){
  const store=memoryStore(),f=browserFixture(),started=Date.now();
  if(stage==='home'){
   const create=f.context.newPage;f.context.newPage=async()=>{const page=await create();page.goto=async()=>new Promise(()=>{});return page};
  }
  const result=await prepareRegisteredMedia(f.context,store,{...f.options,timeoutMs:30,cleanupTimeoutMs:3,
   ...(stage==='media'?{ensureMedia:async()=>new Promise(()=>{})}:{})});
  assert.equal(result.code,'media_prepare_timeout');assert.ok(Date.now()-started<250);assert.equal(f.context.pages().length,0);assert.equal((await store.load()).result,undefined);
 }
});

test('restored pages refusing closure stop before navigation and the new page is still cleaned',async()=>{
 const store=memoryStore(),f=browserFixture();f.pages.old0.close=async()=>{throw new Error('PRIVATE')};
 const result=await prepareRegisteredMedia(f.context,store,f.options);
 assert.equal(result.code,'media_page_cleanup_failed');assert.equal(f.events.includes('home'),false);assert.equal(f.context.pages().some(page=>page.label==='fresh'),false);
});

test('real private journals retain a checkpoint across reopen and complete media exactly once',async t=>{
 const root=await mkdtemp(join(tmpdir(),'adobe-media-checkpoint-')),store=new PrivateRunStore(root,'fixture-profile');
 t.after(async()=>{await store.close();await rm(root,{recursive:true,force:true})});
 await store.init({email,config:{password:'PRIVATE-PASSWORD'}});await store.markSubmissionStarted();
 const started=(await store.load()).submission.startedAt;
 await store.save({creationEvidence:{...stateFixture().creationEvidence,marker:started}});
 await saveMediaCheckpoint(store,fresh());await store.close();await store.init({},{requireExisting:true});
 const reopened=await store.load();assert.equal(reopened.stage,'media_pending');assert.equal(reopened.mediaCheckpoint.emailVerified,true);assert.equal(reopened.result,undefined);
 const f=browserFixture();assert.equal((await prepareRegisteredMedia(f.context,store,f.options)).stage,'completed');
 const completed=await store.load();assert.equal(completed.submission.outcome,'confirmed');assert.equal(completed.result.exportComplete,true);
});

test('present malformed checkpoints never use the legacy fallback',()=>{
 for(const value of [null,false,true,1,'checkpoint',[],{}, {...checkpoint(),account_id:'   '}]){
  const state=stateFixture();state.mediaCheckpoint=value;
  assert.throws(()=>validateMediaState(state,{email,legacyMedia:true}),{code:'media_checkpoint_invalid'});
 }
});

test('a checkpoint survives legacy media failure and unverified legacy identity cannot create one',async()=>{
 for(const emailVerified of [true,false]){
  const store=memoryStore(stateFixture({legacy:true})),f=browserFixture();
  const result=await prepareRegisteredMedia(f.context,store,{...f.options,legacyMedia:true,
   waitIdentity:async()=>({...fresh(),emailVerified}),ensureMedia:async()=>{throw new RegistrationError('media_session_incomplete')}});
  const saved=await store.load();assert.equal(result.stage,'media_pending');assert.equal(saved.result,undefined);
  assert.equal(saved.mediaCheckpoint?.emailVerified,emailVerified?true:undefined);
 }
});

test('a late newPage after the action timeout is closed and never used for login or export',async()=>{
 const store=memoryStore(),f=browserFixture({oldPages:0});let release;
 const create=f.context.newPage;f.context.newPage=()=>new Promise(resolve=>{release=async()=>resolve(await create())});
 const result=await prepareRegisteredMedia(f.context,store,{...f.options,timeoutMs:20,cleanupTimeoutMs:2});
 assert.equal(result.code,'media_prepare_timeout');assert.equal((await store.load()).result,undefined);
 await release();await new Promise(resolve=>setImmediate(resolve));
 assert.deepEqual(f.events,['new','close:fresh']);assert.equal(f.context.pages().length,0);
});

test('failed final page cleanup prevents a successful account result from being published',async()=>{
 const store=memoryStore(),f=browserFixture();
 const create=f.context.newPage;f.context.newPage=async()=>{const page=await create();page.close=async()=>{throw new Error('PRIVATE-close')};return page};
 const result=await prepareRegisteredMedia(f.context,store,f.options);
 assert.equal(result.code,'media_page_cleanup_failed');assert.equal((await store.load()).result,undefined);assert.equal(store.writes.some(write=>write.result),false);
});

test('identity polling receives cancellation-aware waits and stops when the action is cancelled',async()=>{
 const store=memoryStore(),f=browserFixture(),abort=new AbortController();let waiting=false,exited=false;
 const pending=prepareRegisteredMedia(f.context,store,{...f.options,signal:abort.signal,
  waitIdentity:async(page,options)=>{waiting=true;try{await options.pause(10000)}finally{exited=true}}});
 while(!waiting)await new Promise(resolve=>setImmediate(resolve));
 abort.abort();const result=await pending;
 assert.equal(result.code,'media_session_cancelled');assert.equal(exited,true);assert.equal(f.context.pages().length,0);assert.equal((await store.load()).result,undefined);
});
