const SOURCES = new Set(['visible_challenge_frame', 'visible_instruction', 'upstream_captcha_response']);
const NETWORK_CODES = new Set(['verification_dns_failed', 'verification_request_timeout',
  'verification_request_aborted', 'captcha_required']);
const FORM_STAGES = new Set(['awaiting_email', 'awaiting_password', 'awaiting_profile',
  'awaiting_approval', 'awaiting_verification', 'needs_manual']);

/** Worker 仅保留判断依据，不把表单内容、挑战题目、网络原文或凭据交给注册日志。 */
export function registrationDiagnostics(result, state) {
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
  };
}
