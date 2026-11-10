// One Node subprocess belongs to exactly one Python queue reservation.
import readline from 'node:readline';
import path from 'node:path';
import { mkdir, chmod } from 'node:fs/promises';
import { PrivateRunStore, RegistrationError } from './core.mjs';
import { BrowserRegistration } from './browser.mjs';
import { launchPersistentBrowser } from './browser-runtime.mjs';
import { runAutomaticRegistration } from './auto-runner.mjs';
import { refreshSession } from './session-refresh.mjs';
const output = value => process.stdout.write(`${JSON.stringify(value)}\n`);
// Browser libraries may log, but stdout is a credential-bearing RPC pipe only.
console.log = console.info = console.warn = console.error = () => {};
let store, browser, context, init, closed = false;
function safeCode(error) { return /^[a-z][a-z0-9_]{0,79}$/.test(error?.code || '') ? error.code : 'adobe_browser_failed'; }
async function close() {
  if (closed) return;
  if (browser) await browser.close();
  else if (context) await context.close();
  if (store) await store.close();
  closed = true;
}
async function refresh(){const account=await refreshSession(context,init);return {stage:'completed',media_session_refreshed:account.media_session_refreshed,account};}
async function command(request) {
  if (request.action === 'init') {
    if (init) throw new RegistrationError('session_already_initialized');
    init = request;
    process.env.ADOBE_BROWSER_BACKEND = 'cloakbrowser';
    process.env.ADOBE_BROWSER_PROXY = request.proxy || '';
    const root = process.env.CFM_ADOBE_PROFILE_ROOT || '/data/adobe';
    await mkdir(root, {recursive:true, mode:0o700}); await chmod(root, 0o700);
    if (request.mode === 'refresh') {
      const dir = path.join(root, request.profile_id, 'browser');
      context = await launchPersistentBrowser(dir, {headless:process.env.TURNSTILE_HEADLESS === '1'});
    } else {
      store = new PrivateRunStore(root, request.profile_id);
      await store.init({email:request.email, config:{profile:request.profile}});
      browser = new BrowserRegistration(store, {headless:process.env.TURNSTILE_HEADLESS === '1'});
      browser.workerMode = true;
      await browser.open();
    }
    return {stage:'ready'};
  }
  if (request.action === 'close') { await close(); return {stage:'closed'}; }
  if (!init) throw new RegistrationError('session_not_initialized');
  if (request.action === 'refresh') return refresh();
  if (!browser || !['run','verify'].includes(request.action)) throw new RegistrationError('command_invalid');
  const result = await runAutomaticRegistration(browser, store, {
    approveCreate:true, readInput:async () => ({profile:init.profile, ...(request.code ? {code:request.code} : {})}),
  });
  const state = await store.load();
  return {stage:result.stage, code:result.code || '', submission_started:state.submission.started,
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
  } catch (error) { output({id:request?.id, ok:false, code:safeCode(error), ...(error?.diagnostics ? {diagnostics:error.diagnostics}: {})}); }
}
await close();
