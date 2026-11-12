import { performance } from 'node:perf_hooks';

export const STARTUP_STAGES = new Set([
  'starting_browser', 'adobe_loading_account_page', 'adobe_waiting_create_entry',
  'adobe_reloading_account_page', 'adobe_opening_registration',
  'adobe_waiting_registration_form', 'adobe_checking_registration_form',
  'adobe_restoring_registration_page',
]);
export const STARTUP_LIMIT_MS = 150_000;

// Inspect errors only to classify them. Neither messages, URLs nor browser
// launch arguments are retained in progress frames or the private journal.
export function startupExceptionType(error) {
  const message = typeof error?.message === 'string' ? error.message : '';
  if (error?.name === 'TimeoutError' || ['browser_launch_timeout', 'stage_transition_timeout', 'recovery_page_observation_timeout'].includes(error?.code) || /Timeout \d+ms exceeded/i.test(message)) return 'timeout';
  if (/Target page, context or browser has been closed|page has been closed|Browser closed/i.test(message)) return 'page_closed';
  if (/Execution context was destroyed|Frame was detached|Cannot find context with specified id|navigation.*(?:aborted|interrupted)/i.test(message)) return 'navigation';
  return typeof error?.code === 'string' && /^[a-z][a-z0-9_]{0,79}$/.test(error.code) ? 'registration' : 'unknown';
}

/** One bounded stream belongs to one init RPC, measured with a monotonic clock. */
export class StartupProgress {
  constructor({ emit = () => {}, now = () => performance.now() } = {}) {
    this.emit = emit;
    this.now = now;
    this.origin = now();
    this.elapsed = 0;
    this.sequence = 0;
    this.failures = new WeakMap();
  }

  elapsedMS() {
    const value = Math.floor(this.now() - this.origin);
    if (Number.isSafeInteger(value) && value >= 0) this.elapsed = Math.max(this.elapsed, value);
    return this.elapsed;
  }

  publish(step, status, error) {
    const elapsed = this.elapsedMS(), duration = elapsed - step.started;
    const detail = { failure_action: step.stage, failure_phase: 'before_submission',
      exception_type: startupExceptionType(error),
      ...(elapsed <= STARTUP_LIMIT_MS ? { elapsed_ms: elapsed, duration_ms: duration } : {}) };
    if (status === 'failed' && error && (typeof error === 'object' || typeof error === 'function')) this.failures.set(error, detail);
    if (this.sequence < 32 && elapsed <= STARTUP_LIMIT_MS) {
      const progress = { sequence: ++this.sequence, stage: step.stage, status,
        elapsed_ms: elapsed, duration_ms: status === 'started' ? 0 : duration,
        ...(status === 'failed' ? { exception_type: detail.exception_type } : {}) };
      // Reporting cannot change browser actions or their timeout behaviour.
      try { this.emit(progress); } catch { /* The final RPC owns transport failure. */ }
    }
    return detail;
  }

  async run(stage, operation) {
    if (!STARTUP_STAGES.has(stage)) throw new TypeError('invalid_startup_stage');
    const step = { stage, started: this.elapsedMS() };
    this.lastStep = step;
    this.publish(step, 'started');
    try {
      const result = await operation();
      this.publish(step, 'completed');
      return result;
    } catch (error) {
      this.publish(step, 'failed', error);
      throw error;
    }
  }

  failureDetails(error) {
    if (error && (typeof error === 'object' || typeof error === 'function') && this.failures.has(error)) return { ...this.failures.get(error) };
    const elapsed = this.elapsedMS();
    return { failure_action: this.lastStep?.stage || 'starting_browser', failure_phase: 'before_submission',
      exception_type: startupExceptionType(error),
      ...(elapsed <= STARTUP_LIMIT_MS ? { elapsed_ms: elapsed, duration_ms: elapsed - (this.lastStep?.started ?? elapsed) } : {}) };
  }
}
