/** 关闭浏览器后的固定缓存回收。任何状态/凭据文件都不属于删除范围。 */
import { constants } from 'node:fs';
import { open, lstat, unlink } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';

const helper = fileURLToPath(new URL('./profile-cache-helper.py', import.meta.url));
const empty = code => ({ status: 'blocked', code, files: 0, bytes: 0, removedFiles: 0, removedBytes: 0 });
const failure = code => Object.assign(new Error(code), { code });
const same = (a, b) => a.dev === b.dev && a.ino === b.ino && a.mode === b.mode && a.uid === b.uid && a.gid === b.gid;

async function directory(value) {
  const absolute = path.resolve(value);
  if (value !== absolute) throw failure('unsafe_profile_path');
  let current = path.parse(absolute).root;
  for (const component of absolute.slice(current.length).split(path.sep).filter(Boolean)) {
    current = path.join(current, component);
    const info = await lstat(current);
    if (!info.isDirectory() || info.isSymbolicLink()) throw failure('unsafe_profile_path');
  }
  return lstat(absolute);
}

/** 只取得已有profile的互斥锁，不读取或改写注册state。旧锁一律保留。 */
export async function acquireProfileCacheLock(runDir) {
  const before = await directory(runDir);
  if (before.uid !== process.getuid() || (before.mode & 0o777) !== 0o700) throw failure('profile_permissions_unsafe');
  let handle, directoryHandle;
  try {
    directoryHandle = await open(runDir, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
    if (!same(before, await directoryHandle.stat())) throw failure('unsafe_profile_path');
    // Linux生产环境的锁增删由目录fd锚定；其他平台仅用于开发启动，禁止执行缓存删除。
    const filename = process.platform === 'linux' ? `/proc/self/fd/${directoryHandle.fd}/.lock` : path.join(runDir, '.lock');
    handle = await open(filename, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
    if (!same(before, await directory(runDir))) throw failure('unsafe_profile_path');
    await handle.writeFile(JSON.stringify({ pid: process.pid, purpose: 'profile_cache', createdAt: new Date().toISOString() }));
    await handle.sync();
    let closed = false;
    return {
      handle,
      async release() {
        if (closed) return;
        try {
          const owned = await handle.stat();
          const current = await lstat(filename);
          if (!same(owned, current) || current.isSymbolicLink()) throw failure('profile_lock_changed');
          await unlink(filename);
        } finally {
          closed = true;
          try { await handle.close(); } finally { await directoryHandle.close(); }
        }
      },
    };
  } catch (error) {
    if (handle) {
      try {
        const filename = process.platform === 'linux' ? `/proc/self/fd/${directoryHandle.fd}/.lock` : path.join(runDir, '.lock');
        if (same(await handle.stat(), await lstat(filename))) await unlink(filename);
      } catch { /* 不删除身份未知的锁。 */ }
      try { await handle.close(); } catch { /* 仍需释放目录fd。 */ }
    }
    if (directoryHandle) await directoryHandle.close();
    throw failure(error?.code === 'EEXIST' ? 'profile_busy' : 'profile_lock_unavailable');
  }
}

async function inspect(runDir, { apply = false, lockHandle, budgetMs = 30000 } = {}) {
  return new Promise(resolve => {
    let child, output = '', settled = false, timer, final, failure, stopping = false;
    let confirmed = { files: 0, bytes: 0, removedFiles: 0, removedBytes: 0 };
    const finish = value => { if (!settled) { settled = true; clearTimeout(timer); resolve(value); } };
    const interrupted = code => apply
      ? { status: 'partial', code, ...confirmed, outcomeUnknown: true } : empty(code);
    const stop = code => {
      failure ||= code;
      if (stopping) return;
      stopping = true;
      try { child.kill('SIGKILL'); } catch { /* 等待close才能释放profile锁。 */ }
    };
    const accept = raw => {
      const result = JSON.parse(raw);
      if (!['files', 'bytes', 'removedFiles', 'removedBytes'].every(key => Number.isSafeInteger(result[key]) && result[key] >= 0)) throw new Error();
      if (final || result.removedFiles > result.files || result.removedBytes > result.bytes ||
          Object.keys(confirmed).some(key => result[key] < confirmed[key])) throw new Error();
      if (result.progress !== true && (!['eligible', 'cleaned', 'blocked', 'partial'].includes(result.status) || !/^[a-z_]+$/.test(result.code))) throw new Error();
      confirmed = Object.fromEntries(['files', 'bytes', 'removedFiles', 'removedBytes'].map(key => [key, result[key]]));
      if (result.progress === true) return;
      final = { status: result.status, code: result.code, ...confirmed };
    };
    try {
      child = spawn('python3', ['-B', helper], {
        stdio: ['pipe', 'pipe', 'ignore', ...(apply ? [lockHandle.fd] : [])],
      });
      child.stdout.on('data', chunk => {
        output += chunk;
        let newline;
        while ((newline = output.indexOf('\n')) >= 0) {
          const line = output.slice(0, newline); output = output.slice(newline + 1);
          try { accept(line); } catch { stop('cache_helper_invalid_response'); }
        }
        if (output.length > 8192) stop('cache_helper_invalid_response');
      });
      child.on('error', () => {
        if (!child.pid) finish(empty('cache_helper_unavailable'));
        else stop('cache_helper_interrupted');
      });
      child.on('close', code => {
        if (failure) return finish(interrupted(failure));
        if (code !== 0) return finish(interrupted('cache_helper_interrupted'));
        try {
          if (output) accept(output);
          if (!final) throw new Error();
          finish(final);
        } catch { finish(interrupted('cache_helper_invalid_response')); }
      });
      child.stdin.on('error', () => {});
      child.stdin.end(JSON.stringify({ runDir, apply, budgetMs }));
      timer = setTimeout(() => stop('cache_helper_interrupted'), budgetMs + 1500);
    } catch {
      if (child?.pid) stop('cache_helper_interrupted');
      else finish(empty('cache_helper_unavailable'));
    }
  });
}

/** 仅由成功关闭且仍持有同一profile锁的生命周期调用；不向调用者抛清理错误。 */
export async function cleanClosedProfileCache(runDir, lockHandle) {
  return inspect(runDir, { apply: true, lockHandle, budgetMs: 2000 });
}

/** 运维入口：report不创建锁；apply锁内重新预检，输出仅安全汇总。 */
export async function maintainProfileCache(root, runId, { apply = false } = {}) {
  if (typeof root !== 'string' || typeof runId !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$/.test(runId)) return empty('unsafe_profile_path');
  const runDir = path.join(root, runId);
  if (apply && process.platform !== 'linux') return empty('unsupported_platform');
  let lease, result;
  try {
    await directory(root);
    await directory(runDir);
    if (apply) lease = await acquireProfileCacheLock(runDir);
    result = await inspect(runDir, { apply, lockHandle: lease?.handle });
  } catch (error) {
    result = empty(error?.message === 'profile_busy' ? 'profile_busy' : 'cache_inspection_failed');
  } finally {
    if (lease) {
      try { await lease.release(); }
      catch { result = { ...result, status: result?.removedFiles || result?.status === 'partial' ? 'partial' : 'blocked', code: 'profile_lock_changed' }; }
    }
  }
  return result;
}
