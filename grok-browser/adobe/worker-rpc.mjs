// One Node subprocess belongs to exactly one Python queue reservation.
import readline from 'node:readline';
import path from 'node:path';
import { mkdir, chmod } from 'node:fs/promises';
import { PrivateRunStore, RegistrationError } from './core.mjs';
import { BrowserRegistration } from './browser.mjs';
import { launchPersistentBrowser } from './browser-runtime.mjs';
import { runAutomaticRegistration } from './auto-runner.mjs';
import { refreshSession } from './session-refresh.mjs';
import { registrationDiagnostics } from './registration-diagnostics.mjs';
import { resumeRegistration } from './registration-recovery.mjs';
const output = value => process.stdout.write(`${JSON.stringify(value)}\n`);
// Browser libraries may log, but stdout is a credential-bearing RPC pipe only.
console.log = console.info = console.warn = console.error = () => {};
let store, browser, context, init, closed = false, lifecycle = {};
function safeCode(error) { return /^[a-z][a-z0-9_]{0,79}$/.test(error?.code || '') ? error.code : 'adobe_browser_failed'; }
async function recordLifecycle(field) {
  lifecycle = { ...lifecycle, [field]: new Date().toISOString() };
  // Diagnostics must not prevent cleanup of a partially initialized browser.
  try { if (store) await store.save({lastLifecycle:lifecycle}); } catch { /* Keep the original close outcome. */ }
}
async function close() {
  if (closed) return;
  await recordLifecycle('closeStartedAt');
  if (browser) await browser.close();
  else if (context) await context.close();
  await recordLifecycle('closeCompletedAt');
  if (store) await store.close();
  closed = true;
}
async function refresh(){const account=await refreshSession(context,init,{workerMode:true});return {stage:'completed',media_session_refreshed:account.media_session_refreshed,account};}
async function command(request) {
  if (request.action === 'init') {
    if (init) throw new RegistrationError('session_already_initialized');
    init = request;
    if (request.mode === 'resume' && !/^[a-zA-Z0-9_-]{1,128}$/.test(request.recovery_id || '')) throw new RegistrationError('recovery_id_required');
    process.env.ADOBE_BROWSER_BACKEND = 'cloakbrowser';
    process.env.ADOBE_BROWSER_PROXY = request.proxy || '';
    const root = process.env.CFM_ADOBE_PROFILE_ROOT || '/data/adobe';
    await mkdir(root, {recursive:true, mode:0o700}); await chmod(root, 0o700);
    if (request.mode === 'refresh') {
      const dir = path.join(root, request.profile_id, 'browser');
      context = await launchPersistentBrowser(dir, {headless:process.env.TURNSTILE_HEADLESS === '1',restoreLastSession:false});
    } else {
      store = new PrivateRunStore(root, request.profile_id);
      const state = await store.init({email:request.email, config:{profile:request.profile}}, {requireExisting:request.mode === 'resume'});
      init.profile = state.config.profile;
      browser = new BrowserRegistration(store, {headless:process.env.TURNSTILE_HEADLESS === '1'});
      browser.workerMode = true;
      // A saved verified export is already sufficient for compensation. Do not
      // acquire fresh credentials or navigate a completed registration again.
      if (!(request.mode === 'resume' && state.result?.account)) await browser.open({resumeOnly:request.mode === 'resume' && state.submission.started});
    }
    return {stage:'ready'};
  }
  if (request.action === 'close') { await close(); return {stage:'closed'}; }
  if (!init) throw new RegistrationError('session_not_initialized');
  if (request.action === 'refresh') return refresh();
  if (!browser || !['run','verify','resume'].includes(request.action)) throw new RegistrationError('command_invalid');
  if (request.action === 'resume' && init.mode !== 'resume') throw new RegistrationError('recovery_mode_required');
  const result = init.mode === 'resume' ? await resumeRegistration(browser, store, {recoveryId:init.recovery_id,code:request.code}) : await runAutomaticRegistration(browser, store, {
    approveCreate:true, resumeBeforeSubmission:true, readInput:async () => ({profile:init.profile, ...(request.code ? {code:request.code} : {})}),
  });
  const state = await store.load();
  const diagnostics = registrationDiagnostics(result, state, {now:Date.now});
  // The browser closes when this attempt ends. Keep its fixed, credential-free
  // observation so a later investigation does not have to repeat signup.
  lifecycle = {diagnosticsCapturedAt:diagnostics.captured_at, responsePreparedAt:new Date().toISOString()};
  await store.save({lastObservation:diagnostics,lastLifecycle:lifecycle});
  return {stage:result.stage, code:result.code || '', submission_started:state.submission.started, diagnostics,
    ...(init.mode === 'resume' ? {recovery_mode:result.recovery_mode || 'resume_session'} : {}),
    ...(result.verification_requested_at ? {verification_requested_at:result.verification_requested_at} : {}),
    ...(result.stage === 'completed' ? {account:{...state.result.account, profile_id:init.profile_id, password:state.config.password}} : {}),
  };
}
for await (const line of readline.createInterface({input:process.stdin, crlfDelay:Infinity})) {
  let request;
  try {
    if (line.length > 65536) throw new RegistrationError('command_too_large');
    request = JSON.parse(line);
    const result = await command(request);
    output({id:request.id, ok:true, result});
    if (closed) break;
  } catch (error) {
    let state;
    try { state = await store?.load(); } catch { /* Missing state is unknown, never proof of no submission. */ }
    output({id:request?.id, ok:false, code:safeCode(error),
      ...(state ? {submission_started:state.submission.started} : {}),
      ...(error?.diagnostics ? {diagnostics:error.diagnostics}: {})});
  }
}
await close();
