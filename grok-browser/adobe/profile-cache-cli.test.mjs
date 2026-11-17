import test from 'node:test';
import assert from 'node:assert/strict';
import { chmod, lstat, mkdir, mkdtemp, readFile, readdir, realpath, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFile, spawn } from 'node:child_process';
import { once } from 'node:events';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';
import { parseCacheCLIArgs, runProfileCacheCLI } from './profile-cache-cli.mjs';
import { PrivateRunStore } from './core.mjs';

async function fixture(t, names = ['first', 'second']) {
  const temporary = await realpath(await mkdtemp(join(tmpdir(), 'cfm-cache-cli-')));
  t.after(() => rm(temporary, { recursive: true, force: true }));
  const root = join(temporary, 'adobe');
  await mkdir(root, { mode: 0o700 });
  for (const name of names) {
    const dir = join(root, name);
    await mkdir(join(dir, 'browser', 'Default', 'Cache'), { recursive: true, mode: 0o700 });
    await writeFile(join(dir, 'state.json'), '{"secret":"PRIVATE-STATE"}', { mode: 0o600 });
    await writeFile(join(dir, 'browser', 'Default', 'Cookies'), 'PRIVATE-COOKIE', { mode: 0o600 });
    await writeFile(join(dir, 'browser', 'Default', 'Cache', 'entry'), 'synthetic-cache', { mode: 0o600 });
  }
  return { root, temporary };
}

async function snapshot(root) {
  const rows = {};
  async function visit(path, relative) {
    const stat = await lstat(path);
    rows[relative] = { ino: stat.ino, mode: stat.mode, size: stat.size, mtime: stat.mtimeMs };
    if (stat.isFile()) rows[relative].bytes = (await readFile(path)).toString('hex');
    if (stat.isDirectory()) for (const name of await readdir(path)) await visit(join(path, name), `${relative}/${name}`);
  }
  await visit(root, '.');
  return rows;
}

test('default report is read-only and exposes only aggregate cache counts', async t => {
  const { root } = await fixture(t, ['account-private-a', 'account-private-b']);
  const before = await snapshot(root);
  const calls = [];
  const outcome = await runProfileCacheCLI(['--root', root], {
    maintainProfileCache: async (actualRoot, id, options) => {
      calls.push({ actualRoot, id, options });
      return { status: 'eligible', files: 2, bytes: 30, removedFiles: 0, removedBytes: 0,
        privateDiagnostic: { profile: id, cookie: 'PRIVATE-COOKIE' } };
    },
  });
  assert.equal(outcome.exitCode, 0);
  assert.equal(outcome.result.mode, 'report');
  assert.equal(outcome.result.profiles.eligible, 2);
  assert.deepEqual(outcome.result.cache, { files: 4, bytes: 60, removed_files: 0, removed_bytes: 0 });
  assert.equal(calls.length, 2);
  assert.ok(calls.every(call => call.actualRoot === root && call.options.apply === false));
  assert.deepEqual(await snapshot(root), before);
  const output = JSON.stringify(outcome.result);
  for (const privateValue of [root, 'account-private-a', 'account-private-b', 'PRIVATE-STATE', 'PRIVATE-COOKIE', 'entry']) {
    assert.equal(output.includes(privateValue), false, 'report exposed a private identifier or file detail');
  }
});

test('explicit apply preserves blocked and partial outcomes instead of claiming complete cleanup', async t => {
  const { root } = await fixture(t, ['idle', 'busy', 'partial']);
  const calls = [];
  const outcome = await runProfileCacheCLI(['--apply', '--root', root], {
    maintainProfileCache: async (_root, id, options) => {
      calls.push(options.apply);
      if (id === 'busy') return { status: 'blocked', code: 'profile_busy' };
      if (id === 'partial') return { status: 'partial', code: 'cache_helper_interrupted', files: 3, bytes: 40, removedFiles: 1, removedBytes: 10, outcomeUnknown: true };
      return { status: 'cleaned', files: 2, bytes: 30, removedFiles: 2, removedBytes: 30 };
    },
  });
  assert.deepEqual(calls, [true, true, true]);
  assert.equal(outcome.exitCode, 1);
  assert.equal(outcome.result.status, 'partial');
  assert.equal(outcome.result.bytes_kind, 'logical_file_bytes');
  assert.deepEqual(outcome.result.profiles, { inspected: 3, eligible: 1, cleaned: 1, blocked: 1, partial: 1, outcome_unknown: 1 });
  assert.deepEqual(outcome.result.cache, { files: 5, bytes: 70, removed_files: 3, removed_bytes: 40 });
  assert.deepEqual(outcome.result.reasons, { profile_busy: 1, cache_helper_interrupted: 1 });
});

test('read-only report retains measured bytes when the module cannot prove process safety', async t => {
  const { root } = await fixture(t, ['one']);
  const outcome = await runProfileCacheCLI(['--root', root], {
    maintainProfileCache: async () => ({ status: 'blocked', code: 'unsupported_platform', files: 2, bytes: 48 }),
  });
  assert.equal(outcome.exitCode, 0);
  assert.equal(outcome.result.status, 'partial');
  assert.equal(outcome.result.profiles.blocked, 1);
  assert.equal(outcome.result.cache.bytes, 48);
  assert.equal(outcome.result.cache.removed_bytes, 0);
});

test('arguments never opt into apply implicitly or permit force/lock bypass options', async t => {
  const { root } = await fixture(t);
  assert.deepEqual(parseCacheCLIArgs([], { CFM_ADOBE_PROFILE_ROOT: root }), { root, apply: false });
  for (const args of [
    ['--force'], ['--apply', '--apply'], ['--root'], ['--root', root, '--root', root],
    ['--root', 'relative'], ['--root', '/'], ['--root', root, '--ignore-lock'], ['--help', '--apply'], [root],
  ]) {
    let invoked = false;
    const result = await runProfileCacheCLI(args, { maintainProfileCache: async () => { invoked = true; } });
    assert.equal(result.exitCode, 2);
    assert.equal(result.result.status, 'refused');
    assert.equal(invoked, false);
  }
});

test('linked roots, linked ancestors and nonprivate roots are refused before profile maintenance', async t => {
  const { root, temporary } = await fixture(t);
  const linked = join(temporary, 'linked');
  await symlink(root, linked);
  const ancestor = join(temporary, 'ancestor');
  await symlink(temporary, ancestor);
  for (const candidate of [linked, join(ancestor, 'adobe')]) {
    let invoked = false;
    const outcome = await runProfileCacheCLI(['--root', candidate, '--apply'], { maintainProfileCache: async () => { invoked = true; } });
    assert.equal(outcome.exitCode, 1);
    assert.equal(outcome.result.code, 'unsafe_profile_root');
    assert.equal(invoked, false);
    assert.equal(JSON.stringify(outcome.result).includes(candidate), false);
  }
  await chmod(root, 0o755);
  const outcome = await runProfileCacheCLI(['--root', root], { maintainProfileCache: async () => assert.fail('unsafe root visited') });
  assert.equal(outcome.result.code, 'unsafe_profile_root');
});

test('unsafe root entries are not followed and module errors cannot leak paths or credentials', async t => {
  const { root, temporary } = await fixture(t, ['safe']);
  await mkdir(join(temporary, 'outside'), { mode: 0o700 });
  await symlink(join(temporary, 'outside'), join(root, 'linked-profile'));
  await writeFile(join(root, 'credential-file'), 'PRIVATE-TOKEN', { mode: 0o600 });
  const visited = [];
  const outcome = await runProfileCacheCLI(['--root', root, '--apply'], {
    maintainProfileCache: async (_root, id) => {
      visited.push(id);
      throw Object.assign(new Error(`PRIVATE-TOKEN ${root}`), { code: '../PRIVATE-TOKEN' });
    },
  });
  assert.deepEqual(visited, ['safe']);
  assert.equal(outcome.exitCode, 1);
  assert.equal(outcome.result.profiles.blocked, 3);
  assert.deepEqual(outcome.result.reasons, { unsafe_profile_entry: 2, cache_maintenance_failed: 1 });
  assert.equal(JSON.stringify(outcome.result).includes('PRIVATE'), false);
  assert.deepEqual(await readdir(join(temporary, 'outside')), []);
});

test('unknown lowercase reason codes cannot expose private identifiers', async t => {
  const { root } = await fixture(t, ['one']);
  for (const code of ['private_account_identifier', '../PRIVATE-TOKEN']) {
    const outcome = await runProfileCacheCLI(['--root', root], {
      maintainProfileCache: async () => ({ status: 'blocked', code }),
    });
    assert.deepEqual(outcome.result.reasons, { cache_maintenance_failed: 1 });
    assert.equal(JSON.stringify(outcome.result).includes(code), false);
  }
});

test('real maintenance report preserves registration state, credentials, cache and lock absence', async t => {
  const { root } = await fixture(t, ['account-private-a']);
  const before = await snapshot(root);
  const outcome = await runProfileCacheCLI(['--root', root]);
  assert.equal(outcome.exitCode, 0);
  assert.deepEqual(await snapshot(root), before);
  assert.equal(outcome.result.cache.files, 1);
  assert.equal(outcome.result.cache.bytes, Buffer.byteLength('synthetic-cache'));
  assert.equal(outcome.result.cache.removed_files, 0);
  assert.equal(outcome.result.cache.removed_bytes, 0);
  if (process.platform === 'linux') assert.equal(outcome.result.profiles.eligible, 1);
  else assert.deepEqual(outcome.result.reasons, { unsupported_platform: 1 });
  for (const value of [root, 'account-private-a', 'PRIVATE-COOKIE', 'PRIVATE-STATE']) {
    assert.equal(JSON.stringify(outcome.result).includes(value), false);
  }
});

test('real maintenance never replaces or removes a held PrivateRunStore lock', async t => {
  const { root } = await fixture(t, []);
  const store = new PrivateRunStore(root, 'locked');
  await store.init({ email: 'private@example.test' }, { requireNew: true });
  try {
    await store.markSubmissionStarted();
    await mkdir(join(store.runDir, 'browser', 'Default', 'Cache'), { recursive: true, mode: 0o700 });
    await writeFile(join(store.runDir, 'browser', 'Default', 'Cache', 'entry'), 'keep-cache', { mode: 0o600 });
    const before = await snapshot(root);
    for (const args of [['--root', root], ['--root', root, '--apply']]) {
      const outcome = await runProfileCacheCLI(args);
      assert.equal(outcome.exitCode, args.includes('--apply') ? 1 : 0);
      assert.deepEqual(outcome.result.reasons, {
        [process.platform !== 'linux' && args.includes('--apply') ? 'unsupported_platform' : 'profile_busy']: 1,
      });
      assert.equal(outcome.result.cache.removed_files, 0);
      assert.deepEqual(await snapshot(root), before);
    }
    assert.equal((await store.load()).submission.outcome, 'pending');
  } finally {
    await store.close();
  }
});

test('real Linux apply removes only cache and does not recover pending registration state', { skip: process.platform !== 'linux' }, async t => {
  const { root } = await fixture(t, []);
  const store = new PrivateRunStore(root, 'pending');
  await store.init({ email: 'private@example.test' }, { requireNew: true });
  await store.markSubmissionStarted();
  await store.close();
  const browser = join(store.runDir, 'browser');
  const retained = [
    'Local State', 'Default/Preferences', 'Default/Cookies', 'Default/Web Data',
    'Default/Local Storage/leveldb/000003.log', 'Default/Session Storage/000003.log',
    'Default/IndexedDB/example/000003.log', 'Default/Sessions/session', 'Default/Service Worker/CacheStorage/entry',
  ];
  for (const file of [...retained, 'Default/Cache/entry', 'Default/Code Cache/js/entry', 'ShaderCache/entry']) {
    const target = join(browser, file);
    await mkdir(join(target, '..'), { recursive: true, mode: 0o700 });
    await writeFile(target, `private-fixture:${file}`, { mode: 0o600 });
  }
  const stateBefore = await readFile(store.statePath);
  const filesBefore = new Map(await Promise.all(retained.map(async file => [file, await readFile(join(browser, file))])));
  const outcome = await runProfileCacheCLI(['--root', root, '--apply']);
  assert.equal(outcome.exitCode, 0);
  assert.equal(outcome.result.profiles.cleaned, 1);
  assert.equal(outcome.result.cache.removed_files, 3);
  assert.equal(outcome.result.cache.removed_bytes, outcome.result.cache.bytes);
  assert.deepEqual(await readFile(store.statePath), stateBefore);
  for (const [file, bytes] of filesBefore) assert.deepEqual(await readFile(join(browser, file)), bytes);
  for (const file of ['Default/Cache', 'Default/Code Cache', 'ShaderCache']) {
    await assert.rejects(lstat(join(browser, file)), { code: 'ENOENT' });
  }
  await assert.rejects(lstat(store.lockPath), { code: 'ENOENT' });
});

test('real Linux apply refuses a live process referencing the profile', { skip: process.platform !== 'linux' }, async t => {
  const { root } = await fixture(t, ['busy']);
  const browser = join(root, 'busy', 'browser');
  const child = spawn(process.execPath, ['-e', "process.stdout.write('ready'); setInterval(() => {}, 1000)", '--', `--user-data-dir=${browser}`], {
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  t.after(async () => {
    const exited = once(child, 'exit');
    child.kill();
    await exited;
  });
  await once(child.stdout, 'data');
  const before = await snapshot(browser);
  const stateBefore = await readFile(join(root, 'busy', 'state.json'));
  const outcome = await runProfileCacheCLI(['--root', root, '--apply']);
  assert.equal(outcome.exitCode, 1);
  assert.deepEqual(outcome.result.reasons, { browser_still_running: 1 });
  assert.equal(outcome.result.cache.removed_files, 0);
  assert.deepEqual(await snapshot(browser), before);
  assert.deepEqual(await readFile(join(root, 'busy', 'state.json')), stateBefore);
  await assert.rejects(lstat(join(root, 'busy', '.lock')), { code: 'ENOENT' });
});

test('CLI help works without opening profiles or loading browser dependencies', async () => {
  const script = fileURLToPath(new URL('./profile-cache-cli.mjs', import.meta.url));
  const { stdout, stderr } = await promisify(execFile)(process.execPath, [script, '--help'], {
    env: { ...process.env, CFM_ADOBE_PROFILE_ROOT: '/missing/private/profile/root' },
  });
  assert.match(stdout, /Report Adobe browser cache totals without changing files by default/);
  assert.match(stdout, /--apply/);
  assert.equal(stderr, '');
});
