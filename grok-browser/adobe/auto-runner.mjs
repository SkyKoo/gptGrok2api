import { RegistrationError } from './core.mjs';

const ADVANCE_STAGES = new Set(['awaiting_email', 'awaiting_password', 'awaiting_profile']);
const APPROVAL_STAGES = new Set(['awaiting_password', 'awaiting_profile', 'awaiting_approval']);
const BLOCKING_CODES = new Set([
  'region_unavailable', 'upstream_rate_limited', 'account_already_exists', 'form_rejected',
  'continuation_result_unknown', 'early_agreement_requires_manual',
  'existing_account_or_password_required', 'login_ui_not_ready',
]);

function errorCode(error) {
  return error instanceof RegistrationError && /^[a-z][a-z0-9_]{0,79}$/.test(error.code)
    ? error.code : 'automatic_step_failed';
}

function needsPhone(form) {
  return (form?.fields ?? []).some(field => field.type === 'tel' ||
    /(?:^|\s)(?:tel|phone|mobile)(?:\s|$)|phone.?number|手机|电话号码/i.test(
      `${field.name ?? ''} ${field.id ?? ''} ${field.label ?? ''} ${field.autocomplete ?? ''}`));
}

/** 有界推进正常注册阶段；动作由浏览器驱动落盘，未知提交不能在循环中重放。 */
export async function runAutomaticRegistration(browser, store, {
  readInput = async () => ({}), approveCreate = false, onEvent = () => {},
  shouldStop = () => false, maxSteps = 8,
} = {}) {
  if (!Number.isInteger(maxSteps) || maxSteps < 1 || maxSteps > 12) {
    throw new RegistrationError('automatic_step_limit_invalid');
  }
  const attempted = new Set();
  let steps = 0;
  let status = {};
  const paused = code => ({ ...status, code, automatic: { paused: true, steps } });
  const complete = result => ({ ...result, automatic: { paused: false, completed: true, steps } });
  const perform = async (key, action) => {
    if (steps >= maxSteps) throw new RegistrationError('automatic_step_limit_reached');
    if (attempted.has(key)) throw new RegistrationError('automatic_progress_stalled');
    attempted.add(key);
    steps++;
    const result = await action();
    if (!shouldStop() && result !== undefined) onEvent(result);
    return result;
  };

  try {
    for (let observation = 0; observation <= maxSteps; observation++) {
      if (shouldStop()) return paused('automatic_interrupted');
      const state = await store.load();
      status = await browser.status();
      if (shouldStop()) return paused('automatic_interrupted');

      // 已发布的身份快照由 exportSession 离线复用，不能仅凭页面文字宣称成功。
      if (state.stage === 'completed' || state.stage === 'credentials_saved') {
        return complete(await perform('export', () => browser.exportSession()));
      }
      if (status.networkDiagnosis?.code) return paused(status.networkDiagnosis.code);
      if (status.form?.humanVerification?.required === true) return paused('human_verification_required');
      if (needsPhone(status.form)) return paused('phone_verification_required');
      if (BLOCKING_CODES.has(state.code) || BLOCKING_CODES.has(status.code)) {
        return paused(BLOCKING_CODES.has(status.code) ? status.code : state.code);
      }

      const detected = status.detected;
      const onFirefly = status.form?.host === 'firefly.adobe.com';
      if (state.submission.started) {
        if (onFirefly) return complete(await perform('export', () => browser.exportSession()));
        if (status.form?.host === 'account.adobe.com' &&
            !['awaiting_email', 'awaiting_password', 'awaiting_verification'].includes(detected)) {
          await perform('open_firefly', () => browser.advance({ action: 'open_firefly' }));
          continue;
        }
        if (detected !== 'awaiting_verification') return paused('recovery_required');
        if (state.verificationSubmitted) return paused('verification_result_unknown');
        if (steps >= maxSteps) return paused('automatic_step_limit_reached');
        const input = await readInput();
        // 只读取验证码/验证链接；不执行 input.json 中遗留的 challenge_ui 等动作。
        const verification = input.verificationURL
          ? { verificationURL: input.verificationURL }
          : input.code !== undefined ? { code: input.code } : null;
        if (!verification) return paused('verification_required');
        if (shouldStop()) return paused('automatic_interrupted');
        await perform('verify', () => browser.verify(verification));
        continue;
      }
      if (state.stage === 'outcome_unknown') return paused('recovery_required');
      if (state.stage === 'needs_manual') return paused(state.code || 'unrecognized_page');
      if (steps >= maxSteps) return paused('automatic_step_limit_reached');

      if (state.stage === 'awaiting_approval') {
        if (!APPROVAL_STAGES.has(detected)) return paused('approval_not_ready');
        if (approveCreate !== true) return paused('account_agreement_required');
        await perform('create', () => browser.approveCreate());
        continue;
      }
      if (!ADVANCE_STAGES.has(detected)) return paused('unrecognized_page');
      if (detected === 'awaiting_email' && state.emailSubmitted) return paused('email_submission_pending');
      if (detected === 'awaiting_password' && state.passwordSubmitted) return paused('password_submission_pending');

      let input = {};
      if (detected === 'awaiting_profile') {
        const supplied = await readInput();
        const profile = supplied.profile || state.config?.profile;
        if (!profile) return paused('profile_required');
        input = { profile };
      }
      if (shouldStop()) return paused('automatic_interrupted');
      await perform(detected, () => browser.advance(input));
    }
    return paused('automatic_step_limit_reached');
  } catch (error) {
    // 动作可能已落盘后才超时，不能把动作前的状态作为失败结果返回。
    try { status = await browser.status(); }
    catch {
      try {
        const state = await store.load();
        status = { stage: state.stage, recordedStage: state.stage,
          submissionStarted: state.submission?.started };
      } catch {
        // 页面和状态文件都不可读时，丢弃旧快照，尤其不能声称尚未提交。
        status = { stage: 'needs_manual' };
      }
    }
    return paused(shouldStop() ? 'automatic_interrupted' : errorCode(error));
  }
}
