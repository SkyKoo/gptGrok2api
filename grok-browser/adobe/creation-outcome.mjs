import { RegistrationError } from './core.mjs';

const ACCOUNT_URL = 'https://auth.services.adobe.com/signin/v2/accounts';
const UNKNOWN = { creation_outcome: 'unknown' };

// This durable ledger is separate from bounded display diagnostics. A missing
// ledger on an already submitted legacy journal is never proof of rejection.
export function creationOutcome(state) {
  const account = state?.result?.account;
  if (account && typeof account.email === 'string' && account.email.trim().toLowerCase() === state.email?.trim().toLowerCase() &&
      typeof account.account_id === 'string' && account.account_id && typeof account.token === 'string' && account.token) {
    return { creation_outcome: 'accepted' };
  }
  const evidence = state?.creationEvidence;
  if (state?.submission?.started === false && !evidence) return { creation_outcome: 'not_submitted' };
  if (state?.submission?.started !== true || evidence?.version !== 1 || evidence.marker !== state.submission.startedAt) return { ...UNKNOWN };
  const counts = ['requests', 'finished', 'rejected', 'unknown'];
  if (!counts.every(key => Number.isSafeInteger(evidence[key]) && evidence[key] >= 0) || typeof evidence.accepted !== 'boolean' ||
      evidence.finished > evidence.requests || evidence.rejected + evidence.unknown > evidence.finished) return { ...UNKNOWN };
  if (evidence.accepted === true) return { creation_outcome: 'accepted' };
  if (evidence.sealed === true && evidence.complete === true && evidence.requests > 0 &&
      evidence.requests === evidence.finished && evidence.requests === evidence.rejected && evidence.unknown === 0) {
    return { creation_outcome: 'rejected', creation_error_code: 'captcha_required', creation_http_status: 400 };
  }
  return { ...UNKNOWN };
}

function isCreationRequest(request) {
  try {
    const url = new URL(request.url());
    return request.method() === 'POST' && !url.username && !url.password && !url.port &&
      url.origin + url.pathname === ACCOUNT_URL;
  } catch { return false; }
}

async function responseKind(request) {
  let timer;
  try {
    return await Promise.race([
      (async () => {
        if (request.failure()) return 'unknown';
        const response = await request.response();
        const status = response?.status();
        if (Number.isInteger(status) && status >= 200 && status < 300) return 'accepted';
        if (status !== 400) return 'unknown';
        const headers = response.headers();
        if (!/\bapplication\/(?:[\w.-]+\+)?json\b/i.test(headers['content-type'] || '')) return 'unknown';
        const length = Number(headers['content-length']);
        if (Number.isFinite(length) && length > 16384) return 'unknown';
        const body = await response.body();
        if (body.length > 16384) return 'unknown';
        return JSON.parse(body.toString('utf8'))?.errorCode === 'captcha_required' ? 'rejected' : 'unknown';
      })(),
      new Promise(resolve => { timer = setTimeout(() => resolve('unknown'), 1000); }),
    ]);
  } catch { return 'unknown'; }
  finally { clearTimeout(timer); }
}

export class CreationTracker {
  constructor(store, context) {
    this.store = store;
    this.context = context;
    this.requests = new WeakMap();
    this.pending = new Set();
    this.writes = Promise.resolve();
    this.failed = false;
    this.startListener = request => this.started(request);
    this.endListener = request => this.finished(request);
    context?.on?.('request', this.startListener);
    context?.on?.('requestfinished', this.endListener);
    context?.on?.('requestfailed', this.endListener);
  }

  async begin(page) {
    const state = await this.store.load();
    if (!state.submission?.started || state.creationEvidence || this.evidence) throw new RegistrationError('creation_tracking_invalid');
    this.page = page;
    this.evidence = { version: 1, marker: state.submission.startedAt, complete: typeof this.context?.on === 'function', sealed: false,
      requests: 0, finished: 0, rejected: 0, unknown: 0, accepted: false };
    await this.persist();
  }

  persist() {
    const snapshot = structuredClone(this.evidence);
    this.writes = this.writes.then(() => this.store.save({ creationEvidence: snapshot })).catch(() => { this.failed = true; });
    return this.writes.then(() => { if (this.failed) throw new RegistrationError('creation_evidence_write_failed'); });
  }

  started(request) {
    if (!this.evidence || !isCreationRequest(request) || this.requests.has(request)) return;
    let owned = false;
    try { owned = request.frame().page() === this.page; } catch { /* Unknown ownership cannot prove rejection. */ }
    if (!owned) this.evidence.complete = false;
    this.evidence.requests++;
    this.evidence.sealed = false;
    this.requests.set(request, { finished: false, owned });
    void this.persist().catch(() => {});
  }

  finished(request) {
    if (!this.evidence || !isCreationRequest(request)) return;
    if (!this.requests.has(request)) {
      this.evidence.complete = false;
      this.started(request);
      // A completed request not observed at start may predate our Create.
      // Neither its success nor its rejection can certify this attempt.
      const untracked = this.requests.get(request);
      if (untracked) untracked.owned = false;
    }
    const entry = this.requests.get(request);
    if (!entry || entry.finished) return;
    entry.finished = true;
    const operation = (async () => {
      const kind = entry.owned ? await responseKind(request) : 'unknown';
      this.evidence.finished++;
      if (kind === 'accepted') this.evidence.accepted = true;
      else this.evidence[kind]++;
      await this.persist();
    })().catch(() => { this.failed = true; });
    this.pending.add(operation);
    void operation.finally(() => this.pending.delete(operation));
  }

  async accepted() {
    const state = await this.store.load();
    if (!state.submission?.started) return;
    if (!this.evidence) this.evidence = state.creationEvidence?.marker === state.submission.startedAt
      ? structuredClone(state.creationEvidence)
      : { version: 1, marker: state.submission.startedAt, complete: false, sealed: false,
        requests: 0, finished: 0, rejected: 0, unknown: 0, accepted: false };
    this.evidence.accepted = true;
    await this.persist();
  }

  // Only the browser owner calls this AFTER context.close resolves. Unknown
  // close, pending requests or lost callbacks must never authorize a new Create.
  async closed() {
    this.context?.off?.('request', this.startListener);
    this.context?.off?.('requestfinished', this.endListener);
    this.context?.off?.('requestfailed', this.endListener);
    await Promise.all([...this.pending]);
    await this.writes;
    if (!this.evidence) return;
    if (this.failed) throw new RegistrationError('creation_evidence_write_failed');
    this.evidence.sealed = true;
    await this.persist();
  }
}
