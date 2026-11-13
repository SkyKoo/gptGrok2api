import { RegistrationError, isTrustedAdobeURL } from './core.mjs';
import { sanitizeMediaDiagnostics } from './media-diagnostics.mjs';
import { formBlockingCode, formFailureDiagnostics } from './browser.mjs';
import { runAutomaticRegistration, observeAutomaticVerification } from './auto-runner.mjs';
import { creationOutcome } from './creation-outcome.mjs';

const LOGIN = /^(Continue|Next|Sign in|Log in|继续|下一步|登录)$/i;
const SIGN_IN = /^(Sign in|Log in|登录)$/i;
const RESEND = /^(Resend code|Resend verification code|Send a new code|Resend|重新发送验证码|重新发送|重发验证码)$/i;
const LOGIN_STAGES = new Set(['awaiting_email', 'awaiting_password']);
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));

function failureDetails(error, action, phase, form) {
  const message = typeof error?.message === 'string' ? error.message : '';
  const type = error instanceof RegistrationError ? 'registration'
    : error?.name === 'TimeoutError' || /Timeout \d+ms exceeded/i.test(message) ? 'timeout'
    : /Target page, context or browser has been closed|page has been closed|Browser closed/i.test(message) ? 'page_closed'
    : /Execution context was destroyed|Frame was detached|Cannot find context with specified id|navigation.*(?:aborted|interrupted)/i.test(message) ? 'navigation'
    : 'unknown';
  return { failure_action: action, failure_phase: phase, exception_type: type,
    ...(error?.code === 'form_rejected' ? formFailureDiagnostics(form) : {}) };
}

function actionFailure(error, action, phase, form, code) {
  if (error instanceof RegistrationError && error.failureDiagnostics) return error;
  const result = new RegistrationError(code || (error instanceof RegistrationError ? error.code : 'recovery_step_failed'));
  result.failureDiagnostics = failureDetails(error, action, phase, form);
  return result;
}

function blocked(status) {
  if (status.form?.humanVerification?.required) return 'human_verification_required';
  return status.networkDiagnosis?.code || formBlockingCode(status.form) || null;
}

async function patchAttempt(store, id, patch, { allowUnsubmitted = false } = {}) {
  const state = await store.load();
  if (!state.submission.started && !allowUnsubmitted) throw new RegistrationError('registration_not_submitted');
  return store.save({ recoveryAttempts: { ...state.recoveryAttempts,
    [id]: { ...state.recoveryAttempts?.[id], ...patch } } });
}

/** 正常登录入口只接受 Sign in；恢复不会进入注册入口或清除原提交标记。 */
export async function openRecoveryLogin(browser, { timeoutMs = 20000, now = Date.now, wait = pause, observe = async status => status, shouldStop = () => false } = {}) {
  let action = 'open_login', current;
  try {
    if (shouldStop()) throw new RegistrationError('automatic_interrupted');
    await browser.page.goto('https://account.adobe.com/', { waitUntil: 'domcontentloaded', timeout: 45000 });
    const deadline = now() + timeoutMs;
    let clicked = false;
    while (now() < deadline) {
      action = 'inspect_form';
      const status = current = await observe(await browser.status());
      if (shouldStop()) throw new RegistrationError('automatic_interrupted');
      const failure = blocked(status);
      if (failure) throw new RegistrationError(failure);
      if (LOGIN_STAGES.has(status.detected) || status.detected === 'awaiting_verification' || status.form.host === 'firefly.adobe.com') return status;
      const frame = await browser.authFrame();
      const buttons = frame.getByRole('button', { name: SIGN_IN });
      const links = frame.getByRole('link', { name: SIGN_IN });
      const count = await buttons.count() + await links.count();
      if (count > 1) throw new RegistrationError('recovery_login_entry_unrecognized');
      if (count === 1 && !clicked) {
        if (shouldStop()) throw new RegistrationError('automatic_interrupted');
        clicked = true;
        action = 'click_login_entry';
        await (await buttons.count() ? buttons : links).click({ timeout: 15000 });
      }
      await wait(Math.min(250, Math.max(1, deadline - now())));
    }
    throw new RegistrationError('recovery_login_form_unavailable');
  } catch (error) { throw actionFailure(error, action, 'before_submission', current?.form); }
}

/** 每个恢复任务的邮箱/密码阶段最多提交一次；异常后的再次调用仅观察后续阶段。 */
export async function submitRecoveryLogin(browser, store, recoveryId, status) {
  let action = 'inspect_form', phase = 'before_submission', currentForm = status.form;
  try {
    const state = await store.load();
    const form = status.form;
    const failure = blocked(status);
    if (failure) throw new RegistrationError(failure);
    if (!isTrustedAdobeURL(browser.page.url()) || !LOGIN_STAGES.has(status.detected) ||
        form.fields.some(field => field.autocomplete === 'new-password') ||
        /create.{0,20}account|sign up|创建.{0,3}[帐账]户|注册/i.test(form.headings.join(' '))) {
      throw new RegistrationError('recovery_login_form_unrecognized');
    }
    const password = status.detected === 'awaiting_password';
    const step = password ? 'passwordSubmittedAt' : 'emailSubmittedAt';
    if (state.recoveryAttempts?.[recoveryId]?.[step]) {
      phase = 'submission_started';
      throw new RegistrationError('recovery_login_result_unknown');
    }
    const value = password ? state.config.password : state.email;
    if (typeof value !== 'string' || !value) throw new RegistrationError(password ? 'recovery_password_missing' : 'email_required');
    const descriptor = form.fields.find(field => password ? field.type === 'password' :
      field.type === 'email' || /email|username|邮箱|电子邮件/i.test(`${field.name} ${field.label}`));
    if (!descriptor) throw new RegistrationError('recovery_login_form_unrecognized');
    const frame = await browser.authFrame();
    const button = frame.getByRole('button', { name: LOGIN });
    if (await button.count() !== 1) throw new RegistrationError('recovery_login_form_unrecognized');
    // Continue may remain disabled until a valid value is filled. Trial checks
    // actionability only after filling; the durable fence still precedes click.
    action = password ? 'fill_password' : 'fill_email';
    await (await browser.field(frame, descriptor)).fill(value);
    action = password ? 'submit_password' : 'submit_email';
    await button.click({ trial: true, timeout: 10000 });
    await patchAttempt(store, recoveryId, { [step]: new Date().toISOString() });
    phase = 'submission_started';
    // A successful form navigation may itself abort Playwright's click wait.
    // Observe the resulting form once; never click a second time.
    try { await button.click({ timeout: 15000 }); } catch { /* The durable marker fences this action. */ }
    let outcome;
    action = 'wait_login_result';
    try {
      outcome = await browser.waitForStage({ fromStage: status.detected,
        accept: (stage, current) => stage !== status.detected && stage !== 'needs_manual' ||
          ['account.adobe.com', 'firefly.adobe.com'].includes(current.host) });
    } catch (error) {
      const failure = new RegistrationError('recovery_login_result_unknown');
      failure.failureDiagnostics = failureDetails(error, action, phase, currentForm);
      throw failure;
    }
    if (outcome.code) {
      currentForm = outcome.form || currentForm;
      if (outcome.code === 'human_verification_required') {
        const attempt = (await store.load()).recoveryAttempts?.[recoveryId];
        if (!attempt?.verificationObservationStartedAt) await patchAttempt(store, recoveryId, { verificationObservationStartedAt: new Date().toISOString() });
      }
      throw new RegistrationError(outcome.code);
    }
  } catch (error) { throw actionFailure(error, action, phase, currentForm); }
}

/** 旧 OTP 页面必须正常重发；同一补偿任务最多一次，新的任务独立记账。 */
export async function requestRecoveryCode(browser, store, recoveryId) {
  let phase = 'before_submission', current;
  try {
    const state = await store.load();
    const attempt = state.recoveryAttempts?.[recoveryId] || {};
    if (attempt.verificationRequestedAt) return attempt.verificationRequestedAt;
    if (attempt.resendStartedAt) {
      phase = 'submission_started';
      throw new RegistrationError('recovery_verification_result_unknown');
    }
    const status = current = await browser.status();
    const failure = blocked(status);
    if (failure) throw new RegistrationError(failure);
    if (status.detected !== 'awaiting_verification') throw new RegistrationError('verification_not_ready');
    // A normal login just produced this OTP form; its form submission is the
    // authoritative start of the code-delivery window and does not need Resend.
    const loginTime = attempt.passwordSubmittedAt || attempt.emailSubmittedAt;
    if (loginTime) {
      await patchAttempt(store, recoveryId, { verificationRequestedAt: loginTime });
      return loginTime;
    }
    const frame = await browser.authFrame();
    const buttons = frame.getByRole('button', { name: RESEND });
    const links = frame.getByRole('link', { name: RESEND });
    const buttonCount = await buttons.count(), linkCount = await links.count();
    if (buttonCount + linkCount !== 1) throw new RegistrationError('recovery_verification_unavailable');
    const control = buttonCount ? buttons : links;
    try { await control.click({ trial: true, timeout: 10000 }); }
    catch (error) { throw actionFailure(error, 'request_code', phase, current.form, 'recovery_verification_unavailable'); }
    const requestedAt = new Date().toISOString();
    await patchAttempt(store, recoveryId, { resendStartedAt: requestedAt });
    phase = 'submission_started';
    try { await control.click({ timeout: 15000 }); }
    catch (error) { throw actionFailure(error, 'request_code', phase, current.form, 'recovery_verification_result_unknown'); }
    await patchAttempt(store, recoveryId, { verificationRequestedAt: requestedAt });
    return requestedAt;
  } catch (error) { throw actionFailure(error, 'request_code', phase, current?.form); }
}

/** 原 journal 证明尚未提交时继续原流程；已提交时仅登录、验证与导出。 */
export async function resumeRegistration(browser, store, { recoveryId, code = '', loginOptions, verificationWait = {}, shouldStop = () => false } = {}) {
  let status = {};
  let action = 'inspect_form', phase = 'before_submission';
  const stopped = (failure, diagnostics) => ({ ...status, stage: 'needs_manual', code: failure,
    failureDiagnostics: diagnostics || failureDetails(new RegistrationError(failure), action, phase, status.form),
    recovery_mode: 'resume_session' });
  const observe = async current => {
    const human = current.form?.humanVerification;
    if (human?.required !== true || !['visible_challenge_frame', 'upstream_captcha_response', 'visible_instruction'].includes(human.source)) return current;
    const attempt = (await store.load()).recoveryAttempts?.[recoveryId];
    if (attempt?.verificationObservationStartedAt || shouldStop()) return current;
    // This fence belongs to this compensation attempt, not the old signup's
    // terminal stage. Persist before waiting so reconnects cannot extend it.
    await patchAttempt(store, recoveryId, { verificationObservationStartedAt: new Date().toISOString() });
    return observeAutomaticVerification(browser, current, shouldStop, verificationWait);
  };
  const openLogin = () => {
    action = 'open_login';
    return openRecoveryLogin(browser, { ...loginOptions, observe, shouldStop });
  };
  try {
    if (!/^[a-zA-Z0-9_-]{1,128}$/.test(recoveryId || '')) throw new RegistrationError('recovery_id_required');
    let state = await store.load();
    if (state.result?.account || state.stage === 'completed' || state.stage === 'credentials_saved' ||
        browser.deferMedia&&(state.stage==='media_pending'||Object.hasOwn(state,'mediaCheckpoint'))) {
      action = 'export_session';
      return await browser.exportSession();
    }
    if (state.submission.started && creationOutcome(state).creation_outcome !== 'accepted') {
      return stopped('creation_result_unknown');
    }
    if (!state.recoveryAttempts?.[recoveryId]) {
      if (Object.keys(state.recoveryAttempts || {}).length >= 32) throw new RegistrationError('recovery_attempt_limit');
      await patchAttempt(store, recoveryId, { startedAt: new Date().toISOString(),
        ...(!state.submission.started ? { registrationContinuation: true } : {}) }, { allowUnsubmitted: !state.submission.started });
      state = await store.load();
    }
    if (!state.submission.started || state.recoveryAttempts[recoveryId].registrationContinuation === true) {
      // The original runner persists the Create marker before clicking. Even
      // a later invocation of this branch cannot submit an already started run.
      const result = await runAutomaticRegistration(browser, store, { approveCreate: true, resumeBeforeSubmission: true, verificationWait, shouldStop,
        readInput: async () => ({ profile: state.config.profile, ...(code ? { code } : {}) }) });
      return { ...result, recovery_mode: 'continue_registration' };
    }
    let loginOpened = false, fireflyOpened = false;
    for (let step = 0; step < 8; step++) {
      if (shouldStop()) return stopped('automatic_interrupted');
      action = 'inspect_form';
      state = await store.load();
      const attempt = state.recoveryAttempts?.[recoveryId] || {};
      phase = attempt.emailSubmittedAt || attempt.passwordSubmittedAt || attempt.resendStartedAt || attempt.verificationSubmitted
        ? 'submission_started' : 'before_submission';
      status = await observe(await browser.status());
      if (shouldStop()) return stopped('automatic_interrupted');
      const failure = blocked(status);
      if (failure) {
        if (failure === 'human_verification_required') await store.setStage('needs_manual', failure);
        return stopped(failure);
      }
      state = await store.load();
      if (status.detected === 'awaiting_verification') {
        if (state.recoveryAttempts?.[recoveryId]?.verificationSubmitted) return stopped('verification_result_unknown');
        if (!code) {
          action = 'request_code';
          const requestedAt = await requestRecoveryCode(browser, store, recoveryId);
          phase = 'submission_started';
          action = 'inspect_form';
          status = await observe(await browser.status());
          if (shouldStop()) return stopped('automatic_interrupted');
          const currentFailure = blocked(status);
          if (currentFailure) {
            if (currentFailure === 'human_verification_required') await store.setStage('needs_manual', currentFailure);
            return stopped(currentFailure);
          }
          if (status.detected !== 'awaiting_verification') continue;
          await store.setStage('awaiting_verification', 'verification_required');
          return { ...status, stage: 'awaiting_verification', code: 'verification_required', verification_requested_at: requestedAt,
            recovery_mode: 'resume_session' };
        }
        if (!state.recoveryAttempts?.[recoveryId]?.verificationRequestedAt) throw new RegistrationError('verification_not_ready');
        action = 'verify_code';
        await browser.verify({ code }, { recoveryId });
        code = '';
        continue;
      }
      if (status.form.host === 'firefly.adobe.com') {
        action = 'export_session';
        try { return await browser.exportSession(); }
        catch (error) {
          if (!(error instanceof RegistrationError) || error.code !== 'identity_not_verified') throw error;
        }
      }
      if (LOGIN_STAGES.has(status.detected)) {
        const signup = status.form.fields.some(field => field.autocomplete === 'new-password') ||
          /create.{0,20}account|sign up|创建.{0,3}[帐账]户|注册/i.test(status.form.headings.join(' '));
        if (signup) {
          if (loginOpened) return stopped('recovery_login_form_unrecognized');
          loginOpened = true;
          await openLogin();
          continue;
        }
        await submitRecoveryLogin(browser, store, recoveryId, status);
        continue;
      }
      if (status.form.host === 'account.adobe.com' && !fireflyOpened) {
        fireflyOpened = true;
        action = 'open_firefly';
        await browser.page.goto('https://firefly.adobe.com/', { waitUntil: 'domcontentloaded', timeout: 45000 });
        continue;
      }
      if (loginOpened) return stopped('recovery_login_result_unknown');
      loginOpened = true;
      await openLogin();
    }
    return stopped('recovery_progress_stalled');
  } catch (error) {
    // Keep the failing action and its original rejection evidence. A final
    // read can show newer UI, but must not replace the cause with that page.
    const diagnostics = error instanceof RegistrationError && error.failureDiagnostics
      ? error.failureDiagnostics : failureDetails(error, action, phase, status.form);
    const mediaDiagnostics = error instanceof RegistrationError && error.code === 'media_session_incomplete'
      ? sanitizeMediaDiagnostics(error.diagnostics) : undefined;
    try { status = await browser.status(); } catch { /* Original fixed error and durable state remain authoritative. */ }
    if (mediaDiagnostics) status = { ...status, diagnostics: mediaDiagnostics };
    return stopped(shouldStop() ? 'automatic_interrupted' : error instanceof RegistrationError ? error.code : 'recovery_step_failed', diagnostics);
  }
}
