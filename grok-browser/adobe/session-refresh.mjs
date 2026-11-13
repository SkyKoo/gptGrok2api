import { isTrustedAdobeURL, RegistrationError } from './core.mjs';
import { ensureMediaSession, isCompleteMediaSession, verifyMediaSession } from './media-session.mjs';
import { fillEmailOTP, inspectEmailOTP, verificationFailure } from './session-otp.mjs';

const refreshStates = new WeakMap();
const OTP_LIFETIME_MS = 10 * 60 * 1000;
const pendingOTP = state => ({stage:'awaiting_verification',code:'refresh_email_otp_required',verification_requested_at:state.otpRequestedAt});
const loginEvidence = state => Object.fromEntries(['email','password','otp'].flatMap(kind => [
  [`login_${kind}_submission_started`,state[`${kind}SubmissionStarted`] === true],
  [`login_${kind}_submitted`,state[`${kind}Submitted`] === true],
]));

async function loginVerification(page, expected, state, now, fallback) {
  const observed = await inspectEmailOTP(page, expected);
  if (observed.kind === 'email_otp') {
    if (state.otpSubmissionStarted) throw new RegistrationError('refresh_otp_already_submitted');
    if (!state.lastSubmissionAt) throw new RegistrationError('refresh_otp_not_pending');
    state.otpRequestedAt = state.lastSubmissionAt;
    state.otpDeadline = Date.parse(state.otpRequestedAt) + OTP_LIFETIME_MS;
    if (now() >= state.otpDeadline) throw new RegistrationError('refresh_otp_expired');
    state.otpURL = page.url();
    state.otpFields = JSON.stringify(observed.fields);
    return pendingOTP(state);
  }
  if (observed.kind !== 'none') throw verificationFailure(observed.kind);
  // A code control alone cannot identify the factor. Preserve the old fixed
  // unknown-verification failure instead of guessing that it is email.
  const controls = page.locator('input[autocomplete="one-time-code"]:visible, iframe[title*="captcha" i]:visible, iframe[title*="challenge" i]:visible');
  throw new RegistrationError(await controls.count() > 0 ? 'refresh_verification_required' : fallback);
}

// Firefly's current authentication service exposes Observe<T> values. Read the
// profile, access token and signed-in flag from one instance; the IMS SDK's
// getProfile() can reject even while that official service is already signed in.
export async function identityInPage({timeoutMs=750}={}) {
  const toIdentity=(profile,token)=>{
    if(typeof profile?.email!=='string'||!profile.email||typeof profile?.userId!=='string'||!profile.userId||typeof token!=='string'||!token)return null;
    return {email:profile.email,account_id:profile.userId,token,emailVerified:typeof profile.emailVerified==='boolean'?profile.emailVerified:null,user_agent:typeof navigator==='object'?navigator.userAgent:''};
  };
  const isSPLEditor=typeof location==='object'&&location.origin==='https://firefly.adobe.com'&&location.pathname==='/generate/image';
  const findAuthentication=()=>{
    if(typeof document==='undefined')return null;
    const home=!isSPLEditor&&document.querySelector('firefly-home-app')?.service?.authentication;
    if(home)return home;
    const roots=[document];
    for(const root of roots){
      for(const host of root.querySelectorAll('firefly-commerce-dialogs, firefly-commerce-paywall-dialog')){
        if(host.service?.serviceName==='spl-commerce-dialog-service'&&host.service.authentication)return host.service.authentication;
      }
      for(const element of root.querySelectorAll('*'))if(element.shadowRoot)roots.push(element.shadowRoot);
    }
    return null;
  };
  const authentication=findAuthentication();
  if(authentication && ['userProfile','accessToken','isSignedIn'].every(key=>typeof authentication[key]==='function')){
    return new Promise(resolve=>{
      const values={},stops=[];
      let ready=false,finished=false,timer;
      const finish=value=>{
        if(finished)return;
        finished=true;clearTimeout(timer);
        globalThis.removeEventListener('pagehide',onPageHide);
        for(const stop of stops){try{stop();}catch{ /* Do not expose SDK internals. */ }}
        resolve(value);
      };
      const onPageHide=()=>finish(null);
      const inspect=()=>{
        if(!ready||finished||values.isSignedIn!==true)return;
        if(findAuthentication()!==authentication)return finish(null);
        const identity=toIdentity(values.userProfile,values.accessToken);
        if(identity)finish(identity);
      };
      globalThis.addEventListener('pagehide',onPageHide,{once:true});
      timer=setTimeout(()=>finish(null),timeoutMs);
      try{
        for(const key of ['userProfile','accessToken','isSignedIn']){
          const stop=authentication[key](value=>{values[key]=value;inspect();});
          if(typeof stop!=='function'){finish(null);return;}
          stops.push(stop);
        }
        ready=true;inspect();
      }catch{finish(null);}
    });
  }
  // Post-media verification must observe the same editor authentication, not
  // an unrelated/stale IMS identity if that service disappears.
  if(isSPLEditor)return null;
  try{
    const ims=window.adobeIMS;
    if(!ims?.isSignedInUser||!ims?.getProfile||!ims?.getAccessToken||!await ims.isSignedInUser())return null;
    const profile=await ims.getProfile(),credential=await ims.getAccessToken();
    return toIdentity(profile,credential?.token);
  }catch{return null;}
}

async function readIdentity(page) { return page.evaluate(identityInPage); }

export async function waitForIdentity(page, {timeoutMs=45000, now=Date.now, pause=ms=>new Promise(resolve=>setTimeout(resolve,ms))}={}) {
  const deadline=now()+timeoutMs;
  while(now()<deadline){
    try {const identity=await readIdentity(page);if(identity)return identity;}
    catch { /* Observe transient SDK initialization/navigation without a second action. */ }
    await pause(1000);
  }
  return null;
}

export function validateIdentity(identity, expected) {
  if (!identity) throw new RegistrationError('refresh_login_required');
  if (identity.account_id !== expected.account_id) throw new RegistrationError('refresh_account_mismatch');
  if (identity.email?.toLowerCase() !== expected.email.toLowerCase()) throw new RegistrationError('refresh_email_mismatch');
  if (!identity.token) throw new RegistrationError('refresh_token_missing');
  if (identity.emailVerified === false) throw new RegistrationError('refresh_email_unverified');
  return identity;
}

// Existing-account login only: no Create Account control or challenge action is
// ever used. Only a recognized email OTP can pause and continue this login.
export async function signInExistingAccount(page, expected, {landingTimeoutMs=15000, now=Date.now, pause=ms=>new Promise(resolve=>setTimeout(resolve,ms)),signal,loginState={},resumeAfterOTP=false}={}) {
  if(!expected.password)throw new RegistrationError('refresh_login_required');
  const checkCancelled=()=>{if(signal?.aborted)throw new RegistrationError('media_session_cancelled');};
  const alreadySignedIn=async()=>{
    let identity=null;
    try {identity=await readIdentity(page);} catch { /* The account shell may not expose IMS yet. */ }
    if(identity){validateIdentity(identity,expected);return true;}
    return false;
  };
  const submitPassword=async()=>{
    checkCancelled();
    const verification=await inspectEmailOTP(page,expected);
    if(verification.kind!=='none')return loginVerification(page,expected,loginState,now,'refresh_login_form_unrecognized');
    if(loginState.passwordSubmissionStarted)throw new RegistrationError('refresh_login_result_unknown');
    const password=page.locator('input[type="password"]:visible');
    if(await password.count()!==1)throw new RegistrationError('refresh_login_form_unrecognized');
    await password.fill(expected.password);
    const button=page.getByRole('button',{name:/^(Continue|Sign in|继续|登录)$/i});
    if(await button.count()!==1)throw new RegistrationError('refresh_login_form_unrecognized');
    loginState.passwordSubmissionStarted=true;loginState.lastSubmissionAt=new Date(now()).toISOString();
    try {await button.click({timeout:15000});loginState.passwordSubmitted=true;} catch { /* Observe once; never repeat the click. */ }
    try {await page.waitForURL(url=>url.hostname==='account.adobe.com'||url.hostname==='firefly.adobe.com',{timeout:30000});}
    catch {return loginVerification(page,expected,loginState,now,'refresh_login_completion_unconfirmed');}
  };
  if(resumeAfterOTP)return submitPassword();
  checkCancelled();
  await page.goto('https://account.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
  if(await alreadySignedIn())return;
  const email=page.locator('input[type="email"]:visible, input[name="username"]:visible');
  const password=page.locator('input[type="password"]:visible');
  const signInButtons=page.getByRole('button',{name:/^(Sign in|登录)$/i});
  const signInLinks=page.getByRole('link',{name:/^(Sign in|登录)$/i});
  const deadline=now()+landingTimeoutMs;
  let hasForm=false, entry=null;
  while(now()<deadline){
    checkCancelled();
    hasForm=await email.count()>0 || await password.count()>0;
    if(hasForm)break;
    const buttons=await signInButtons.count(),links=await signInLinks.count();
    if(buttons+links>1)throw new RegistrationError('refresh_login_entry_unrecognized');
    if(buttons+links===1){entry=buttons===1?signInButtons:signInLinks;break;}
    await pause(Math.min(250,Math.max(1,deadline-now())));
  }
  if(!hasForm){
    if(await alreadySignedIn())return;
    if(!entry)throw new RegistrationError('refresh_login_entry_unrecognized');
    checkCancelled();
    await entry.click({timeout:15000});
    // The account homepage is a shell: its explicit Sign in action opens the
    // normal email/password form. Do not mistake that shell for a challenge.
    const forms=page.locator('input[type="email"]:visible, input[name="username"]:visible, input[type="password"]:visible');
    try {await forms.first().waitFor({state:'visible',timeout:20000});}
    catch {
      if(await alreadySignedIn())return;
      throw new RegistrationError('refresh_login_form_unavailable');
    }
  }
  const proceed=()=>page.getByRole('button',{name:/^(Continue|Sign in|继续|登录)$/i});
  checkCancelled();
  if(await email.count()===1){
    if(loginState.emailSubmissionStarted)throw new RegistrationError('refresh_login_result_unknown');
    await email.fill(expected.email);
    const button=proceed();if(await button.count()!==1)throw new RegistrationError('refresh_login_form_unrecognized');
    loginState.emailSubmissionStarted=true;loginState.lastSubmissionAt=new Date(now()).toISOString();
    try {await button.click({timeout:15000});loginState.emailSubmitted=true;} catch { /* Observe once; never repeat the click. */ }
  }
  try {await password.waitFor({state:'visible',timeout:20000});}
  catch {
    if(await alreadySignedIn())return;
    return loginVerification(page,expected,loginState,now,'refresh_password_form_unavailable');
  }
  return submitPassword();
}

// Only the queue-owned persistent context may discard restored tabs. The
// caller explicitly opts in; no other profile or user browser is discovered.
export async function prepareWorkerRefreshPage(context, {signal, closeTimeoutMs=5000}={}) {
  const checkCancelled=()=>{if(signal?.aborted)throw new RegistrationError('media_session_cancelled');};
  checkCancelled();
  const restored=context.pages(),page=restored[0]||await context.newPage();
  let timer;
  try {
    const closed=await Promise.race([
      Promise.allSettled(restored.slice(1).map(other=>Promise.resolve().then(()=>other.close({runBeforeUnload:false})))),
      new Promise((_,reject)=>{timer=setTimeout(()=>reject(new RegistrationError('refresh_page_cleanup_failed')),closeTimeoutMs);}),
    ]);
    checkCancelled();
    if(closed.some(result=>result.status==='rejected'))throw new RegistrationError('refresh_page_cleanup_failed');
    const remaining=context.pages();
    if(remaining.length!==1||remaining[0]!==page)throw new RegistrationError('refresh_page_cleanup_failed');
    return page;
  } catch(error) {
    checkCancelled();
    if(error instanceof RegistrationError)throw error;
    throw new RegistrationError('refresh_page_cleanup_failed');
  } finally {clearTimeout(timer);}
}

async function observeOTPCompletion(page, expected, state, {signal, now, pause}) {
  const deadline = now() + 20000;
  while (now() < deadline) {
    if(signal?.aborted)throw new RegistrationError('media_session_cancelled');
    if (!isTrustedAdobeURL(page.url())) throw new RegistrationError('refresh_otp_form_changed');
    const observed = await inspectEmailOTP(page, expected);
    if (['challenge','expired','invalid'].includes(observed.kind)) throw verificationFailure(observed.kind);
    if (observed.kind === 'none') {
      const host = new URL(page.url()).hostname;
      if (['account.adobe.com','firefly.adobe.com'].includes(host)) return 'signed_in';
      if (!state.passwordSubmissionStarted && await page.locator('input[type="password"]:visible').count() === 1) return 'password';
    }
    await pause(Math.min(250, Math.max(1, deadline - now())));
  }
  throw new RegistrationError('refresh_otp_result_unknown');
}

export async function refreshSession(context, expected, {waitIdentity=waitForIdentity,ensureMedia=ensureMediaSession,workerMode=false,signal,code='',now=Date.now,pause=ms=>new Promise(resolve=>setTimeout(resolve,ms))}={}) {
  if(signal?.aborted)throw new RegistrationError('media_session_cancelled');
  const binding = JSON.stringify([expected.account_id,expected.email?.trim().toLowerCase(),expected.profile_id]);
  let state = refreshStates.get(context);
  if (state && state.binding !== binding) throw new RegistrationError('refresh_account_mismatch');
  if (state && state.phase !== 'pending') throw new RegistrationError(state.otpSubmissionStarted ? 'refresh_otp_already_submitted' : 'refresh_session_ended');
  if (!state && code !== '') throw new RegistrationError('refresh_otp_not_pending');
  if (!state) {
    state = {binding,phase:'running'};
    refreshStates.set(context,state);
  }
  try {
    let page = state.page, identity;
    if (state.phase === 'pending') {
      if (now() >= state.otpDeadline) throw new RegistrationError('refresh_otp_expired');
      if (code === '') return pendingOTP(state);
      if (typeof code !== 'string' || !/^\d{6}$/.test(code)) throw new RegistrationError('refresh_otp_invalid');
      if (!page || !context.pages().includes(page) || page.url() !== state.otpURL) throw new RegistrationError('refresh_otp_form_changed');
      const observed = await inspectEmailOTP(page,expected);
      if (observed.kind !== 'email_otp') throw verificationFailure(observed.kind,'refresh_otp_form_changed');
      if (JSON.stringify(observed.fields) !== state.otpFields) throw new RegistrationError('refresh_otp_form_changed');
      state.phase = 'running';
      state.otpSubmissionStarted = true;
      try {await fillEmailOTP(page,code,observed);state.otpSubmitted=true;}
      catch (error) {if(error instanceof RegistrationError)throw error; /* Navigation can interrupt fill; observe without replay. */ }
      const next = await observeOTPCompletion(page,expected,state,{signal,now,pause});
      if (next === 'password') {
        const login = await signInExistingAccount(page,expected,{signal,now,pause,loginState:state,resumeAfterOTP:true});
        if (login?.stage === 'awaiting_verification') throw new RegistrationError('refresh_otp_already_submitted');
      }
      await page.goto('https://firefly.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
      identity = await waitIdentity(page);
    } else {
      page = state.page = workerMode ? await prepareWorkerRefreshPage(context,{signal}) : context.pages()[0] || await context.newPage();
      await page.goto('https://firefly.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
      identity = await waitIdentity(page,{timeoutMs:expected.password?15000:45000});
      if(!identity&&expected.password){
        const login = await signInExistingAccount(page,expected,{signal,now,pause,loginState:state});
        if (login?.stage === 'awaiting_verification') {state.phase='pending';return login;}
        await page.goto('https://firefly.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
        identity=await waitIdentity(page);
      }
    }
    if(!identity){
      const error=new RegistrationError('refresh_login_required');
      error.diagnostics=await sessionDiagnostics(page,context);
      throw error;
    }
    const account = await collectRefreshedSession(context,page,expected,identity,{waitIdentity,ensureMedia,signal,state});
    state.phase='completed';
    return account;
  } catch(error) {
    state.phase='failed';
    throw error;
  }
}

async function collectRefreshedSession(context,page,expected,identity,{waitIdentity,ensureMedia,signal,state}) {
  validateIdentity(identity,expected);
  // A successful IMS refresh alone cannot claim that generation credentials
  // were renewed. Let incomplete/cancelled runtime ARP fail without exporting
  // a partial replacement for the previously stored account.
  let media;
  try {media=await ensureMedia(page,context,{signal,identity});}
  catch(error){
    if(error instanceof RegistrationError && error.code!=='media_session_cancelled'){
      try {error.diagnostics={...error.diagnostics,...await sessionDiagnostics(page,context)};} catch { /* Preserve the original failure. */ }
    }
    throw error;
  }
  if(!isCompleteMediaSession(media))throw new RegistrationError('media_session_incomplete');
  // Media initialization navigates to SPL. Re-check that current runtime identity
  // before pairing it with the observed nonempty ARP value.
  identity=validateIdentity(await waitIdentity(page,{timeoutMs:10000}),expected);
  await verifyMediaSession(page,media,identity,{signal});
  if(signal?.aborted)throw new RegistrationError('media_session_cancelled');
  const diagnostics={...await sessionDiagnostics(page,context),media_runtime:media.runtime_state,...loginEvidence(state)};
  const cookieString=list=>list.map(c=>`${c.name}=${c.value}`).join('; ');
  return {...identity,profile_id:expected.profile_id,media_session_refreshed:true,media_session_status:'complete',session_diagnostics:diagnostics,
    cookie:media.cookie,media_session_arp_mode:media.media_session_arp_mode,media_session_arp_source:media.media_session_arp_source,
    refresh_cookie:cookieString(await context.cookies('https://adobeid-na1.services.adobe.com/ims/check/v6/token')),
  };
}


async function sessionDiagnostics(page,context){
 const cookies=await context.cookies('https://firefly.adobe.com/');
 const scopes=cookies.filter(c=>c.name==='sherlockToken').map(c=>({domain:c.domain,path:c.path,present:Boolean(c.value)}));
 let host='';try{host=new URL(page.url()).hostname;}catch{}
 return {host,sherlock_cookies:scopes};
}
