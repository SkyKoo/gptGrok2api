import { randomUUID } from 'node:crypto';
import { RegistrationError } from './core.mjs';

const FIREFLY_HOME = 'https://firefly.adobe.com/';
const FIREFLY_IMAGE = 'https://firefly.adobe.com/generate/image';

// ARP is an opaque value supplied by Adobe's runtime, never reconstructed from
// undocumented fields. null is retained only for existing omitted records.
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

// SPL initially publishes null before the host's asynchronous ARP callback.
// A nonempty token can also be provisional. Observe the normal lifecycle until
// the document's sole SDK instance reports a token for every known vendor.
export function runtimeARPInPage({timeoutMs, cancelEvent}) {
  return new Promise(resolve => {
    let finished = false, tick, deadline, inspectionQueued = false;
    let authentication, source, stop, subscribed = false, emitted = false, value;
    let stableCandidate = '', stableSince = 0;
    let observation = {state:'unavailable',readiness_reason:'authentication_missing'};
    const startedAt = Date.now();
    // Failure-only, bounded observations; changes exclude the first value.
    // No token, SDK entry or authentication value is exported by these fields.
    const timing = {candidate_changes:0,source_changes:0,authentication_changes:0,emissions:0,value_changes:0,proof_changes:0};
    let previousSource, previousAuthentication, previousValue, previousProof;
    let sourceSeen = false, authenticationSeen = false, valueSeen = false, candidateSeen = false;
    const elapsed = () => Math.max(0,Math.min(60000,Math.trunc(Date.now()-startedAt)));
    const count = key => { timing[key]=Math.min(65535,timing[key]+1); };
    const recordSource = (current, currentSource) => {
      if (authenticationSeen && current !== previousAuthentication) count('authentication_changes');
      if (current || authenticationSeen) { authenticationSeen=true;previousAuthentication=current; }
      if (sourceSeen && currentSource !== previousSource) count('source_changes');
      if (typeof currentSource === 'function' || sourceSeen) {
        if (!sourceSeen) timing.first_source_ms=elapsed();
        sourceSeen=true;previousSource=currentSource;
      }
    };
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
    // Only counts and fixed route categories leave the page. This bounded,
    // read-only failure snapshot never changes which authentication is used.
    const pageDiagnostics = () => {
      const result = {route:'unavailable',matching_host_count:0,authentication_count:0,arp_observer_count:0,scan_complete:false};
      const authentications = new Set(), observers = new Set(), visited = new Set();
      let inspected = 0;
      try {
        const location = globalThis.location;
        if (location) result.route = location.origin === 'https://firefly.adobe.com' && location.pathname === '/generate/image' ? 'editor' : 'other';
        const roots = [document];
        for (const root of roots) {
          if (visited.has(root)) continue;
          if (visited.size >= 32) return result;
          visited.add(root);
          for (const host of root.querySelectorAll('firefly-commerce-dialogs, firefly-commerce-paywall-dialog')) {
            if (++inspected > 4096) return result;
            const service = host.service;
            if (service?.serviceName !== 'spl-commerce-dialog-service') continue;
            if (result.matching_host_count >= 32) return result;
            result.matching_host_count++;
            const authentication = service.authentication;
            if (!authentication) continue;
            authentications.add(authentication);result.authentication_count = authentications.size;
            if (typeof authentication.arpSessionToken === 'function') observers.add(authentication);
            result.arp_observer_count = observers.size;
          }
          for (const element of root.querySelectorAll('*')) {
            if (++inspected > 4096) return result;
            if (element.shadowRoot) roots.push(element.shadowRoot);
          }
        }
        result.scan_complete = true;
      } catch { /* The failed scan exposes no runtime values. */ }
      return result;
    };
    const unsubscribe = () => {
      const release = stop;
      stop = undefined;subscribed = false;
      if (typeof release === 'function') { try { release(); } catch { /* No runtime internals in diagnostics. */ } }
    };
    const finish = result => {
      if (finished) return;
      finished = true;
      clearInterval(tick);clearTimeout(deadline);
      globalThis.removeEventListener(cancelEvent, cancelled);
      globalThis.removeEventListener('pagehide', pageHidden);
      unsubscribe();
      resolve(result.state === 'ready' ? result : {...result,media_page:pageDiagnostics(),media_timing:{...timing}});
    };
    const cancelled = () => finish({state:'unavailable',readiness_reason:'observation_cancelled'});
    const pageHidden = () => finish({state:'unavailable',readiness_reason:'page_hidden'});
    const inspectVendorReadiness = () => {
      const pending = (readiness_reason, vendors_started=0, vendors_complete=0) => ({state:'pending',source:'spl_runtime',readiness_reason,vendors_started,vendors_complete});
      let marks, measures;
      try {
        marks = globalThis.performance?.getEntriesByType('mark');
        measures = globalThis.performance?.getEntriesByType('measure');
      } catch { return pending('performance_unavailable'); }
      if (!Array.isArray(marks) || !Array.isArray(measures)) return pending('performance_unavailable');
      const inits = marks.filter(entry => /^sherlock:[a-zA-Z0-9_-]{1,128}:init:start$/.test(entry.name) && Number.isFinite(entry.startTime) && entry.startTime >= 0);
      if (!inits.length) return pending('instance_missing');
      // Performance records do not bind an SDK instance to SPL authentication.
      // Multiple instances in one document are ambiguous even if the newest
      // one has completed; never borrow another instance's vendor evidence.
      if (new Set(inits.map(entry=>entry.name)).size !== 1) return pending('instance_ambiguous');
      const newest = Math.max(...inits.map(entry=>entry.startTime)), latest = inits.filter(entry=>entry.startTime===newest);
      if (latest.length !== 1) return pending('instance_ambiguous');
      const init = latest[0], prefix = init.name.slice(0,-'init:start'.length), expected = ['arkose','bfp','forter'];
      const started = new Map();
      for (const entry of marks) {
        if (!entry.name.startsWith(prefix) || !Number.isFinite(entry.startTime) || entry.startTime < init.startTime) continue;
        const match = /^([^:]+):start$/.exec(entry.name.slice(prefix.length));
        if (!match || match[1] === 'init') continue;
        if (!expected.includes(match[1])) return pending('vendor_unrecognized',started.size);
        const previous = started.get(match[1]);
        if (!previous || entry.startTime > previous.startTime) started.set(match[1],entry);
      }
      let complete = 0;
      for (const vendor of expected) {
        const mark = started.get(vendor);
        if (mark && measures.some(entry=>entry.name===`${mark.name}:duration` && entry.startTime===mark.startTime && Number.isFinite(entry.duration) && entry.duration>=0)) complete++;
      }
      if (started.size !== expected.length || complete !== expected.length) return pending('vendors_pending',started.size,complete);
      // Keep the exact observed cycle only in memory for the final identity
      // check. A later SDK instance or vendor refresh invalidates this proof.
      const entries = [...marks,...measures].filter(entry=>entry.name.startsWith(prefix) && entry.startTime>=init.startTime)
        .map(entry=>[entry.name,entry.entryType,entry.startTime,entry.duration])
        .sort((a,b)=>a[2]-b[2] || a[0].localeCompare(b[0]) || a[3]-b[3]);
      return {state:'ready',proof:{instance:init.name,start_time:init.startTime,entries}};
    };
    const scheduleInspection = () => {
      if (finished || inspectionQueued) return;
      inspectionQueued = true;
      queueMicrotask(() => { inspectionQueued=false;inspect(); });
    };
    const inspect = () => {
      if (finished) return;
      if (globalThis.location?.origin !== 'https://firefly.adobe.com' || globalThis.location?.pathname !== '/generate/image') return finish({state:'unavailable',readiness_reason:'editor_route_changed'});
      let current, currentSource;
      try { current = findAuthentication();currentSource = current?.arpSessionToken; } catch { observation={state:'unavailable',readiness_reason:'evaluation_failed'};return; }
      recordSource(current,currentSource);
      if (current !== authentication || currentSource !== source) {
        unsubscribe();authentication = current;source = currentSource;emitted = false;value = undefined;stableCandidate='';
      }
      if (typeof source !== 'function') { observation = {state:'unavailable',readiness_reason:authentication?'arp_observer_missing':'authentication_missing'};return; }
      try {
        if (!subscribed) {
          const subscribedSource = source, subscribedAuthentication = authentication;
          stop = source(next => {
            if (finished || source !== subscribedSource || authentication !== subscribedAuthentication) return;
            count('emissions');
            if (valueSeen && next !== previousValue) count('value_changes');
            valueSeen=true;previousValue=next;
            if (timing.first_nonempty_ms === undefined && typeof next === 'string' && next.length > 0) timing.first_nonempty_ms=elapsed();
            // Do not lose a transient A→null/B→A update when callbacks are
            // coalesced into one inspection microtask. Equal emissions are OK.
            if (!emitted || next !== value) stableCandidate='';
            emitted = true;value = next;scheduleInspection();
          });
          if (typeof stop !== 'function') return finish({state:'invalid_contract'});
          subscribed = true;
          // Vendor callbacks can fire inside the SDK's synchronous init loop.
          // Let that loop finish starting its other vendors before checking.
          scheduleInspection();return;
        }
        // A callback can replace the singleton. Never pair its value with a
        // different authentication instance, even during synchronous subscribe.
        current = findAuthentication();
        currentSource = current?.arpSessionToken;recordSource(current,currentSource);
        if (current !== authentication || currentSource !== source) { unsubscribe();return; }
        if (!emitted || value === null || value === undefined || value === '') {
          stableCandidate='';
          observation = {state:'pending', source:'spl_runtime', value_kind:!emitted?'unobserved':value === null?'null':value === undefined?'undefined':'empty_string'};
          return;
        }
        if (typeof value === 'string' && value.length <= 65536 && /^[\x21-\x7e]+$/.test(value) && !/[;,]/.test(value)) {
          const readiness = inspectVendorReadiness();
          if (readiness.state !== 'ready') { observation=readiness;stableCandidate='';return; }
          if (timing.vendors_ready_ms === undefined) timing.vendors_ready_ms=elapsed();
          const proof = JSON.stringify(readiness.proof);
          if (previousProof !== undefined && proof !== previousProof) count('proof_changes');
          previousProof=proof;
          const candidate = JSON.stringify([value,readiness.proof]);
          if (candidate !== stableCandidate) {
            if (candidateSeen) count('candidate_changes');
            candidateSeen=true;timing.last_candidate_ms=elapsed();
            stableCandidate=candidate;stableSince=Date.now();
          }
          // First vendor callbacks can be followed immediately by richer
          // official updates. Require a short unchanged observation window,
          // within the existing deadline, without reading token internals.
          if (Date.now()-stableSince < 250) {
            observation={state:'pending',source:'spl_runtime',readiness_reason:'token_settling',vendors_started:3,vendors_complete:3};return;
          }
          return finish({state:'ready', source:'spl_runtime', token:value, runtime_proof:readiness.proof});
        }
        finish({state:'invalid', source:'spl_runtime'});
      } catch { finish({state:'invalid_contract'}); }
    };
    globalThis.addEventListener(cancelEvent, cancelled, {once:true});
    globalThis.addEventListener('pagehide', pageHidden, {once:true});
    deadline = setTimeout(() => finish(observation), timeoutMs);
    tick = setInterval(scheduleInspection, Math.min(100, timeoutMs));
    inspect();
  });
}

// Observe all four values from one current SPL instance. Observe<T> may deliver
// its first value asynchronously; absence is pending, never proof of equality.
// Known changes still fail immediately without changing producers or cookies.
export function mediaSessionMatchesInPage({token, identity, runtimeProof, timeoutMs=1500, cancelEvent}) {
  return new Promise(resolve => {
    const stops = [], values = {}, sources = {}, confirmed = new Set();
    let finished = false, subscribed = false, timer, tick, authentication;
    const finish = result => {
      if (finished) return;
      finished = true;
      clearTimeout(timer);clearInterval(tick);
      globalThis.removeEventListener('pagehide', cancelled);
      if (cancelEvent) globalThis.removeEventListener(cancelEvent, cancelled);
      for (const stop of stops.splice(0)) { try { stop(); } catch { /* No runtime values in diagnostics. */ } }
      resolve(result);
    };
    const reject = reason => finish({matches:false, reason});
    const cancelled = () => reject('runtime_changed');
    const findAuthentication = () => {
      const roots = [document];
      for (const root of roots) {
        for (const host of root.querySelectorAll('firefly-commerce-dialogs, firefly-commerce-paywall-dialog')) {
          if (host.service?.serviceName === 'spl-commerce-dialog-service' && host.service.authentication) return host.service.authentication;
        }
        for (const element of root.querySelectorAll('*')) if (element.shadowRoot) roots.push(element.shadowRoot);
      }
      return null;
    };
    const sameProof = () => {
      if (!runtimeProof || typeof runtimeProof.instance !== 'string' || !Array.isArray(runtimeProof.entries)) return false;
      const marks = globalThis.performance?.getEntriesByType('mark'), measures = globalThis.performance?.getEntriesByType('measure');
      if (!Array.isArray(marks) || !Array.isArray(measures)) return false;
      const inits = marks.filter(entry=>/^sherlock:[a-zA-Z0-9_-]{1,128}:init:start$/.test(entry.name) && Number.isFinite(entry.startTime) && entry.startTime>=0);
      if (new Set(inits.map(entry=>entry.name)).size!==1) return false;
      const newest = Math.max(...inits.map(entry=>entry.startTime)), latest = inits.filter(entry=>entry.startTime===newest);
      if (latest.length!==1 || latest[0].name!==runtimeProof.instance || latest[0].startTime!==runtimeProof.start_time) return false;
      const prefix = latest[0].name.slice(0,-'init:start'.length);
      const entries = [...marks,...measures].filter(entry=>entry.name.startsWith(prefix) && entry.startTime>=latest[0].startTime)
        .map(entry=>[entry.name,entry.entryType,entry.startTime,entry.duration])
        .sort((a,b)=>a[2]-b[2] || a[0].localeCompare(b[0]) || a[3]-b[3]);
      return JSON.stringify(entries)===JSON.stringify(runtimeProof.entries);
    };
    const absent = value => value === null || value === undefined || value === '';
    const inspect = () => {
      if (finished) return;
      try {
        if (globalThis.location?.origin !== 'https://firefly.adobe.com' || globalThis.location?.pathname !== '/generate/image' ||
            findAuthentication() !== authentication || Object.entries(sources).some(([key, source]) => authentication[key] !== source) || !sameProof()) return reject('runtime_changed');
        const profile = values.userProfile;
        const profilePresent = !absent(profile);
        if (profilePresent && (typeof profile !== 'object' || profile.emailVerified === false ||
            !absent(profile.userId) && profile.userId !== identity.account_id ||
            !absent(profile.email) && (typeof profile.email !== 'string' || profile.email.toLowerCase() !== identity.email.toLowerCase()))) return reject('identity_changed');
        const matches = {
          userProfile:profilePresent && profile.userId === identity.account_id && typeof profile.email === 'string' && profile.email.toLowerCase() === identity.email.toLowerCase(),
          accessToken:values.accessToken === identity.token,
          isSignedIn:values.isSignedIn === true,
          arpSessionToken:values.arpSessionToken === token,
        };
        for (const key of Object.keys(sources)) {
          const reason = key === 'arpSessionToken' ? 'runtime_changed' : 'identity_changed';
          if (confirmed.has(key) && !matches[key]) return reject(reason);
          if (key !== 'userProfile' && !absent(values[key]) && !matches[key]) return reject(reason);
          if (matches[key]) confirmed.add(key);
        }
        if (subscribed && confirmed.size === 4) finish(true);
      } catch { reject('observer_unavailable'); }
    };
    try {
      if (![token,identity?.account_id,identity?.email,identity?.token].every(value=>typeof value === 'string' && value.length > 0)) return reject('identity_changed');
      authentication = findAuthentication();
      for (const key of ['userProfile','accessToken','isSignedIn','arpSessionToken']) {
        const source = authentication?.[key];
        if (typeof source !== 'function') return reject('observer_unavailable');
        sources[key] = source;
      }
      globalThis.addEventListener('pagehide', cancelled, {once:true});
      if (cancelEvent) globalThis.addEventListener(cancelEvent, cancelled, {once:true});
      timer = setTimeout(() => reject('identity_pending'), Number.isFinite(timeoutMs) ? Math.max(1,Math.min(timeoutMs,2000)) : 1500);
      tick = setInterval(inspect, 50);
      for (const [key, source] of Object.entries(sources)) {
        const stop = source(value => { if (!finished) { values[key] = value;inspect(); } });
        if (typeof stop !== 'function') return reject('observer_unavailable');
        // A synchronous callback can reject before subscribe returns its stop.
        if (finished) { try { stop(); } catch {} return; }
        stops.push(stop);
      }
      subscribed = true;inspect();
    } catch { reject('observer_unavailable'); }
  });
}

export async function verifyMediaSession(page, media, identity, {signal, timeoutMs=1500} = {}) {
  if (signal?.aborted) throw cancelledError();
  const token = media?.cookie?.split(';').map(item=>item.trim()).find(item=>item.startsWith('sherlockToken='))?.slice('sherlockToken='.length);
  const fail = reason => {
    const error = new RegistrationError('media_session_incomplete');
    error.diagnostics = {media_runtime:{state:'changed',readiness_reason:reason}};
    throw error;
  };
  if (media?.media_session_arp_mode !== 'present' || !isCompleteMediaSession(media) || !isUsableSherlock(token)) fail('runtime_changed');
  const waitMs = Number.isFinite(timeoutMs) ? Math.max(1,Math.min(timeoutMs,2000)) : 1500;
  const cancelEvent = `cfm-adobe-verify-cancel-${randomUUID()}`;
  let watchdog, rejectCancelled;
  const cancelPageWait = () => {
    void page.evaluate(name => globalThis.dispatchEvent(new Event(name)), cancelEvent).catch(() => {});
  };
  const onAbort = () => { cancelPageWait();rejectCancelled(cancelledError()); };
  const cancellation = new Promise((_, reject) => { rejectCancelled = reject; });
  signal?.addEventListener('abort', onAbort, {once:true});
  const bounded = new Promise(resolve => {
    watchdog = setTimeout(() => { cancelPageWait();resolve({matches:false,reason:'verification_timeout'}); }, waitMs+1000);
  });
  try {
    let result;
    try { result = await Promise.race([page.evaluate(mediaSessionMatchesInPage, {token, identity, runtimeProof:media.runtime_proof,timeoutMs:waitMs,cancelEvent}), cancellation, bounded]); }
    catch (error) {
      if (signal?.aborted) throw cancelledError();
      if (error instanceof RegistrationError) throw error;
      fail('observer_unavailable');
    }
    if (signal?.aborted) throw cancelledError();
    if (result !== true) {
      const reasons = ['identity_pending','identity_changed','runtime_changed','observer_unavailable','verification_timeout'];
      fail(reasons.includes(result?.reason) ? result.reason : 'runtime_changed');
    }
  } finally {
    clearTimeout(watchdog);signal?.removeEventListener('abort', onAbort);
  }
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
      cancelPageWait();
      const error = new RegistrationError('media_session_incomplete');
      error.diagnostics = {media_runtime:{state:'observation_timeout'}};
      reject(error);
    }, timeoutMs + 1000);
  });
  try {
    return await Promise.race([page.evaluate(runtimeARPInPage, {timeoutMs, cancelEvent}), cancellation, bounded]);
  } catch (error) {
    if (signal?.aborted) throw cancelledError();
    if (error instanceof RegistrationError) throw error;
    return {state:'unavailable',readiness_reason:'evaluation_failed'};
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
  let runtimeState = {state:'unavailable',readiness_reason:'navigation_budget_exhausted'}, mediaPage, mediaTiming;
  if (!atEditor) {
    try { await page.goto(FIREFLY_IMAGE, {waitUntil:'domcontentloaded', timeout:Math.max(1, deadline-now())}); }
    catch (error) {
      if (signal?.aborted) throw cancelledError();
      if (now() < deadline) throw error;
      const incomplete = new RegistrationError('media_session_incomplete');
      incomplete.diagnostics = {media_runtime:runtimeState};
      throw incomplete;
    }
  }
  while (now() < deadline) {
    if (signal?.aborted) throw cancelledError();
    const observation = await observeRuntimeARP(page, Math.max(1, deadline-now()), signal);
    if (signal?.aborted) throw cancelledError();
    runtimeState = {state:observation?.state || 'unavailable', ...(observation?.value_kind?{value_kind:observation.value_kind}:{}),
      ...(observation?.readiness_reason?{readiness_reason:observation.readiness_reason}:{}),
      ...(Number.isInteger(observation?.vendors_started)?{vendors_started:observation.vendors_started}:{}),
      ...(Number.isInteger(observation?.vendors_complete)?{vendors_complete:observation.vendors_complete}:{})};
    mediaPage=observation?.media_page;
    mediaTiming=observation?.media_timing;
    const present = observation?.state === 'ready' && isUsableSherlock(observation.token);
    if (observation?.source === 'spl_runtime' && present) {
      const cookies = await context.cookies(FIREFLY_HOME);
      if (signal?.aborted) throw cancelledError();
      return {cookie:mediaCookieHeader(cookies, observation.token),
        media_session_arp_mode:'present', media_session_arp_source:'spl_runtime', runtime_state:runtimeState, runtime_proof:observation.runtime_proof};
    }
    if (observation?.state === 'invalid' || observation?.state === 'invalid_contract') break;
    const remaining = deadline-now();
    if (remaining > 0) await pauseAbortably(Math.min(250, remaining), pause, signal);
  }
  const error = new RegistrationError('media_session_incomplete');
  error.diagnostics = {media_runtime:runtimeState,...(mediaPage?{media_page:mediaPage}:{}),...(mediaTiming?{media_timing:mediaTiming}:{})};
  throw error;
}
