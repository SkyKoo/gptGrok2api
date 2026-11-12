import test from 'node:test';
import assert from 'node:assert/strict';
import { diagnosticRequest, hasCaptchaRequirement, NetworkDiagnostics, verificationDiagnosis } from './network-diagnostics.mjs';
import { isTrustedAdobeURL } from './core.mjs';

const arkURL = 'https://arks-client.adobe.com/fc/gt2/public_key/PRIVATE_KEY?token=PRIVATE_TOKEN';
const accountURL = 'https://auth.services.adobe.com/signin/v2/accounts?email=PRIVATE_EMAIL';
const start = Date.parse('2026-11-10T05:00:00Z');

function request({ url = arkURL, method = 'POST', status = 200, error = null,
  body = {}, elapsed = 100, offset = 0, contentType = 'application/json', page = {}, bodyReader } = {}) {
  const response = {
    status: () => status,
    headers: () => ({ 'content-type': contentType }),
    body: bodyReader || (async () => Buffer.from(JSON.stringify(body))),
  };
  return {
    url: () => url, method: () => method,
    timing: () => ({ startTime: start + offset, responseEnd: elapsed }),
    failure: () => error ? { errorText: error } : null,
    response: async () => response,
    frame: () => ({ page: () => page }),
  };
}

test('diagnostic URLs redact dynamic identifiers and do not expand trusted verification links', () => {
  assert.deepEqual(diagnosticRequest(arkURL, 'POST'), {
    method: 'POST', path: 'https://arks-client.adobe.com/fc/gt2/public_key/:key',
  });
  assert.equal(isTrustedAdobeURL(arkURL), false);
  assert.deepEqual(diagnosticRequest('https://auth.services.adobe.com/users/PRIVATE_ID?token=PRIVATE_TOKEN', 'GET'), {
    method: 'GET', path: 'https://auth.services.adobe.com/:resource',
  });
  for (const url of ['https://arks-client.adobe.com.example.test/fc/a', 'http://arks-client.adobe.com/fc/a',
    'https://user:secret@arks-client.adobe.com/fc/a', 'https://arks-client.adobe.com:8443/fc/a', 'not a url']) {
    assert.equal(diagnosticRequest(url, 'POST'), null);
  }
});

test('DNS, timeout and explicit abort are distinct and retain only normalized metadata', async () => {
  for (const [error, code, elapsed] of [
    ['net::ERR_NAME_NOT_RESOLVED', 'verification_dns_failed', 0],
    ['net::ERR_TIMED_OUT', 'verification_request_timeout', 20000],
    ['net::ERR_ABORTED', 'verification_request_aborted', 20000],
  ]) {
    const diagnostics = new NetworkDiagnostics({now: () => start + 30_000});
    const page = {};
    await diagnostics.record(request({ error, elapsed }), page);
    const events = diagnostics.snapshot(page);
    assert.deepEqual(events, [{ method: 'POST', path: 'https://arks-client.adobe.com/fc/gt2/public_key/:key',
      time: '2026-11-10T05:00:00.000Z', elapsedMs: elapsed, errorCode: error.replace('net::', ''),
      observedAt:'2026-11-10T05:00:30.000Z', source:'retained' }]);
    assert.deepEqual(verificationDiagnosis(events), { code });
    assert.doesNotMatch(JSON.stringify(events), /PRIVATE|token|public_key\/[A-Z]/);
  }
});

test('only an exact signup error code in its completed JSON response establishes captcha', async () => {
  const diagnostics = new NetworkDiagnostics({now: () => start + 500});
  const page = {};
  await diagnostics.record(request({ url: accountURL, status: 400,
    body: { errorCode: 'captcha_required', email: 'PRIVATE_EMAIL', token: 'PRIVATE_TOKEN', message: 'PRIVATE_BODY' } }), page);
  const events = diagnostics.snapshot(page);
  assert.deepEqual(events, [{ method: 'POST', path: 'https://auth.services.adobe.com/signin/v2/accounts',
    time: '2026-11-10T05:00:00.000Z', elapsedMs: 100, status: 400, errorCode: 'captcha_required',
    observedAt:'2026-11-10T05:00:00.500Z', source:'retained' }]);
  assert.equal(hasCaptchaRequirement(events), true);
  assert.deepEqual(verificationDiagnosis(events), { code: 'captcha_required' });
  assert.doesNotMatch(JSON.stringify(events), /PRIVATE|email|token|message|body/);
  for (const changes of [{ method: 'GET' }, { status: 403 }, { contentType: 'text/html' },
    { url: 'https://arks-client.adobe.com/fc/a' }, { body: { errorCode: 'PRIVATE_TOKEN', message: 'captcha_required' } }]) {
    const other = new NetworkDiagnostics();
    await other.record(request({ url: accountURL, status: 400, body: { errorCode: 'captcha_required' }, ...changes }), page);
    assert.equal(hasCaptchaRequirement(other.snapshot(page)), false);
    assert.doesNotMatch(JSON.stringify(other.snapshot(page)), /PRIVATE/);
  }
});

test('a successful retry clears only that endpoint failure and old tabs cannot affect the current page', async () => {
  const diagnostics = new NetworkDiagnostics();
  const page = {};
  const otherPage = {};
  await diagnostics.record(request({ url: accountURL, status: 400, body: { errorCode: 'captcha_required' } }), page);
  await diagnostics.record(request({ error: 'net::ERR_NAME_NOT_RESOLVED', offset: 100 }), page);
  await diagnostics.record(request({ url: 'https://arks-client.adobe.com/assets/api.js', method: 'GET', offset: 200 }), page);
  assert.equal(verificationDiagnosis(diagnostics.snapshot(page)).code, 'verification_dns_failed');
  await diagnostics.record(request({ status: 200, offset: 300 }), page);
  assert.equal(verificationDiagnosis(diagnostics.snapshot(page)).code, 'captcha_required');
  await diagnostics.record(request({ url: accountURL, status: 200, offset: 400 }), page);
  assert.equal(hasCaptchaRequirement(diagnostics.snapshot(page)), false);
  assert.equal(verificationDiagnosis(diagnostics.snapshot(page)).code, null);
  await diagnostics.record(request({ error: 'net::ERR_TIMED_OUT', offset: 500 }), otherPage);
  assert.equal(verificationDiagnosis(diagnostics.snapshot(page)).code, null);
  assert.equal(verificationDiagnosis(diagnostics.snapshot(otherPage)).code, 'verification_request_timeout');
});

test('retained requests backfill once without waiting for unfinished requests or repeating responses', async () => {
  const diagnostics = new NetworkDiagnostics();
  let responseReads = 0;
  const completed = request({ url: accountURL, status: 400, bodyReader: async () => {
    responseReads++;
    return Buffer.from('{"errorCode":"captcha_required"}');
  } });
  const pending = request({ elapsed: -1 });
  pending.response = async () => assert.fail('Do not await an unfinished response');
  const page = { requests: async () => [pending, completed] };
  await diagnostics.capture(page);
  await diagnostics.capture(page);
  assert.equal(responseReads, 1);
  assert.equal(diagnostics.snapshot(page).length, 1);
  assert.equal(verificationDiagnosis(diagnostics.snapshot(page)).code, 'captcha_required');
});

test('status capture waits for an existing response observation and context listeners attach once', async () => {
  const diagnostics = new NetworkDiagnostics();
  const listeners = new Map();
  const context = { on: (event, callback) => {
    assert.equal(listeners.has(event), false);
    listeners.set(event, callback);
  } };
  diagnostics.attach(context);
  diagnostics.attach(context);
  let releaseBody;
  const bodyReady = new Promise(resolve => { releaseBody = resolve; });
  const page = {};
  const completed = request({ page, url: accountURL, status: 400, bodyReader: () => bodyReady });
  page.requests = async () => [completed];
  listeners.get('requestfinished')(completed);
  const capture = diagnostics.capture(page);
  releaseBody(Buffer.from('{"errorCode":"captcha_required"}'));
  await capture;
  assert.equal(verificationDiagnosis(diagnostics.snapshot(page)).code, 'captcha_required');
  assert.equal(diagnostics.snapshot(page).length, 1);
});

test('unknown network error strings are redacted and history stays bounded', async () => {
  const diagnostics = new NetworkDiagnostics();
  const page = {};
  for (let offset = 0; offset < 60; offset++) {
    await diagnostics.record(request({ error: 'private URL https://example.test/?token=PRIVATE_TOKEN', offset }), page);
  }
  assert.equal(diagnostics.snapshot(page).length, 40);
  assert.equal(diagnostics.snapshot(page)[0].errorCode, 'NETWORK_ERROR');
  assert.doesNotMatch(JSON.stringify(diagnostics.snapshot(page)), /PRIVATE|example.test/);
});

test('superseding captcha uses exact observed events and preserves history, new events, other pages and network errors', async () => {
  const diagnostics=new NetworkDiagnostics();
  const page={},otherPage={};
  const captcha=()=>request({url:accountURL,status:400,body:{errorCode:'captcha_required'}});
  await diagnostics.record(captcha(),page);
  await diagnostics.record(request({error:'net::ERR_NAME_NOT_RESOLVED'}),page);
  const observed=diagnostics.activeSnapshot(page);
  await diagnostics.record(captcha(),page);
  await diagnostics.record(captcha(),otherPage);
  assert.equal(diagnostics.supersedeCaptchaEvidence(page,observed),1);
  assert.equal(diagnostics.snapshot(page).length,3);
  assert.equal(diagnostics.activeSnapshot(page).length,2);
  assert.equal(hasCaptchaRequirement(diagnostics.activeSnapshot(page)),true);
  assert.equal(hasCaptchaRequirement(diagnostics.activeSnapshot(otherPage)),true);
  assert.equal(verificationDiagnosis(diagnostics.activeSnapshot(page)).code,'verification_dns_failed');
  assert.equal(diagnostics.supersedeCaptchaEvidence(page,observed.map(event=>({...event}))),0);
});

test('requests cancelled after a top-level navigation retain old-document ownership without blocking the current page', async () => {
  let now=start;
  const diagnostics=new NetworkDiagnostics({now:()=>now});
  const pageEvents=new Map(),contextEvents=new Map();
  const frame={};
  const page={on:(name,handler)=>pageEvents.set(name,handler),mainFrame:()=>frame};
  diagnostics.attach({pages:()=>[page],on:(name,handler)=>contextEvents.set(name,handler)});
  const old=request({page,error:'net::ERR_ABORTED'});
  contextEvents.get('request')(old);
  now+=1000;
  pageEvents.get('framenavigated')(frame);
  await diagnostics.record(old,page,true);
  assert.equal(verificationDiagnosis(diagnostics.activeSnapshot(page)).code,null);
  assert.equal(diagnostics.snapshot(page)[0].errorCode,'ERR_ABORTED');
  const current=request({page,error:'net::ERR_ABORTED',offset:1001});
  contextEvents.get('request')(current);
  await diagnostics.record(current,page,true);
  assert.equal(verificationDiagnosis(diagnostics.activeSnapshot(page)).code,'verification_request_aborted');
  pageEvents.get('framenavigated')({});
  assert.equal(verificationDiagnosis(diagnostics.activeSnapshot(page)).code,'verification_request_aborted');
});

test('late retained old-document requests and delayed response parsing do not revive stale captcha', async () => {
  let now=start;
  const diagnostics=new NetworkDiagnostics({now:()=>now});
  const events=new Map(),frame={};
  const page={on:(name,handler)=>events.set(name,handler),mainFrame:()=>frame};
  diagnostics.attachPage(page);
  let release;
  const pending=request({page,url:accountURL,status:400,bodyReader:()=>new Promise(resolve=>{release=resolve;})});
  const operation=diagnostics.record(pending,page,true);
  await Promise.resolve();
  now+=1000;events.get('framenavigated')(frame);
  release(Buffer.from('{"errorCode":"captcha_required"}'));
  await operation;
  await diagnostics.record(request({page,error:'net::ERR_ABORTED',offset:500}),page,true);
  assert.equal(diagnostics.snapshot(page).length,2);
  assert.equal(diagnostics.activeSnapshot(page).length,0);
});

test('event observation keeps its original timestamp when a retained capture later sees the same request', async () => {
  let now = start + 1_000;
  const diagnostics = new NetworkDiagnostics({now: () => now});
  const listeners = new Map(), page = {};
  diagnostics.attach({on: (name, listener) => listeners.set(name, listener)});
  const aborted = request({page, error:'net::ERR_ABORTED'});
  listeners.get('requestfailed')(aborted);
  page.requests = async () => [aborted];
  await diagnostics.capture(page);
  now += 30_000;
  await diagnostics.capture(page);
  assert.equal(diagnostics.snapshot(page).length, 1);
  assert.equal(diagnostics.snapshot(page)[0].source, 'event');
  assert.equal(diagnostics.snapshot(page)[0].observedAt, '2026-11-10T05:00:01.000Z');
});

test('retained-only observation does not claim the browser failure happened at capture time', async () => {
  const diagnostics = new NetworkDiagnostics({now: () => start + 60_000});
  const page = {requests: async () => [request({error:'net::ERR_ABORTED', elapsed:20_000})]};
  await diagnostics.capture(page);
  assert.deepEqual(diagnostics.snapshot(page).map(({time, elapsedMs, observedAt, source}) => ({time, elapsedMs, observedAt, source})),
    [{time:'2026-11-10T05:00:00.000Z',elapsedMs:20_000,observedAt:'2026-11-10T05:01:00.000Z',source:'retained'}]);
});
