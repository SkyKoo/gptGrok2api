import { spawn } from 'node:child_process';
import { RegistrationError } from './core.mjs';
import { browserProxy } from './browser-runtime.mjs';

const COUNTRIES = new Set('AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW'.split(' '));
const SOURCES = new Set(['ipwho_is', 'default_us', 'preserved']);
const LOOKUP_URL = 'https://ipwho.is/';
const MAX_BYTES = 64 * 1024;
const TIMEOUT_MS = 15_000;
const DIAGNOSTIC_LIMIT_MS = 60_000;
const META_MARKER = '\nCFM_COUNTRY_META:';
const FAILURE_KINDS = new Set(['none', 'timeout', 'proxy_auth', 'proxy_tunnel', 'proxy_connection',
  'dns', 'connection_failed', 'tls', 'http_error', 'http_redirect', 'lookup_rejected', 'invalid_response',
  'response_too_large', 'spawn_failed', 'transport_failed']);
const validHTTPStatus = value => Number.isInteger(value) && (value === 0 || value >= 100 && value <= 599);
function validDiagnostics(value) {
  return value && typeof value === 'object' && !Array.isArray(value) &&
    Object.keys(value).every(key => ['failure_kind', 'curl_exit_code', 'http_status', 'connect_status', 'elapsed_ms'].includes(key)) &&
    FAILURE_KINDS.has(value.failure_kind) && validHTTPStatus(value.http_status) && validHTTPStatus(value.connect_status) &&
    Number.isInteger(value.elapsed_ms) && value.elapsed_ms >= 0 && value.elapsed_ms <= DIAGNOSTIC_LIMIT_MS &&
    (value.curl_exit_code === undefined || Number.isInteger(value.curl_exit_code) && value.curl_exit_code >= 0 && value.curl_exit_code <= 255);
}
export function validCountry(value) { return typeof value === 'string' && COUNTRIES.has(value); }
function validSelection(value) {
  return validCountry(value?.country) && SOURCES.has(value?.source) && (value.source !== 'default_us' || value.country === 'US');
}
function safeSelection(value) {
  if (!validSelection(value)) throw new RegistrationError('registration_country_invalid');
  const diagnostics = value.diagnostics;
  // Optional evidence must never prevent recovery of a valid saved identity.
  return { country: value.country, source: value.source,
    ...(validDiagnostics(diagnostics) &&
      (value.source === 'default_us' ? diagnostics.failure_kind !== 'none' : diagnostics.failure_kind === 'none')
      ? { diagnostics: { ...diagnostics } } : {}) };
}

// curl is already supplied by the pinned Worker base. Its separate process has
// no browser cookies/storage. Proxy credentials travel on stdin, never argv.
export function countryLookupConfig(proxy) {
  const parsed = browserProxy(proxy === 'direct' ? '' : proxy);
  let route = '';
  if (parsed) {
    const url = new URL(parsed.server);
    if (url.protocol === 'socks5:') url.protocol = 'socks5h:';
    if (parsed.username) url.username = encodeURIComponent(parsed.username);
    if (parsed.password) url.password = encodeURIComponent(parsed.password);
    route = url.href;
  }
  const quote = value => `"${value.replaceAll('\\', '\\\\').replaceAll('"', '\\"')}"`;
  return `url = ${quote(LOOKUP_URL)}\nproxy = ${quote(route)}\nnoproxy = ""\n`;
}

export async function lookupExitCountry(proxy, { signal, spawnProcess = spawn, timeoutMs = TIMEOUT_MS,
  now = () => performance.now() } = {}) {
  signal?.throwIfAborted();
  const config = countryLookupConfig(proxy);
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0 || timeoutMs > TIMEOUT_MS) throw new RegistrationError('registration_country_timeout_invalid');
  const started = now();
  return new Promise((resolve, reject) => {
    // Disable curlrc, redirects, retries and environment proxies. The lookup
    // never uses browser cookies, changes route, or sends the raw response out.
    const env = { ...process.env };
    for (const key of Object.keys(env)) if (/^(?:https?|all|no)_proxy$/i.test(key)) delete env[key];
    let child, timer, length = 0, overflow = false, timedOut = false, settled = false;
    const chunks = [];
    const abort = () => child?.kill('SIGKILL');
    const cleanup = () => { clearTimeout(timer); signal?.removeEventListener('abort', abort); };
    const finish = (code, forcedKind) => {
      if (settled) return;
      settled = true;
      cleanup();
      if (signal?.aborted) { reject(signal.reason); return; }
      const elapsed = Math.min(DIAGNOSTIC_LIMIT_MS, Math.max(0, Math.round(now() - started)));
      const raw = Buffer.concat(chunks).toString('utf8');
      const marker = raw.lastIndexOf(META_MARKER);
      const metadata = marker < 0 ? null : /^(\d{3}) (\d{3})$/.exec(raw.slice(marker + META_MARKER.length));
      const httpStatus = metadata && validHTTPStatus(Number(metadata[1])) ? Number(metadata[1]) : 0;
      const connectStatus = metadata && validHTTPStatus(Number(metadata[2])) ? Number(metadata[2]) : 0;
      let country, kind = forcedKind;
      if (!kind) {
        if (timedOut || code === 28) kind = 'timeout';
        else if (overflow || code === 63) kind = 'response_too_large';
        else if (connectStatus === 407 || httpStatus === 407) kind = 'proxy_auth';
        else if (connectStatus >= 300) kind = 'proxy_tunnel';
        else if (code === 5 || code === 6) kind = 'dns';
        else if (code === 7) kind = proxy && proxy !== 'direct' ? 'proxy_connection' : 'connection_failed';
        else if (code === 97) kind = 'proxy_tunnel';
        else if ([35, 51, 58, 59, 60, 64, 66, 77, 80, 82, 83, 90, 91].includes(code)) kind = 'tls';
        else if (httpStatus >= 300 && httpStatus < 400) kind = 'http_redirect';
        else if (httpStatus >= 400) kind = 'http_error';
        else if (code !== 0) kind = 'transport_failed';
        else if (httpStatus !== 200 || !metadata) kind = 'invalid_response';
        else {
          try {
            const value = JSON.parse(raw.slice(0, marker));
            if (value?.success === false) kind = 'lookup_rejected';
            else if (value?.success === true && validCountry(value.country_code)) { country = value.country_code; kind = 'none'; }
            else kind = 'invalid_response';
          } catch { kind = 'invalid_response'; }
        }
      }
      const diagnostics = { failure_kind: kind,
        ...(Number.isInteger(code) && code >= 0 && code <= 255 ? { curl_exit_code: code } : {}),
        http_status: httpStatus, connect_status: connectStatus, elapsed_ms: elapsed };
      resolve({ country: country || 'US', source: country ? 'ipwho_is' : 'default_us', diagnostics });
    };
    try {
      child = spawnProcess('curl', ['-q', '--config', '-', '--silent', '--fail', '--proto', '=https',
        '--max-redirs', '0', '--max-time', String(timeoutMs / 1000), '--connect-timeout', String(timeoutMs / 1000),
        '--max-filesize', String(MAX_BYTES), '--write-out', `${META_MARKER}%{http_code} %{http_connect}`],
      { env, stdio: ['pipe', 'pipe', 'ignore'], shell: false });
    } catch { finish(undefined, 'spawn_failed'); return; }
    signal?.addEventListener('abort', abort, { once: true });
    timer = setTimeout(() => { timedOut = true; abort(); }, timeoutMs);
    child.stdout.on('data', chunk => {
      length += chunk.length;
      if (length > MAX_BYTES + META_MARKER.length + 7) { overflow = true; abort(); }
      else chunks.push(chunk);
    });
    child.stdin.on('error', () => {});
    child.on('error', () => finish(undefined, 'spawn_failed'));
    child.on('close', code => finish(code));
    child.stdin.end(config);
    if (signal?.aborted) abort();
  });
}

// Idempotent within one private profile. Resuming a pre-existing profile never
// changes its country, even if it predates country-source metadata.
export async function prepareRegistrationCountry(store, { mode, requireNew, proxy, signal } = {}, lookup = lookupExitCountry) {
  signal?.throwIfAborted();
  const state = await store.load();
  const profile = state.config?.profile;
  if (!validCountry(profile?.country)) throw new RegistrationError('registration_country_invalid');
  const saved = { country: profile.country, source: profile.countrySource,
    ...(profile.countryDiagnostics ? { diagnostics: profile.countryDiagnostics } : {}) };
  if (profile.countrySource) {
    return safeSelection(saved);
  }
  if (mode !== 'registration' || !requireNew || state.submission.started) return { country: profile.country, source: 'preserved' };
  const result = await lookup(proxy, { signal });
  signal?.throwIfAborted();
  const selection = safeSelection(result);
  const { countryDiagnostics: _discardedDiagnostics, ...identity } = profile;
  await store.save({ config: { ...state.config, profile: { ...identity, country: selection.country, countrySource: selection.source,
    ...(selection.diagnostics ? { countryDiagnostics: selection.diagnostics } : {}) } } });
  return selection;
}
