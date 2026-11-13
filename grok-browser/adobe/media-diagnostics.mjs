const STATES = new Set(['unavailable', 'pending', 'invalid', 'invalid_contract', 'changed',
  'observation_timeout', 'ready', 'editor_not_ready', 'observer_unavailable', 'context_changed']);
const REASONS = new Set(['performance_unavailable', 'instance_missing', 'instance_ambiguous',
  'vendor_unrecognized', 'vendors_pending', 'token_settling', 'token_pending', 'editor_not_ready',
  'observer_unavailable', 'context_changed', 'identity_pending', 'identity_changed',
  'runtime_changed', 'verification_timeout', 'authentication_missing', 'arp_observer_missing',
  'editor_route_changed', 'page_hidden', 'observation_cancelled', 'evaluation_failed',
  'navigation_budget_exhausted']);
const VALUE_KINDS = new Set(['unobserved', 'null', 'undefined', 'empty_string']);
const ROUTES = new Set(['unavailable', 'editor', 'other']);
const PAGE_COUNTS = ['matching_host_count', 'authentication_count', 'arp_observer_count'];
const TIMING_COUNTS = ['candidate_changes', 'source_changes', 'authentication_changes',
  'emissions', 'value_changes', 'proof_changes'];
const TIMING_MS = ['first_source_ms', 'first_nonempty_ms', 'vendors_ready_ms', 'last_candidate_ms'];

const record = value => value !== null && typeof value === 'object' && !Array.isArray(value) ? value : {};
const bounded = (value, max) => Number.isSafeInteger(value) && value >= 0 && value <= max;
const addEnum = (target, source, key, allowed) => { if (allowed.has(source[key])) target[key] = source[key]; };
const addCounts = (target, source, keys, max) => {
  for (const key of keys) if (bounded(source[key], max)) target[key] = source[key];
};

// Failure observations only. Never copy opaque runtime tokens, proofs, SDK
// entries, URLs, cookies or arbitrary text into registration state or logs.
export function sanitizeMediaDiagnostics(value) {
  const source = record(value), output = {};
  const runtime = record(source.media_runtime), safeRuntime = {};
  addEnum(safeRuntime, runtime, 'state', STATES);
  addEnum(safeRuntime, runtime, 'readiness_reason', REASONS);
  addEnum(safeRuntime, runtime, 'value_kind', VALUE_KINDS);
  addCounts(safeRuntime, runtime, ['vendors_started', 'vendors_complete'], 3);
  if (safeRuntime.vendors_complete > safeRuntime.vendors_started) {
    delete safeRuntime.vendors_started; delete safeRuntime.vendors_complete;
  }
  if (Object.keys(safeRuntime).length) output.media_runtime = safeRuntime;

  const page = record(source.media_page), safePage = {};
  addEnum(safePage, page, 'route', ROUTES);
  addCounts(safePage, page, PAGE_COUNTS, 32);
  if (safePage.authentication_count > safePage.matching_host_count || safePage.arp_observer_count > safePage.authentication_count) {
    for (const key of PAGE_COUNTS) delete safePage[key];
  }
  if (typeof page.scan_complete === 'boolean') safePage.scan_complete = page.scan_complete;
  if (Object.keys(safePage).length) output.media_page = safePage;

  const timing = record(source.media_timing), safeTiming = {};
  addCounts(safeTiming, timing, TIMING_COUNTS, 65535);
  addCounts(safeTiming, timing, TIMING_MS, 60000);
  if (Object.keys(safeTiming).length) output.media_timing = safeTiming;
  return output;
}
