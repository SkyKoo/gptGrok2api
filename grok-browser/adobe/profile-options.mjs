import { RegistrationError } from './core.mjs';

const PROFILE_FIELDS = Object.freeze({
  country: 'country', birthMonth: 'birth_month', birthYear: 'birth_year', birthDay: 'birth_day',
});
const MAX_OPTIONS = 400;
const MAX_WAIT_MS = 5000;
const pauseFor = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));

function optionFailure(key, reason, options) {
  const error = new RegistrationError('profile_option_invalid');
  error.failureDiagnostics = {
    ...(Object.hasOwn(PROFILE_FIELDS, key) ? { profile_field: PROFILE_FIELDS[key] } : {}),
    profile_option_reason: reason,
    option_count: Array.isArray(options) ? Math.min(options.length, MAX_OPTIONS) : 0,
  };
  return error;
}

function requestedValue(key, value, options) {
  if (!Object.hasOwn(PROFILE_FIELDS, key) || !['string', 'number'].includes(typeof value)) {
    throw optionFailure(key, 'invalid', options);
  }
  const text = String(value);
  if (key === 'country') {
    if (typeof value !== 'string' || !text.trim() || text.length > 120) throw optionFailure(key, 'invalid', options);
  } else if (key === 'birthYear') {
    if (!/^\d{4}$/.test(text)) throw optionFailure(key, 'invalid', options);
  } else if (!/^\d{1,2}$/.test(text) || Number(text) < 1 || Number(text) > (key === 'birthMonth' ? 12 : 31)) {
    throw optionFailure(key, 'invalid', options);
  }
  return text;
}

/** 只选择当前列表明确提供的值；不修改日期、地区或通过默认选项兜底。 */
export function resolveProfileOption(key, value, options) {
  const requested = requestedValue(key, value, options);
  if (!Array.isArray(options) || options.length > MAX_OPTIONS || options.some(option =>
    !option || typeof option !== 'object' || typeof option.value !== 'string' || typeof option.label !== 'string' ||
    (option.disabled !== undefined && typeof option.disabled !== 'boolean'))) {
    throw optionFailure(key, 'invalid', options);
  }
  // 原生 select 的空值占位项不代表可选择的地区或日期。
  const choices = options.filter(option => option.value !== '');
  if (!choices.length) throw optionFailure(key, 'missing', options);

  let matches;
  if (key === 'birthMonth') {
    const values = new Set(choices.map(option => option.value));
    if (values.size !== choices.length) throw optionFailure(key, 'ambiguous', options);
    const zeroBased = [...values].every(item => /^(?:[0-9]|1[01])$/.test(item));
    const oneBased = [...values].every(item => /^(?:[1-9]|1[0-2])$/.test(item));
    if (!zeroBased && !oneBased || choices.length > 12) throw optionFailure(key, 'invalid', options);
    if (choices.length < 12) throw optionFailure(key, 'incomplete', options);
    // 只有完整的 12 个月证明基数；局部列表中的 "3" 不能决定它代表三月还是四月。
    const target = String(Number(requested) - (zeroBased ? 1 : 0));
    matches = choices.filter(option => option.value === target);
  } else {
    matches = choices.filter(option => option.value === requested);
    if (!matches.length) matches = choices.filter(option => option.label === requested);
  }
  if (!matches.length) throw optionFailure(key, 'missing', options);
  if (matches.length !== 1) throw optionFailure(key, 'ambiguous', options);
  if (matches[0].disabled === true) throw optionFailure(key, 'disabled', options);
  return matches[0];
}

/** 最多观察同一选项列表 5 秒，只重读匹配失败，不点击或更改目标值。 */
export async function waitForProfileOption(readOptions, key, value, {
  timeoutMs = MAX_WAIT_MS, pollIntervalMs = 100, now = Date.now, pause = pauseFor,
} = {}) {
  requestedValue(key, value, []);
  if (typeof readOptions !== 'function' || typeof now !== 'function' || typeof pause !== 'function' ||
    !Number.isFinite(timeoutMs) || timeoutMs < 0 || timeoutMs > MAX_WAIT_MS ||
    !Number.isFinite(pollIntervalMs) || pollIntervalMs <= 0) {
    throw optionFailure(key, 'invalid', []);
  }
  const deadline = now() + timeoutMs;
  let failure = optionFailure(key, 'missing', []);
  while (true) {
    const remaining = Math.max(0, deadline - now());
    let timer;
    let options;
    try {
      // 页面 evaluate 也可能迟迟不返回；读操作同样受剩余观察预算约束。
      options = await Promise.race([
        Promise.resolve().then(readOptions),
        new Promise((_, reject) => { timer = setTimeout(() => reject(failure), remaining); }),
      ]);
    } finally { clearTimeout(timer); }
    try {
      const option = resolveProfileOption(key, value, options);
      if (now() > deadline) throw failure;
      return option;
    } catch (error) {
      if (!(error instanceof RegistrationError) || error.code !== 'profile_option_invalid' || !error.failureDiagnostics) throw error;
      failure = error;
      if (error.failureDiagnostics.profile_option_reason === 'invalid') throw error;
    }
    const delay = Math.min(pollIntervalMs, deadline - now());
    if (delay <= 0) throw failure;
    await pause(delay);
  }
}
