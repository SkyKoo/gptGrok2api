import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, readdir, writeFile, chmod, lstat, symlink, link, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { PrivateRunStore, RegistrationError, isTrustedAdobeURL, protocolProbe, writePrivateJSONOnce } from './core.mjs';

const temporaryDirectories = [];
after(async () => {
  await Promise.all(temporaryDirectories.map((path) => rm(path, { recursive: true, force: true })));
});
async function fixture() {
  const temporary = await mkdtemp(join(tmpdir(), 'adobe-core-test-'));
  temporaryDirectories.push(temporary);
  return { temporary, root: join(temporary, 'private') };
}
const codeIs = (code) => (error) => error instanceof RegistrationError && error.code === code;

test('new registration refuses every existing run without modifying its journal or lock', async () => {
  const { root } = await fixture();
  const first = new PrivateRunStore(root, 'first');
  await first.init({email:'first@example.test'}, {requireNew:true});
  await first.markSubmissionStarted();
  await first.close();
  const original = await readFile(first.statePath);
  for (const email of ['first@example.test','second@example.test']) {
    await assert.rejects(new PrivateRunStore(root, 'first').init({email}, {requireNew:true}),codeIs('run_already_exists'));
    assert.deepEqual(await readFile(first.statePath),original);
    await assert.rejects(lstat(first.lockPath),{code:'ENOENT'});
  }
  const second = new PrivateRunStore(root, 'second');
  await second.init({email:'second@example.test'}, {requireNew:true});
  assert.notEqual(first.runDir,second.runDir);
  await second.close();
  const resumed = new PrivateRunStore(root,'first');
  const state = await resumed.init({email:'first@example.test'},{requireExisting:true});
  assert.equal(state.submission.started,true);
  await resumed.close();
});

test('recovery requires an existing journal and never creates a replacement profile', async t => {
  const { root } = await fixture();
  const missing = new PrivateRunStore(root, 'missing');
  await assert.rejects(missing.init({email:'fixture@example.test'}, {requireExisting:true}), codeIs('state_missing'));
  await assert.rejects(lstat(root), {code:'ENOENT'});
  const original = new PrivateRunStore(root, 'one');
  await original.init({email:'fixture@example.test',config:{password:'saved-secret'}});
  await original.markSubmissionStarted();
  const started = (await original.load()).submission.startedAt;
  await original.close();
  const resumed = new PrivateRunStore(root, 'one');
  t.after(() => resumed.close());
  const state = await resumed.init({email:'fixture@example.test',config:{password:'replacement'}}, {requireExisting:true});
  assert.equal(state.submission.started,true);
  assert.equal(state.submission.startedAt,started);
  assert.equal(state.config.password,'saved-secret');
  await assert.rejects(resumed.markSubmissionStarted(),codeIs('submission_already_started'));
  const absent = new PrivateRunStore(root,'absent');
  await assert.rejects(absent.init({}, {requireExisting:true}),codeIs('state_missing'));
  await assert.rejects(lstat(absent.runDir),{code:'ENOENT'});
});

test('private export publishes mode 0600 and reuses identical bytes without replacing the inode', async (t) => {
  const { root } = await fixture(t);
  await mkdir(root, { mode: 0o700 });
  const payload = { accounts: [{ token: 'synthetic-private-token', email: 'a@example.test' }] };
  const path = await writePrivateJSONOnce(root, 'adobe2api-import.json', payload);
  assert.equal(path, join(root, 'adobe2api-import.json'));
  const first = await lstat(path);
  assert.equal(first.mode & 0o777, 0o600);
  assert.equal(first.nlink, 1);
  assert.equal(await readFile(path, 'utf8'), `${JSON.stringify(payload, null, 2)}\n`);
  assert.equal(await writePrivateJSONOnce(root, 'adobe2api-import.json', payload), path);
  assert.equal((await lstat(path)).ino, first.ino);
  assert.deepEqual(await readdir(root), ['adobe2api-import.json']);
});

test('private export refuses different content, links, and public files without modifying them', async (t) => {
  const { root } = await fixture(t);
  await mkdir(root, { mode: 0o700 });
  const original = await writePrivateJSONOnce(root, 'original.json', { token: 'original-secret' });
  const originalBytes = await readFile(original);
  await assert.rejects(writePrivateJSONOnce(root, 'original.json', { token: 'changed-secret' }), codeIs('private_file_conflict'));
  assert.deepEqual(await readFile(original), originalBytes);
  await symlink(original, join(root, 'symlink.json'));
  await assert.rejects(writePrivateJSONOnce(root, 'symlink.json', { token: 'original-secret' }), codeIs('unsafe_storage'));
  await link(original, join(root, 'hardlink.json'));
  await assert.rejects(writePrivateJSONOnce(root, 'hardlink.json', { token: 'original-secret' }), codeIs('unsafe_storage'));
  await writeFile(join(root, 'public.json'), originalBytes, { mode: 0o644 });
  await assert.rejects(writePrivateJSONOnce(root, 'public.json', { token: 'original-secret' }), codeIs('unsafe_storage'));
  assert.deepEqual(await readFile(original), originalBytes);
  assert.equal((await readdir(root)).some((name) => name.startsWith('.export-')), false);
});

test('private export rejects unsafe paths, oversized output, and shared directories before publishing', async (t) => {
  const { root } = await fixture(t);
  await mkdir(root, { mode: 0o700 });
  for (const filename of ['../outside.json', '/outside.json', '.', '..', 'nested/file.json', 'a\\b.json']) {
    await assert.rejects(writePrivateJSONOnce(root, filename, {}), codeIs('invalid_private_filename'));
  }
  await assert.rejects(writePrivateJSONOnce(root, 'large.json', { value: 'x'.repeat(1024 * 1024) }), codeIs('private_file_too_large'));
  await chmod(root, 0o755);
  await assert.rejects(writePrivateJSONOnce(root, 'public-dir.json', {}), codeIs('unsafe_storage'));
  assert.deepEqual(await readdir(root), []);
});

test('trusted URL check uses exact Adobe hosts and HTTPS', () => {
  assert.equal(isTrustedAdobeURL('https://firefly.adobe.com/'), true);
  assert.equal(isTrustedAdobeURL('https://auth-light.identity.adobe.com/auth/v2?state=example'), true);
  for (const value of [
    'http://firefly.adobe.com/', 'https://firefly.adobe.com.evil.test/',
    'https://evil.test@firefly.adobe.com/', 'https://firefly.adobe.com:8443/',
    'https://firefly.adobe.com./', 'https://other.adobe.com/', 'javascript:alert(1)',
    ' https://firefly.adobe.com/', 'https://firefly.adobe.com\\@evil.test/',
    'https://firefly.adobe.com/\n', null,
  ]) assert.equal(isTrustedAdobeURL(value), false, String(value));
});

test('private journal round-trips secrets without returning them in errors', async (t) => {
  const { root } = await fixture(t);
  const store = new PrivateRunStore(root, 'one');
  t.after(() => store.close());
  const initial = await store.init({ email: 'person@example.test', config: { password: 'private-example' } });
  assert.equal(initial.submission.started, false);
  assert.equal((await lstat(root)).mode & 0o777, 0o700);
  assert.equal((await lstat(store.runDir)).mode & 0o777, 0o700);
  assert.equal((await lstat(store.statePath)).mode & 0o777, 0o600);
  assert.equal((await lstat(store.lockPath)).mode & 0o777, 0o600);
  await store.setStage('form_ready');
  await store.save({ result: { cookie: 'private-cookie' } });
  const loaded = await store.load();
  loaded.config.password = 'mutated';
  assert.equal((await store.load()).config.password, 'private-example');
  await assert.rejects(store.setStage('private-cookie'), codeIs('invalid_state'));
  assert.equal((await store.load()).stage, 'form_ready');
});

test('competing writers cannot acquire an existing run lock', async (t) => {
  const { root } = await fixture(t);
  const first = new PrivateRunStore(root, 'one');
  const second = new PrivateRunStore(root, 'one');
  t.after(() => first.close());
  t.after(() => second.close());
  await first.init();
  await assert.rejects(second.init(), codeIs('run_locked'));
  await first.close();
  assert.equal((await second.init()).stage, 'initialized');
});

test('stale-looking locks are never deleted automatically', async (t) => {
  const { root } = await fixture(t);
  await mkdir(join(root, 'one'), { recursive: true, mode: 0o700 });
  const lock = join(root, 'one', '.lock');
  await writeFile(lock, JSON.stringify({ pid: 99999999 }), { mode: 0o600 });
  const store = new PrivateRunStore(root, 'one');
  await assert.rejects(store.init(), codeIs('run_locked'));
  assert.match(await readFile(lock, 'utf8'), /99999999/);
});

test('concurrent submission markers persist exactly one accepted submission', async (t) => {
  const { root } = await fixture(t);
  const store = new PrivateRunStore(root, 'one');
  t.after(() => store.close());
  await store.init();
  const outcomes = await Promise.allSettled([store.markSubmissionStarted(), store.markSubmissionStarted()]);
  assert.equal(outcomes.filter((item) => item.status === 'fulfilled').length, 1);
  assert.equal(outcomes.find((item) => item.status === 'rejected').reason.code, 'submission_already_started');
  await assert.rejects(store.save({ submission: { started: false } }), codeIs('protected_state_field'));
  await assert.rejects(store.setStage('form_ready'), codeIs('invalid_transition'));
  const saved = JSON.parse(await readFile(store.statePath, 'utf8'));
  assert.equal(saved.submission.started, true);
});

test('reopening a submitted run becomes unknown and cannot replay', async (t) => {
  const { root } = await fixture(t);
  const first = new PrivateRunStore(root, 'one');
  await first.init({ email: 'first@example.test' });
  await first.markSubmissionStarted();
  await first.close();
  const resumed = new PrivateRunStore(root, 'one');
  t.after(() => resumed.close());
  const state = await resumed.init({ email: 'first@example.test' });
  assert.equal(state.email, 'first@example.test');
  assert.equal(state.stage, 'outcome_unknown');
  assert.equal(state.submission.outcome, 'unknown');
  await assert.rejects(resumed.markSubmissionStarted(), codeIs('submission_already_started'));
});

test('private profile and resume cannot silently switch the selected email', async (t) => {
  const { root } = await fixture(t);
  const store = new PrivateRunStore(root, 'one');
  await store.init({ email: 'first@example.test', config: { email: 'first@example.test', name: 'Example' } });
  await assert.rejects(store.save({ email: 'second@example.test' }), codeIs('run_email_mismatch'));
  await assert.rejects(store.save({ config: { email: 'second@example.test' } }), codeIs('run_email_mismatch'));
  await store.setStage('awaiting_password');
  await store.setStage('awaiting_approval');
  await store.markSubmissionStarted();
  await store.setStage('needs_manual');
  await assert.rejects(store.save({ config: { email: 'first@example.test', name: 'Different' } }), codeIs('submitted_config_immutable'));
  await store.close();
  await assert.rejects(new PrivateRunStore(root, 'one').init({ email: 'second@example.test' }), codeIs('run_email_mismatch'));
  const resumed = new PrivateRunStore(root, 'one');
  t.after(() => resumed.close());
  assert.equal((await resumed.init()).email, 'first@example.test');
});

test('confirmed result survives reopen and cannot become a new registration', async (t) => {
  const { root } = await fixture(t);
  const store = new PrivateRunStore(root, 'one');
  await store.init();
  await store.markSubmissionStarted();
  await store.save({ result: { accountId: 'synthetic-id' }, stage: 'completed' });
  await store.close();
  const resumed = new PrivateRunStore(root, 'one');
  t.after(() => resumed.close());
  assert.equal((await resumed.init()).submission.outcome, 'confirmed');
  await assert.rejects(resumed.markOutcomeUnknown(), codeIs('run_already_completed'));
  await assert.rejects(resumed.markSubmissionStarted(), codeIs('submission_already_started'));
});

test('missing submission marker cannot be forged via generic stage changes', async (t) => {
  const { root } = await fixture(t);
  const store = new PrivateRunStore(root, 'one');
  t.after(() => store.close());
  await store.init();
  await assert.rejects(store.setStage('submission_started'), codeIs('submission_marker_required'));
  await assert.rejects(store.setStage('completed'), codeIs('invalid_transition'));
  await assert.rejects(store.markOutcomeUnknown(), codeIs('submission_not_started'));
});

test('malformed existing state blocks init and preserves original bytes', async (t) => {
  const { root } = await fixture(t);
  await mkdir(join(root, 'one'), { recursive: true, mode: 0o700 });
  const path = join(root, 'one', 'state.json');
  await writeFile(path, '{private-invalid-json', { mode: 0o600 });
  const store = new PrivateRunStore(root, 'one');
  await assert.rejects(store.init(), codeIs('invalid_state'));
  assert.equal(await readFile(path, 'utf8'), '{private-invalid-json');
  await assert.rejects(lstat(store.lockPath), { code: 'ENOENT' });
});

test('an existing run with a lost journal is not treated as a new signup', async (t) => {
  const { root } = await fixture(t);
  const store = new PrivateRunStore(root, 'one');
  await store.init();
  await store.markSubmissionStarted();
  await store.close();
  await rm(store.statePath);
  await assert.rejects(new PrivateRunStore(root, 'one').init(), codeIs('state_missing'));
  await assert.rejects(lstat(store.statePath), { code: 'ENOENT' });
});

test('symlink roots, state files, and locks never redirect private writes', async (t) => {
  const { root, temporary } = await fixture(t);
  const outside = join(temporary, 'outside');
  await mkdir(outside, { mode: 0o700 });
  await symlink(outside, root);
  await assert.rejects(new PrivateRunStore(root, 'root').init(), codeIs('unsafe_storage'));
  await rm(root);
  await mkdir(join(root, 'state'), { recursive: true, mode: 0o700 });
  const secret = join(outside, 'secret');
  await writeFile(secret, 'keep-this-content', { mode: 0o600 });
  await symlink(secret, join(root, 'state', 'state.json'));
  await assert.rejects(new PrivateRunStore(root, 'state').init(), codeIs('storage_error'));
  assert.equal(await readFile(secret, 'utf8'), 'keep-this-content');
  await mkdir(join(root, 'lock'), { mode: 0o700 });
  await symlink(secret, join(root, 'lock', '.lock'));
  await assert.rejects(new PrivateRunStore(root, 'lock').init(), codeIs('run_locked'));
  assert.equal(await readFile(secret, 'utf8'), 'keep-this-content');
});

test('public permissions and hard-linked state files are refused', async (t) => {
  const { root, temporary } = await fixture(t);
  const store = new PrivateRunStore(root, 'one');
  await store.init();
  await store.close();
  await chmod(store.statePath, 0o644);
  await assert.rejects(new PrivateRunStore(root, 'one').init(), codeIs('unsafe_storage'));
  await chmod(store.statePath, 0o600);
  await link(store.statePath, join(temporary, 'linked-state'));
  await assert.rejects(new PrivateRunStore(root, 'one').init(), codeIs('unsafe_storage'));
});

test('invalid identities and JSON cannot escape the private run directory', async (t) => {
  const { root } = await fixture(t);
  for (const id of ['../escape', '.', 'a/b', '', 'bad\nname']) {
    assert.throws(() => new PrivateRunStore(root, id), codeIs('invalid_run_id'));
  }
  const store = new PrivateRunStore(root, 'one');
  t.after(() => store.close());
  await store.init();
  await assert.rejects(store.save(JSON.parse('{"__proto__":{"polluted":true}}')), codeIs('invalid_state'));
  await assert.rejects(store.save({ result: 'x'.repeat(1024 * 1024) }), codeIs('state_too_large'));
  assert.equal({}.polluted, undefined);
});

test('protocol probe makes one fixed GET and discards the body', async () => {
  let calls = 0;
  let cancelled = false;
  const result = await protocolProbe(async (url, options) => {
    calls++;
    assert.equal(url, 'https://account.adobe.com/');
    assert.equal(options.method, 'GET');
    assert.equal(options.redirect, 'manual');
    assert.equal(options.credentials, 'omit');
    assert.equal(options.body, undefined);
    return {
      url, status: 302, headers: { get() { throw new Error('must not inspect secret redirect'); } },
      body: { async cancel() { cancelled = true; } },
      text() { throw new Error('must not read body'); },
    };
  });
  assert.equal(calls, 1);
  assert.equal(cancelled, true);
  assert.deepEqual(result, {
    status: 302, hostname: 'account.adobe.com', path: '/', code: 'browser_required',
    browserRequired: true, signupFormVerified: false, bootstrap: 'redirect',
  });
});

test('protocol probe emits bounded diagnostics and never exception secrets', async () => {
  const result = await protocolProbe(async () => { throw new Error('private-token=https://evil.test/?secret=abc'); });
  assert.equal(result.status, 0);
  assert.equal(result.code, 'browser_required');
  assert.equal(JSON.stringify(result).includes('private-token'), false);
  const hostile = await protocolProbe(async () => ({ url: 'https://evil.test/?secret=abc', status: 200 }));
  assert.equal(hostile.status, 0);
  assert.equal(hostile.bootstrap, 'untrusted_response');
  assert.equal(JSON.stringify(hostile).includes('secret'), false);
});
