import { isTrustedAdobeURL, RegistrationError } from './core.mjs';

// Return only fixed classifications and control descriptors. Email text and
// entered values stay inside the page; an arbitrary OTP field is not proof of
// an email challenge (SMS, TOTP and CAPTCHA must never use the mailbox code).
export function emailOTPInPage({ email }) {
  const visible = element => {
    const style = getComputedStyle(element), rect = element.getBoundingClientRect();
    return style.display !== 'none' && style.visibility !== 'hidden' && rect.width > 0 && rect.height > 0;
  };
  const inputs = [...document.querySelectorAll('input')].filter(visible);
  const controls = inputs.map((input, index) => ({
    index, type: input.type, id: input.id || '', name: input.name || '',
    autocomplete: input.autocomplete || '', maxlength: input.maxLength,
    inputmode: input.getAttribute('inputmode') || '', disabled: input.disabled || input.readOnly,
    invalid: input.getAttribute('aria-invalid') === 'true',
  }));
  const candidates = controls.filter(input => !input.disabled && ['text', 'tel', 'number', 'password'].includes(input.type) &&
    (/one-time-code/i.test(input.autocomplete) || /verification.?code|otp|^code$/i.test(`${input.name} ${input.id}`) ||
      input.maxlength === 1 && ['numeric', 'decimal'].includes(input.inputmode)));
  const container = candidates.length ? inputs[candidates[0].index].closest('form') || document.body : document.body;
  const words = [...container.querySelectorAll('h1,h2,h3,p,label,[role="alert"],[aria-live]')]
    .filter(visible).slice(0, 100).map(element => (element.innerText || element.textContent || '').slice(0, 1000)).join(' ');
  const errors = [...container.querySelectorAll('[role="alert"],[aria-live],[class*="error" i]')]
    .filter(visible).slice(0, 30).map(element => (element.innerText || element.textContent || '').slice(0, 500)).join(' ');
  const challenge = /captcha|verify you are human|prove you(?:'|’)re human|not a robot|start puzzle|人机验证|开始拼图|完成拼图/i.test(words) ||
    [...document.querySelectorAll('iframe')].some(frame => visible(frame) &&
      /captcha|challenge|arkoselabs|funcaptcha|arks-client\.adobe\.com/i.test(`${frame.title} ${frame.getAttribute('src') || ''}`));
  if (challenge) return { kind: 'challenge' };
  if (/\b(?:code|verification).{0,45}(?:expired|timed out)|\bexpired.{0,30}code|验证码.{0,15}(?:过期|失效)/i.test(errors)) return { kind: 'expired' };
  if (/\b(?:invalid|incorrect|wrong).{0,30}(?:code|verification)|\bcode.{0,30}(?:invalid|incorrect|wrong)|验证码.{0,15}(?:无效|错误|不正确)/i.test(errors) || candidates.some(input => input.invalid)) return { kind: 'invalid' };
  const emailHint = /verify your e-?mail|check your (?:e-?mail|inbox)|(?:code|sent).{0,100}e-?mail|e-?mail.{0,100}(?:code|sent)|(?:验证|查看).{0,15}(?:电子邮件|邮箱)|(?:验证码|发送).{0,50}(?:电子邮件|邮箱)/i.test(words) ||
    typeof email === 'string' && email.length > 3 && words.toLowerCase().includes(email.toLowerCase());
  const otherFactor = /\bsms\b|text message|authenticator|authentication app|backup code|recovery code|security key|(?:sent|code).{0,50}(?:phone|mobile)|短信|验证器|身份验证应用|恢复代码|备用码|安全密钥/i.test(words);
  if (otherFactor) return { kind: candidates.length ? 'unknown' : 'none' };
  const single = candidates.length === 1 && (candidates[0].maxlength === 6 ||
    candidates[0].maxlength <= 0 && /one-time-code/i.test(candidates[0].autocomplete));
  const split = candidates.length === 6 && candidates.every(input => input.maxlength === 1 && input.type !== 'password');
  if (!emailHint || !(single || split)) return { kind: candidates.length ? 'unknown' : 'none' };
  return { kind: 'email_otp', fields: candidates.map(({ index, type, id, name, autocomplete, maxlength }) =>
    ({ index, type, id, name, autocomplete, maxlength })) };
}

export async function inspectEmailOTP(page, expected) {
  try {
    if (!isTrustedAdobeURL(page.url())) return { kind: 'unknown' };
    const result = await page.evaluate(emailOTPInPage, { email: expected.email });
    if (!result || typeof result !== 'object' || Array.isArray(result)) return { kind: 'unknown' };
    if (result.kind === 'email_otp') {
      const fields = result.fields;
      if (!Array.isArray(fields) || ![1, 6].includes(fields.length) || fields.some((field, position) =>
        !field || typeof field !== 'object' || Array.isArray(field) ||
        !Number.isInteger(field.index) || field.index < 0 || position > 0 && field.index <= fields[position - 1].index ||
        !['text', 'tel', 'number', 'password'].includes(field.type) ||
        !['id', 'name', 'autocomplete'].every(key => typeof field[key] === 'string') || !Number.isInteger(field.maxlength))) return { kind: 'unknown' };
      const single = fields.length === 1 && (fields[0].maxlength === 6 || fields[0].maxlength <= 0 && /one-time-code/i.test(fields[0].autocomplete));
      const split = fields.length === 6 && fields.every(field => field.maxlength === 1 && field.type !== 'password');
      if (!single && !split) return { kind: 'unknown' };
      return { kind: 'email_otp', fields: fields.map(({ index, type, id, name, autocomplete, maxlength }) =>
        ({ index, type, id, name, autocomplete, maxlength })) };
    }
    if (['challenge', 'expired', 'invalid', 'unknown', 'none'].includes(result.kind)) return { kind: result.kind };
  } catch { /* A navigating/unknown page is not an email OTP form. */ }
  // Inspection failure is not evidence that verification is absent. In
  // particular, a masked OTP must never receive the saved account password.
  return { kind: 'unknown' };
}

export function verificationFailure(kind, fallback = 'refresh_verification_required') {
  return new RegistrationError({challenge:'refresh_challenge_required', expired:'refresh_otp_expired', invalid:'refresh_otp_invalid'}[kind] || fallback);
}

export async function fillEmailOTP(page, code, snapshot) {
  // Input may auto-submit on the final digit. The caller fences the whole
  // action first and never adds a second Verify click or retries a failed fill.
  const inputs = page.locator('input:visible');
  for (const [position, descriptor] of snapshot.fields.entries()) {
    const input = inputs.nth(descriptor.index);
    const same = await input.evaluate((element, wanted) =>
      element.type === wanted.type && (element.id || '') === wanted.id && (element.name || '') === wanted.name &&
      (element.autocomplete || '') === wanted.autocomplete && element.maxLength === wanted.maxlength && !element.disabled && !element.readOnly, descriptor);
    if (!same) throw new RegistrationError('refresh_otp_form_changed');
    await input.fill(snapshot.fields.length === 1 ? code : code[position]);
  }
}
