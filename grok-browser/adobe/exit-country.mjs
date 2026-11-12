import { spawn } from 'node:child_process';
import { RegistrationError } from './core.mjs';
import { browserProxy } from './browser-runtime.mjs';

const COUNTRIES = new Set('AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW'.split(' '));
const SOURCES = new Set(['ipwho_is', 'default_us', 'preserved']);
const LOOKUP_URL = 'https://ipwho.is/';
const MAX_BYTES = 64 * 1024;
const TIMEOUT_MS = 4500;
export function validCountry(value) { return typeof value === 'string' && COUNTRIES.has(value); }
function validSelection(value) {
  return validCountry(value?.country) && SOURCES.has(value?.source) && (value.source !== 'default_us' || value.country === 'US');
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

export async function lookupExitCountry(proxy, { signal, spawnProcess = spawn, timeoutMs = TIMEOUT_MS } = {}) {
  signal?.throwIfAborted();
  const config = countryLookupConfig(proxy);
  return new Promise((resolve, reject) => {
    const fallback = () => signal?.aborted ? reject(signal.reason) : resolve({ country: 'US', source: 'default_us' });
    // Disable curlrc, redirects, retries, environment proxies and indefinite
    // downloads. Only the fixed HTTPS endpoint can receive this request.
    const env = { ...process.env };
    for (const key of Object.keys(env)) if (/^(?:https?|all|no)_proxy$/i.test(key)) delete env[key];
    let child, timer, length = 0, overflow = false, timedOut = false;
    const chunks = [];
    const abort = () => child?.kill('SIGKILL');
    const cleanup = () => { clearTimeout(timer); signal?.removeEventListener('abort', abort); };
    try {
      child = spawnProcess('curl', ['-q', '--config', '-', '--silent', '--fail', '--proto', '=https',
        '--max-redirs', '0', '--max-time', String(timeoutMs / 1000), '--connect-timeout', String(timeoutMs / 1000),
        '--max-filesize', String(MAX_BYTES), '--write-out', '\n%{http_code}'],
      { env, stdio: ['pipe', 'pipe', 'ignore'], shell: false });
    } catch { fallback(); return; }
    signal?.addEventListener('abort', abort, { once: true });
    timer = setTimeout(() => { timedOut = true; abort(); }, timeoutMs);
    child.stdout.on('data', chunk => {
      length += chunk.length;
      if (length > MAX_BYTES + 4) { overflow = true; abort(); }
      else chunks.push(chunk);
    });
    child.stdin.on('error', () => {});
    child.on('error', () => { cleanup(); fallback(); });
    child.on('close', code => {
      cleanup();
      if (signal?.aborted) { reject(signal.reason); return; }
      let country;
      if (code === 0 && !overflow && !timedOut) {
        try {
          const raw = Buffer.concat(chunks).toString('utf8');
          if (!raw.endsWith('\n200')) throw new Error();
          const value = JSON.parse(raw.slice(0, -4));
          if (value && value.success === true && validCountry(value.country_code)) country = value.country_code;
        } catch { /* A malformed, redirected or failed response is not location evidence. */ }
      }
      resolve(country ? { country, source: 'ipwho_is' } : { country: 'US', source: 'default_us' });
    });
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
  const saved = { country: profile.country, source: profile.countrySource };
  if (profile.countrySource) {
    if (!validSelection(saved)) throw new RegistrationError('registration_country_invalid');
    return saved;
  }
  if (mode !== 'registration' || !requireNew || state.submission.started) return { country: profile.country, source: 'preserved' };
  const selection = await lookup(proxy, { signal });
  signal?.throwIfAborted();
  if (!validSelection(selection)) throw new RegistrationError('registration_country_invalid');
  await store.save({ config: { ...state.config, profile: { ...profile, country: selection.country, countrySource: selection.source } } });
  return selection;
}
