import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import {isUsableSherlock, mediaCookieHeader, isCompleteMediaSession, runtimeARPInPage, ensureMediaSession} from './media-session.mjs';

function observable(initial) {
  let value=initial, subscriptions=0, unsubscriptions=0;
  const listeners=new Set();
  return {observe(listener){subscriptions++;listeners.add(listener);if(value!==undefined)listener(value);return()=>{if(listeners.delete(listener))unsubscriptions++;}},
    emit(next){value=next;for(const listener of [...listeners])listener(value)},
    get active(){return listeners.size},get subscribed(){return subscriptions},get unsubscribed(){return unsubscriptions}};
}
function pageHarness(source,{url='https://firefly.adobe.com/generate/image',shadow=true,serviceName='spl-commerce-dialog-service'}={}) {
  const listeners=new Map(),navigations=[];
  let currentSource=source;
  const authentication={get arpSessionToken(){return currentSource?.observe}};
  const host={service:{serviceName,authentication}};
  const editorRoot={querySelectorAll(selector){return selector==='*'?[host]:currentSource?[host]:[]}};
  const globals={Event,setTimeout,clearTimeout,setInterval,clearInterval,
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
  return {page,globals,navigations,setSource(next){currentSource=next},get listeners(){return [...listeners.values()].reduce((n,items)=>n+items.size,0)}};
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
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:50,cancelEvent:'cancel-test'});
 assert.equal(result.token,'runtime-ready');assert.equal(result.source,'spl_runtime');assert.equal(source.active,0);assert.equal(source.subscribed,1);assert.equal(source.unsubscribed,1);assert.equal(h.listeners,0);
});
test('explicit null and a non-emitting SPL source remain distinguishable snapshots',async()=>{
 for(const [value,state] of [[null,'empty'],[undefined,'unobserved']]){
  const source=observable(value),h=pageHarness(source);
  const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:50,cancelEvent:'cancel-test'});
  assert.equal(result.state,state);assert.equal(result.source,'spl_runtime');assert.equal('token' in result,false);
  if(value===null)assert.equal(result.value_kind,'null');assert.equal(source.active,0);assert.equal(h.listeners,0);
 }
});
test('a missing source is not confused with the official null default and times out cleanly',async()=>{
 const h=pageHarness(null);
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:15,cancelEvent:'cancel-test'});
 assert.equal(result.state,'unavailable');assert.equal(result.source,undefined);assert.equal(h.listeners,0);
});
test('normal editor initialization reads SPL rather than the unrelated homepage service',async()=>{
 const h=pageHarness(observable('runtime'),{url:'https://firefly.adobe.com/'});
 const media=await ensureMediaSession(h.page,fixtureContext([{name:'sherlockToken',value:'old'}]));
 assert.deepEqual(h.navigations,['https://firefly.adobe.com/generate/image']);assert.equal(media.cookie,'sherlockToken=runtime');assert.equal(media.media_session_arp_mode,'present');
});
test('official null omits ARP and discards an old Cookie instead of retaining it',async()=>{
 const h=pageHarness(observable(null));
 const media=await ensureMediaSession(h.page,fixtureContext([{name:'sherlockToken',value:'stale'},{name:'other',value:'kept'}]));
 assert.equal(media.cookie,'other=kept');assert.equal(media.media_session_arp_mode,'omitted');assert.equal(media.media_session_arp_source,'spl_runtime');assert.equal(media.runtime_state.state,'empty');
});
test('the official first-value null fallback is supported only after the SPL source is found',async()=>{
 const h=pageHarness(observable(undefined));
 const media=await ensureMediaSession(h.page,fixtureContext([{name:'sherlockToken',value:'old'}]));
 assert.equal(media.cookie,'');assert.equal(media.media_session_arp_mode,'omitted');assert.equal(media.runtime_state.state,'unobserved');
 const missing=pageHarness(null);
 await assert.rejects(ensureMediaSession(missing.page,{cookies:async()=>assert.fail('missing runtime')},{timeoutMs:15}),e=>e.code==='media_session_incomplete'&&e.diagnostics.media_runtime.state==='unavailable');
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
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:200,cancelEvent:'cancel-test'});
 assert.equal(result.token,'fresh');assert.equal(fresh.active,0);assert.equal(h.listeners,0);
});
test('late emissions after the official snapshot do not alter its omitted state',async()=>{
 const source=observable(undefined),h=pageHarness(source);
 const result=await h.page.evaluate(runtimeARPInPage,{timeoutMs:50,cancelEvent:'cancel-test'});source.emit('later');
 assert.equal(result.state,'unobserved');assert.equal('token' in result,false);assert.equal(source.active,0);
});
test('cancellation while waiting for the component releases page listeners without collecting cookies',async()=>{
 const h=pageHarness(null),abort=new AbortController();
 const pending=ensureMediaSession(h.page,{cookies:async()=>assert.fail('cancelled')},{timeoutMs:1000,signal:abort.signal});abort.abort();
 await assert.rejects(pending,e=>e.code==='media_session_cancelled');assert.equal(h.listeners,0);
});
test('page navigation cancels component discovery and pre-cancelled operations do not navigate',async()=>{
 const h=pageHarness(null),pending=h.page.evaluate(runtimeARPInPage,{timeoutMs:100,cancelEvent:'cancel-test'});
 h.globals.dispatchEvent(new Event('pagehide'));assert.equal((await pending).state,'unavailable');assert.equal(h.listeners,0);
 const abort=new AbortController();abort.abort();
 await assert.rejects(ensureMediaSession(h.page,fixtureContext([]),{signal:abort.signal}),e=>e.code==='media_session_cancelled');assert.deepEqual(h.navigations,[]);
});
