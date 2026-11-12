import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { PrivateRunStore } from './core.mjs';
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
      child.stdout.write(output);
      queueMicrotask(() => child.emit('close', exitCode));
    });
    return child;
  };
  return { seen, spawnProcess };
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
    assert.deepEqual(result, { country: 'JP', source: 'ipwho_is' });
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
    assert.deepEqual(await lookupExitCountry('http://proxy.invalid:1234', f), { country: 'US', source: 'default_us' });
    assert.ok(f.seen.stdin.includes('proxy.invalid'));
  }
  assert.equal(validCountry('ZZ'), false); assert.equal(validCountry('GB'), true);
});

test('deadline kills the lookup and cancellation never persists fallback as a completed lookup', async () => {
  const timeout = processFixture('', 0, true);
  assert.deepEqual(await lookupExitCountry('direct', { ...timeout, timeoutMs: 15 }), { country: 'US', source: 'default_us' });
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
  const abort = new AbortController();
  await assert.rejects(prepareRegistrationCountry(store, { ...options, signal: abort.signal }, async () => {
    abort.abort(); return { country: 'JP', source: 'ipwho_is' };
  }), { name: 'AbortError' });
  assert.equal((await store.load()).config.profile.countrySource, undefined);
});
