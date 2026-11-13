import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import {isUsableSherlock, mediaCookieHeader, isCompleteMediaSession, runtimeARPInPage, ensureMediaSession, verifyMediaSession, bootstrapRecoveryAllowedInPage} from './media-session.mjs';

function observable(initial) {
  let value=initial, subscriptions=0, unsubscriptions=0;
  const listeners=new Set();
  return {observe(listener){subscriptions++;listeners.add(listener);if(value!==undefined)listener(value);return()=>{if(listeners.delete(listener))unsubscriptions++;}},
    emit(next){value=next;for(const listener of [...listeners])listener(value)},
    get active(){return listeners.size},get subscribed(){return subscriptions},get unsubscribed(){return unsubscriptions}};
}
function performanceFixture({instance='fixture-sdk',start=1,complete=true,vendors=['arkose','bfp','forter']}={}) {
  const entries=[];
  const mark=(name,startTime)=>entries.push({name,entryType:'mark',startTime,duration:0});
  const measure=(name,startTime,duration=1)=>entries.push({name,entryType:'measure',startTime,duration});
  mark(`sherlock:${instance}:init:start`,start);
  for(const [index,vendor] of vendors.entries()){
    const name=`sherlock:${instance}:${vendor}:start`,time=start+index+1;
    mark(name,time);if(complete)measure(`${name}:duration`,time);
  }
  return {entries,mark,measure,getEntriesByType:type=>entries.filter(entry=>entry.entryType===type)};
}
function pageHarness(source,{url='https://firefly.adobe.com/generate/image',shadow=true,serviceName='spl-commerce-dialog-service',performance=performanceFixture()}={}) {
  const listeners=new Map(),navigations=[];
  let currentSource=source;
  const authentication={get arpSessionToken(){return currentSource?.observe}};
  const host={service:{serviceName,authentication}};
  const editorRoot={querySelectorAll(selector){return selector==='*'?[host]:selector==='firefly-commerce-dialogs, firefly-commerce-paywall-dialog'&&currentSource?[host]:[]}};
  const globals={Event,setTimeout,clearTimeout,setInterval,clearInterval,queueMicrotask,performance,
    location:{origin:new URL(url).origin,pathname:new URL(url).pathname},
    document:{querySelectorAll(selector){
      if(globals.location.pathname!=='/generate/image')return[];
      if(!shadow)return editorRoot.querySelectorAll(selector);
      return selector==='*'?[{shadowRoot:editorRoot}]:[];
    }},
    addEventListener(name,listener){if(!listeners.has(name))listeners.set(name,new Set());listeners.get(name).add(listener)},
    removeEventListener(name,listener){listeners.get(name)?.delete(listener)},
    dispatchEvent(event){for(const listener of [...(listeners.get(event.type)||[])])listener(event);return true}};
  const context=vm.createContext(globals);
  const page={url:()=>url,async goto(next,options){assert.ok(options.timeout>0);navigations.push(next);url=next;globals.location.origin=new URL(url).origin;globals.location.pathname=new URL(url).pathname},
    async evaluate(fn,arg){globals.argument=arg;return vm.runInContext(`(${fn.toString()})(argument)`,context)}};
  return {page,globals,navigations,authentication,setSource(next){currentSource=next},get listeners(){return [...listeners.values()].reduce((n,items)=>n+items.size,0)}};
}
const fixtureContext=items=>({cookies:async url=>{assert.equal(url,'https://firefly.adobe.com/');return items}});

test('opaque runtime values require no undocumented fields and missing input never reuses old cookies',()=>{
 const opaque=Buffer.from(JSON.stringify({sid:'fixture',ftr:'fixture'})).toString('base64');
 assert.equal(isUsableSherlock(opaque),true);assert.equal(isUsableSherlock('official-opaque-value'),true);
 assert.equal(mediaCookieHeader([{name:'sherlockToken',value:opaque}]),'');
 for(const value of ['',undefined,'bad\r\nheader','session;other=field','comma,value','white space'])assert.equal(isUsableSherlock(value),false);
});
test('present and omitted runtime modes both remove every historic same-name cookie',()=>{
 const items=[{name:'sherlockToken',value:'old-root'},{name:'other',value:'kept'},{name:'sherlockToken',value:'old-domain'}];
 assert.equal(mediaCookieHeader(items,'runtime-value'),'other=kept; sherlockToken=runtime-value');
 assert.equal(mediaCookieHeader(items,null),'other=kept');assert.equal(items[0].value,'old-root');
});
test('only an explicit SPL contract with consistent cookie contents is complete',()=>{
 assert.equal(isCompleteMediaSession({cookie:'',media_session_arp_mode:'omitted',media_session_arp_source:'spl_runtime'}),true);
 assert.equal(isCompleteMediaSession({cookie:'sherlockToken=ready',media_session_arp_mode:'present',media_session_arp_source:'spl_runtime'}),true);
 for(const media of ['sherlockToken=legacy',{cookie:''},{cookie:'sherlockToken=old',media_session_arp_mode:'omitted',media_session_arp_source:'spl_runtime'},
  {cookie:'',media_session_arp_mode:'present',media_session_arp_source:'spl_runtime'},{cookie:'',media_session_arp_mode:'omitted',media_session_arp_source:'home'}])assert.equal(isCompleteMediaSession(media),false);
});
test('SPL authentication is found inside shadow roots and its synchronous value is unsubscribed',async()=>{
 const source=observable('runtime-ready'),h=pageHarness(source);
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:700,cancelEvent:'cancel-test'});
 assert.equal(result.token,'runtime-ready');assert.equal(result.source,'spl_runtime');assert.equal(source.active,0);assert.equal(source.subscribed,1);assert.equal(source.unsubscribed,1);assert.equal(h.listeners,0);
 assert.equal('media_timing' in result,false);assert.equal('media_page' in result,false);
});
test('initial null and a non-emitting SPL source remain pending until the observation deadline',async()=>{
 for(const [value,kind] of [[null,'null'],[undefined,'unobserved']]){
  const source=observable(value),h=pageHarness(source);
  const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
  assert.equal(source.active,1);
  const result=await pending;
  assert.equal(result.state,'pending');assert.equal(result.source,'spl_runtime');assert.equal('token' in result,false);
  assert.equal(result.value_kind,kind);assert.equal(source.active,0);assert.equal(h.listeners,0);
 }
});
test('missing runtime tokens still expose vendor initialization progress without exporting proof or accepting a session',async()=>{
 for(const [value,kind] of [[null,'null'],[undefined,'unobserved'],['','empty_string']]){
  for(const [performance,reason,started,complete] of [
   [{getEntriesByType:()=>[]},'instance_missing',0,0],
   [performanceFixture({vendors:['arkose','bfp']}),'vendors_pending',2,2],
   [performanceFixture({complete:false}),'vendors_pending',3,0],
   [performanceFixture(),'token_pending',3,3],
  ]){
   const source=observable(value),h=pageHarness(source,{performance});
   const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
   assert.equal(result.state,'pending');assert.equal(result.value_kind,kind);assert.equal(result.readiness_reason,reason);
   assert.equal(result.vendors_started,started);assert.equal(result.vendors_complete,complete);
   assert.equal(result.token,undefined);assert.equal(result.runtime_proof,undefined);assert.equal(result.proof,undefined);
   assert.equal(result.media_timing.first_nonempty_ms,undefined);assert.equal(result.media_timing.last_candidate_ms,undefined);
   assert.equal(JSON.stringify(result).includes('fixture-sdk'),false);assert.equal(source.active,0);assert.equal(h.listeners,0);
   await assert.rejects(ensureMediaSession(h.page,{cookies:async()=>assert.fail('missing token must not export cookies')},{timeoutMs:15}),error=>{
    assert.equal(error.code,'media_session_incomplete');assert.equal(error.diagnostics.media_runtime.readiness_reason,reason);return true;
   });
  }
 }
});
test('a missing source is not confused with the official null default and times out cleanly',async()=>{
 const h=pageHarness(null);
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
 assert.equal(result.state,'unavailable');assert.equal(result.readiness_reason,'authentication_missing');assert.equal(result.source,undefined);assert.equal(h.listeners,0);
 assert.deepEqual({...result.media_page},{route:'editor',matching_host_count:0,authentication_count:0,arp_observer_count:0,scan_complete:true});
});
test('an authentication without an ARP observer is distinguished from a missing authentication',async()=>{
 const h=pageHarness({});
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
 assert.equal(result.state,'unavailable');assert.equal(result.readiness_reason,'arp_observer_missing');assert.equal(h.listeners,0);
 assert.deepEqual({...result.media_page},{route:'editor',matching_host_count:1,authentication_count:1,arp_observer_count:0,scan_complete:true});
});
test('failure diagnostics count distinct hosts without selecting a later usable authentication',async()=>{
 const source=observable('later-private-token'),h=pageHarness({});
 const first={service:{serviceName:'spl-commerce-dialog-service',authentication:{}}};
 const second={service:{serviceName:'spl-commerce-dialog-service',authentication:{arpSessionToken:source.observe}}};
 h.globals.document.querySelectorAll=selector=>selector==='*'?[]:[first,second,second];
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
 assert.equal(result.readiness_reason,'arp_observer_missing');assert.equal(source.subscribed,0);assert.equal(h.listeners,0);
 assert.deepEqual({...result.media_page},{route:'editor',matching_host_count:3,authentication_count:2,arp_observer_count:1,scan_complete:true});
 assert.equal(JSON.stringify(result).includes('later-private-token'),false);
});
test('failure page diagnostics cap candidate counts and label an incomplete scan',async()=>{
 const h=pageHarness({}),hosts=Array.from({length:40},()=>({service:{serviceName:'spl-commerce-dialog-service',authentication:{}}}));
 h.globals.document.querySelectorAll=selector=>selector==='*'?[]:hosts;
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
 assert.equal(result.readiness_reason,'arp_observer_missing');assert.equal(result.media_page.scan_complete,false);
 assert.equal(result.media_page.matching_host_count,32);assert.equal(result.media_page.authentication_count,32);assert.equal(result.media_page.arp_observer_count,0);
});
test('editor route changes, page hiding and explicit observation cancellation have separate reasons',async()=>{
 const other=pageHarness(observable(null),{url:'https://example.test/generate/image?private=value'});
 const changed=await other.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
 assert.equal(changed.readiness_reason,'editor_route_changed');assert.equal(changed.media_page.route,'other');assert.equal(other.listeners,0);
 assert.equal(JSON.stringify(changed).includes('example.test'),false);assert.equal(JSON.stringify(changed).includes('private'),false);
 for(const [event,reason] of [['pagehide','page_hidden'],['cancel-test','observation_cancelled']]){
  const source=observable(null),h=pageHarness(source);
  const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:500,cancelEvent:'cancel-test'});
  assert.equal(source.active,1);h.globals.dispatchEvent(new Event(event));
  const result=await pending;
  assert.equal(result.state,'unavailable');assert.equal(result.readiness_reason,reason);assert.equal(source.active,0);assert.equal(h.listeners,0);
 }
});
test('in-page discovery exceptions are fixed diagnostics without runtime values or leaked listeners',async()=>{
 const h=pageHarness({});
 h.globals.document.querySelectorAll=()=>{throw new Error('private-runtime-value')};
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
 assert.equal(result.readiness_reason,'evaluation_failed');assert.equal(result.media_page.scan_complete,false);assert.equal(h.listeners,0);
 assert.equal(JSON.stringify(result).includes('private-runtime-value'),false);
});
test('failed observations record when a source, nonempty value and complete vendors first appeared',async()=>{
 const perf=performanceFixture({complete:false}),source=observable(null),h=pageHarness(source,{performance:perf});
 let time=0;h.globals.Date={now:()=>time};
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:30,cancelEvent:'cancel-test'});
 await Promise.resolve();time=10;source.emit('private-opaque-value');await Promise.resolve();
 time=20;
 for(const [vendor,start] of [['arkose',2],['bfp',3],['forter',4]])perf.measure(`sherlock:fixture-sdk:${vendor}:start:duration`,start);
 source.emit('private-opaque-value');await Promise.resolve();
 const result=await pending;
 assert.equal(result.readiness_reason,'token_settling');
 assert.deepEqual({...result.media_timing},{candidate_changes:0,source_changes:0,authentication_changes:0,emissions:3,value_changes:1,proof_changes:0,
  first_source_ms:0,first_nonempty_ms:10,vendors_ready_ms:20,last_candidate_ms:20});
 assert.equal(JSON.stringify(result).includes('private-opaque-value'),false);assert.equal(JSON.stringify(result).includes('fixture-sdk'),false);
 assert.equal(source.active,0);assert.equal(h.listeners,0);
});
test('equal emissions do not reset a candidate but continued performance proof additions do',async()=>{
 for(const changeProof of [false,true]){
  const perf=performanceFixture(),source=observable('same-private-token'),h=pageHarness(source,{performance:perf});
  let time=0;h.globals.Date={now:()=>time};
  const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:30,cancelEvent:'cancel-test'});
  await Promise.resolve();
  for(const nextTime of [10,20]){
   time=nextTime;if(changeProof)perf.measure('sherlock:fixture-sdk:private-runtime-event',nextTime);
   source.emit('same-private-token');await Promise.resolve();
  }
  const result=await pending;
  assert.equal(result.readiness_reason,'token_settling');assert.equal(result.media_timing.emissions,3);assert.equal(result.media_timing.value_changes,0);
  assert.equal(result.media_timing.proof_changes,changeProof?2:0);assert.equal(result.media_timing.candidate_changes,changeProof?2:0);
  assert.equal(result.media_timing.last_candidate_ms,changeProof?20:0);assert.equal(result.media_timing.source_changes,0);
  assert.equal(JSON.stringify(result).includes('private-'),false);assert.equal(source.active,0);assert.equal(h.listeners,0);
 }
});
test('source and authentication replacements are counted independently from equal token values',async()=>{
 const old=observable('same-value'),fresh=observable('same-value'),h=pageHarness(old);
 let time=0;h.globals.Date={now:()=>time};
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:30,cancelEvent:'cancel-test'});
 await Promise.resolve();time=10;h.setSource(fresh);old.emit('same-value');await Promise.resolve();await Promise.resolve();
 const replacement={service:{serviceName:'spl-commerce-dialog-service',authentication:{arpSessionToken:fresh.observe}}};
 time=20;h.globals.document.querySelectorAll=selector=>selector==='*'?[]:[replacement];
 fresh.emit('same-value');await Promise.resolve();await Promise.resolve();
 const result=await pending;
 assert.equal(result.media_timing.source_changes,1);assert.equal(result.media_timing.authentication_changes,1);
 assert.equal(result.media_timing.emissions,5);assert.equal(result.media_timing.value_changes,0);assert.equal(result.media_timing.proof_changes,0);
 assert.equal(result.media_timing.candidate_changes,2);assert.equal(result.media_timing.last_candidate_ms,20);
 assert.equal(old.active,0);assert.equal(fresh.active,0);assert.equal(h.listeners,0);
});
test('timing fields omit unseen milestones and bound clock values and event counts',async()=>{
 const missing=pageHarness(null);
 const absent=await missing.page.evaluate(runtimeARPInPage,{timeoutMs:10,cancelEvent:'cancel-test'});
 assert.deepEqual({...absent.media_timing},{candidate_changes:0,source_changes:0,authentication_changes:0,emissions:0,value_changes:0,proof_changes:0});
 const source=observable(undefined),h=pageHarness(source);
 let time=100;h.globals.Date={now:()=>time};
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:10,cancelEvent:'cancel-test'});
 time=-100;source.emit('first');await Promise.resolve();
 time=1e9;for(let i=0;i<66000;i++)source.emit(i%2?'first':'second');await Promise.resolve();
 h.globals.dispatchEvent(new Event('cancel-test'));
 const result=await pending;
 assert.equal(result.media_timing.first_nonempty_ms,0);assert.equal(result.media_timing.last_candidate_ms,60000);
 assert.equal(result.media_timing.emissions,65535);assert.equal(result.media_timing.value_changes,65535);
 for(const value of Object.values(result.media_timing))assert.ok(Number.isInteger(value)&&value>=0&&value<=65535);
 assert.equal(source.active,0);assert.equal(h.listeners,0);
});
test('execution context failures and exhausted editor navigation budgets remain distinguishable',async()=>{
 let time=0;
 const page={url:()=> 'https://firefly.adobe.com/generate/image',evaluate:async()=>{time=20;throw new Error('private-context-detail')}};
 await assert.rejects(ensureMediaSession(page,fixtureContext([]),{timeoutMs:20,now:()=>time}),error=>{
  assert.deepEqual(error.diagnostics,{media_runtime:{state:'unavailable',readiness_reason:'evaluation_failed'}});
  assert.equal(JSON.stringify(error).includes('private-context-detail'),false);return true;
 });
 for(const throws of [false,true]){
  time=0;
  const navigating={url:()=> 'https://firefly.adobe.com/',goto:async()=>{time=20;if(throws)throw new Error('private-navigation-detail')},evaluate:async()=>assert.fail('no budget remains for observation')};
  await assert.rejects(ensureMediaSession(navigating,fixtureContext([]),{timeoutMs:20,now:()=>time}),error=>{
   assert.equal(error.code,'media_session_incomplete');assert.deepEqual(error.diagnostics,{media_runtime:{state:'unavailable',readiness_reason:'navigation_budget_exhausted'}});return true;
  });
 }
});
test('navigation errors before the deadline retain their original classification',async()=>{
 const original=new Error('fixture network error'),page={url:()=> 'https://firefly.adobe.com/',goto:async()=>{throw original}};
 await assert.rejects(ensureMediaSession(page,fixtureContext([]),{timeoutMs:20,now:()=>0}),error=>error===original);
});
test('normal editor initialization reads SPL rather than the unrelated homepage service',async()=>{
 const h=pageHarness(observable('runtime'),{url:'https://firefly.adobe.com/'});
 const media=await ensureMediaSession(h.page,fixtureContext([{name:'sherlockToken',value:'old'}]));
 assert.deepEqual(h.navigations,['https://firefly.adobe.com/generate/image']);assert.equal(media.cookie,'sherlockToken=runtime');assert.equal(media.media_session_arp_mode,'present');
});
test('initial official null cannot replace an existing cookie with an omitted session',async()=>{
 const h=pageHarness(observable(null));
 await assert.rejects(ensureMediaSession(h.page,{cookies:async()=>assert.fail('pending media must not export cookies')},{timeoutMs:15}),error=>{
  assert.equal(error.code,'media_session_incomplete');assert.deepEqual(error.diagnostics.media_runtime,{state:'pending',value_kind:'null',readiness_reason:'token_pending',vendors_started:3,vendors_complete:3});
  assert.equal(error.diagnostics.media_timing.emissions,1);assert.equal(error.diagnostics.media_timing.value_changes,0);return true;
 });
});
test('pending and absent runtime sources produce different sanitized diagnostics',async()=>{
 const h=pageHarness(observable(undefined));
 await assert.rejects(ensureMediaSession(h.page,{cookies:async()=>assert.fail('pending source')},{timeoutMs:15}),error=>{
  assert.deepEqual(error.diagnostics.media_runtime,{state:'pending',value_kind:'unobserved',readiness_reason:'token_pending',vendors_started:3,vendors_complete:3});return true;
 });
 const missing=pageHarness(null);
 await assert.rejects(ensureMediaSession(missing.page,{cookies:async()=>assert.fail('missing runtime')},{timeoutMs:15}),e=>{
  assert.equal(e.code,'media_session_incomplete');assert.deepEqual(e.diagnostics.media_runtime,{state:'unavailable',readiness_reason:'authentication_missing'});
  assert.deepEqual({...e.diagnostics.media_page},{route:'editor',matching_host_count:0,authentication_count:0,arp_observer_count:0,scan_complete:true});return true;
 });
});
test('an unresponsive page is distinguished from a completed pending runtime observation',{timeout:3000},async()=>{
 let cancellationAttempts=0;
 const page={url:()=> 'https://firefly.adobe.com/generate/image',evaluate:async fn=>{
  if(fn.name==='runtimeARPInPage')return new Promise(()=>{});
  cancellationAttempts++;
 }};
 await assert.rejects(ensureMediaSession(page,{cookies:async()=>assert.fail('unresponsive runtime')},{timeoutMs:10}),error=>{
  assert.equal(error.code,'media_session_incomplete');assert.deepEqual(error.diagnostics,{media_runtime:{state:'observation_timeout'}});return true;
 });assert.equal(cancellationAttempts,1);
});
test('invalid runtime values cannot fall back to a historic Cookie',async()=>{
 const h=pageHarness(observable('invalid;cookie=injection'));
 await assert.rejects(ensureMediaSession(h.page,{cookies:async()=>assert.fail('invalid runtime')},{timeoutMs:15}),e=>{
  assert.equal(e.code,'media_session_incomplete');assert.equal(e.diagnostics.media_runtime.state,'invalid');assert.equal(JSON.stringify(e.diagnostics).includes('injection'),false);return true;
 });assert.equal(h.listeners,0);
});
test('another origin, another service or a changed subscription contract cannot prove optional ARP',async()=>{
 for(const h of [pageHarness(observable(null),{url:'https://example.test/generate/image'}),pageHarness(observable(null),{serviceName:'unknown-service'}),pageHarness({observe:callback=>{callback(null);return{unsubscribe(){}}}})]){
  const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
  assert.notEqual(result.source,'spl_runtime');assert.equal(h.listeners,0);
 }
});
test('replacement during subscription discards the old singleton and reads the current source',async()=>{
 const fresh=observable('fresh');let h;
 const old={observe(callback){callback('old');h.setSource(fresh);return()=>{}}};h=pageHarness(old);
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:700,cancelEvent:'cancel-test'});
 assert.equal(result.token,'fresh');assert.equal(fresh.active,0);assert.equal(h.listeners,0);
});
test('normal host initialization may publish a nonempty token after its initial null',async()=>{
 for(const initial of [undefined,null]){
  const source=observable(initial),h=pageHarness(source);
  const pending=ensureMediaSession(h.page,fixtureContext([{name:'sherlockToken',value:'stale'}]),{timeoutMs:700});
  assert.equal(source.active,1);source.emit(null);assert.equal(source.active,1);source.emit('later');
  const media=await pending;
  assert.equal(media.cookie,'sherlockToken=later');assert.equal(media.media_session_arp_mode,'present');assert.equal(source.active,0);assert.equal(h.listeners,0);
 }
});
test('replacing a pending source unsubscribes it before waiting for the current source',async()=>{
 const old=observable(null),fresh=observable(undefined),h=pageHarness(old);
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:500,cancelEvent:'cancel-test'});
 h.setSource(fresh);old.emit('stale');await Promise.resolve();assert.equal(old.active,0);
 fresh.emit('current');
 assert.equal((await pending).token,'current');assert.equal(fresh.active,0);assert.equal(h.listeners,0);
});

test('provisional nonempty tokens wait for the latest instance vendor token measurements',async()=>{
 const perf=performanceFixture({complete:false}),source=observable('initial-opaque'),h=pageHarness(source,{performance:perf});
 let completed=false;
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:800,cancelEvent:'cancel-test'}).then(result=>{completed=true;return result});
 await Promise.resolve();assert.equal(completed,false);
 for(const [vendor,start,token] of [['forter',4,'second-opaque'],['arkose',2,'third-opaque']]){
  perf.measure(`sherlock:fixture-sdk:${vendor}:start:duration`,start);source.emit(token);await Promise.resolve();assert.equal(completed,false);
 }
 perf.measure('sherlock:fixture-sdk:bfp:start:duration',3);source.emit('final-opaque');
 const result=await pending;
 assert.equal(result.token,'final-opaque');assert.equal(result.state,'ready');assert.equal(source.active,0);assert.equal(h.listeners,0);
});
test('an old completed SDK instance cannot validate a new instance provisional token',async()=>{
 const perf=performanceFixture(),next=performanceFixture({instance:'new-sdk',start:10,complete:false});perf.entries.push(...next.entries);
 const source=observable('new-initial'),h=pageHarness(source,{performance:perf});
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:30,cancelEvent:'cancel-test'});
 const result=await pending;
 assert.equal(result.state,'pending');assert.equal(result.readiness_reason,'instance_ambiguous');assert.equal(result.vendors_complete,0);assert.equal('token' in result,false);
 assert.equal(source.active,0);assert.equal(h.listeners,0);
});
test('a new completed SDK cannot lend its vendor evidence to a stale SPL source',async()=>{
 const perf=performanceFixture({complete:false}),next=performanceFixture({instance:'new-sdk',start:10});perf.entries.push(...next.entries);
 const source=observable('old-sdk-opaque'),h=pageHarness(source,{performance:perf});
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:30,cancelEvent:'cancel-test'});
 assert.equal(result.state,'pending');assert.equal(result.readiness_reason,'instance_ambiguous');assert.equal('token' in result,false);
 assert.equal(source.active,0);assert.equal(h.listeners,0);
});
test('post-vendor updates reset the stable window and preserve the final official token',async()=>{
 const source=observable('first-complete'),h=pageHarness(source);
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:900,cancelEvent:'cancel-test'});
 await new Promise(resolve=>setTimeout(resolve,120));source.emit('richer-complete');
 await new Promise(resolve=>setTimeout(resolve,160));assert.equal(source.active,1);
 const result=await pending;
 assert.equal(result.token,'richer-complete');assert.equal(source.active,0);
});
test('transient token changes in one call stack still reset the stable observation',async()=>{
 const source=observable('same-final'),h=pageHarness(source);
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:1000,cancelEvent:'cancel-test'});
 await new Promise(resolve=>setTimeout(resolve,190));source.emit(null);source.emit('same-final');
 await new Promise(resolve=>setTimeout(resolve,140));assert.equal(source.active,1);
 assert.equal((await pending).token,'same-final');assert.equal(source.active,0);
});
test('missing performance evidence or vendor participation times out without inferring token contents',async()=>{
 for(const [perf,reason] of [[null,'performance_unavailable'],[{getEntriesByType:()=>[]},'instance_missing'],[performanceFixture({vendors:['forter']}),'vendors_pending'],[performanceFixture({vendors:['arkose','bfp','forter','unknown']}),'vendor_unrecognized']]){
  const source=observable('opaque-with-any-length'),h=pageHarness(source,{performance:perf});
  await assert.rejects(ensureMediaSession(h.page,{cookies:async()=>assert.fail('incomplete vendors cannot export')},{timeoutMs:20}),error=>{
   assert.equal(error.code,'media_session_incomplete');assert.equal(error.diagnostics.media_runtime.readiness_reason,reason);return true;
  });
  assert.equal(source.active,0);assert.equal(h.listeners,0);
 }
});
test('a vendor restart needs its own new token measurement',async()=>{
 const perf=performanceFixture();perf.mark('sherlock:fixture-sdk:arkose:start',20);
 const source=observable('old-composite'),h=pageHarness(source,{performance:perf});
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:700,cancelEvent:'cancel-test'});
 await Promise.resolve();assert.equal(source.active,1);
 perf.measure('sherlock:fixture-sdk:arkose:start:duration',20);source.emit('new-composite');
 assert.equal((await pending).token,'new-composite');assert.equal(source.active,0);
});
test('the vendor loop may complete and emit again in the subscribing call stack',async()=>{
 const perf=performanceFixture({complete:false}),source=observable('initial'),h=pageHarness(source,{performance:perf});
 const pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:700,cancelEvent:'cancel-test'});
 for(const [vendor,start] of [['arkose',2],['bfp',3],['forter',4]])perf.measure(`sherlock:fixture-sdk:${vendor}:start:duration`,start);
 source.emit('complete-after-loop');
 assert.equal((await pending).token,'complete-after-loop');assert.equal(source.unsubscribed,1);
});
test('cancellation during provisional vendor loading removes every observer',async()=>{
 const source=observable('initial'),h=pageHarness(source,{performance:performanceFixture({complete:false})}),abort=new AbortController();
 const pending=ensureMediaSession(h.page,{cookies:async()=>assert.fail('cancelled')},{timeoutMs:1000,signal:abort.signal});
 await Promise.resolve();abort.abort();
 await assert.rejects(pending,e=>e.code==='media_session_cancelled');assert.equal(source.active,0);assert.equal(h.listeners,0);
});

function verifiedMediaHarness(token='runtime') {
 const source=observable(token),h=pageHarness(source);
 const identity={account_id:'fixture-account',email:'fixture@example.test',token:'fixture-access'};
 const values={userProfile:observable({userId:identity.account_id,email:identity.email,emailVerified:true}),accessToken:observable(identity.token),isSignedIn:observable(true)};
 Object.assign(h.authentication,Object.fromEntries(Object.entries(values).map(([key,value])=>[key,value.observe])));
 return {...h,source,identity,values};
}
test('final media verification confirms ARP and identity from one current SPL instance',async()=>{
 const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([]));
 await verifyMediaSession(h.page,media,h.identity);
 assert.equal(h.source.active,0);for(const value of Object.values(h.values))assert.equal(value.active,0);
});
test('changed ARP, identity, access token and sign-in status cannot pass final verification',async()=>{
 for(const [change,reason] of [[h=>h.source.emit('changed-arp'),'runtime_changed'],[h=>h.source.emit(null),'identity_pending'],[h=>h.values.accessToken.emit('changed-access'),'identity_changed'],[h=>h.values.userProfile.emit({userId:'other',email:h.identity.email}),'identity_changed'],[h=>h.values.isSignedIn.emit(false),'identity_changed']]){
  const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([]));change(h);
  await assert.rejects(verifyMediaSession(h.page,media,h.identity,{timeoutMs:30}),error=>{
   assert.equal(error.code,'media_session_incomplete');assert.deepEqual(error.diagnostics,{media_runtime:{state:'changed',readiness_reason:reason}});return true;
  });
  assert.equal(h.source.active,0);for(const value of Object.values(h.values))assert.equal(value.active,0);
 }
});

function pendingVerificationSources(h) {
 const values={userProfile:observable(undefined),accessToken:observable(undefined),isSignedIn:observable(undefined),arpSessionToken:observable(undefined)};
 for(const key of ['userProfile','accessToken','isSignedIn'])h.authentication[key]=values[key].observe;
 h.setSource(values.arpSessionToken);
 return values;
}
function assertVerificationReleased(h,sources) {
 for(const source of Object.values(sources))assert.equal(source.active,0);
 assert.equal(h.listeners,0);
}
function expectedVerificationValues(h) {
 return {userProfile:{userId:h.identity.account_id,email:h.identity.email,emailVerified:true},accessToken:h.identity.token,isSignedIn:true,arpSessionToken:'runtime'};
}
test('final verification waits for all four asynchronous observable emissions',async()=>{
 const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([])),sources=pendingVerificationSources(h);
 let finished=false;
 const pending=verifyMediaSession(h.page,media,h.identity,{timeoutMs:300}).then(()=>{finished=true});
 assert.equal(Object.values(sources).reduce((n,source)=>n+source.active,0),4);
 const expected=expectedVerificationValues(h);
 for(const key of ['userProfile','accessToken','isSignedIn']){sources[key].emit(expected[key]);await Promise.resolve();assert.equal(finished,false);}
 sources.arpSessionToken.emit(null);await Promise.resolve();assert.equal(finished,false);
 sources.arpSessionToken.emit(expected.arpSessionToken);
 await pending;assert.equal(finished,true);assertVerificationReleased(h,sources);
});
test('an unobserved or missing final value fails at the bounded deadline without leaking subscriptions',async()=>{
 for(const missing of ['userProfile','accessToken','isSignedIn','arpSessionToken']){
  const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([])),sources=pendingVerificationSources(h);
  const pending=verifyMediaSession(h.page,media,h.identity,{timeoutMs:20});
  for(const [key,value] of Object.entries(expectedVerificationValues(h)))if(key!==missing)sources[key].emit(value);
  if(missing==='arpSessionToken')sources[missing].emit(null);
  await assert.rejects(pending,error=>error.code==='media_session_incomplete'&&error.diagnostics.media_runtime.readiness_reason==='identity_pending');
  assertVerificationReleased(h,sources);
 }
});
test('asynchronous mismatches and transient matching-to-missing updates reject before other values arrive',async()=>{
 for(const [key,value,reason] of [['userProfile',{userId:'another',email:'other@example.test'},'identity_changed'],['accessToken','changed-access','identity_changed'],['isSignedIn',false,'identity_changed'],['arpSessionToken','changed-arp','runtime_changed'],['arpSessionToken',null,'runtime_changed']]){
  const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([])),sources=pendingVerificationSources(h);
  const pending=assert.rejects(verifyMediaSession(h.page,media,h.identity,{timeoutMs:500}),error=>{
   assert.equal(error.code,'media_session_incomplete');assert.equal(error.diagnostics.media_runtime.readiness_reason,reason);
   assert.equal(JSON.stringify(error.diagnostics).includes('changed-'),false);return true;
  });
  if(value===null)sources[key].emit(expectedVerificationValues(h)[key]);
  sources[key].emit(value);sources[key].emit(expectedVerificationValues(h)[key]);
  await pending;assertVerificationReleased(h,sources);
 }
});
test('runtime replacement or new SDK evidence during asynchronous verification fails closed',async()=>{
 for(const change of [h=>h.setSource(observable('runtime')),h=>{h.authentication.accessToken=observable(h.identity.token).observe},h=>h.globals.performance.entries.push(...performanceFixture({instance:'another-sdk',start:20}).entries)]){
  const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([])),sources=pendingVerificationSources(h);
  const pending=verifyMediaSession(h.page,media,h.identity,{timeoutMs:300});change(h);
  await assert.rejects(pending,error=>error.code==='media_session_incomplete'&&error.diagnostics.media_runtime.readiness_reason==='runtime_changed');
  assertVerificationReleased(h,sources);
 }
});
test('abort and pagehide cancel final asynchronous verification and remove every subscription',async()=>{
 for(const mode of ['abort','pagehide']){
  const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([])),sources=pendingVerificationSources(h),abort=new AbortController();
  const pending=verifyMediaSession(h.page,media,h.identity,{signal:abort.signal,timeoutMs:300});
  if(mode==='abort')abort.abort();else h.globals.dispatchEvent(new Event('pagehide'));
  await assert.rejects(pending,error=>mode==='abort'?error.code==='media_session_cancelled':error.code==='media_session_incomplete'&&error.diagnostics.media_runtime.readiness_reason==='runtime_changed');
  assertVerificationReleased(h,sources);
 }
});
test('final verification rejects unsupported observers and cleans earlier subscriptions',async()=>{
 const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([]));
 h.authentication.accessToken=callback=>{callback(h.identity.token);return{}};
 await assert.rejects(verifyMediaSession(h.page,media,h.identity),error=>error.code==='media_session_incomplete'&&error.diagnostics.media_runtime.readiness_reason==='observer_unavailable');
 assertVerificationReleased(h,{...h.values,arpSessionToken:h.source});
});
test('an unresponsive final page evaluation is externally bounded',{timeout:3000},async()=>{
 const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([]));
 let cancelAttempts=0;
 const page={evaluate:async fn=>{if(fn.name==='mediaSessionMatchesInPage')return new Promise(()=>{});cancelAttempts++;}};
 await assert.rejects(verifyMediaSession(page,media,h.identity,{timeoutMs:10}),error=>error.code==='media_session_incomplete'&&error.diagnostics.media_runtime.readiness_reason==='verification_timeout');
 assert.equal(cancelAttempts,1);
});
test('a new SDK cycle after capture invalidates final verification even when token is unchanged',async()=>{
 for(const change of [h=>h.globals.performance.entries.push(...performanceFixture({instance:'new-sdk',start:20,complete:false}).entries),h=>h.globals.performance.mark('sherlock:fixture-sdk:arkose:start',20)]){
  const h=verifiedMediaHarness(),media=await ensureMediaSession(h.page,fixtureContext([]));change(h);
  await assert.rejects(verifyMediaSession(h.page,media,h.identity),error=>error.code==='media_session_incomplete');
  assert.equal(h.source.active,0);for(const value of Object.values(h.values))assert.equal(value.active,0);
 }
});
test('old omitted records remain readable but cannot be presented as a newly verified media session',async()=>{
 const media={cookie:'',media_session_arp_mode:'omitted',media_session_arp_source:'spl_runtime'};
 assert.equal(isCompleteMediaSession(media),true);
 await assert.rejects(verifyMediaSession({evaluate:async()=>assert.fail('no current runtime token')},media,{}),e=>e.code==='media_session_incomplete');
});
test('cancellation while waiting for the component releases page listeners without collecting cookies',async()=>{
 const h=pageHarness(null),abort=new AbortController();
 const pending=ensureMediaSession(h.page,{cookies:async()=>assert.fail('cancelled')},{timeoutMs:1000,signal:abort.signal});abort.abort();
 await assert.rejects(pending,e=>e.code==='media_session_cancelled');assert.equal(h.listeners,0);
});
test('page navigation cancels component discovery and pre-cancelled operations do not navigate',async()=>{
 const h=pageHarness(null),pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:700,cancelEvent:'cancel-test'});
 h.globals.dispatchEvent(new Event('pagehide'));assert.equal((await pending).state,'unavailable');assert.equal(h.listeners,0);
 const abort=new AbortController();abort.abort();
 await assert.rejects(ensureMediaSession(h.page,fixtureContext([]),{signal:abort.signal}),e=>e.code==='media_session_cancelled');assert.deepEqual(h.navigations,[]);
});

const bootstrapIdentity={account_id:'fixture-account',email:'fixture@example.test',token:'fixture-access'};
function allowBootstrapInspection(h) {
 const identity=h.identity??bootstrapIdentity;
 for(const [key,value] of Object.entries({userProfile:{userId:identity.account_id,email:identity.email,emailVerified:true},accessToken:identity.token,isSignedIn:true})){
  if(typeof h.authentication[key]!=='function')h.authentication[key]=observable(value).observe;
 }
 const evaluate=h.page.evaluate;h.page.evaluate=(fn,args)=>evaluate(fn,fn===bootstrapRecoveryAllowedInPage?{identity,...args}:args);
 h.globals.getComputedStyle=()=>({display:'block',visibility:'visible'});
 h.globals.document.body={innerText:''};
 return h;
}
function emptyPerformance() {return {getEntriesByType:()=>[]};}

test('bootstrap recovery rechecks the live source and releases its subscription',async()=>{
 for(const value of [null,'',undefined,'new-runtime-value',{private:'value'}]){
  const source=observable(value),h=allowBootstrapInspection(pageHarness(source,{performance:emptyPerformance()}));
  const allowed=await h.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel'});
  assert.equal(allowed,value===null||value==='');
  assert.equal(source.active,0);assert.equal(source.subscribed,1);assert.equal(source.unsubscribed,1);assert.equal(h.listeners,0);
 }
 const missing=allowBootstrapInspection(pageHarness(null,{performance:emptyPerformance()}));
 assert.equal(await missing.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel'}),false);
 assert.equal(await missing.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel',allowPending:true}),true);
 const noObserver=allowBootstrapInspection(pageHarness({},{performance:emptyPerformance()}));
 assert.equal(await noObserver.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel'}),false);
});

test('initialized SDK, official navigation, lifecycle evidence and visible challenges prevent reload',async()=>{
 for(const change of [
  h=>{h.globals.SherlockSdk={}},h=>{h.globals.UniversalNav=()=>{}},
  h=>{h.globals.performance=performanceFixture({complete:false})},
  h=>{h.globals.document.body.innerText='Please verify you are human'},
  h=>{h.globals.location.origin='https://auth.services.adobe.com'},
  h=>{h.globals.performance=null},
 ]){
  const source=observable(null),h=allowBootstrapInspection(pageHarness(source,{performance:emptyPerformance()}));change(h);
  assert.equal(await h.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel'}),false);
  assert.equal(source.active,0);assert.equal(source.subscribed,0);assert.equal(h.listeners,0);
 }
 for(const visible of [false,true]){
  const source=observable(null),h=allowBootstrapInspection(pageHarness(source,{performance:emptyPerformance()}));
  const original=h.globals.document.querySelectorAll;
  h.globals.document.querySelectorAll=selector=>selector==='iframe'?[{src:'https://challenges.cloudflare.com/widget',getClientRects:()=>visible?[{width:100,height:100}]:[]}]:original(selector);
  assert.equal(await h.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel'}),!visible);
  assert.equal(source.active,0);assert.equal(h.listeners,0);
 }
});

test('bootstrap recovery rejects a changed source and cancels an unobserved source cleanly',async()=>{
 const first=observable(undefined),next=observable(null),h=allowBootstrapInspection(pageHarness(first,{performance:emptyPerformance()}));
 const pending=h.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:50,cancelEvent:'bootstrap-cancel'});
 h.setSource(next);first.emit(null);assert.equal(await pending,false);assert.equal(first.active,0);assert.equal(next.active,0);assert.equal(h.listeners,0);
 const waiting=observable(undefined),other=allowBootstrapInspection(pageHarness(waiting,{performance:emptyPerformance()}));
 const cancelled=other.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:1000,cancelEvent:'bootstrap-cancel'});
 other.globals.dispatchEvent(new Event('bootstrap-cancel'));assert.equal(await cancelled,false);assert.equal(waiting.active,0);assert.equal(other.listeners,0);
});

function bootstrapSequence({observations=[],initialURL='https://firefly.adobe.com/generate/image',navigationExhausted=false,guard=true,reload}={}) {
 let time=0,url=initialURL,index=0;
 const calls=[];
 const page={url:()=>url,
  async goto(next,options){calls.push(['goto',options.timeout]);url=next;if(navigationExhausted){time+=options.timeout;throw Error('navigation timeout')}},
  async reload(options){calls.push(['reload',options.timeout]);if(reload)return reload({options,setTime:value=>{time=value}});time+=1;},
  async evaluate(fn,args){
   if(fn===bootstrapRecoveryAllowedInPage){calls.push(['guard']);return typeof guard==='function'?guard():guard;}
   assert.equal(fn,runtimeARPInPage);calls.push(['observe',args.timeoutMs]);
   const observation=observations[index++]??{state:'unavailable',readiness_reason:'authentication_missing'};
   if(observation.state!=='ready')time+=args.timeoutMs;
   return observation;
  }};
 return {page,calls,identity:bootstrapIdentity,now:()=>time,pause:async ms=>{time+=ms},get time(){return time}};
}
const missingRuntime=()=>({state:'pending',source:'spl_runtime',value_kind:'null',readiness_reason:'instance_missing',vendors_started:0,vendors_complete:0,
 media_timing:{emissions:1,source_changes:0,authentication_changes:0}});
const readyRuntime=()=>({state:'ready',source:'spl_runtime',token:'fresh-runtime',runtime_proof:{instance:'fixture-new-document'}});

test('a navigation timeout on the trusted editor gets one bounded observation and one ordinary reload',async()=>{
 const h=bootstrapSequence({initialURL:'https://firefly.adobe.com/',navigationExhausted:true,observations:[missingRuntime(),readyRuntime()]});
 const media=await ensureMediaSession(h.page,fixtureContext([]),{timeoutMs:30,now:h.now,pause:h.pause,identity:h.identity});
 assert.equal(media.cookie,'sherlockToken=fresh-runtime');assert.deepEqual(media.runtime_proof,{instance:'fixture-new-document'});
 assert.deepEqual(h.calls,[['goto',30],['guard'],['observe',10],['guard'],['reload',10],['observe',19]]);assert.ok(h.time<=60);
});

test('fast success and same-document recovery do not reload or issue an extra navigation',async()=>{
 for(const observations of [[readyRuntime()],[missingRuntime(),readyRuntime()]]){
  const h=bootstrapSequence({observations});
  const media=await ensureMediaSession(h.page,fixtureContext([]),{timeoutMs:30,now:h.now,pause:h.pause,identity:h.identity});
  assert.equal(media.cookie,'sherlockToken=fresh-runtime');assert.equal(h.calls.some(call=>['goto','reload'].includes(call[0])),false);
  assert.equal(h.calls.filter(call=>call[0]==='observe').length,observations.length);
 }
});

test('recovery never reloads twice and retains the final observation within its total budget',async()=>{
 const h=bootstrapSequence({observations:[missingRuntime(),missingRuntime(),missingRuntime()]});
 await assert.rejects(ensureMediaSession(h.page,fixtureContext([]),{timeoutMs:30,now:h.now,pause:h.pause,identity:h.identity}),error=>{
  assert.equal(error.code,'media_session_incomplete');assert.equal(error.diagnostics.media_runtime.readiness_reason,'instance_missing');return true;
 });
 assert.equal(h.calls.filter(call=>call[0]==='reload').length,1);assert.equal(h.calls.filter(call=>call[0]==='observe').length,3);assert.equal(h.time,60);
});

test('existing tokens, changed identities, vendor progress and invalid contracts cannot trigger recovery',async()=>{
 for(const observation of [
  {...missingRuntime(),media_timing:{first_nonempty_ms:0}},
  {...missingRuntime(),media_timing:{authentication_changes:1}},
  {...missingRuntime(),media_timing:{source_changes:1}},
  {state:'pending',readiness_reason:'vendors_pending',vendors_started:3,vendors_complete:2},
  {state:'pending',readiness_reason:'token_pending',vendors_started:3,vendors_complete:3},
  {state:'changed',readiness_reason:'identity_changed'},
  {state:'unavailable',readiness_reason:'editor_route_changed'},
  {state:'invalid'},{state:'invalid_contract'},
 ]){
  const h=bootstrapSequence({observations:[observation]});
  await assert.rejects(ensureMediaSession(h.page,fixtureContext([]),{timeoutMs:30,now:h.now,pause:h.pause,identity:h.identity}),error=>error.code==='media_session_incomplete');
  assert.deepEqual(h.calls,[['observe',30]]);
 }
 const changed=bootstrapSequence({observations:[missingRuntime()],guard:false});
 await assert.rejects(ensureMediaSession(changed.page,fixtureContext([]),{timeoutMs:30,now:changed.now,pause:changed.pause,identity:changed.identity}),error=>error.code==='media_session_incomplete');
 assert.deepEqual(changed.calls,[['observe',30],['guard']]);
});

test('a token observed during same-document recovery prevents a later reload even with no SDK',async()=>{
 const h=bootstrapSequence({observations:[missingRuntime(),{...missingRuntime(),media_timing:{first_nonempty_ms:1}}]});
 await assert.rejects(ensureMediaSession(h.page,fixtureContext([]),{timeoutMs:30,now:h.now,pause:h.pause,identity:h.identity}),error=>error.code==='media_session_incomplete');
 assert.deepEqual(h.calls,[['observe',30],['guard'],['observe',10]]);
});

test('a stalled reload is cancelled at the overall budget without exporting cookies',async()=>{
 const h=bootstrapSequence({observations:[missingRuntime(),missingRuntime()],reload:async()=>new Promise(()=>{})});
 const started=Date.now();
 await assert.rejects(ensureMediaSession(h.page,{cookies:async()=>assert.fail('no session')},{timeoutMs:20,now:h.now,pause:h.pause,identity:h.identity}),error=>error.code==='media_session_incomplete');
 assert.ok(Date.now()-started<250);assert.equal(h.calls.filter(call=>call[0]==='reload').length,1);
});

test('external cancellation during the sole reload does not start another observation',async()=>{
 const abort=new AbortController();
 const h=bootstrapSequence({observations:[missingRuntime(),missingRuntime()],reload:async()=>{abort.abort();return new Promise(()=>{})}});
 await assert.rejects(ensureMediaSession(h.page,fixtureContext([]),{timeoutMs:30,now:h.now,pause:h.pause,signal:abort.signal,identity:h.identity}),error=>error.code==='media_session_cancelled');
 assert.equal(h.calls.filter(call=>call[0]==='reload').length,1);assert.equal(h.calls.filter(call=>call[0]==='observe').length,2);
});

test('a transient nonempty guard emission forbids reload even if the last value is null',async()=>{
 let releases=0;
 const h=allowBootstrapInspection(pageHarness({observe:listener=>{listener('transient');listener(null);return()=>{releases++}}},{performance:emptyPerformance()}));
 assert.equal(await h.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel'}),false);
 assert.equal(releases,1);assert.equal(h.listeners,0);
});

test('a recovered document still needs its own runtime proof and matching final identity',async()=>{
 const h=allowBootstrapInspection(verifiedMediaHarness('fresh-runtime'));
 h.source.emit(null);h.globals.performance=emptyPerformance();
 let reloads=0;
 h.page.reload=async()=>{reloads++;h.globals.performance=performanceFixture({instance:'reloaded-sdk'});h.source.emit('fresh-runtime')};
 const media=await ensureMediaSession(h.page,fixtureContext([]),{timeoutMs:750,identity:h.identity});
 assert.equal(reloads,1);assert.equal(media.runtime_proof.instance,'sherlock:reloaded-sdk:init:start');
 await verifyMediaSession(h.page,media,h.identity);
 h.values.userProfile.emit({userId:'other-account',email:h.identity.email,emailVerified:true});
 await assert.rejects(verifyMediaSession(h.page,media,h.identity),error=>error.diagnostics.media_runtime.readiness_reason==='identity_changed');
 assertVerificationReleased(h,h.values);assert.equal(h.source.active,0);
});

test('the current authentication identity must still match before a reload is permitted',async()=>{
 for(const allowPending of [false,true]){
  for(const [key,value] of [['userProfile',{userId:'other-account',email:bootstrapIdentity.email}],['accessToken','changed-access'],['isSignedIn',false],['userProfile',{userId:bootstrapIdentity.account_id,email:'other@example.test'}]]){
   const h=allowBootstrapInspection(pageHarness(observable(null),{performance:emptyPerformance()}));
   const changed=observable(value);h.authentication[key]=changed.observe;
   assert.equal(await h.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel',allowPending}),false);
   assert.equal(changed.active,0);assert.equal(h.listeners,0);
  }
 }
 const h=allowBootstrapInspection(pageHarness(observable(null),{performance:emptyPerformance()}));
 let releases=0;
 h.authentication.isSignedIn=listener=>{listener(true);listener(false);listener(true);return()=>{releases++}};
 assert.equal(await h.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel'}),false);
 assert.equal(releases,1);assert.equal(h.listeners,0);
});

test('bootstrap guards release a disposer returned after synchronous pagehide or cancellation',async()=>{
 for(const event of ['pagehide','bootstrap-cancel']){
  let active=0;
  const h=allowBootstrapInspection(pageHarness({observe:listener=>{active++;listener(null);h.globals.dispatchEvent(new Event(event));return()=>{active--}}},{performance:emptyPerformance()}));
  assert.equal(await h.page.evaluate(bootstrapRecoveryAllowedInPage,{timeoutMs:10,cancelEvent:'bootstrap-cancel'}),false);
  assert.equal(active,0);assert.equal(h.listeners,0);
 }
});

test('an explicit previously verified identity is required to enable bootstrap recovery',async()=>{
 const h=bootstrapSequence({observations:[missingRuntime()]});
 await assert.rejects(ensureMediaSession(h.page,fixtureContext([]),{timeoutMs:30,now:h.now,pause:h.pause}),error=>error.code==='media_session_incomplete');
 assert.deepEqual(h.calls,[['observe',30]]);
});

test('reload waits for commit and reserves observation time even when that navigation times out',async()=>{
 const h=bootstrapSequence({observations:[missingRuntime(),missingRuntime(),readyRuntime()],reload:async({options,setTime})=>{
  assert.equal(options.waitUntil,'commit');assert.equal(options.timeout,10);setTime(50);throw Error('commit timeout');
 }});
 const media=await ensureMediaSession(h.page,fixtureContext([]),{timeoutMs:30,now:h.now,pause:h.pause,identity:h.identity});
 assert.equal(media.cookie,'sherlockToken=fresh-runtime');assert.equal(h.calls.at(-1)[0],'observe');assert.equal(h.calls.at(-1)[1],10);
});
