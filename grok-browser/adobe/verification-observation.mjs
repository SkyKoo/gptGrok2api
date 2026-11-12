export const VERIFICATION_WAIT_OUTCOMES = new Set([
  'still_required', 'resolved', 'network_error', 'interrupted', 'observation_error',
]);
export const VERIFICATION_WAIT_LIMIT_MS = 150_000;

/** Measure the existing passive observation window without changing its deadline. */
export function verificationObservation(now, report = () => {}) {
  let started;
  return {
    begin() { started ??= now(); },
    finish(outcome) {
      if (started === undefined) return;
      const elapsed = Math.floor(now() - started);
      if (!VERIFICATION_WAIT_OUTCOMES.has(outcome) || !Number.isSafeInteger(elapsed) ||
          elapsed < 0 || elapsed > VERIFICATION_WAIT_LIMIT_MS) return;
      // Diagnostics must never turn observation into a browser action or error.
      try { report({ elapsed_ms: elapsed, outcome }); } catch { /* Preserve flow result. */ }
    },
  };
}
