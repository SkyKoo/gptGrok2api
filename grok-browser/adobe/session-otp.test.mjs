import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { emailOTPInPage, inspectEmailOTP } from './session-otp.mjs';
import { refreshSession } from './session-refresh.mjs';

function classify({text='Enter the verification code sent to your email',count=1,length=6,challenge=false,errors='',disabled=false,autocomplete='one-time-code',type='text'}={}) {
  const visible={getBoundingClientRect:()=>({width:100,height:20})};
  const words={...visible,innerText:text},error={...visible,innerText:errors};
  const container={querySelectorAll:selector=>selector.startsWith('h1')?[words]:errors?[error]:[]};
  const inputs=Array.from({length:count},(_,index)=>({...visible,type,id:'otp-'+index,name:'code',autocomplete,maxLength:length,disabled,readOnly:false,
    closest:()=>container,getAttribute:name=>name==='inputmode'?'numeric':null}));
  const frame={...visible,title:'Challenge',getAttribute:()=> 'https://arks-client.adobe.com/'};
  const context=vm.createContext({document:{body:container,querySelectorAll:selector=>selector==='input'?inputs:challenge?[frame]:[]},getComputedStyle:()=>({display:'block',visibility:'visible'}),email:'fixture@example.test'});
  return JSON.parse(JSON.stringify(vm.runInContext(`(${emailOTPInPage.toString()})({email})`,context)));
}

test('email OTP classifier requires email delivery evidence, exactly six digits, and no other verification factor',()=>{
  assert.equal(classify().kind,'email_otp');
  assert.equal(classify({type:'password'}).kind,'email_otp');
  assert.equal(classify({count:6,length:1}).fields.length,6);
  assert.equal(classify({text:'Enter the code sent to fixture@example.test'}).kind,'email_otp');
  for(const text of ['Enter your authenticator code','SMS code sent to your phone','Enter a recovery code','Enter verification code'])assert.equal(classify({text}).kind,'unknown');
  assert.equal(classify({challenge:true}).kind,'challenge');
  assert.equal(classify({text:'Verify your email. Start puzzle'}).kind,'challenge');
  assert.equal(classify({count:5,length:1}).kind,'unknown');
  assert.equal(classify({length:4}).kind,'unknown');
  assert.equal(classify({disabled:true}).kind,'none');
  assert.equal(classify({errors:'Your verification code has expired'}).kind,'expired');
  assert.equal(classify({errors:'Incorrect verification code'}).kind,'invalid');
  assert.doesNotMatch(JSON.stringify(classify({text:'Enter the code sent to fixture@example.test'})),/fixture@example/);
});

function loginFixture({otpAfter='password',digits=1,afterOTP='',firstKind='email_otp',secondOTP=false,mediaMatches=true,changeIdentity=false,maskedOTP=false,otpInspectionFailure}={}) {
  const expected={email:'fixture@example.test',account_id:'fixture-id',profile_id:'fixture-profile',password:'fixture-password'};
  const actions=[],filled=[];
  let phase='initial',time=1700000000000,authenticated=false,mediaCalls=0,overrideURL='',changedForm=false,closed=false;
  const fields=Array.from({length:digits},(_,index)=>({index,type:maskedOTP?'password':'text',id:'otp-'+index,name:'code',autocomplete:'one-time-code',maxlength:digits===1?6:1}));
  const identity=()=>authenticated?{...expected,account_id:changeIdentity&&mediaCalls?'different-id':expected.account_id,token:'fixture-token'}:null;
  const goAfterOTP=()=>{
    phase=afterOTP||(otpAfter==='email'?'password':'signed_in');
    if(phase==='signed_in')authenticated=true;
  };
  const password={count:async()=>phase==='password'||maskedOTP&&phase==='otp'?1:0,waitFor:async()=>{if(phase!=='password'&&!(maskedOTP&&phase==='otp'))throw new Error('password not visible')},fill:async value=>{assert.equal(phase,'password');assert.equal(value,expected.password);actions.push('password-fill')}};
  const email={count:async()=>phase==='email'?1:0,fill:async value=>{assert.equal(value,expected.email);actions.push('email-fill')}};
  const page={
    url:()=>overrideURL||(phase==='otp'||phase==='invalid'||phase==='expired'||phase==='challenge'?'https://auth.services.adobe.com/verify-email':authenticated?'https://account.adobe.com/':'https://auth.services.adobe.com/login'),
    goto:async url=>{actions.push('goto:'+new URL(url).hostname);if(new URL(url).hostname==='account.adobe.com'&&!authenticated)phase='email'},
    evaluate:async(fn,args)=>{
      if(fn.name==='identityInPage')return identity();
      if(fn.name==='mediaSessionMatchesInPage')return mediaMatches;
      if(fn.name==='emailOTPInPage'){
        if(['invalid','expired','challenge'].includes(phase))return {kind:phase};
        if(phase==='otp'&&otpInspectionFailure){
          if(otpInspectionFailure==='throw')throw new Error('Execution context was destroyed');
          return otpInspectionFailure;
        }
        if(phase==='otp')return {kind:firstKind,fields:fields.map(field=>({...field,id:changedForm?'changed':field.id}))};
        return {kind:'none'};
      }
      assert.fail('unexpected page evaluation');
    },
    locator:selector=>{
      if(selector==='input:visible')return {nth:index=>({evaluate:async(_fn,descriptor)=>descriptor.id===fields[index].id,fill:async value=>{
        assert.equal(phase,'otp');filled.push(value);actions.push('otp-fill');if(filled.length===digits)goAfterOTP();
      }})};
      if(selector.includes('one-time-code'))return {count:async()=>phase==='otp'?digits:0};
      return selector.includes('email')?email:password;
    },
    getByRole:(_role,{name})=>({count:async()=>1,click:async()=>{
      assert.equal(name.test('Create account'),false);
      actions.push(phase+'-click');
      if(phase==='email')phase=otpAfter==='email'?'otp':'password';
      else if(phase==='password'){phase=otpAfter==='password'||secondOTP?'otp':'signed_in';if(phase==='signed_in')authenticated=true;}
      else assert.fail('unexpected or repeated credential submit');
    }}),
    waitForURL:async predicate=>{if(!authenticated||!predicate(new URL('https://account.adobe.com/')))throw new Error('verification still present')},
  };
  const context={pages:()=>closed?[]:[page],cookies:async url=>url.includes('adobeid')?[{name:'ims',value:'fixture'}]:[]};
  const options={workerMode:true,now:()=>time,pause:async ms=>{time+=ms},waitIdentity:async()=>identity(),ensureMedia:async()=>{
    mediaCalls++;return {cookie:'sherlockToken=fixture',media_session_arp_mode:'present',media_session_arp_source:'spl_runtime',runtime_state:{state:'ready'}};
  }};
  return {expected,context,actions,filled,options,run:code=>refreshSession(context,expected,{...options,...(code===undefined?{}:{code})}),
    expire:()=>{time+=600001},changePage:()=>{overrideURL='https://auth.services.adobe.com/other'},changeForm:()=>{changedForm=true},close:()=>{closed=true}};
}

test('email OTP after email or password resumes the same page once and verifies final identity plus media',async t=>{
  for(const otpAfter of ['email','password'])for(const digits of [1,6])await t.test(`${otpAfter}/${digits}`,async()=>{
    const f=loginFixture({otpAfter,digits});
    const pending=await f.run();
    assert.equal(pending.stage,'awaiting_verification');assert.equal(pending.code,'refresh_email_otp_required');
    assert.equal(pending.verification_requested_at,new Date(1700000000000).toISOString());
    const before=[...f.actions];assert.deepEqual(await f.run(),pending);assert.deepEqual(f.actions,before);
    const result=await f.run('123456');
    assert.equal(result.account_id,f.expected.account_id);assert.equal(result.media_session_refreshed,true);
    for(const kind of ['email','password','otp']){assert.equal(result.session_diagnostics[`login_${kind}_submission_started`],true);assert.equal(result.session_diagnostics[`login_${kind}_submitted`],true);}
    assert.equal(f.actions.filter(action=>action==='goto:account.adobe.com').length,1);
    assert.equal(f.actions.filter(action=>action==='email-click').length,1);assert.equal(f.actions.filter(action=>action==='password-click').length,1);
    assert.deepEqual(f.filled,digits===1?['123456']:['1','2','3','4','5','6']);
    await assert.rejects(f.run('654321'),error=>error.code==='refresh_otp_already_submitted');
    assert.equal(f.filled.length,digits);
  });
});

test('OTP expiry, stale form, closed context and mismatched account fail before entering a code',async t=>{
  for(const [mutation,code] of [['expire','refresh_otp_expired'],['changePage','refresh_otp_form_changed'],['changeForm','refresh_otp_form_changed'],['close','refresh_otp_form_changed']])await t.test(mutation,async()=>{
    const f=loginFixture();await f.run();f[mutation]();
    await assert.rejects(f.run('123456'),error=>error.code===code);assert.equal(f.filled.length,0);
  });
  const f=loginFixture();await f.run();
  await assert.rejects(refreshSession(f.context,{...f.expected,account_id:'different'}, {...f.options,code:'123456'}),error=>error.code==='refresh_account_mismatch');
  assert.equal(f.filled.length,0);
});

test('a masked email OTP input is never filled with the account password',async()=>{
  const f=loginFixture({otpAfter:'email',maskedOTP:true});
  assert.equal((await f.run()).code,'refresh_email_otp_required');
  assert.equal(f.actions.includes('password-fill'),false);
  assert.equal((await f.run('123456')).media_session_refreshed,true);
  assert.equal(f.actions.filter(action=>action==='password-fill').length,1);
  assert.deepEqual(f.filled,['123456']);
});

test('failed or malformed OTP inspection never treats a masked OTP as the password form',async()=>{
  for(const otpInspectionFailure of ['throw',{}, {kind:'email_otp'}, {kind:'email_otp',fields:[{type:'password'}]}]){
    const f=loginFixture({otpAfter:'email',maskedOTP:true,otpInspectionFailure});
    await assert.rejects(f.run(),error=>error.code==='refresh_verification_required');
    assert.equal(f.actions.includes('password-fill'),false);
    assert.equal(f.actions.includes('password-click'),false);
    assert.equal(f.filled.length,0);
    const before=[...f.actions];
    await assert.rejects(f.run(),error=>error.code==='refresh_session_ended');
    assert.deepEqual(f.actions,before);
  }
});

test('OTP inspection fails closed on invalid results and returns only validated descriptors',async()=>{
  const field={index:0,type:'password',id:'otp',name:'code',autocomplete:'one-time-code',maxlength:6};
  const inspect=result=>inspectEmailOTP({url:()=> 'https://auth.services.adobe.com/login',evaluate:async()=>result},{email:'fixture@example.test'});
  for(const result of [null,[],{kind:'unexpected'},{kind:'email_otp',fields:[]},
    {kind:'email_otp',fields:[{...field,index:-1}]},{kind:'email_otp',fields:[{...field,maxlength:4}]},
    {kind:'email_otp',fields:Array.from({length:6},()=>({...field,type:'text',maxlength:1}))}]){
    assert.deepEqual(await inspect(result),{kind:'unknown'});
  }
  assert.deepEqual(await inspect({kind:'none',raw:'PRIVATE'}),{kind:'none'});
  assert.deepEqual(await inspect({kind:'email_otp',fields:[{...field,value:'PRIVATE'}],raw:'PRIVATE'}),{kind:'email_otp',fields:[field]});
});

test('wrong or expired code and challenges after submission never retry input or collect media',async t=>{
  for(const [afterOTP,code] of [['invalid','refresh_otp_invalid'],['expired','refresh_otp_expired'],['challenge','refresh_challenge_required'],['otp','refresh_otp_result_unknown']])await t.test(afterOTP,async()=>{
    const f=loginFixture({afterOTP});await f.run();const before=[...f.actions];
    await assert.rejects(f.run('123456'),error=>error.code===code);
    await assert.rejects(f.run('123456'),error=>error.code==='refresh_otp_already_submitted');
    assert.deepEqual(f.actions,[...before,'otp-fill']);assert.equal(f.filled.length,1);
  });
});

test('CAPTCHA and unknown factors never return a mailbox OTP continuation',async()=>{
  for(const [firstKind,code] of [['challenge','refresh_challenge_required'],['unknown','refresh_verification_required']]){
    const f=loginFixture({firstKind});await assert.rejects(f.run(),error=>error.code===code);assert.equal(f.filled.length,0);
  }
});

test('OTP continuation cannot resend credentials or request a second code',async()=>{
  const early=loginFixture();await assert.rejects(early.run('123456'),error=>error.code==='refresh_otp_not_pending');assert.equal(early.actions.length,0);
  const invalid=loginFixture();await invalid.run();await assert.rejects(invalid.run('bad'),error=>error.code==='refresh_otp_invalid');assert.equal(invalid.filled.length,0);
  const again=loginFixture({otpAfter:'email',secondOTP:true});await again.run();
  await assert.rejects(again.run('123456'),error=>error.code==='refresh_otp_already_submitted');
  assert.equal(again.actions.filter(action=>action==='email-click').length,1);assert.equal(again.actions.filter(action=>action==='password-click').length,1);assert.equal(again.filled.length,1);
  const cancelled=loginFixture(),abort=new AbortController();await cancelled.run();abort.abort();
  await assert.rejects(refreshSession(cancelled.context,cancelled.expected,{...cancelled.options,signal:abort.signal,code:'123456'}),error=>error.code==='media_session_cancelled');
  assert.equal(cancelled.filled.length,0);
});

test('completed OTP still requires current account identity and media-session binding',async()=>{
  for(const [options,code] of [[{mediaMatches:false},'media_session_incomplete'],[{changeIdentity:true},'refresh_account_mismatch']]){
    const f=loginFixture(options);await f.run();await assert.rejects(f.run('123456'),error=>error.code===code);
    assert.equal(f.filled.length,1);
  }
});
