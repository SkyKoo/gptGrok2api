import { RegistrationError } from './core.mjs';
import { ensureMediaSession } from './media-session.mjs';

export async function waitForIdentity(page, {timeoutMs=45000, now=Date.now, pause=ms=>new Promise(resolve=>setTimeout(resolve,ms))}={}) {
  const deadline=now()+timeoutMs;
  while(now()<deadline){
    try {
      const identity=await page.evaluate(async () => {
        const ims=window.adobeIMS;
        if (!ims?.isSignedInUser || !ims?.getProfile || !ims?.getAccessToken || !await ims.isSignedInUser()) return null;
        const profile=await ims.getProfile();
        const credential=await ims.getAccessToken();
        if (!profile?.email || !profile?.userId || !credential?.token) return null;
        return {email:profile.email, account_id:profile.userId, token:credential.token, emailVerified:profile.emailVerified, user_agent:typeof navigator==='object'?navigator.userAgent:''};
      });
      if(identity)return identity;
    } catch { /* Observe transient SDK initialization/navigation without a second action. */ }
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
export async function signInExistingAccount(page, expected) {
  if(!expected.password)throw new RegistrationError('refresh_login_required');
  await page.goto('https://account.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
  const email=page.locator('input[type="email"]:visible, input[name="username"]:visible');
  const password=page.locator('input[type="password"]:visible');
  try {await email.first().waitFor({state:'visible',timeout:15000});} catch { /* May restore the password page. */ }
  const proceed=()=>page.getByRole('button',{name:/^(Continue|Sign in|继续|登录)$/i});
  if(await email.count()===1){
    await email.fill(expected.email);
    const button=proceed();if(await button.count()!==1)throw new RegistrationError('refresh_login_form_unrecognized');
    await button.click({timeout:15000});
  }
  try {await password.waitFor({state:'visible',timeout:20000});}
  catch {throw new RegistrationError('refresh_verification_required');}
  if(await password.count()!==1)throw new RegistrationError('refresh_login_form_unrecognized');
  await password.fill(expected.password);
  const button=proceed();if(await button.count()!==1)throw new RegistrationError('refresh_login_form_unrecognized');
  await button.click({timeout:15000});
  // Observe completion before navigating; a challenge/OTP stays on Adobe auth.
  try {await page.waitForURL(url=>url.hostname==='account.adobe.com',{timeout:30000});}
  catch {throw new RegistrationError('refresh_verification_required');}
}

export async function refreshSession(context, expected, {waitIdentity=waitForIdentity,ensureMedia=ensureMediaSession}={}) {
  const page=context.pages()[0]||await context.newPage();
  await page.goto('https://firefly.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
  let identity=await waitIdentity(page,{timeoutMs:expected.password?15000:45000});
  if(!identity&&expected.password){
    await signInExistingAccount(page,expected);
    await page.goto('https://firefly.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
    identity=await waitIdentity(page);
  }
  if(!identity){
    const error=new RegistrationError('refresh_login_required');
    error.diagnostics=await sessionDiagnostics(page,context);
    throw error;
  }
  validateIdentity(identity,expected);
  let cookie='',media_session_status='retained_previous';
  try {cookie=await ensureMedia(page,context);media_session_status='complete';}
  catch(error){if(error.code!=='media_session_incomplete')throw error;}
  const diagnostics=await sessionDiagnostics(page,context);
  const cookieString=list=>list.map(c=>`${c.name}=${c.value}`).join('; ');
  return {...identity,profile_id:expected.profile_id,media_session_refreshed:media_session_status==='complete',media_session_status,session_diagnostics:diagnostics,
    cookie,
    refresh_cookie:cookieString(await context.cookies('https://adobeid-na1.services.adobe.com/ims/check/v6/token')),
  };
}


async function sessionDiagnostics(page,context){
 const cookies=await context.cookies('https://firefly.adobe.com/');
 const scopes=cookies.filter(c=>c.name==='sherlockToken').map(c=>{
  let fields={};try{fields=JSON.parse(Buffer.from(c.value,'base64').toString('utf8'));}catch{}
  return {domain:c.domain,path:c.path,present_fields:['sid','ark','bfp','ftr'].filter(k=>typeof fields[k]==='string'&&fields[k])};
 });
 let host='';try{host=new URL(page.url()).hostname;}catch{}
 return {host,sherlock_cookies:scopes};
}
