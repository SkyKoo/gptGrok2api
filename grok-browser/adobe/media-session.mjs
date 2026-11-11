import { randomUUID } from 'node:crypto';
import { RegistrationError } from './core.mjs';

const FIREFLY_HOME = 'https://firefly.adobe.com/';
const FIREFLY_IMAGE = 'https://firefly.adobe.com/generate/image';

// ARP is an opaque value supplied by Adobe's runtime, never reconstructed from
// undocumented fields. null means an explicitly observed, optional header.
export function isUsableSherlock(value) {
  return typeof value === 'string' && value.length > 0 && value.length <= 65536 &&
    /^[\x21-\x7e]+$/.test(value) && !/[;,]/.test(value);
}

export function mediaCookieHeader(items, runtimeToken) {
  if (runtimeToken !== null && !isUsableSherlock(runtimeToken)) return '';
  const cookies = items.filter(cookie => cookie.name !== 'sherlockToken');
  if (runtimeToken !== null) cookies.push({name:'sherlockToken', value:runtimeToken});
  return cookies.map(cookie => `${cookie.name}=${cookie.value}`).join('; ');
}

export function isCompleteMediaSession(media) {
  if (media?.media_session_arp_source !== 'spl_runtime' || typeof media.cookie !== 'string') return false;
  const values = media.cookie.split(';').map(item=>item.trim()).filter(item=>item.startsWith('sherlockToken=')).map(item=>item.slice('sherlockToken='.length));
  return media.media_session_arp_mode === 'omitted' ? values.length === 0 :
    media.media_session_arp_mode === 'present' && values.length === 1 && isUsableSherlock(values[0]);
}

// SPL's real authentication singleton is exposed by commerce elements inside
// shadow roots. Current UGS fx reads Observe<T> once and seeds null when it does
// not emit synchronously; only a nonempty value adds x-arp-session-id. Mirror
// that read contract, without initializing a producer or calling a setter.
export function runtimeARPInPage({timeoutMs, cancelEvent}) {
  return new Promise(resolve => {
    let finished = false, tick, deadline;
    const findAuthentication = () => {
      const roots = [document];
      for (const root of roots) {
        for (const host of root.querySelectorAll('firefly-commerce-dialogs, firefly-commerce-paywall-dialog')) {
          const service = host.service;
          if (service?.serviceName === 'spl-commerce-dialog-service' && service.authentication) return service.authentication;
        }
        for (const element of root.querySelectorAll('*')) if (element.shadowRoot) roots.push(element.shadowRoot);
      }
      return null;
    };
    const finish = result => {
      if (finished) return;
      finished = true;
      clearInterval(tick);clearTimeout(deadline);
      globalThis.removeEventListener(cancelEvent, cancelled);
      globalThis.removeEventListener('pagehide', cancelled);
      resolve(result);
    };
    const cancelled = () => finish({state:'unavailable'});
    const inspect = () => {
      if (finished) return;
      if (globalThis.location?.origin !== 'https://firefly.adobe.com' || globalThis.location?.pathname !== '/generate/image') return cancelled();
      let authentication;
      try { authentication = findAuthentication(); } catch { return; }
      const source = authentication?.arpSessionToken;
      if (typeof source !== 'function') return;
      let emitted = false, value, stop;
      try {
        stop = source(next => { emitted = true;value = next; });
        if (typeof stop !== 'function') return finish({state:'invalid_contract'});
        // Ensure the inspected singleton was not replaced during subscription.
        const current = findAuthentication();
        if (current !== authentication || current?.arpSessionToken !== source) return;
        if (!emitted) return finish({state:'unobserved', source:'spl_runtime'});
        if (value === null || value === undefined || value === '') {
          return finish({state:'empty', source:'spl_runtime', value_kind:value === null?'null':value === undefined?'undefined':'empty_string'});
        }
        if (typeof value === 'string' && value.length <= 65536 && /^[\x21-\x7e]+$/.test(value) && !/[;,]/.test(value)) {
          return finish({state:'ready', source:'spl_runtime', token:value});
        }
        finish({state:'invalid', source:'spl_runtime'});
      } catch { finish({state:'invalid_contract'}); }
      finally { if (typeof stop === 'function') { try { stop(); } catch { /* Do not expose runtime internals. */ } } }
    };
    globalThis.addEventListener(cancelEvent, cancelled, {once:true});
    globalThis.addEventListener('pagehide', cancelled, {once:true});
    deadline = setTimeout(() => finish({state:'unavailable'}), timeoutMs);
    tick = setInterval(inspect, Math.min(100, timeoutMs));
    inspect();
  });
}

function cancelledError() { return new RegistrationError('media_session_cancelled'); }

async function observeRuntimeARP(page, timeoutMs, signal) {
  if (signal?.aborted) throw cancelledError();
  const cancelEvent = `cfm-adobe-media-cancel-${randomUUID()}`;
  let watchdog, rejectCancelled;
  const cancelPageWait = () => {
    void page.evaluate(name => globalThis.dispatchEvent(new Event(name)), cancelEvent).catch(() => {});
  };
  const onAbort = () => { cancelPageWait(); rejectCancelled(cancelledError()); };
  const cancellation = new Promise((_, reject) => { rejectCancelled = reject; });
  signal?.addEventListener('abort', onAbort, {once:true});
  const bounded = new Promise((_, reject) => {
    watchdog = setTimeout(() => {
      cancelPageWait();reject(new RegistrationError('media_session_incomplete'));
    }, timeoutMs + 1000);
  });
  try {
    return await Promise.race([page.evaluate(runtimeARPInPage, {timeoutMs, cancelEvent}), cancellation, bounded]);
  } catch (error) {
    if (signal?.aborted) throw cancelledError();
    if (error instanceof RegistrationError) throw error;
    return {state:'unavailable'};
  } finally {
    clearTimeout(watchdog);signal?.removeEventListener('abort', onAbort);
  }
}

async function pauseAbortably(ms, pause, signal) {
  if (signal?.aborted) throw cancelledError();
  let abort;
  const cancelled = new Promise((_, reject) => {
    abort = () => reject(cancelledError());signal?.addEventListener('abort', abort, {once:true});
  });
  try { await Promise.race([pause(ms), cancelled]); }
  finally { signal?.removeEventListener('abort', abort); }
}

export async function ensureMediaSession(page, context, {
  timeoutMs = 30000, now = Date.now, pause = ms => new Promise(resolve => setTimeout(resolve, ms)), signal,
} = {}) {
  if (signal?.aborted) throw cancelledError();
  const deadline = now() + timeoutMs;
  let atEditor = false;
  try { const url = new URL(page.url());atEditor = url.origin === new URL(FIREFLY_HOME).origin && url.pathname === '/generate/image'; } catch {}
  if (!atEditor) await page.goto(FIREFLY_IMAGE, {waitUntil:'domcontentloaded', timeout:Math.max(1, deadline-now())});
  let runtimeState = {state:'unavailable'};
  while (now() < deadline) {
    if (signal?.aborted) throw cancelledError();
    const observation = await observeRuntimeARP(page, Math.min(1000, deadline-now()), signal);
    if (signal?.aborted) throw cancelledError();
    runtimeState = {state:observation?.state || 'unavailable', ...(observation?.value_kind?{value_kind:observation.value_kind}:{})};
    const present = observation?.state === 'ready' && isUsableSherlock(observation.token);
    const omitted = observation?.state === 'empty' || observation?.state === 'unobserved';
    if (observation?.source === 'spl_runtime' && (present || omitted)) {
      const cookies = await context.cookies(FIREFLY_HOME);
      if (signal?.aborted) throw cancelledError();
      return {cookie:mediaCookieHeader(cookies, present?observation.token:null),
        media_session_arp_mode:present?'present':'omitted', media_session_arp_source:'spl_runtime', runtime_state:runtimeState};
    }
    if (observation?.state === 'invalid' || observation?.state === 'invalid_contract') break;
    const remaining = deadline-now();
    if (remaining > 0) await pauseAbortably(Math.min(250, remaining), pause, signal);
  }
  const error = new RegistrationError('media_session_incomplete');
  error.diagnostics = {media_runtime:runtimeState};
  throw error;
}
