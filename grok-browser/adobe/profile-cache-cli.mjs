#!/usr/bin/env node
import { lstat, readdir, realpath } from 'node:fs/promises';
import { isAbsolute, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HELP = `Usage: node profile-cache-cli.mjs [--root ABSOLUTE_PATH] [--apply]

Report Adobe browser cache totals without changing files by default.
Root defaults to CFM_ADOBE_PROFILE_ROOT, or /data/adobe when unset.
--apply removes only caches approved by the shared profile maintenance module.
Busy or unsafe profiles remain untouched; apply returns a nonzero exit code
when any profile is blocked. Authentication and registration state are retained.
Run inside the browser worker so process and mount checks see its namespace.
Output contains aggregate counts and bytes, never profile identifiers or state.
Bytes describe logical file sizes, not filesystem space released. When a cleanup
outcome is unknown, removed counts and bytes are confirmed lower bounds only.
`;
const validProfile = value => /^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$/.test(value);
const REASON_CODES = new Set([
  'invalid_arguments', 'invalid_root', 'unsafe_profile_root', 'unsafe_profile_entry',
  'profile_count_limit', 'profile_root_changed', 'invalid_cache_report',
  'unsafe_profile_path', 'profile_permissions_unsafe', 'profile_directory_changed', 'profile_busy', 'profile_lock_unavailable', 'profile_lock_changed',
  'profile_lock_required', 'profile_contains_mount', 'browser_singleton_present',
  'unsupported_platform', 'browser_still_running', 'process_check_unavailable',
  'cache_cleanup_budget', 'cache_filesystem_changed', 'unsafe_cache_entry', 'cache_tree_changed',
  'cache_inspection_failed', 'cache_helper_unavailable', 'cache_helper_interrupted',
  'cache_helper_invalid_response', 'cache_maintenance_failed',
]);
const safeCode = error => REASON_CODES.has(error?.code) ? error.code : 'cache_maintenance_failed';
const fail = code => { throw Object.assign(new Error(code), { code }); };

export function parseCacheCLIArgs(args, environment = process.env) {
  let root = environment.CFM_ADOBE_PROFILE_ROOT || '/data/adobe';
  let apply = false, explicitRoot = false;
  if (args.length === 1 && args[0] === '--help') return { help: true };
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--apply' && !apply) apply = true;
    else if (args[i] === '--root' && !explicitRoot && args[i + 1] && !args[i + 1].startsWith('--')) {
      root = args[++i];
      explicitRoot = true;
    } else fail('invalid_arguments');
  }
  if (!isAbsolute(root) || root.includes('\0') || resolve(root) === '/') fail('invalid_root');
  return { root: resolve(root), apply };
}

async function inspectRoot(root) {
  const stat = await lstat(root);
  if (!stat.isDirectory() || stat.isSymbolicLink() || (stat.mode & 0o077) !== 0 ||
      (typeof process.getuid === 'function' && stat.uid !== process.getuid()) || await realpath(root) !== root) fail('unsafe_profile_root');
  return stat;
}

// Per-profile safety checks, lock ownership and deletion live in one shared
// implementation, also used by the browser lifecycle. This wrapper only selects
// immediate children and emits a bounded, credential-free aggregate report.
export async function runProfileCacheCLI(args, dependencies = {}) {
  let options;
  try { options = parseCacheCLIArgs(args, dependencies.environment); }
  catch (error) { return { exitCode: 2, result: { status: 'refused', code: safeCode(error) } }; }
  if (options.help) return { exitCode: 0, help: HELP };
  const result = { format: 'cfm-adobe-profile-cache-v1', mode: options.apply ? 'apply' : 'report', status: 'completed',
    bytes_kind: 'logical_file_bytes',
    profiles: { inspected: 0, eligible: 0, cleaned: 0, blocked: 0, partial: 0, outcome_unknown: 0 },
    cache: { files: 0, bytes: 0, removed_files: 0, removed_bytes: 0 }, reasons: {} };
  const block = code => {
    result.profiles.blocked++;
    result.reasons[code] = (result.reasons[code] || 0) + 1;
  };
  try {
    const rootStat = await inspectRoot(options.root);
    const entries = await readdir(options.root, { withFileTypes: true });
    if (entries.length > 10000) fail('profile_count_limit');
    const maintain = dependencies.maintainProfileCache || (await import('./profile-cache.mjs')).maintainProfileCache;
    for (const entry of entries) {
      result.profiles.inspected++;
      if (!validProfile(entry.name) || !entry.isDirectory() || entry.isSymbolicLink()) {
        block('unsafe_profile_entry');
        continue;
      }
      try {
        const currentRoot = await inspectRoot(options.root);
        if (currentRoot.dev !== rootStat.dev || currentRoot.ino !== rootStat.ino) fail('profile_root_changed');
        const row = await maintain(options.root, entry.name, { apply: options.apply });
        if (!row || !['eligible', 'cleaned', 'blocked', 'partial'].includes(row.status)) fail('invalid_cache_report');
        for (const field of ['files', 'bytes', 'removedFiles', 'removedBytes']) {
          if (!Number.isSafeInteger(row[field] ?? 0) || (row[field] ?? 0) < 0) fail('invalid_cache_report');
        }
        if (!options.apply && (row.status === 'cleaned' || row.removedFiles || row.removedBytes)) fail('invalid_cache_report');
        if (row.status === 'blocked' && (row.removedFiles || row.removedBytes)) fail('invalid_cache_report');
        if (row.outcomeUnknown !== undefined && typeof row.outcomeUnknown !== 'boolean') fail('invalid_cache_report');
        if (row.outcomeUnknown && (!options.apply || row.status !== 'partial')) fail('invalid_cache_report');
        result.cache.files += row.files || 0;
        result.cache.bytes += row.bytes || 0;
        result.cache.removed_files += row.removedFiles || 0;
        result.cache.removed_bytes += row.removedBytes || 0;
        if (row.outcomeUnknown) result.profiles.outcome_unknown++;
        if (row.status === 'blocked') { block(safeCode(row)); continue; }
        if (row.status === 'partial') {
          result.profiles.partial++;
          const code = safeCode(row);
          result.reasons[code] = (result.reasons[code] || 0) + 1;
        } else {
          result.profiles.eligible++;
          if (row.status === 'cleaned') result.profiles.cleaned++;
        }
      } catch (error) { block(safeCode(error)); }
    }
  } catch (error) {
    result.status = 'refused';
    result.code = safeCode(error);
    return { exitCode: 1, result };
  }
  if (result.profiles.blocked || result.profiles.partial) result.status = 'partial';
  return { exitCode: options.apply && result.status === 'partial' ? 1 : 0, result };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const outcome = await runProfileCacheCLI(process.argv.slice(2));
  process.stdout.write(outcome.help || `${JSON.stringify(outcome.result)}\n`);
  process.exitCode = outcome.exitCode;
}
