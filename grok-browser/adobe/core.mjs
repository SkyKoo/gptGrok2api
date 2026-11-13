import { constants } from 'node:fs';
import { link, lstat, mkdir, open, rename, unlink } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { randomBytes } from 'node:crypto';

const MAX_STATE_BYTES = 1024 * 1024;
const ADOBE_HOSTS = new Set([
  'account.adobe.com', 'firefly.adobe.com', 'auth.services.adobe.com',
  'auth-light.identity.adobe.com', 'ims-na1.adobelogin.com',
]);
export const REGISTRATION_STAGES = Object.freeze([
  'initialized', 'configured', 'protocol_probe', 'browser_required', 'browser_opened',
  'form_ready', 'awaiting_profile', 'awaiting_email', 'awaiting_password', 'awaiting_approval',
  'awaiting_verification', 'needs_manual',
  'verification', 'ready_to_submit', 'submission_started', 'outcome_unknown',
  'session', 'media_pending', 'credentials_saved', 'completed', 'failed', 'cancelled',
]);
const STAGES = new Set(REGISTRATION_STAGES);
const POST_SUBMISSION_STAGES = new Set([
  'submission_started', 'outcome_unknown', 'awaiting_verification', 'verification',
  'needs_manual', 'session', 'media_pending', 'credentials_saved', 'completed', 'failed', 'cancelled',
]);
const PROTECTED_FIELDS = new Set(['schemaVersion', 'runId', 'createdAt', 'updatedAt', 'submission']);

export class RegistrationError extends Error {
  constructor(code) {
    const safeCode = typeof code === 'string' && /^[a-z][a-z0-9_]{0,79}$/.test(code)
      ? code : 'registration_error';
    super(safeCode);
    this.name = 'RegistrationError';
    this.code = safeCode;
  }
}

export function isTrustedAdobeURL(raw) {
  if (typeof raw !== 'string' || raw !== raw.trim() || /[\u0000-\u0020\u007f\\]/.test(raw)) return false;
  try {
    const url = new URL(raw);
    return url.protocol === 'https:' && !url.username && !url.password &&
      !url.port && ADOBE_HOSTS.has(url.hostname);
  } catch { return false; }
}

function fail(code) { throw new RegistrationError(code); }
function isRecord(value) {
  return value !== null && typeof value === 'object' &&
    !Array.isArray(value) && Object.getPrototypeOf(value) === Object.prototype;
}
function validateJSON(value, depth = 0, ancestors = new Set()) {
  if (depth > 30) fail('invalid_state');
  if (value === null || typeof value === 'string' || typeof value === 'boolean') return;
  if (typeof value === 'number' && Number.isFinite(value)) return;
  if (!Array.isArray(value) && !isRecord(value)) fail('invalid_state');
  if (ancestors.has(value)) fail('invalid_state');
  ancestors.add(value);
  for (const [key, child] of Object.entries(value)) {
    if (['__proto__', 'constructor', 'prototype'].includes(key)) fail('invalid_state');
    validateJSON(child, depth + 1, ancestors);
  }
  ancestors.delete(value);
}
function validCode(code) {
  return code === null || (typeof code === 'string' && /^[a-z][a-z0-9_]{0,79}$/.test(code));
}
function validateState(state, runId) {
  validateJSON(state);
  if (!isRecord(state) || state.schemaVersion !== 1 || state.runId !== runId ||
      typeof state.email !== 'string' || state.email.length > 320 || /[\r\n\0]/.test(state.email) ||
      !isRecord(state.config) || !STAGES.has(state.stage) || !validCode(state.code) ||
      !Number.isFinite(Date.parse(state.createdAt)) || !Number.isFinite(Date.parse(state.updatedAt)) ||
      !isRecord(state.submission) || typeof state.submission.started !== 'boolean') fail('invalid_state');
  if (state.config.email !== undefined && (typeof state.config.email !== 'string' ||
      state.config.email.trim().toLowerCase() !== state.email.trim().toLowerCase())) fail('run_email_mismatch');
  const submission = state.submission;
  if (submission.started) {
    if (!Number.isFinite(Date.parse(submission.startedAt)) ||
        !['pending', 'unknown', 'confirmed'].includes(submission.outcome) ||
        !POST_SUBMISSION_STAGES.has(state.stage)) fail('invalid_state');
    if ((submission.outcome === 'confirmed') !== (state.stage === 'completed')) fail('invalid_state');
  } else if (submission.startedAt !== null || submission.outcome !== 'not_started' ||
             ['submission_started', 'outcome_unknown', 'media_pending', 'completed'].includes(state.stage)) fail('invalid_state');
  if (Buffer.byteLength(JSON.stringify(state)) > MAX_STATE_BYTES) fail('state_too_large');
}
function privateMode(stat, directory) {
  if ((directory ? !stat.isDirectory() : !stat.isFile()) || (stat.mode & 0o077) !== 0 ||
      (!directory && stat.nlink !== 1) ||
      (typeof process.getuid === 'function' && stat.uid !== process.getuid())) fail('unsafe_storage');
}
async function privateDirectory(path, create = false) {
  let created = false;
  if (create) {
    try { await mkdir(path, { mode: 0o700 }); created = true; }
    catch (error) { if (error.code !== 'EEXIST') throw error; }
  }
  const stat = await lstat(path);
  if (stat.isSymbolicLink()) fail('unsafe_storage');
  privateMode(stat, true);
  return created;
}
function safeStorageError(error) {
  return error instanceof RegistrationError ? error : new RegistrationError('storage_error');
}

/** Publish a private export exactly once, or reuse identical existing bytes. */
export async function writePrivateJSONOnce(runDir, filename, payload) {
  let temporary;
  let handle;
  try {
    if (typeof runDir !== 'string' || !runDir || typeof filename !== 'string' ||
        !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(filename)) fail('invalid_private_filename');
    validateJSON(payload);
    const bytes = Buffer.from(`${JSON.stringify(payload, null, 2)}\n`, 'utf8');
    if (bytes.length > MAX_STATE_BYTES) fail('private_file_too_large');
    const directoryPath = resolve(runDir);
    await privateDirectory(directoryPath);
    const directoryStat = await lstat(directoryPath);
    if ((directoryStat.mode & 0o777) !== 0o700) fail('unsafe_storage');
    const target = join(directoryPath, filename);
    temporary = join(directoryPath, `.export-${randomBytes(16).toString('hex')}.tmp`);
    handle = await open(temporary, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
    await handle.chmod(0o600);
    await handle.writeFile(bytes);
    await handle.sync();
    await handle.close();
    handle = null;
    try {
      // link() fails atomically when the destination already exists; rename() would overwrite it.
      await link(temporary, target);
    } catch (error) {
      if (error.code !== 'EEXIST') throw error;
      const before = await lstat(target);
      if (before.isSymbolicLink() || (before.mode & 0o777) !== 0o600) fail('unsafe_storage');
      privateMode(before, false);
      if (before.size > MAX_STATE_BYTES) fail('private_file_too_large');
      handle = await open(target, constants.O_RDONLY | constants.O_NOFOLLOW);
      const actual = await handle.stat();
      privateMode(actual, false);
      if ((actual.mode & 0o777) !== 0o600 || actual.ino !== before.ino || actual.dev !== before.dev) fail('unsafe_storage');
      if (actual.size > MAX_STATE_BYTES) fail('private_file_too_large');
      const buffer = Buffer.alloc(MAX_STATE_BYTES + 1);
      let length = 0;
      while (length < buffer.length) {
        const chunk = await handle.read(buffer, length, buffer.length - length, null);
        if (chunk.bytesRead === 0) break;
        length += chunk.bytesRead;
      }
      if (length > MAX_STATE_BYTES) fail('private_file_too_large');
      if (!buffer.subarray(0, length).equals(bytes)) fail('private_file_conflict');
      await handle.close();
      handle = null;
    }
    await unlink(temporary);
    temporary = null;
    const directory = await open(directoryPath, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
    try { await directory.sync(); } finally { await directory.close(); }
    return target;
  } catch (error) {
    throw safeStorageError(error);
  } finally {
    if (handle) await handle.close().catch(() => {});
    if (temporary) await unlink(temporary).catch(() => {});
  }
}

/** One local run owns one exclusive lock. Existing locks are never guessed stale. */
export class PrivateRunStore {
  #lock = null;
  #ready = false;
  #queue = Promise.resolve();
  constructor(root, runId) {
    if (typeof root !== 'string' || !root || typeof runId !== 'string' ||
        !/^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$/.test(runId)) fail('invalid_run_id');
    this.root = resolve(root);
    this.runId = runId;
    this.runDir = join(this.root, runId);
    this.statePath = join(this.runDir, 'state.json');
    this.lockPath = join(this.runDir, '.lock');
  }
  #serial(operation) {
    const result = this.#queue.then(operation);
    this.#queue = result.catch(() => {});
    return result.catch((error) => { throw safeStorageError(error); });
  }
  async #checkDirectories() {
    await privateDirectory(this.root);
    await privateDirectory(this.runDir);
  }
  async #assertReady() {
    if (!this.#ready || !this.#lock) fail('store_not_open');
    await this.#checkDirectories();
    const current = await lstat(this.lockPath);
    const owned = await this.#lock.stat();
    if (current.isSymbolicLink() || current.ino !== owned.ino || current.dev !== owned.dev) fail('lock_changed');
    privateMode(current, false);
  }
  async #read() {
    let handle;
    try {
      handle = await open(this.statePath, constants.O_RDONLY | constants.O_NOFOLLOW);
      const stat = await handle.stat();
      privateMode(stat, false);
      if (stat.size > MAX_STATE_BYTES) fail('state_too_large');
      const buffer = Buffer.alloc(MAX_STATE_BYTES + 1);
      let bytesRead = 0;
      while (bytesRead < buffer.length) {
        const chunk = await handle.read(buffer, bytesRead, buffer.length - bytesRead, null);
        if (chunk.bytesRead === 0) break;
        bytesRead += chunk.bytesRead;
      }
      if (bytesRead > MAX_STATE_BYTES) fail('state_too_large');
      const raw = buffer.subarray(0, bytesRead).toString('utf8');
      let state;
      try { state = JSON.parse(raw); } catch { fail('invalid_state'); }
      validateState(state, this.runId);
      return state;
    } finally { if (handle) await handle.close(); }
  }
  async #commit(state) {
    validateState(state, this.runId);
    await this.#checkDirectories();
    try {
      const existing = await lstat(this.statePath);
      if (existing.isSymbolicLink()) fail('unsafe_storage');
      privateMode(existing, false);
    } catch (error) { if (error.code !== 'ENOENT') throw error; }
    const temporary = join(this.runDir, `.state-${randomBytes(16).toString('hex')}.tmp`);
    let handle;
    try {
      handle = await open(temporary, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
      await handle.writeFile(`${JSON.stringify(state, null, 2)}\n`, 'utf8');
      await handle.sync();
      await handle.close();
      handle = null;
      await rename(temporary, this.statePath);
      const directory = await open(this.runDir, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
      try { await directory.sync(); } finally { await directory.close(); }
      return structuredClone(state);
    } finally {
      if (handle) await handle.close();
      await unlink(temporary).catch((error) => { if (error.code !== 'ENOENT') throw error; });
    }
  }
  async #unlock() {
    if (!this.#lock) return;
    const handle = this.#lock;
    this.#lock = null;
    this.#ready = false;
    try {
      const owned = await handle.stat();
      const current = await lstat(this.lockPath);
      if (current.isSymbolicLink() || current.ino !== owned.ino || current.dev !== owned.dev) fail('lock_changed');
      await unlink(this.lockPath);
    } finally { await handle.close(); }
  }
  init(initial = {}, { requireExisting = false, requireNew = false } = {}) {
    return this.#serial(async () => {
      if (this.#ready || this.#lock) fail('store_already_open');
      if (requireExisting && requireNew) fail('invalid_storage_mode');
      if (!isRecord(initial)) fail('invalid_state');
      validateJSON(initial);
      // The caller creates its parent directory; do not recursively traverse untrusted paths.
      let newRun;
      try {
        await privateDirectory(this.root, !requireExisting);
        newRun = await privateDirectory(this.runDir, !requireExisting);
      } catch (error) {
        if (requireExisting && error.code === 'ENOENT') fail('state_missing');
        throw error;
      }
      // A newly allocated account must never adopt an old profile, including
      // one for the same email. Recovery opts into reuse separately.
      if (requireNew && !newRun) fail('run_already_exists');
      try {
        this.#lock = await open(this.lockPath, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
      } catch (error) {
        if (error.code === 'EEXIST' || error.code === 'ELOOP') fail('run_locked');
        throw error;
      }
      try {
        await this.#lock.writeFile(JSON.stringify({ pid: process.pid, createdAt: new Date().toISOString() }));
        await this.#lock.sync();
        let state;
        try { state = await this.#read(); }
        catch (error) {
          if (error.code !== 'ENOENT') throw error;
          if (requireExisting || !newRun) fail('state_missing');
          const now = new Date().toISOString();
          state = {
            schemaVersion: 1, runId: this.runId, email: initial.email ?? '', config: initial.config ?? {},
            stage: 'initialized', code: null, createdAt: now, updatedAt: now,
            submission: { started: false, startedAt: null, outcome: 'not_started' },
          };
          state = await this.#commit(state);
        }
        if (initial.email && (typeof initial.email !== 'string' ||
            initial.email.trim().toLowerCase() !== state.email.trim().toLowerCase())) fail('run_email_mismatch');
        if (state.submission.started && state.submission.outcome !== 'confirmed') {
          if (state.stage !== 'media_pending') {
            state.stage = 'outcome_unknown';
            state.code = 'submission_result_unknown';
          }
          state.submission.outcome = 'unknown';
          state.updatedAt = new Date().toISOString();
          state = await this.#commit(state);
        }
        this.#ready = true;
        return structuredClone(state);
      } catch (error) {
        await this.#unlock();
        throw error;
      }
    });
  }
  load() {
    return this.#serial(async () => { await this.#assertReady(); return this.#read(); });
  }
  async #savePatch(patch) {
    await this.#assertReady();
    if (!isRecord(patch)) fail('invalid_state');
    validateJSON(patch);
    if (Object.keys(patch).some((key) => PROTECTED_FIELDS.has(key))) fail('protected_state_field');
    if (patch.stage === 'submission_started') fail('submission_marker_required');
    const state = await this.#read();
    if (patch.email !== undefined && state.email && (typeof patch.email !== 'string' ||
        patch.email.trim().toLowerCase() !== state.email.trim().toLowerCase())) fail('run_email_mismatch');
    if (state.submission.started && patch.config !== undefined &&
        JSON.stringify(patch.config) !== JSON.stringify(state.config)) fail('submitted_config_immutable');
    if (state.submission.started && patch.stage && !POST_SUBMISSION_STAGES.has(patch.stage)) fail('invalid_transition');
    Object.assign(state, patch, { updatedAt: new Date().toISOString() });
    if (state.stage === 'completed') {
      if (!state.submission.started) fail('invalid_transition');
      state.submission.outcome = 'confirmed';
    } else if (state.submission.outcome === 'confirmed') fail('run_already_completed');
    if (state.stage === 'outcome_unknown') state.submission.outcome = 'unknown';
    return this.#commit(state);
  }
  save(patch) { return this.#serial(() => this.#savePatch(patch)); }
  setStage(stage, code = null) { return this.save({ stage, code }); }
  markSubmissionStarted() {
    return this.#serial(async () => {
      await this.#assertReady();
      const state = await this.#read();
      if (state.submission.started) fail('submission_already_started');
      const now = new Date().toISOString();
      state.stage = 'submission_started';
      state.code = null;
      state.updatedAt = now;
      state.submission = { started: true, startedAt: now, outcome: 'pending' };
      return this.#commit(state);
    });
  }
  markOutcomeUnknown(code = 'submission_result_unknown') {
    return this.#serial(async () => {
      await this.#assertReady();
      const state = await this.#read();
      if (!state.submission.started) fail('submission_not_started');
      if (state.submission.outcome === 'confirmed') fail('run_already_completed');
      state.stage = 'outcome_unknown';
      state.code = code;
      state.updatedAt = new Date().toISOString();
      state.submission.outcome = 'unknown';
      return this.#commit(state);
    });
  }
  close() { return this.#serial(() => this.#unlock()); }
}

/** Only inspect Adobe's public HTTP bootstrap; no signup requests or JavaScript. */
export async function protocolProbe(fetchImpl = globalThis.fetch) {
  const result = {
    status: 0, hostname: 'account.adobe.com', path: '/', code: 'browser_required',
    browserRequired: true, signupFormVerified: false, bootstrap: 'network_error',
  };
  try {
    const response = await fetchImpl('https://account.adobe.com/', {
      method: 'GET', redirect: 'manual', credentials: 'omit', cache: 'no-store',
      referrerPolicy: 'no-referrer', headers: { Accept: 'text/html' },
      signal: AbortSignal.timeout(15_000),
    });
    if (response.url && !isTrustedAdobeURL(response.url)) {
      result.bootstrap = 'untrusted_response';
    } else {
      result.status = Number.isInteger(response.status) && response.status >= 100 && response.status <= 599 ? response.status : 0;
      result.bootstrap = result.status >= 200 && result.status < 300 ? 'available'
        : result.status >= 300 && result.status < 400 ? 'redirect'
          : result.status >= 400 && result.status < 500 ? 'rejected' : 'unavailable';
    }
    // Discard the body without parsing, logging, or executing any remote content.
    if (response.body?.cancel) await response.body.cancel();
  } catch { /* Only bounded fixed diagnostics leave this function. */ }
  return result;
}
