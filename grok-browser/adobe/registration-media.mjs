import { RegistrationError } from './core.mjs';
import { creationOutcome } from './creation-outcome.mjs';
import { ensureMediaSession, isCompleteMediaSession, verifyMediaSession } from './media-session.mjs';
import { waitForIdentity } from './session-refresh.mjs';
import { sanitizeMediaDiagnostics } from './media-diagnostics.mjs';
import { registrationDiagnostics } from './registration-diagnostics.mjs';

const HOME = 'https://firefly.adobe.com/';
const REFRESH_URL = 'https://adobeid-na1.services.adobe.com/ims/check/v6/token';
const CHECKPOINT_KEYS = new Set(['version', 'email', 'account_id', 'emailVerified', 'createdAt']);
const FAILURE_CODES = new Set(['media_login_required', 'media_identity_mismatch', 'media_email_unverified',
  'media_session_incomplete', 'media_prepare_timeout', 'media_session_cancelled', 'media_page_cleanup_failed',
  'media_prepare_failed', 'media_checkpoint_invalid', 'media_checkpoint_missing', 'media_result_invalid']);
const record = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const nonempty = value => typeof value === 'string' && value.length > 0 && value === value.trim() && value.length <= 65536 && !/[\r\n\0]/.test(value);
const validEmail = value => typeof value === 'string' && value.length > 0 && value.length <= 320 &&
  value === value.trim() && !/[\s\0]/.test(value) && /^[^@]+@[^@]+$/.test(value);
const sameEmail = (a,b) => validEmail(a) && validEmail(b) && a.toLowerCase() === b.toLowerCase();
const fail = code => { throw new RegistrationError(code); };

function validTimestamp(value) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{3})?Z$/.test(value)) return false;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) && new Date(parsed).toISOString().replace('.000Z','Z') === value.replace('.000Z','Z');
}

/** A checkpoint proves identity only; it never serves as generation credentials. */
export function validateMediaState(state, {email, legacyMedia=false}={}) {
  if (!record(state) || state.submission?.started !== true) fail('registration_not_submitted');
  if (!sameEmail(email,state.email)) fail('run_email_mismatch');
  if (creationOutcome(state).creation_outcome !== 'accepted') fail('media_creation_not_accepted');
  const checkpoint = state.mediaCheckpoint;
  if (checkpoint === undefined) {
    if (!legacyMedia) fail('media_checkpoint_missing');
    return state;
  }
  if (!record(checkpoint) || Object.keys(checkpoint).length !== CHECKPOINT_KEYS.size ||
      Object.keys(checkpoint).some(key=>!CHECKPOINT_KEYS.has(key)) || checkpoint.version !== 1 ||
      checkpoint.email !== state.email || !nonempty(checkpoint.account_id) || checkpoint.emailVerified !== true ||
      !validTimestamp(checkpoint.createdAt)) fail('media_checkpoint_invalid');
  return state;
}

function checkedIdentity(identity, state) {
  if (!record(identity)) fail('media_login_required');
  if (!sameEmail(identity.email,state.email) || !nonempty(identity.account_id) || !nonempty(identity.token) ||
      state.mediaCheckpoint && identity.account_id !== state.mediaCheckpoint.account_id) fail('media_identity_mismatch');
  if (identity.emailVerified !== true) fail('media_email_unverified');
  return identity;
}

export async function saveMediaCheckpoint(store, identity, {now=Date.now}={}) {
  const current=await store.load();
  const state = validateMediaState(current, {email:current.email, legacyMedia:true});
  checkedIdentity(identity,state);
  if (state.stage === 'completed' || state.result?.account) fail('media_result_invalid');
  const checkpoint = state.mediaCheckpoint || {version:1,email:state.email,account_id:identity.account_id,
    emailVerified:true,createdAt:new Date(now()).toISOString()};
  if (!validTimestamp(checkpoint.createdAt)) fail('media_checkpoint_invalid');
  return store.save({stage:'media_pending',code:null,mediaCheckpoint:checkpoint});
}

function completeResult(state, store) {
  const account = state.result?.account;
  if (!state.mediaCheckpoint || state.stage !== 'completed' || state.result?.fireflySessionVerified !== true ||
      state.result?.emailVerified !== true || state.result?.exportComplete !== true || !record(account) ||
      !sameEmail(account.email,state.email) || account.account_id !== state.mediaCheckpoint.account_id ||
      !nonempty(account.token) || typeof account.refresh_cookie !== 'string' || typeof account.user_agent !== 'string' ||
      account.media_session_arp_mode !== 'present' || !isCompleteMediaSession(account)) fail('media_result_invalid');
  if (typeof store.runId !== 'string' || !store.runId || typeof state.config?.password !== 'string') fail('media_result_invalid');
  return {stage:'completed',code:null,fireflySessionVerified:true,emailVerified:true,generationVerified:false,
    account:{...account,profile_id:store.runId,password:state.config.password}};
}

/** Resume only media preparation in the existing, queue-owned persistent context. */
export async function prepareRegisteredMedia(context, store, {
  email, legacyMedia=false, signal, waitIdentity=waitForIdentity, ensureMedia=ensureMediaSession,
  verifyMedia=verifyMediaSession, now=Date.now, timeoutMs=120000, cleanupTimeoutMs=5000,
}={}) {
  let state = validateMediaState(await store.load(), {email,legacyMedia});
  if (state.stage === 'completed' || state.result?.account) return completeResult(state,store);
  if (typeof store.runId !== 'string' || !store.runId || typeof state.config?.password !== 'string') fail('media_result_invalid');
  if (signal?.aborted) throw new RegistrationError('media_session_cancelled');
  const total = Number.isFinite(timeoutMs) ? Math.max(10,Math.min(120000,timeoutMs)) : 120000;
  const cleanupBudget = Math.max(1,Math.min(cleanupTimeoutMs,5000,total/10));
  const hardDeadline = now()+total, operationDeadline = hardDeadline-cleanupBudget;
  const controller = new AbortController();
  let page, timedOut=false, closing=false, closed=false, finalized=false;
  const abortedError = () => new RegistrationError(timedOut?'media_prepare_timeout':'media_session_cancelled');
  const externalAbort = () => controller.abort();
  signal?.addEventListener('abort',externalAbort,{once:true});
  const timer = setTimeout(()=>{timedOut=true;controller.abort();},total-cleanupBudget);
  const check = () => {
    if (controller.signal.aborted) throw abortedError();
    if (now() >= operationDeadline) {timedOut=true;controller.abort();throw abortedError();}
  };
  const phase = async (work,limit) => {
    check();
    let stageTimer,abort;
    const budget = Math.max(1,Math.min(limit,operationDeadline-now()));
    const cancelled = new Promise((_,reject)=>{
      abort=()=>reject(abortedError());controller.signal.addEventListener('abort',abort,{once:true});
      stageTimer=setTimeout(()=>{timedOut=true;controller.abort();},budget);
    });
    try {
      const result=await Promise.race([Promise.resolve().then(()=>{check();return work(budget);}),cancelled]);
      check();return result;
    } finally {clearTimeout(stageTimer);controller.signal.removeEventListener('abort',abort);}
  };
  const identityPause = ms => new Promise((resolve,reject)=>{
    if (controller.signal.aborted) {reject(abortedError());return;}
    let sleepTimer;
    const abort=()=>{clearTimeout(sleepTimer);controller.signal.removeEventListener('abort',abort);reject(abortedError());};
    controller.signal.addEventListener('abort',abort,{once:true});
    sleepTimer=setTimeout(()=>{controller.signal.removeEventListener('abort',abort);resolve();},ms);
  });
  const closePage = async () => {
    if (!page || closed || closing) return;
    closing=true;
    let closeTimer;
    try {
      const remaining=Math.max(1,Math.min(cleanupBudget,hardDeadline-now()));
      await Promise.race([
        Promise.resolve().then(()=>page.close({runBeforeUnload:false})),
        new Promise((_,reject)=>{closeTimer=setTimeout(()=>reject(new RegistrationError('media_page_cleanup_failed')),remaining);}),
      ]);
      if (context.pages().includes(page)) fail('media_page_cleanup_failed');
      closed=true;
    } catch {fail('media_page_cleanup_failed');}
    finally {clearTimeout(closeTimer);}
  };
  try {
    const restored = context.pages();
    page = await phase(async()=>{
      const created=await context.newPage();
      // A timed-out browser command may return later. Its newly allocated page
      // still belongs to this action and must not survive the caller's cleanup.
      if (controller.signal.aborted) {void created.close({runBeforeUnload:false}).catch(()=>{});throw abortedError();}
      return created;
    },5000);
    await phase(async()=>{
      const results=await Promise.allSettled(restored.map(old=>Promise.resolve().then(()=>old.close({runBeforeUnload:false}))));
      if (results.some(result=>result.status==='rejected') || context.pages().length!==1 || context.pages()[0]!==page) fail('media_page_cleanup_failed');
    },5000);
    await phase(budget=>page.goto(HOME,{waitUntil:'domcontentloaded',timeout:budget}),30000);
    let identity = checkedIdentity(await phase(budget=>waitIdentity(page,{timeoutMs:budget,signal:controller.signal,now,pause:identityPause}),15000),state);
    check();
    // Write no token or Cookie here. A later failure remains recoverable using
    // the same account identity and original profile, without another signup.
    state = await saveMediaCheckpoint(store,{...identity,email:state.email},{now});
    check();
    const media = await phase(budget=>ensureMedia(page,context,{identity,signal:controller.signal,timeoutMs:Math.min(30000,budget/2)}),60000);
    if (media?.media_session_arp_mode !== 'present' || !isCompleteMediaSession(media)) fail('media_session_incomplete');
    identity = checkedIdentity(await phase(budget=>waitIdentity(page,{timeoutMs:budget,signal:controller.signal,now,pause:identityPause}),10000),state);
    await phase(budget=>verifyMedia(page,media,identity,{signal:controller.signal,timeoutMs:Math.min(1500,Math.max(1,budget-1000))}),2500);
    const refreshCookies = await phase(()=>context.cookies(REFRESH_URL),2000);
    if (!Array.isArray(refreshCookies) || refreshCookies.some(cookie=>!record(cookie)||!nonempty(cookie.name)||typeof cookie.value!=='string'||/[\r\n\0]/.test(cookie.value))) fail('media_session_incomplete');
    const account = {email:state.email,account_id:identity.account_id,token:identity.token,cookie:media.cookie,
      refresh_cookie:refreshCookies.map(cookie=>`${cookie.name}=${cookie.value}`).join('; '),
      user_agent:typeof identity.user_agent==='string'?identity.user_agent:'',
      media_session_arp_mode:media.media_session_arp_mode,media_session_arp_source:media.media_session_arp_source};
    await closePage();check();
    const current=validateMediaState(await store.load(),{email});
    if (current.mediaCheckpoint.account_id!==identity.account_id) fail('media_identity_mismatch');
    if (current.stage==='completed'||current.result?.account) return completeResult(current,store);
    check();
    const saved = await store.save({stage:'completed',code:null,result:{account,fireflySessionVerified:true,
      emailVerified:true,generationVerified:false,exportComplete:true}});
    finalized=true;
    return completeResult(saved,store);
  } catch(error) {
    if (finalized) throw error;
    const code = timedOut?'media_prepare_timeout':signal?.aborted?'media_session_cancelled':
      FAILURE_CODES.has(error?.code)?error.code:'media_prepare_failed';
    const diagnostics = sanitizeMediaDiagnostics(error?.diagnostics);
    const result = {stage:'media_pending',code,diagnostics};
    try {
      const current=validateMediaState(await store.load(),{email,legacyMedia});
      // Do not overwrite an atomic completion that has already become visible.
      if (current.stage==='completed'||current.result?.account) return completeResult(current,store);
      const lastObservation=registrationDiagnostics(result,current,{now});
      await store.save({stage:'media_pending',code,lastObservation});
    } catch {throw new RegistrationError('media_state_write_failed');}
    return result;
  } finally {
    clearTimeout(timer);signal?.removeEventListener('abort',externalAbort);controller.abort();
    try {await closePage();} catch { /* The fixed failure outcome is already persisted; the RPC closes its context. */ }
  }
}
