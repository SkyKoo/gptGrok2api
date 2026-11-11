import { RegistrationError } from './core.mjs';
import { ensureMediaSession, isCompleteMediaSession } from './media-session.mjs';

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
// ever used. Each form is submitted at most once and MFA stops the fallback.
export async function signInExistingAccount(page, expected, {landingTimeoutMs=15000, now=Date.now, pause=ms=>new Promise(resolve=>setTimeout(resolve,ms)),signal}={}) {
  if(!expected.password)throw new RegistrationError('refresh_login_required');
  const checkCancelled=()=>{if(signal?.aborted)throw new RegistrationError('media_session_cancelled');};
  const alreadySignedIn=async()=>{
    let identity=null;
    try {identity=await readIdentity(page);} catch { /* The account shell may not expose IMS yet. */ }
    if(identity){validateIdentity(identity,expected);return true;}
    return false;
  };
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
    await email.fill(expected.email);
    const button=proceed();if(await button.count()!==1)throw new RegistrationError('refresh_login_form_unrecognized');
    await button.click({timeout:15000});
  }
  try {await password.waitFor({state:'visible',timeout:20000});}
  catch {
    if(await alreadySignedIn())return;
    const verification=page.locator('input[autocomplete="one-time-code"]:visible, iframe[title*="captcha" i]:visible, iframe[title*="challenge" i]:visible');
    throw new RegistrationError(await verification.count()>0?'refresh_verification_required':'refresh_password_form_unavailable');
  }
  checkCancelled();
  if(await password.count()!==1)throw new RegistrationError('refresh_login_form_unrecognized');
  await password.fill(expected.password);
  const button=proceed();if(await button.count()!==1)throw new RegistrationError('refresh_login_form_unrecognized');
  await button.click({timeout:15000});
  // Observe completion before navigating; a challenge/OTP stays on Adobe auth.
  try {await page.waitForURL(url=>url.hostname==='account.adobe.com',{timeout:30000});}
  catch {
    const verification=page.locator('input[autocomplete="one-time-code"]:visible, iframe[title*="captcha" i]:visible, iframe[title*="challenge" i]:visible');
    throw new RegistrationError(await verification.count()>0?'refresh_verification_required':'refresh_login_completion_unconfirmed');
  }
}

export async function refreshSession(context, expected, {waitIdentity=waitForIdentity,ensureMedia=ensureMediaSession,signal}={}) {
  if(signal?.aborted)throw new RegistrationError('media_session_cancelled');
  const page=context.pages()[0]||await context.newPage();
  await page.goto('https://firefly.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
  let identity=await waitIdentity(page,{timeoutMs:expected.password?15000:45000});
  if(!identity&&expected.password){
    await signInExistingAccount(page,expected,{signal});
    await page.goto('https://firefly.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
    identity=await waitIdentity(page);
  }
  if(!identity){
    const error=new RegistrationError('refresh_login_required');
    error.diagnostics=await sessionDiagnostics(page,context);
    throw error;
  }
  validateIdentity(identity,expected);
  // A successful IMS refresh alone cannot claim that generation credentials
  // were renewed. Let incomplete/cancelled runtime ARP fail without exporting
  // a partial replacement for the previously stored account.
  let media;
  try {media=await ensureMedia(page,context,{signal});}
  catch(error){
    if(error instanceof RegistrationError && error.code!=='media_session_cancelled'){
      try {error.diagnostics={...error.diagnostics,...await sessionDiagnostics(page,context)};} catch { /* Preserve the original failure. */ }
    }
    throw error;
  }
  if(!isCompleteMediaSession(media))throw new RegistrationError('media_session_incomplete');
  // Media initialization navigates to SPL. Re-check that current runtime identity
  // before pairing it with the observed present/omitted ARP state.
  identity=validateIdentity(await waitIdentity(page,{timeoutMs:10000}),expected);
  if(signal?.aborted)throw new RegistrationError('media_session_cancelled');
  const diagnostics={...await sessionDiagnostics(page,context),media_runtime:media.runtime_state};
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
