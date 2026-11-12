const SOURCES = new Set(['visible_challenge_frame', 'visible_instruction', 'upstream_captcha_response']);
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
  return {
    submission_started: state?.submission?.started === true,
    human_verification_required: human?.required === true,
    ...(source ? { verification_source: source } : {}),
    ...(networkCode ? { verification_network_code: networkCode } : {}),
    ...(FORM_STAGES.has(result?.detected) ? { detected_stage: result.detected } : {}),
    ...networkTimeline(result, now),
  };
}
