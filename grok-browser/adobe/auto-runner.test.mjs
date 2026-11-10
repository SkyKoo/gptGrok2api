import test from 'node:test';
import assert from 'node:assert/strict';
import { runAutomaticRegistration } from './auto-runner.mjs';
import { RegistrationError } from './core.mjs';

const profile = { firstName: 'Fixture', lastName: 'Person', birthMonth: 6, birthYear: '1980', country: 'US' };

function fixture({ phase = 'awaiting_email', submitted = false, patch = {}, human = false, fields = [] } = {}) {
  const state = {
    stage: submitted ? 'outcome_unknown' : 'form_ready', code: null, config: {},
    submission: { started: submitted, outcome: submitted ? 'unknown' : 'not_started' },
    ...patch,
  };
  const calls = [];
  const store = { load: async () => structuredClone(state) };
  const browser = {
    status: async () => ({ stage: state.stage, code: state.code, detected: phase === 'account' ? 'needs_manual' : phase, submissionStarted: state.submission.started,
      form: { host: phase === 'firefly' ? 'firefly.adobe.com' : phase === 'account' ? 'account.adobe.com' : 'auth.services.adobe.com',
        fields, headings: phase === 'unknown' ? ['Welcome'] : [], humanVerification: { required: human } } }),
    advance: async input => {
      calls.push(['advance', phase, input]);
      if (input.action === 'open_firefly') { assert.equal(state.submission.started, true); phase = 'firefly'; return; }
      if (phase === 'awaiting_email') { state.emailSubmitted = true; phase = 'awaiting_password'; }
      else if (phase === 'awaiting_password') { state.passwordSubmitted = true; phase = 'awaiting_profile'; }
      else if (phase === 'awaiting_profile') { state.config.profile = input.profile; state.stage = 'awaiting_approval'; }
      else assert.fail('unexpected advance');
    },
    approveCreate: async () => {
      assert.equal(state.submission.started, false);
      assert.equal(state.stage, 'awaiting_approval');
      calls.push(['create']);
      state.submission = { started: true, outcome: 'pending' };
      state.stage = 'awaiting_verification';
      phase = 'awaiting_verification';
    },
    verify: async input => {
      assert.equal(state.verificationSubmitted, undefined);
      calls.push(['verify', input]);
      state.verificationSubmitted = true;
      phase = 'firefly';
    },
    exportSession: async () => {
      calls.push(['export']);
      state.stage = 'completed';
      return { stage: 'completed', fireflySessionVerified: true };
    },
  };
  return { browser, store, state, calls, setPhase: value => { phase = value; } };
}

test('automatic signup fills normal stages and pauses before the final agreement by default', async () => {
  const f = fixture();
  const result = await runAutomaticRegistration(f.browser, f.store, {
    readInput: async () => ({ profile, action: 'challenge_ui', button: 'Submit', approved: true }),
  });
  assert.equal(result.code, 'account_agreement_required');
  assert.equal(result.automatic.steps, 3);
  assert.deepEqual(f.calls, [
    ['advance', 'awaiting_email', {}], ['advance', 'awaiting_password', {}],
    ['advance', 'awaiting_profile', { profile }],
  ]);
  assert.equal(f.state.submission.started, false);
});

test('explicit automatic approval creates once, verifies once, then uses the existing identity export', async () => {
  const f = fixture();
  const result = await runAutomaticRegistration(f.browser, f.store, {
    approveCreate: true, readInput: async () => ({ profile, code: '123456', action: 'challenge_ui' }),
  });
  assert.equal(result.stage, 'completed');
  assert.equal(result.fireflySessionVerified, true);
  assert.equal(result.automatic.completed, true);
  assert.equal(f.calls.filter(call => call[0] === 'create').length, 1);
  assert.deepEqual(f.calls.filter(call => call[0] === 'verify'), [['verify', { code: '123456' }]]);
  assert.equal(f.calls.at(-1)[0], 'export');
});

test('a previous unknown submission never advances signup or repeats creation even when authorized', async () => {
  const f = fixture({ phase: 'awaiting_profile', submitted: true });
  const result = await runAutomaticRegistration(f.browser, f.store, {
    approveCreate: true, readInput: async () => { assert.fail('must not read signup input'); },
  });
  assert.equal(result.code, 'recovery_required');
  assert.deepEqual(f.calls, []);
  assert.equal(f.state.submission.outcome, 'unknown');
});

test('creation landing on the account page proceeds once to Firefly and identity export without requesting email verification', async () => {
  const f = fixture({ phase: 'awaiting_profile', patch: { stage: 'awaiting_approval' } });
  const create = f.browser.approveCreate;
  f.browser.approveCreate = async () => {
    await create();
    f.state.stage = 'needs_manual';
    f.state.code = 'firefly_identity_required';
    f.setPhase('account');
  };
  const result = await runAutomaticRegistration(f.browser, f.store, {
    approveCreate: true, readInput: async () => assert.fail('Do not request or submit email verification without a form'),
  });
  assert.equal(result.stage, 'completed');
  assert.equal(result.fireflySessionVerified, true);
  assert.deepEqual(f.calls, [['create'], ['advance', 'account', { action: 'open_firefly' }], ['export']]);
  assert.equal(result.automatic.steps, 3);
});

test('an account page with an actual email verification form remains at verification', async () => {
  const f = fixture({ phase: 'awaiting_verification', submitted: true });
  const status = f.browser.status;
  f.browser.status = async () => {
    const result = await status();
    result.form.host = 'account.adobe.com';
    return result;
  };
  const result = await runAutomaticRegistration(f.browser, f.store);
  assert.equal(result.code, 'verification_required');
  assert.deepEqual(f.calls, []);
});

test('Firefly redirecting to email verification pauses for the code without exporting or resubmitting', async () => {
  const f = fixture({ phase: 'account', submitted: true });
  const advance = f.browser.advance;
  f.browser.advance = async input => { await advance(input); f.setPhase('awaiting_verification'); };
  const result = await runAutomaticRegistration(f.browser, f.store);
  assert.equal(result.code, 'verification_required');
  assert.equal(result.submissionStarted, true);
  assert.deepEqual(f.calls, [['advance', 'account', { action: 'open_firefly' }]]);
});

test('a previous submission can finish email verification without creating a second account', async () => {
  const f = fixture({ phase: 'awaiting_verification', submitted: true });
  const result = await runAutomaticRegistration(f.browser, f.store, { readInput: async () => ({ code: '123456' }) });
  assert.equal(result.stage, 'completed');
  assert.deepEqual(f.calls, [['verify', { code: '123456' }], ['export']]);
});

test('unknown verification results stop even if a different code is available', async () => {
  const f = fixture({ phase: 'awaiting_verification', submitted: true, patch: { verificationSubmitted: true } });
  const result = await runAutomaticRegistration(f.browser, f.store, {
    readInput: async () => { assert.fail('must not read a replacement code'); },
  });
  assert.equal(result.code, 'verification_result_unknown');
  assert.deepEqual(f.calls, []);
});

test('missing profile and email code return actionable pauses without submitting empty inputs', async () => {
  const profileFixture = fixture({ phase: 'awaiting_profile' });
  assert.equal((await runAutomaticRegistration(profileFixture.browser, profileFixture.store)).code, 'profile_required');
  assert.deepEqual(profileFixture.calls, []);
  const verificationFixture = fixture({ phase: 'awaiting_verification', submitted: true });
  assert.equal((await runAutomaticRegistration(verificationFixture.browser, verificationFixture.store)).code, 'verification_required');
  assert.deepEqual(verificationFixture.calls, []);
});

test('visible human verification, phone requirements, and explicit upstream errors stop automatic actions', async () => {
  for (const [options, code] of [
    [{ phase: 'awaiting_profile', human: true, patch: { stage: 'awaiting_approval' } }, 'human_verification_required'],
    [{ phase: 'awaiting_profile', fields: [{ type: 'tel', label: 'Phone number' }] }, 'phone_verification_required'],
    [{ phase: 'awaiting_profile', patch: { stage: 'needs_manual', code: 'form_rejected' } }, 'form_rejected'],
    [{ phase: 'awaiting_profile', patch: { stage: 'needs_manual', code: 'upstream_rate_limited' } }, 'upstream_rate_limited'],
  ]) {
    const f = fixture(options);
    assert.equal((await runAutomaticRegistration(f.browser, f.store, { approveCreate: true })).code, code);
    assert.deepEqual(f.calls, []);
  }
});

test('persisted email/password markers stop a form that has not moved to the next phase', async () => {
  for (const [phase, marker, code] of [
    ['awaiting_email', 'emailSubmitted', 'email_submission_pending'],
    ['awaiting_password', 'passwordSubmitted', 'password_submission_pending'],
  ]) {
    const f = fixture({ phase, patch: { [marker]: true } });
    assert.equal((await runAutomaticRegistration(f.browser, f.store)).code, code);
    assert.deepEqual(f.calls, []);
  }
});

test('automatic pauses retain the specific verification network diagnosis instead of a generic captcha message', async () => {
  for (const code of ['verification_dns_failed', 'verification_request_timeout', 'verification_request_aborted', 'captcha_required']) {
    const f = fixture({ phase: 'awaiting_profile', submitted: true, human: true });
    const originalStatus = f.browser.status;
    f.browser.status = async () => ({ ...await originalStatus(), code, networkDiagnosis: { code } });
    const result = await runAutomaticRegistration(f.browser, f.store, { approveCreate: true });
    assert.equal(result.code, code);
    assert.equal(result.submissionStarted, true);
    assert.deepEqual(f.calls, []);
  }
});

test('a changed page at approval and a generic welcome page cannot create or claim success', async () => {
  const changed = fixture({ phase: 'awaiting_email', patch: { stage: 'awaiting_approval' } });
  assert.equal((await runAutomaticRegistration(changed.browser, changed.store, { approveCreate: true })).code, 'approval_not_ready');
  assert.deepEqual(changed.calls, []);
  const welcome = fixture({ phase: 'unknown', submitted: true });
  assert.equal((await runAutomaticRegistration(welcome.browser, welcome.store)).code, 'recovery_required');
  assert.deepEqual(welcome.calls, []);
});

test('a Firefly page still requires the existing export method to prove identity', async () => {
  const f = fixture({ phase: 'firefly', submitted: true });
  f.browser.exportSession = async () => { f.calls.push(['export']); throw new RegistrationError('identity_not_verified'); };
  const result = await runAutomaticRegistration(f.browser, f.store);
  assert.equal(result.code, 'identity_not_verified');
  assert.equal(result.automatic.paused, true);
  assert.notEqual(result.stage, 'completed');
  assert.deepEqual(f.calls, [['export']]);
});

test('creation timeout remains unknown across subsequent automatic invocations', async () => {
  const f = fixture({ phase: 'awaiting_profile', patch: { stage: 'awaiting_approval' } });
  f.browser.approveCreate = async () => {
    f.calls.push(['create']);
    f.state.submission = { started: true, outcome: 'unknown' };
    f.state.stage = 'outcome_unknown';
    throw new RegistrationError('submission_result_unknown');
  };
  const result = await runAutomaticRegistration(f.browser, f.store, { approveCreate: true });
  assert.equal(result.code, 'submission_result_unknown');
  assert.equal(result.stage, 'outcome_unknown');
  assert.equal(result.submissionStarted, true);
  assert.deepEqual(result.automatic, { paused: true, steps: 1 });
  assert.equal((await runAutomaticRegistration(f.browser, f.store, { approveCreate: true })).code, 'recovery_required');
  assert.deepEqual(f.calls, [['create']]);
});

test('a creation exception falls back to persisted state when the browser status cannot refresh', async t => {
  for (const failure of [new RegistrationError('submission_result_unknown'), new Error('page closed with private detail')]) {
    await t.test(failure.constructor.name, async () => {
      const f = fixture({ phase: 'awaiting_profile', patch: {
        stage: 'awaiting_approval', config: { password: 'must-never-appear-in-output' },
      } });
      const originalStatus = f.browser.status;
      f.browser.status = async () => {
        if (f.state.submission.started) throw new Error('page closed');
        return originalStatus();
      };
      f.browser.approveCreate = async () => {
        f.calls.push(['create']);
        f.state.submission = { started: true, outcome: 'unknown' };
        f.state.stage = 'outcome_unknown';
        throw failure;
      };
      const result = await runAutomaticRegistration(f.browser, f.store, { approveCreate: true });
      assert.deepEqual(result, {
        stage: 'outcome_unknown', recordedStage: 'outcome_unknown', submissionStarted: true,
        code: failure instanceof RegistrationError ? 'submission_result_unknown' : 'automatic_step_failed',
        automatic: { paused: true, steps: 1 },
      });
      assert.deepEqual(f.calls, [['create']]);
      assert.doesNotMatch(JSON.stringify(result), /must-never-appear|private detail|awaiting_approval/);
    });
  }
});

test('unreadable browser and store after a creation error never return stale non-submission status', async () => {
  const f = fixture({ phase: 'awaiting_profile', patch: { stage: 'awaiting_approval' } });
  f.browser.approveCreate = async () => {
    f.calls.push(['create']);
    f.state.submission = { started: true, outcome: 'unknown' };
    f.state.stage = 'outcome_unknown';
    f.browser.status = async () => { throw new Error('page closed'); };
    f.store.load = async () => { throw new Error('state inaccessible'); };
    throw new RegistrationError('submission_result_unknown');
  };
  const result = await runAutomaticRegistration(f.browser, f.store, { approveCreate: true });
  assert.deepEqual(result, {
    stage: 'needs_manual', code: 'submission_result_unknown', automatic: { paused: true, steps: 1 },
  });
  assert.equal(Object.hasOwn(result, 'submissionStarted'), false);
  assert.deepEqual(f.calls, [['create']]);
});

test('bounded attempts stop stalled stages and cancellation prevents subsequent actions', async () => {
  const stalled = fixture();
  stalled.browser.advance = async () => { stalled.calls.push(['advance']); };
  assert.equal((await runAutomaticRegistration(stalled.browser, stalled.store)).code, 'automatic_progress_stalled');
  assert.equal(stalled.calls.length, 1);
  const bounded = fixture();
  assert.equal((await runAutomaticRegistration(bounded.browser, bounded.store, { maxSteps: 1 })).code, 'automatic_step_limit_reached');
  assert.equal(bounded.calls.length, 1);
  const interrupted = fixture();
  let stop = false;
  const advance = interrupted.browser.advance;
  interrupted.browser.advance = async value => { await advance(value); stop = true; };
  const result = await runAutomaticRegistration(interrupted.browser, interrupted.store, {
    shouldStop: () => stop,
  });
  assert.equal(result.code, 'automatic_interrupted');
  assert.equal(interrupted.calls.length, 1);
});
