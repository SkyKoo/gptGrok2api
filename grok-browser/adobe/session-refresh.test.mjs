import test from 'node:test';
import { RegistrationError } from './core.mjs';
import assert from 'node:assert/strict';
import {waitForIdentity,validateIdentity,signInExistingAccount,refreshSession} from './session-refresh.mjs';

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
 const page={goto:async()=>actions.push('navigate-account'),locator:s=>s.includes('email')?email:password,
  getByRole:(role,{name})=>{assert.equal(name.test('Create an account'),false);return {count:async()=>1,click:async()=>actions.push('continue')}}};
 await assert.rejects(signInExistingAccount(page,{email:'fixture@example.test',password:'fixture-password'}),e=>e.code==='refresh_verification_required');
 assert.deepEqual(actions,['navigate-account','email','continue']);
});
test('existing login submits email and password once without account creation',async()=>{
 const actions=[];
 const email={first(){return this},waitFor:async()=>{},count:async()=>1,fill:async()=>actions.push('email')};
 const password={waitFor:async()=>{},count:async()=>1,fill:async()=>actions.push('password')};
 const page={goto:async()=>actions.push('navigate-account'),locator:s=>s.includes('email')?email:password,
  getByRole:(role,{name})=>{assert.equal(name.test('Create account'),false);return {count:async()=>1,click:async()=>actions.push('continue')}},
  waitForURL:async predicate=>{assert.equal(predicate(new URL('https://account.adobe.com/')),true);actions.push('observe-account')}};
 await signInExistingAccount(page,{email:'fixture@example.test',password:'fixture-password'});
 assert.deepEqual(actions,['navigate-account','email','continue','password','continue','observe-account']);
});

test('token refresh preserves previous media cookie when new session is incomplete',async()=>{
 const expected={account_id:'expected',email:'fixture@example.test',profile_id:'fixture'};
 const page={goto:async()=>{},url:()=> 'https://firefly.adobe.com/'};
 const context={pages:()=>[page],cookies:async url=>url.includes('adobeid')?[{name:'ims',value:'fixture'}]:[]};
 const result=await refreshSession(context,expected,{waitIdentity:async()=>({...expected,token:'fixture-token'}),ensureMedia:async()=>{throw new RegistrationError('media_session_incomplete')}});
 assert.equal(result.token,'fixture-token');assert.equal(result.cookie,'');assert.equal(result.refresh_cookie,'ims=fixture');assert.equal(result.media_session_refreshed,false);
});
