import { isTrustedAdobeURL } from './core.mjs';

const METHODS = new Set(['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS']);
const ERRORS = new Set(['ERR_NAME_NOT_RESOLVED', 'ERR_DNS_TIMED_OUT', 'ERR_TIMED_OUT',
  'ERR_CONNECTION_TIMED_OUT', 'ERR_ABORTED', 'ERR_FAILED', 'ERR_CONNECTION_RESET',
  'ERR_CONNECTION_CLOSED', 'ERR_CONNECTION_REFUSED', 'ERR_INTERNET_DISCONNECTED']);
const VERIFICATION_HOST = 'arks-client.adobe.com';
const MAX_EVENTS = 40;

/** 诊断域与可导航/验证链接白名单分离；动态路径、查询参数和身份信息一律不保留。 */
export function diagnosticRequest(raw, method) {
  try {
    const url = new URL(raw);
    const verification = url.protocol === 'https:' && !url.username && !url.password && !url.port &&
      url.hostname === VERIFICATION_HOST;
    if (!verification && !isTrustedAdobeURL(raw)) return null;
    let path = '/:resource';
    if (verification) {
      if (/^\/fc\/gt2\/public_key\/[^/]+\/?$/.test(url.pathname)) path = '/fc/gt2/public_key/:key';
      else if (/\/(?:api|enforcement)\.js$/.test(url.pathname)) path = '/:resource/api.js';
      else if (/\/settings(?:\/|$)/.test(url.pathname)) path = '/:resource/settings';
      else if (/^\/fc\/(?:a|ca|gfct|answer)\/?$/.test(url.pathname)) path = url.pathname.replace(/\/$/, '');
    } else if (url.hostname === 'auth.services.adobe.com' && url.pathname === '/signin/v2/accounts') {
      path = '/signin/v2/accounts';
    }
    return { method: METHODS.has(method) ? method : 'OTHER', path: `https://${url.hostname}${path}` };
  } catch { return null; }
}

function requestTime(request, now) {
  let timing;
  try { timing = request.timing(); } catch { timing = {}; }
  const started = Number.isFinite(timing.startTime) && timing.startTime > 0 ? timing.startTime : now();
  const elapsed = Number.isFinite(timing.responseEnd) && timing.responseEnd >= 0 ? timing.responseEnd : null;
  return { time: new Date(started).toISOString(), ...(elapsed === null ? {} : { elapsedMs: Math.round(elapsed) }) };
}

function safeFailure(failure) {
  const code = String(failure?.errorText || '').replace(/^net::/, '');
  return ERRORS.has(code) ? code : 'NETWORK_ERROR';
}

async function readCaptchaCode(response) {
  try {
    const headers = response.headers();
    if (!/\bapplication\/(?:[\w.-]+\+)?json\b/i.test(headers['content-type'] || '')) return null;
    const length = Number(headers['content-length']);
    if (Number.isFinite(length) && length > 16384) return null;
    // 只读已完成响应，不发新请求；原文不进入事件、日志或持久状态。
    const body = await response.body();
    if (body.length > 16384) return null;
    return JSON.parse(body.toString('utf8'))?.errorCode === 'captcha_required' ? 'captcha_required' : null;
  } catch { return null; }
}

/** 只使用同一页面每条归一化请求路径的最新终态，成功重载会清除该路径的旧网络失败。 */
function latestEvents(events) {
  const latest = new Map();
  for (const event of events) latest.set(`${event.method} ${event.path}`, event);
  return [...latest.values()];
}

export function hasCaptchaRequirement(events) {
  return latestEvents(events).some(event => event.errorCode === 'captcha_required');
}

export function verificationDiagnosis(events) {
  const active = latestEvents(events);
  const failures = active.filter(event => event.path.startsWith(`https://${VERIFICATION_HOST}/`));
  if (failures.some(event => ['ERR_NAME_NOT_RESOLVED', 'ERR_DNS_TIMED_OUT'].includes(event.errorCode))) {
    return { code: 'verification_dns_failed' };
  }
  if (failures.some(event => ['ERR_TIMED_OUT', 'ERR_CONNECTION_TIMED_OUT'].includes(event.errorCode))) {
    return { code: 'verification_request_timeout' };
  }
  // ERR_ABORTED 只证明被取消；即使持续约 20 秒，也不能据此断言网络超时。
  if (failures.some(event => event.errorCode === 'ERR_ABORTED')) return { code: 'verification_request_aborted' };
  if (active.some(event => event.errorCode === 'captcha_required')) return { code: 'captcha_required' };
  return { code: null };
}

export class NetworkDiagnostics {
  constructor({ now = Date.now } = {}) {
    this.now = now;
    this.events = [];
    this.seen = new WeakSet();
    this.pending = new WeakMap();
    this.contexts = new WeakSet();
  }

  attach(context) {
    if (!context || this.contexts.has(context) || typeof context.on !== 'function') return;
    this.contexts.add(context);
    const observe = request => {
      let page;
      try { page = request.frame().page(); } catch { return; }
      void this.record(request, page, true).catch(() => {});
    };
    context.on('requestfailed', observe);
    context.on('requestfinished', observe);
  }

  record(request, page, completed = false) {
    if (this.pending.has(request)) return this.pending.get(request);
    const operation = this.recordCompleted(request, page, completed);
    this.pending.set(request, operation);
    operation.then(() => this.pending.delete(request), () => this.pending.delete(request));
    return operation;
  }

  async recordCompleted(request, page, completed) {
    if (this.seen.has(request)) return;
    const metadata = diagnosticRequest(request.url(), request.method());
    if (!metadata) return;
    const failure = request.failure();
    // 保留请求可能仍在进行中；不能用 response() 等待它完成，阻塞只读 status。
    if (!completed && !failure && !(request.timing()?.responseEnd >= 0)) return;
    this.seen.add(request);
    const event = { ...metadata, ...requestTime(request, this.now) };
    if (failure) event.errorCode = safeFailure(failure);
    else {
      let timer;
      try {
        const details = await Promise.race([
          (async () => {
            const response = await request.response();
            if (!response) return null;
            const result = { status: response.status() };
            if (result.status < 400 && metadata.path.endsWith('/:resource')) return null;
            if (metadata.method === 'POST' && metadata.path === 'https://auth.services.adobe.com/signin/v2/accounts' && result.status === 400) {
              const code = await readCaptchaCode(response);
              if (code) result.errorCode = code;
            }
            return result;
          })(),
          new Promise(resolve => { timer = setTimeout(() => resolve(null), 1000); }),
        ]);
        if (!details) return;
        Object.assign(event, details);
      } catch { return; }
      finally { clearTimeout(timer); }
    }
    this.events.push({ page, event });
    this.events = this.events.slice(-MAX_EVENTS);
  }

  async capture(page) {
    if (typeof page?.requests !== 'function') return;
    try {
      const requests = await page.requests();
      await Promise.all(requests.slice(-100).map(request => this.record(request, page).catch(() => {})));
    } catch { /* 页面关闭或旧版 Playwright 不提供保留请求时，仅使用已捕获事件。 */ }
  }

  snapshot(page) {
    return this.events.filter(item => item.page === page).map(({ event }) => ({ ...event }))
      .sort((left, right) => left.time.localeCompare(right.time));
  }
}
