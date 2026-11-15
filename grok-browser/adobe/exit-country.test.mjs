import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { PrivateRunStore } from './core.mjs';
import { CreationTracker, creationOutcome } from './creation-outcome.mjs';
import { countryLookupConfig, lookupExitCountry, prepareRegistrationCountry, validCountry } from './exit-country.mjs';

function processFixture(output, exitCode = 0, hang = false) {
  const seen = {};
  const spawnProcess = (command, args, options) => {
    Object.assign(seen, { command, args, options, kills: 0, stdin: '' });
    const child = new EventEmitter();
    child.stdin = new PassThrough(); child.stdout = new PassThrough();
    child.stdin.on('data', chunk => { seen.stdin += chunk; });
    child.kill = () => { seen.kills++; queueMicrotask(() => child.emit('close', null)); };
    child.stdin.on('finish', () => {
      if (hang) return;
      child.stdout.write(output.replace(/\n(\d{3})$/, '\nCFM_COUNTRY_META:$1 200'));
      queueMicrotask(() => child.emit('close', exitCode));
    });
    return child;
  };
  return { seen, spawnProcess, now: () => 0 };
}

test('bound proxy is explicit, credentials stay off argv, and no browser context or environment proxy is reused', async () => {
  for (const [input, expected] of [
    ['direct', 'proxy = ""'],
    ['http://user:secret@127.0.0.1:1234/', 'proxy = "http://user:secret@127.0.0.1:1234/"'],
    ['https://user:secret@127.0.0.1:1234', 'proxy = "https://user:secret@127.0.0.1:1234/"'],
    ['socks5://user:p%40ss@127.0.0.1:1234', 'proxy = "socks5h://user:p%40ss@127.0.0.1:1234"'],
    ['socks5h://user:secret@[::1]:1234', 'proxy = "socks5h://user:secret@[::1]:1234"'],
    ['http://fixture%253Auser:fake%252Fpass@127.0.0.1:1234', 'proxy = "http://fixture%253Auser:fake%252Fpass@127.0.0.1:1234/"'],
  ]) {
    const f = processFixture('{"success":true,"country_code":"JP","ip":"DO_NOT_RETAIN"}\n200');
    const result = await lookupExitCountry(input, f);
    assert.deepEqual(result, { country: 'JP', source: 'ipwho_is', diagnostics: { failure_kind: 'none', curl_exit_code: 0, http_status: 200, connect_status: 200, elapsed_ms: 0 } });
    assert.ok(f.seen.stdin.includes(expected), f.seen.stdin);
    assert.equal(f.seen.command, 'curl'); assert.equal(f.seen.args[0], '-q');
    assert.ok(!f.seen.args.join(' ').includes('secret'));
    assert.ok(!Object.keys(f.seen.options.env).some(k => /^(https?|all|no)_proxy$/i.test(k)));
    assert.equal(f.seen.options.shell, false);
    assert.ok(!/cookie|Authorization|Adobe/i.test(f.seen.stdin));
    assert.ok(!f.seen.args.includes('--location'));
  }
  for (const proxy of ['https://host/other', 'http://x/?secret', 'file:///tmp/private', 'http://x\nCookie: private']) {
    assert.throws(() => countryLookupConfig(proxy), { code: 'browser_proxy_invalid' });
  }
});

test('redirect, invalid country, failed service, huge body and proxy failure produce explicit US fallback without another request', async () => {
  for (const [body, code] of [
    ['{"country_code":"JP"}\n302', 0], ['{"country_code":"ZZ"}\n200', 0],
    ['{"country_code":["JP"]}\n200', 0], ['{"country_code":"jp"}\n200', 0],
    ['{"success":false,"country_code":"JP"}\n200', 0], ['not json\n200', 0],
    ['{"success":"false","country_code":"JP"}\n200', 0], ['{"country_code":"JP"}\n200', 0],
    ['{"country_code":"JP"}\n200', 7], ['x'.repeat(65541), 0],
  ]) {
    const f = processFixture(body, code);
    const result = await lookupExitCountry('http://proxy.invalid:1234', f);
    assert.equal(result.country, 'US'); assert.equal(result.source, 'default_us');
    assert.notEqual(result.diagnostics.failure_kind, 'none');
    assert.equal(JSON.stringify(result).includes('PRIVATE'), false);
    assert.ok(f.seen.stdin.includes('proxy.invalid'));
  }
  assert.equal(validCountry('ZZ'), false); assert.equal(validCountry('GB'), true);
});

test('deadline kills the lookup and cancellation never persists fallback as a completed lookup', async () => {
  const timeout = processFixture('', 0, true);
  assert.deepEqual(await lookupExitCountry('direct', { ...timeout, timeoutMs: 15 }), { country: 'US', source: 'default_us',
    diagnostics: { failure_kind: 'timeout', http_status: 0, connect_status: 0, elapsed_ms: 0 } });
  assert.equal(timeout.seen.kills, 1);
  const abort = new AbortController(), f = processFixture('', 0, true);
  const pending = lookupExitCountry('direct', { ...f, signal: abort.signal });
  abort.abort();
  await assert.rejects(pending, { name: 'AbortError' });
  assert.equal(f.seen.kills, 1);
  const already = new AbortController(); already.abort();
  await assert.rejects(lookupExitCountry('direct', { signal: already.signal, spawnProcess: () => assert.fail('aborted lookup spawned') }), { name: 'AbortError' });
  const duringSpawn = new AbortController();
  await assert.rejects(lookupExitCountry('direct', { signal: duringSpawn.signal, spawnProcess: () => { duringSpawn.abort(); throw new Error('spawn failed'); } }), { name: 'AbortError' });
  const spawnError = new AbortController();
  await assert.rejects(lookupExitCountry('direct', { signal: spawnError.signal, spawnProcess: () => {
    const child = new EventEmitter(); child.stdin = new PassThrough(); child.stdout = new PassThrough(); child.kill = () => {};
    queueMicrotask(() => { spawnError.abort(); child.emit('error', new Error('spawn failed')); });
    return child;
  } }), { name: 'AbortError' });
});

async function storeFixture(t, profile = { country: 'US', birthYear: 1988, birthMonth: 6, firstName: 'Fixture' }) {
  const root = await mkdtemp(join(tmpdir(), 'adobe-country-'));
  const store = new PrivateRunStore(root, 'fixture');
  await store.init({ email: 'fixture@example.test', config: { profile, password: 'unrelated-private' } });
  t.after(async () => { await store.close(); await rm(root, { recursive: true, force: true }); });
  return store;
}

test('fresh selection is saved once with the full original identity, and repeated preparation cannot follow a rotating proxy', async t => {
  const store = await storeFixture(t);
  const options = { mode: 'registration', requireNew: true, proxy: 'socks5://fixed:credentials@proxy.invalid:1080' };
  const result = await prepareRegistrationCountry(store, options, async proxy => {
    assert.equal(proxy, options.proxy); return { country: 'GB', source: 'ipwho_is' };
  });
  const state = await store.load();
  assert.equal(state.config.profile.country, 'GB'); assert.equal(state.config.profile.countrySource, 'ipwho_is');
  assert.equal(state.config.profile.birthYear, 1988); assert.equal(state.config.password, 'unrelated-private');
  assert.deepEqual(await prepareRegistrationCountry(store, options, () => assert.fail('same profile was probed again')), result);
});

test('resume, previously loaded profiles and accepted submissions preserve their original country without lookup', async t => {
  const store = await storeFixture(t, { country: 'CA' });
  for (const options of [{ mode: 'resume', requireNew: false }, { mode: 'registration', requireNew: false }]) {
    assert.deepEqual(await prepareRegistrationCountry(store, options, () => assert.fail('saved profile was re-geolocated')), { country: 'CA', source: 'preserved' });
  }
  await store.markSubmissionStarted();
  assert.deepEqual(await prepareRegistrationCountry(store, { mode: 'registration', requireNew: true }, () => assert.fail('submitted identity changed')), { country: 'CA', source: 'preserved' });
  assert.equal((await store.load()).config.profile.countrySource, undefined);
});

test('lookup cancellation and invalid results leave registration profile unchanged', async t => {
  const store = await storeFixture(t);
  const options = { mode: 'registration', requireNew: true };
  await assert.rejects(prepareRegistrationCountry(store, options, async () => ({ country: 'ZZ', source: 'ipwho_is' })), { code: 'registration_country_invalid' });
  await assert.rejects(prepareRegistrationCountry(store, options, async () => ({ country: 'US', source: 'PRIVATE' })), { code: 'registration_country_invalid' });
  await assert.rejects(prepareRegistrationCountry(store, options, async () => ({ country: 'JP', source: 'default_us' })), { code: 'registration_country_invalid' });
  const abort = new AbortController();
  await assert.rejects(prepareRegistrationCountry(store, { ...options, signal: abort.signal }, async () => {
    abort.abort(); return { country: 'JP', source: 'ipwho_is' };
  }), { name: 'AbortError' });
  assert.equal((await store.load()).config.profile.countrySource, undefined);
});


test('fixed curl metadata distinguishes fast CONNECT failures from slow lookup without exposing output', async () => {
  for (const [raw, exitCode, kind, httpStatus, connectStatus] of [
    ['\nCFM_COUNTRY_META:000 502', 22, 'proxy_tunnel', 0, 502],
    ['\nCFM_COUNTRY_META:000 407', 56, 'proxy_auth', 0, 407],
    ['PRIVATE_BODY\nCFM_COUNTRY_META:429 200', 22, 'http_error', 429, 200],
    ['PRIVATE_BODY\nCFM_COUNTRY_META:302 200', 0, 'http_redirect', 302, 200],
    ['\nCFM_COUNTRY_META:000 000', 28, 'timeout', 0, 0],
    ['\nCFM_COUNTRY_META:000 000', 7, 'proxy_connection', 0, 0],
    ['\nCFM_COUNTRY_META:000 000', 6, 'dns', 0, 0],
    ['\nCFM_COUNTRY_META:000 200', 60, 'tls', 0, 200],
    ['{"success":false,"message":"PRIVATE_BODY"}\nCFM_COUNTRY_META:200 200', 0, 'lookup_rejected', 200, 200],
    ['PRIVATE_BODY\nCFM_COUNTRY_META:200 200', 0, 'invalid_response', 200, 200],
    ['PRIVATE_BODY', 0, 'invalid_response', 0, 0],
    ['x'.repeat(66_000), 0, 'response_too_large', 0, 0],
  ]) {
    const fixture = processFixture(raw, exitCode);
    const result = await lookupExitCountry('http://fixture:PRIVATE_PROXY@127.0.0.1:1', fixture);
    assert.equal(result.source, 'default_us');
    assert.deepEqual(result.diagnostics, { failure_kind: kind,
      ...(kind === 'response_too_large' ? {} : { curl_exit_code: exitCode }),
      http_status: httpStatus, connect_status: connectStatus, elapsed_ms: 0 });
    assert.equal(JSON.stringify(result).includes('PRIVATE'), false);
    assert.equal(fixture.seen.args[fixture.seen.args.indexOf('--max-time') + 1], '15');
    assert.equal(fixture.seen.args[fixture.seen.args.indexOf('--connect-timeout') + 1], '15');
    assert.equal(fixture.seen.args.includes('--retry'), false);
  }
  const direct = await lookupExitCountry('direct', processFixture('', 7));
  assert.equal(direct.diagnostics.failure_kind, 'connection_failed');
  const missing = await lookupExitCountry('direct', { now: () => 0, spawnProcess() { throw new Error('PRIVATE_PATH'); } });
  assert.deepEqual(missing.diagnostics, { failure_kind: 'spawn_failed', http_status: 0, connect_status: 0, elapsed_ms: 0 });
});

test('lookup diagnostic survives real store reload and resume without another request or changed country', async t => {
  const store = await storeFixture(t);
  const selected = await prepareRegistrationCountry(store, { mode: 'registration', requireNew: true, proxy: 'http://127.0.0.1:1' },
    proxy => lookupExitCountry(proxy, processFixture('\nCFM_COUNTRY_META:000 502', 22)));
  assert.equal(selected.diagnostics.failure_kind, 'proxy_tunnel');
  assert.equal((await store.load()).config.profile.countryDiagnostics.connect_status, 502);
  await store.markSubmissionStarted();
  await store.close();
  const reload = new PrivateRunStore(store.root, store.runId);
  await reload.init({ email: 'fixture@example.test' }, { requireExisting: true });
  const saved = await prepareRegistrationCountry(reload, { mode: 'resume', requireNew: false, proxy: 'direct' },
    () => assert.fail('saved identity was probed again'));
  assert.deepEqual(saved, selected);
  await reload.close();
});

test('untrusted diagnostics are discarded without rejecting a valid new country', async t => {
  const base = { country: 'US', source: 'default_us', diagnostics: { failure_kind: 'timeout', http_status: 0, connect_status: 0, elapsed_ms: 15 } };
  for (const diagnostics of [null, [], 'PRIVATE', { failure_kind: 'timeout' },
    ...[{ failure_kind: 'PRIVATE' }, { failure_kind: 'none' }, { elapsed_ms: -1 }, { http_status: 999 },
      { connect_status: '502' }, { curl_exit_code: null }, { curl_exit_code: 300 }, { raw: 'PRIVATE' }].map(patch => ({ ...base.diagnostics, ...patch }))]) {
    const store = await storeFixture(t);
    assert.deepEqual(await prepareRegistrationCountry(store, { mode: 'registration', requireNew: true }, async () =>
      ({ ...base, diagnostics })), { country: 'US', source: 'default_us' });
    const saved = (await store.load()).config.profile;
    assert.equal(saved.countrySource, 'default_us');
    assert.equal(saved.countryDiagnostics, undefined);
  }
});

test('malformed saved diagnostics cannot block accepted identity recovery or trigger another lookup', async t => {
  for (const countryDiagnostics of [null, [], 'PRIVATE', { failure_kind: 'timeout', http_status: 'PRIVATE', connect_status: 502, elapsed_ms: 15 },
    { failure_kind: 'timeout', http_status: 0, connect_status: 502, elapsed_ms: -1 }, { raw: 'PRIVATE' }]) {
    const store = await storeFixture(t, { country: 'US', countrySource: 'default_us', countryDiagnostics, birthYear: 1988 });
    await store.markSubmissionStarted();
    await new CreationTracker(store).accepted();
    await store.close();
    const reload = new PrivateRunStore(store.root, store.runId);
    await reload.init({ email: 'fixture@example.test' }, { requireExisting: true });
    try {
      const before = await reload.load();
      assert.deepEqual(await prepareRegistrationCountry(reload, { mode: 'resume', requireNew: false, proxy: 'direct' },
        () => assert.fail('accepted identity was probed again')), { country: 'US', source: 'default_us' });
      assert.deepEqual(await reload.load(), before, 'optional diagnostic changed saved identity or submission ledger');
      assert.equal(before.submission.started, true);
      assert.equal(creationOutcome(before).creation_outcome, 'accepted');
    } finally { await reload.close(); }
  }
});
