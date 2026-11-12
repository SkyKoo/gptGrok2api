import { formFailureDiagnostics } from './browser.mjs';
import { STARTUP_STAGES, STARTUP_LIMIT_MS } from './startup-progress.mjs';
import { VERIFICATION_WAIT_OUTCOMES, VERIFICATION_WAIT_LIMIT_MS } from './verification-observation.mjs';

const SOURCES = new Set(['visible_challenge_frame', 'visible_instruction', 'upstream_captcha_response']);
const INSTRUCTIONS = new Set(['start_puzzle', 'complete_puzzle', 'prove_human', 'not_robot', 'identity_instruction']);
const FRAME_KINDS = new Set(['adobe_verification', 'challenge_provider', 'adobe_page', 'other_frame']);
const NETWORK_CODES = new Set(['verification_dns_failed', 'verification_request_timeout',
  'verification_request_aborted', 'captcha_required']);
const FORM_STAGES = new Set(['awaiting_email', 'awaiting_password', 'awaiting_profile',
  'awaiting_approval', 'awaiting_verification', 'needs_manual']);
const METHODS = new Set(['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS']);
const ERROR_CODES = new Set(['ERR_NAME_NOT_RESOLVED', 'ERR_DNS_TIMED_OUT', 'ERR_TIMED_OUT',
  'ERR_CONNECTION_TIMED_OUT', 'ERR_ABORTED', 'ERR_FAILED', 'ERR_CONNECTION_RESET',
  'ERR_CONNECTION_CLOSED', 'ERR_CONNECTION_REFUSED', 'ERR_INTERNET_DISCONNECTED',
  'NETWORK_ERROR', 'captcha_required']);
const RESOURCES = new Map([
  ['https://auth.services.adobe.com/signin/v2/accounts', 'account_creation'],
  ['https://arks-client.adobe.com/fc/gt2/public_key/:key', 'verification_session'],
  ['https://arks-client.adobe.com/:resource/api.js', 'verification_script'],
  ['https://arks-client.adobe.com/:resource/settings', 'verification_settings'],
  ['https://arks-client.adobe.com/fc/a', 'verification_a'],
  ['https://arks-client.adobe.com/fc/ca', 'verification_ca'],
  ['https://arks-client.adobe.com/fc/gfct', 'verification_gfct'],
  ['https://arks-client.adobe.com/fc/answer', 'verification_answer'],
  ['https://arks-client.adobe.com/:resource', 'verification_other'],
]);
const WINDOW_MS = 15 * 60 * 1000;
const FORM_ERRORS = new Set(['account_not_found', 'invalid_email', 'invalid_password',
  'invalid_verification_code', 'invalid_field', 'generic_notice']);
const INVALID_FIELDS = new Set(['email', 'password', 'verification_code', 'profile',
  'first_name', 'last_name', 'birth_day', 'birth_month', 'birth_year', 'country', 'terms', 'phone', 'other']);
const FIELD_REASONS = new Set(['required', 'format', 'range', 'unchecked', 'invalid']);
const FAILURE_ACTIONS = new Set(['inspect_form', 'open_login', 'click_login_entry', 'fill_email',
  'fill_password', 'submit_email', 'submit_password', 'wait_login_result', 'request_code',
  'verify_code', 'export_session', 'open_firefly', ...STARTUP_STAGES]);
const FAILURE_PHASES = new Set(['before_submission', 'submission_started']);
const EXCEPTION_TYPES = new Set(['registration', 'timeout', 'navigation', 'page_closed', 'unknown']);
const PROFILE_FIELDS = new Set(['country', 'birth_month', 'birth_year', 'birth_day']);
const OPTION_REASONS = new Set(['missing', 'incomplete', 'ambiguous', 'disabled', 'invalid']);

function failureDiagnostics(result) {
  if (typeof result?.code !== 'string' || !result.code) return {};
  // A helper may preserve the exact rejecting form before a later status read.
  // Revalidate every value at the persistence boundary, including helper output.
  const source = result.failureDiagnostics && typeof result.failureDiagnostics === 'object'
    && !Array.isArray(result.failureDiagnostics) ? result.failureDiagnostics : {};
  const form = result.code === 'form_rejected' && !FAILURE_ACTIONS.has(source.failure_action)
    ? formFailureDiagnostics(result.form) : {};
  const classification = FORM_ERRORS.has(source.form_error) ? source.form_error : form.form_error;
  const evidence = Array.isArray(source.invalid_fields) ? source : form;
  const invalid = [...new Set((Array.isArray(evidence.invalid_fields) ? evidence.invalid_fields : [])
    .slice(0, 20).filter(value => INVALID_FIELDS.has(value)))].slice(0, INVALID_FIELDS.size);
  const reasons = [];
  const reasonFields = new Set();
  for (const item of (Array.isArray(evidence.invalid_field_reasons) ? evidence.invalid_field_reasons.slice(0, 20) : [])) {
    if (!item || typeof item !== 'object' || Array.isArray(item) || !invalid.includes(item.field)
      || !FIELD_REASONS.has(item.reason) || reasonFields.has(item.field)) continue;
    reasonFields.add(item.field);
    reasons.push({ field: item.field, reason: item.reason });
  }
  return { ...(result.code === 'form_rejected' && classification ? { form_error: classification } : {}),
    ...(result.code === 'profile_option_invalid' && PROFILE_FIELDS.has(source.profile_field) && OPTION_REASONS.has(source.profile_option_reason)
      ? { profile_field: source.profile_field, profile_option_reason: source.profile_option_reason,
        ...(Number.isSafeInteger(source.option_count) && source.option_count >= 0 && source.option_count <= 400
          ? { option_count: source.option_count } : {}) } : {}),
    ...(result.code === 'form_rejected' && invalid.length ? { invalid_fields: invalid } : {}),
    ...(result.code === 'form_rejected' && reasons.length ? { invalid_field_reasons: reasons } : {}),
    ...(FAILURE_ACTIONS.has(source.failure_action) ? { failure_action: source.failure_action } : {}),
    ...(FAILURE_PHASES.has(source.failure_phase) ? { failure_phase: source.failure_phase } : {}),
    ...(EXCEPTION_TYPES.has(source.exception_type) ? { exception_type: source.exception_type } : {}),
    ...(STARTUP_STAGES.has(source.failure_action) && Number.isSafeInteger(source.elapsed_ms) && source.elapsed_ms >= 0 && source.elapsed_ms <= STARTUP_LIMIT_MS
      ? { elapsed_ms: source.elapsed_ms } : {}),
    ...(STARTUP_STAGES.has(source.failure_action) && Number.isSafeInteger(source.duration_ms) && source.duration_ms >= 0 && source.duration_ms <= STARTUP_LIMIT_MS
      ? { duration_ms: source.duration_ms } : {}) };
}

function timestamp(value) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{3})?Z$/.test(value)) return null;
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) return null;
  const canonical = new Date(parsed).toISOString();
  return canonical.replace('.000Z', 'Z') === value.replace('.000Z', 'Z') ? parsed : null;
}

/** The Worker opts in while its browser is still open; old callers keep their schema. */
function networkTimeline(result, now) {
  if (typeof now !== 'function') return {};
  let captured;
  try { captured = now(); } catch { return {}; }
  if (!Number.isSafeInteger(captured) || captured < Date.UTC(2000, 0) || captured >= Date.UTC(2100, 0)) return {};
  const events = [];
  for (const raw of (Array.isArray(result?.network) ? result.network.slice(-40) : [])) {
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) continue;
    const resource = RESOURCES.get(raw.path);
    const started = timestamp(raw.time);
    if (!resource || !METHODS.has(raw.method) || started === null || started < captured - WINDOW_MS || started > captured + 5000) continue;
    const status = Number.isInteger(raw.status) && raw.status >= 100 && raw.status <= 599 ? raw.status : undefined;
    const error = ERROR_CODES.has(raw.errorCode) ? raw.errorCode : undefined;
    if (status === undefined && error === undefined) continue;
    const event = { resource, method: raw.method, started_at: new Date(started).toISOString(),
      ...(status !== undefined ? { status } : {}), ...(error ? { error_code: error } : {}) };
    const observed = timestamp(raw.observedAt);
    if (observed !== null && observed >= started - 5000 && observed <= captured + 5000) {
      event.observed_at = new Date(observed).toISOString();
    }
    if (raw.source === 'event' || raw.source === 'retained') event.source = raw.source;
    if (Number.isInteger(raw.elapsedMs) && raw.elapsedMs >= 0 && raw.elapsedMs <= WINDOW_MS) event.elapsed_ms = raw.elapsedMs;
    events.push(event);
  }
  return { captured_at: new Date(captured).toISOString(),
    ...(events.length ? { network_events: events.slice(-8) } : {}) };
}

/** Worker 仅保留判断依据，不把表单内容、挑战题目、网络原文或凭据交给注册日志。 */
export function registrationDiagnostics(result, state, { now } = {}) {
  const human = result?.form?.humanVerification;
  const source = SOURCES.has(human?.source) ? human.source : undefined;
  const networkCode = NETWORK_CODES.has(result?.networkDiagnosis?.code)
    ? result.networkDiagnosis.code : undefined;
  const wait = result?.verificationWait;
  const validWait = wait && VERIFICATION_WAIT_OUTCOMES.has(wait.outcome) &&
    Number.isSafeInteger(wait.elapsed_ms) && wait.elapsed_ms >= 0 && wait.elapsed_ms <= VERIFICATION_WAIT_LIMIT_MS;
  return {
    submission_started: state?.submission?.started === true,
    human_verification_required: human?.required === true,
    ...(source ? { verification_source: source } : {}),
    ...(INSTRUCTIONS.has(human?.instruction_kind) ? {verification_instruction_kind:human.instruction_kind} : {}),
    ...(FRAME_KINDS.has(human?.frame_kind) ? {verification_frame_kind:human.frame_kind} : {}),
    ...(typeof human?.puzzle_entry_visible === 'boolean' ? {puzzle_entry_visible:human.puzzle_entry_visible} : {}),
    ...(networkCode ? { verification_network_code: networkCode } : {}),
    ...(FORM_STAGES.has(result?.detected) ? { detected_stage: result.detected } : {}),
    ...(validWait ? { verification_wait_elapsed_ms: wait.elapsed_ms, verification_wait_outcome: wait.outcome } : {}),
    ...failureDiagnostics(result),
    ...networkTimeline(result, now),
  };
}
