import test from 'node:test';
import { RegistrationError } from './core.mjs';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import {identityInPage,waitForIdentity,validateIdentity,signInExistingAccount,refreshSession,prepareWorkerRefreshPage} from './session-refresh.mjs';
const capturedMedia=(cookie='sherlockToken=runtime',mode='present')=>({cookie,media_session_arp_mode:mode,media_session_arp_source:'spl_runtime',runtime_state:{state:mode==='present'?'ready':'empty'}});

test('worker refresh closes every extra restored tab before navigating its one retained page',async()=>{
 const events=[],retained={goto:async()=>{assert.equal(pages.length,1);events.push('navigate')},url:()=> 'https://firefly.adobe.com/'};
 let pages=[retained];
 for(let index=0;index<8;index++){
  const page={close:async options=>{assert.deepEqual(options,{runBeforeUnload:false});pages=pages.filter(item=>item!==page);events.push('close')}};pages.push(page);
 }
 const context={pages:()=>[...pages],cookies:async()=>[],newPage:async()=>assert.fail('existing page retained')};
 await assert.rejects(refreshSession(context,{email:'fixture@example.test'},{workerMode:true,waitIdentity:async()=>null}),e=>e.code==='refresh_login_required');
 assert.deepEqual(events,[...Array(8).fill('close'),'navigate']);assert.equal(pages[0],retained);
});
test('normal refresh never closes tabs without the dedicated worker opt-in',async()=>{
 const retained={goto:async()=>{},url:()=> 'https://firefly.adobe.com/'};
 const other={close:async()=>assert.fail('not a worker-owned context')};
 await assert.rejects(refreshSession({pages:()=>[retained,other],cookies:async()=>[]},{email:'fixture@example.test'},{waitIdentity:async()=>null}),e=>e.code==='refresh_login_required');
});
test('worker page cleanup fails closed before navigation if a restored page refuses closure',async()=>{
 let navigations=0;
 const retained={goto:async()=>navigations++},other={close:async()=>{throw new Error('private page details')}};
 await assert.rejects(refreshSession({pages:()=>[retained,other]},{},{workerMode:true}),e=>{
  assert.equal(e.code,'refresh_page_cleanup_failed');assert.equal(e.message.includes('private'),false);return true;
 });assert.equal(navigations,0);
});
test('worker page cleanup is bounded and rejects unconfirmed closure or newly opened tabs',async()=>{
 const retained={};
 for(const context of [
  {pages:()=>[retained,{close:async()=>new Promise(()=>{})}]},
  {pages:()=>[retained,{close:async()=>{}}]},
 ])await assert.rejects(prepareWorkerRefreshPage(context,{closeTimeoutMs:10}),e=>e.code==='refresh_page_cleanup_failed');
 let pages=[retained];const other={close:async()=>{pages=[retained,{}]}};pages.push(other);
 await assert.rejects(prepareWorkerRefreshPage({pages:()=>pages}),e=>e.code==='refresh_page_cleanup_failed');
});
test('worker page cleanup creates one page for an empty dedicated context and honors cancellation',async()=>{
 const page={};let pages=[],created=0;
 const context={pages:()=>pages,newPage:async()=>{created++;pages=[page];return page}};
 assert.equal(await prepareWorkerRefreshPage(context),page);assert.equal(created,1);
 const abort=new AbortController();abort.abort();
 await assert.rejects(prepareWorkerRefreshPage({pages:()=>assert.fail('cancelled'),newPage:async()=>assert.fail('cancelled')},{signal:abort.signal}),e=>e.code==='media_session_cancelled');
});

test('identity polling tolerates SDK startup without submitting or navigating',async()=>{
 let time=0,calls=0;
 const identity={email:'fixture@example.test',account_id:'fixture',token:'secret'};
 const page={evaluate:async()=>++calls<3?null:identity};
 assert.equal(await waitForIdentity(page,{now:()=>time,pause:async ms=>{time+=ms}}),identity);
 assert.equal(calls,3);
});
test('identity polling is bounded and does not invent a session',async()=>{
 let time=0,calls=0;
 const page={evaluate:async()=>{calls++;return null}};
 assert.equal(await waitForIdentity(page,{timeoutMs:2000,now:()=>time,pause:async ms=>{time+=ms}}),null);
 assert.equal(calls,2);
});
test('refresh rejects another account, email and unverified session',()=>{
 const wanted={account_id:'expected',email:'fixture@example.test'};
 for(const identity of [null,{account_id:'other',email:wanted.email,token:'secret'},{account_id:'expected',email:'other@example.test',token:'secret'},{...wanted,token:'secret',emailVerified:false}]){
  assert.throws(()=>validateIdentity(identity,wanted),e=>e.code.startsWith('refresh_')&&!e.message.includes('secret'));
 }
 assert.equal(validateIdentity({...wanted,token:'secret'},wanted).account_id,'expected');
});
test('password fallback never starts without stored credentials',async()=>{
 let navigations=0;
 await assert.rejects(signInExistingAccount({goto:async()=>navigations++},{email:'fixture@example.test'}),e=>e.code==='refresh_login_required');
 assert.equal(navigations,0);
});

test('existing login stops at verification after a single email submission',async()=>{
 const actions=[];
 const email={first(){return this},waitFor:async()=>{},count:async()=>1,fill:async()=>actions.push('email')};
 const password={waitFor:async()=>{throw new Error('verification page')},count:async()=>0};
 const page={goto:async()=>actions.push('navigate-account'),locator:s=>s.includes('one-time-code')?{count:async()=>1}:s.includes('email')?email:password,
  getByRole:(role,{name})=>{assert.equal(name.test('Create an account'),false);return {count:async()=>1,click:async()=>actions.push('continue')}}};
 await assert.rejects(signInExistingAccount(page,{email:'fixture@example.test',password:'fixture-password'}),e=>e.code==='refresh_verification_required');
 assert.deepEqual(actions,['navigate-account','email','continue']);
});
test('existing login submits email and password once without account creation',async()=>{
 const actions=[];
 const email={first(){return this},waitFor:async()=>{},count:async()=>1,fill:async()=>actions.push('email')};
 const password={waitFor:async()=>{},count:async()=>1,fill:async()=>actions.push('password')};
 const page={url:()=> 'https://auth.services.adobe.com/login',evaluate:async fn=>fn.name==='emailOTPInPage'?{kind:'none'}:null,
  goto:async()=>actions.push('navigate-account'),locator:s=>s.includes('email')?email:password,
  getByRole:(role,{name})=>{assert.equal(name.test('Create account'),false);return {count:async()=>1,click:async()=>actions.push('continue')}},
  waitForURL:async predicate=>{assert.equal(predicate(new URL('https://account.adobe.com/')),true);actions.push('observe-account')}};
 await signInExistingAccount(page,{email:'fixture@example.test',password:'fixture-password'});
 assert.deepEqual(actions,['navigate-account','email','continue','password','continue','observe-account']);
});

test('IMS success with incomplete media fails instead of completing with retained credentials',async()=>{
 const expected={account_id:'expected',email:'fixture@example.test',profile_id:'fixture'};
 const page={goto:async()=>{},url:()=> 'https://firefly.adobe.com/'};
 const context={pages:()=>[page],cookies:async()=>[{name:'sherlockToken',value:'stale',domain:'.adobe.com',path:'/'}]};
 await assert.rejects(refreshSession(context,expected,{waitIdentity:async()=>({...expected,token:'fixture-token'}),ensureMedia:async()=>{throw new RegistrationError('media_session_incomplete')}}),e=>{
  assert.equal(e.code,'media_session_incomplete');
  assert.deepEqual(e.diagnostics.sherlock_cookies,[{domain:'.adobe.com',path:'/',present:true}]);
  assert.equal(JSON.stringify(e.diagnostics).includes('stale'),false);return true;
 });
});
test('complete media refresh returns the verified final identity and explicit readiness contract',async()=>{
 const expected={account_id:'expected',email:'fixture@example.test',profile_id:'fixture'};let identities=0;
 const page={goto:async()=>{},url:()=> 'https://firefly.adobe.com/',evaluate:async(fn,args)=>{
  assert.equal(fn.name,'mediaSessionMatchesInPage');assert.equal(identities,2);assert.equal(args.identity.token,'after-token');assert.equal(args.token,'runtime');return true;
 }};
 const context={pages:()=>[page],cookies:async url=>url.includes('adobeid')?[{name:'ims',value:'fixture'}]:[]};
 const result=await refreshSession(context,expected,{waitIdentity:async()=>({...expected,token:++identities===1?'before-token':'after-token'}),ensureMedia:async()=> capturedMedia('other=kept; sherlockToken=runtime')});
 assert.equal(identities,2);assert.equal(result.token,'after-token');assert.equal(result.cookie,'other=kept; sherlockToken=runtime');
 assert.equal(result.refresh_cookie,'ims=fixture');assert.equal(result.media_session_refreshed,true);assert.equal(result.media_session_status,'complete');
});
test('an identity change during normal media initialization refuses the whole refresh',async()=>{
 const expected={account_id:'expected',email:'fixture@example.test',profile_id:'fixture'};let identities=0;
 const page={goto:async()=>{},url:()=> 'https://firefly.adobe.com/'};
 const context={pages:()=>[page],cookies:async()=>[]};
 await assert.rejects(refreshSession(context,expected,{waitIdentity:async()=>({...expected,account_id:++identities===1?'expected':'changed',token:'fixture-token'}),ensureMedia:async()=> capturedMedia()}),e=>e.code==='refresh_account_mismatch');
});
test('media cancellation is forwarded and never reported as a completed token-only refresh',async()=>{
 const expected={account_id:'expected',email:'fixture@example.test',profile_id:'fixture'},abort=new AbortController();
 const page={goto:async()=>{},url:()=> 'https://firefly.adobe.com/'};
 const context={pages:()=>[page],cookies:async()=>assert.fail('cancelled refresh must not collect diagnostics')};
 await assert.rejects(refreshSession(context,expected,{signal:abort.signal,waitIdentity:async()=>({...expected,token:'fixture-token'}),ensureMedia:async(_page,_context,options)=>{
  assert.equal(options.signal,abort.signal);throw new RegistrationError('media_session_cancelled');
 }}),e=>e.code==='media_session_cancelled');
});

test('account homepage uses its unique Sign in link before submitting existing credentials',async()=>{
 const expected={email:'fixture@example.test',account_id:'expected',password:'fixture-password'};
 const actions=[];let opened=false,passwordVisible=false;
 const email={count:async()=>opened&&!passwordVisible?1:0,fill:async()=>actions.push('email')};
 const password={count:async()=>passwordVisible?1:0,waitFor:async()=>assert.equal(passwordVisible,true),fill:async()=>actions.push('password')};
 const forms={first(){return this},waitFor:async()=>{assert.equal(opened,true);actions.push('form-ready')}};
 const page={url:()=> 'https://auth.services.adobe.com/login',evaluate:async fn=>fn.name==='emailOTPInPage'?{kind:'none'}:null,goto:async()=>actions.push('account'),
  locator:selector=>selector.includes('email')&&selector.includes('password')?forms:selector.includes('email')?email:password,
  getByRole:(role,{name})=>{
   assert.equal(name.test('Create an account'),false);
   const isEntry=!name.test('Continue');
   if(isEntry)return {count:async()=>role==='link'?1:0,click:async()=>{assert.equal(role,'link');opened=true;actions.push('sign-in')}};
   return {count:async()=>1,click:async()=>{passwordVisible=true;actions.push('continue')}};
  },waitForURL:async predicate=>{assert.equal(predicate(new URL('https://account.adobe.com/')),true);actions.push('signed-in')}};
 await signInExistingAccount(page,expected);
 assert.deepEqual(actions,['account','sign-in','form-ready','email','continue','password','continue','signed-in']);
});
test('account homepage already signed in to the expected identity does not submit a form',async()=>{
 const expected={email:'fixture@example.test',account_id:'expected',password:'fixture-password'};let navigations=0;
 const page={goto:async()=>navigations++,evaluate:async()=>({...expected,token:'identity-token'}),locator:()=>assert.fail('already signed in')};
 await signInExistingAccount(page,expected);assert.equal(navigations,1);
 await assert.rejects(signInExistingAccount({...page,evaluate:async()=>({...expected,account_id:'different',token:'identity-token'})},expected),e=>e.code==='refresh_account_mismatch');
});
test('ambiguous Sign in controls do not trigger navigation or credential submission',async()=>{
 const page={goto:async()=>{},evaluate:async()=>null,locator:()=>({count:async()=>0}),getByRole:()=>({count:async()=>1,click:async()=>assert.fail('ambiguous entry')})};
 await assert.rejects(signInExistingAccount(page,{email:'fixture@example.test',password:'fixture-password'}),e=>e.code==='refresh_login_entry_unrecognized');
});
test('an account shell with no login controls is not reported as human verification',async()=>{
 let time=0;
 const page={goto:async()=>{},evaluate:async()=>null,locator:()=>({count:async()=>0}),getByRole:()=>({count:async()=>0,click:async()=>assert.fail('no entry')})};
 await assert.rejects(signInExistingAccount(page,{email:'fixture@example.test',password:'fixture-password'},{landingTimeoutMs:1000,now:()=>time,pause:async ms=>{time+=ms}}),e=>e.code==='refresh_login_entry_unrecognized');
 assert.equal(time,1000);
});
test('a clicked Sign in entry that never opens a form stops without submitting an email',async()=>{
 let clicks=0;
 const forms={count:async()=>0,first(){return this},waitFor:async()=>{throw new Error('form did not open')},fill:async()=>assert.fail('no form')};
 const page={goto:async()=>{},evaluate:async()=>null,locator:()=>forms,getByRole:role=>({count:async()=>role==='button'?1:0,click:async()=>clicks++})};
 await assert.rejects(signInExistingAccount(page,{email:'fixture@example.test',password:'fixture-password'}),e=>e.code==='refresh_login_form_unavailable');
 assert.equal(clicks,1);
});
test('missing password after one email submission is not mislabeled as a challenge',async()=>{
 let emails=0,clicks=0;
 const email={count:async()=>1,fill:async()=>emails++};
 const password={count:async()=>0,waitFor:async()=>{throw new Error('password missing')}};
 const page={url:()=> 'https://auth.services.adobe.com/login',goto:async()=>{},evaluate:async fn=>fn.name==='emailOTPInPage'?{kind:'none'}:null,locator:selector=>selector.includes('email')?email:password,
  getByRole:()=>({count:async()=>1,click:async()=>clicks++})};
 await assert.rejects(signInExistingAccount(page,{email:'fixture@example.test',password:'fixture-password'}),e=>e.code==='refresh_password_form_unavailable');
 assert.equal(emails,1);assert.equal(clicks,1);
});


function runtimeIdentityHarness(initial={}) {
 const sources={};
 for(const key of ['userProfile','accessToken','isSignedIn']){
  const listeners=new Set();let value=initial[key];
  sources[key]={observe(callback){listeners.add(callback);if(value!==undefined)callback(value);return()=>listeners.delete(callback)},
   emit(next){value=next;for(const callback of [...listeners])callback(value)},get active(){return listeners.size}};
 }
 const authentication=Object.fromEntries(Object.entries(sources).map(([key,source])=>[key,source.observe]));
 let current=authentication;const events=new Map();
 const globals={setTimeout,clearTimeout,navigator:{userAgent:'fixture-agent'},
  window:{adobeIMS:{getProfile:()=>{throw new Error('SDK profile unavailable')},isSignedInUser:()=>true,getAccessToken:()=>({token:'sdk-token'})}},
  document:{querySelector:()=>current?{service:{authentication:current}}:null,querySelectorAll:()=>[]},
  addEventListener(name,listener){events.set(name,listener)},removeEventListener(name){events.delete(name)}};
 const context=vm.createContext(globals);
 return {sources,authentication,globals,get active(){return Object.values(sources).reduce((n,source)=>n+source.active,0)},get listeners(){return events.size},
  replace(next){current=next},pagehide(){events.get('pagehide')?.()},
  evaluate:timeoutMs=>{globals.options={timeoutMs};return vm.runInContext(`(${identityInPage.toString()})(options)`,context)}};
}
const runtimeProfile={email:'fixture@example.test',userId:'fixture-account',emailVerified:true};

test('official runtime identity remains usable when the older IMS profile API rejects',async()=>{
 const h=runtimeIdentityHarness({userProfile:runtimeProfile,accessToken:'runtime-token',isSignedIn:true});
 const identity=await h.evaluate(50);
 assert.equal(identity.email,runtimeProfile.email);assert.equal(identity.account_id,runtimeProfile.userId);
 assert.equal(identity.token,'runtime-token');assert.equal(identity.user_agent,'fixture-agent');
 assert.equal(h.active,0);assert.equal(h.listeners,0);
});
test('runtime identity waits for all three observables and releases asynchronous subscriptions',async()=>{
 const h=runtimeIdentityHarness(),pending=h.evaluate(100);
 assert.equal(h.active,3);h.sources.userProfile.emit(runtimeProfile);h.sources.accessToken.emit('runtime-token');
 assert.equal(h.active,3);h.sources.isSignedIn.emit(true);
 assert.equal((await pending).account_id,runtimeProfile.userId);assert.equal(h.active,0);assert.equal(h.listeners,0);
});
test('a signed-out or incomplete runtime never borrows an IMS token to claim signed-in identity',async()=>{
 for(const initial of [{userProfile:runtimeProfile,accessToken:'token',isSignedIn:false},{userProfile:runtimeProfile,isSignedIn:true}]){
  const h=runtimeIdentityHarness(initial);h.globals.window.adobeIMS.getProfile=()=>runtimeProfile;
  assert.equal(await h.evaluate(10),null);assert.equal(h.active,0);assert.equal(h.listeners,0);
 }
});
test('runtime replacement and page navigation cannot combine credentials from different instances',async()=>{
 const replaced=runtimeIdentityHarness({userProfile:runtimeProfile,accessToken:'old-token'}),pending=replaced.evaluate(100);
 replaced.replace({});replaced.sources.isSignedIn.emit(true);
 assert.equal(await pending,null);assert.equal(replaced.active,0);assert.equal(replaced.listeners,0);
 const navigated=runtimeIdentityHarness(),navigating=navigated.evaluate(100);navigated.pagehide();
 assert.equal(await navigating,null);assert.equal(navigated.active,0);assert.equal(navigated.listeners,0);
});
test('a changed identity observable contract is refused and earlier subscriptions are cleaned',async()=>{
 const h=runtimeIdentityHarness();h.replace({userProfile:h.sources.userProfile.observe,accessToken:callback=>{callback('token');return{}},isSignedIn:h.sources.isSignedIn.observe});
 assert.equal(await h.evaluate(50),null);assert.equal(h.active,0);assert.equal(h.listeners,0);
});
test('pages without the Firefly service retain the normal IMS identity reader',async()=>{
 const h=runtimeIdentityHarness();h.replace(null);h.globals.window.adobeIMS.getProfile=()=>runtimeProfile;
 const identity=await h.evaluate(50);assert.equal(identity.account_id,runtimeProfile.userId);assert.equal(identity.token,'sdk-token');assert.equal(h.active,0);
 h.globals.window.adobeIMS.getProfile=()=>{throw new Error('not ready')};assert.equal(await h.evaluate(50),null);
});


test('verification after one password submission is identified without resubmitting credentials',async()=>{
 const actions=[];
 const email={count:async()=>1,fill:async()=>actions.push('email')};
 const password={count:async()=>1,waitFor:async()=>{},fill:async()=>actions.push('password')};
 const page={url:()=> 'https://auth.services.adobe.com/login',goto:async()=>{},evaluate:async fn=>fn.name==='emailOTPInPage'?{kind:'none'}:null,
  locator:selector=>selector.includes('one-time-code')?{count:async()=>1}:selector.includes('email')?email:password,
  getByRole:()=>({count:async()=>1,click:async()=>actions.push('continue')}),
  waitForURL:async()=>{throw new Error('OTP or challenge remains')}};
 await assert.rejects(signInExistingAccount(page,{email:'fixture@example.test',password:'fixture-password'}),e=>e.code==='refresh_verification_required');
 assert.deepEqual(actions,['email','continue','password','continue']);
});


test('refresh augments incomplete media diagnostics without discarding the observed runtime state',async()=>{
 const expected={account_id:'expected',email:'fixture@example.test'},page={goto:async()=>{},url:()=> 'https://firefly.adobe.com/'};
 const context={pages:()=>[page],cookies:async()=>[]};
 await assert.rejects(refreshSession(context,expected,{waitIdentity:async()=>({...expected,token:'fixture-token'}),ensureMedia:async()=>{
  const error=new RegistrationError('media_session_incomplete');error.diagnostics={media_runtime:{state:'empty',value_kind:'null'}};throw error;
 }}),error=>{
  assert.deepEqual(error.diagnostics,{media_runtime:{state:'empty',value_kind:'null'},host:'firefly.adobe.com',sherlock_cookies:[]});return true;
 });
});


test('signed-in SPL identity is read from the same authentication instance inside shadow roots',async()=>{
 const h=runtimeIdentityHarness({userProfile:runtimeProfile,accessToken:'spl-token',isSignedIn:true});
 const host={service:{serviceName:'spl-commerce-dialog-service',authentication:h.authentication}};
 const shadowRoot={querySelectorAll:selector=>selector==='*'?[host]:[host]};
 h.globals.document={querySelector:()=>null,querySelectorAll:selector=>selector==='*'?[{shadowRoot}]:[]};
 const identity=await h.evaluate(50);assert.equal(identity.account_id,runtimeProfile.userId);assert.equal(identity.token,'spl-token');assert.equal(h.active,0);assert.equal(h.listeners,0);
});
test('refresh cannot reuse an old omitted record as a newly collected media session',async()=>{
 const expected={account_id:'expected',email:'fixture@example.test'};let checks=0;
 const page={goto:async()=>{},url:()=> 'https://firefly.adobe.com/generate/image'},context={pages:()=>[page],cookies:async()=>[]};
 await assert.rejects(refreshSession(context,expected,{waitIdentity:async()=>{checks++;return {...expected,token:'fixture-token'}},ensureMedia:async()=>capturedMedia('','omitted')}),e=>e.code==='media_session_incomplete');
 assert.equal(checks,2);
});
test('a runtime change after final identity verification does not return replacement credentials',async()=>{
 const expected={account_id:'expected',email:'fixture@example.test'},events=[];
 const page={goto:async()=>{},url:()=> 'https://firefly.adobe.com/generate/image',evaluate:async()=>{events.push('verify');return false}};
 const context={pages:()=>[page],cookies:async()=>assert.fail('changed session must not be exported')};
 await assert.rejects(refreshSession(context,expected,{waitIdentity:async()=>{events.push('identity');return {...expected,token:'fixture-token'}},ensureMedia:async()=>{events.push('media');return capturedMedia()}}),e=>e.code==='media_session_incomplete'&&e.diagnostics.media_runtime.state==='changed');
 assert.deepEqual(events,['identity','media','identity','verify']);
});
test('refresh rejects a false omission claim carrying an old cookie or an unproven source',async()=>{
 const expected={account_id:'expected',email:'fixture@example.test'},page={goto:async()=>{},url:()=> 'https://firefly.adobe.com/'};
 for(const media of [{...capturedMedia('','omitted'),media_session_arp_source:'home_runtime'},capturedMedia('sherlockToken=stale','omitted')]){
  await assert.rejects(refreshSession({pages:()=>[page]},expected,{waitIdentity:async()=>({...expected,token:'fixture-token'}),ensureMedia:async()=>media}),e=>e.code==='media_session_incomplete');
 }
});


test('an editor with no current SPL service cannot borrow an IMS or hidden homepage identity',async()=>{
 const h=runtimeIdentityHarness({userProfile:runtimeProfile,accessToken:'home-token',isSignedIn:true});
 h.globals.location={origin:'https://firefly.adobe.com',pathname:'/generate/image'};
 h.globals.window.adobeIMS.getProfile=()=>runtimeProfile;
 assert.equal(await h.evaluate(20),null);assert.equal(h.active,0);
});
