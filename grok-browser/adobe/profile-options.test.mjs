import test from 'node:test';
import assert from 'node:assert/strict';
import { RegistrationError } from './core.mjs';
import { resolveProfileOption, waitForProfileOption } from './profile-options.mjs';

const option = (value, label = value, patch = {}) => ({ value: String(value), label: String(label), ...patch });
const months = base => [option('', 'Choose month'), ...Array.from({ length: 12 }, (_, index) => option(base + index, index + 1))];
function rejectsOption(key, value, options, reason) {
  assert.throws(() => resolveProfileOption(key, value, options), error => {
    assert.equal(error.code, 'profile_option_invalid');
    assert.equal(error.failureDiagnostics.profile_option_reason, reason);
    return true;
  });
}

test('all twelve months use complete value schemas before numeric labels', () => {
  for (const base of [0, 1]) {
    const choices = months(base).reverse();
    for (let month = 1; month <= 12; month++) {
      const selected = resolveProfileOption('birthMonth', month, choices);
      assert.equal(selected.value, String(month - 1 + base));
      assert.equal(selected, choices.find(item => item.value === selected.value));
      assert.equal(resolveProfileOption('birthMonth', String(month), choices), selected);
    }
  }
});

test('a partial month list never guesses its base even when the requested value exists', () => {
  for (const base of [0, 1]) {
    const partial = months(base).filter(item => item.value !== String(base + 11));
    rejectsOption('birthMonth', 3, partial, 'incomplete');
  }
  rejectsOption('birthMonth', 3, [option('2', '3'), option('3', '4')], 'incomplete');
  rejectsOption('birthMonth', 3, months(1).map(item => item.value ? option('month-' + item.value, item.label) : item), 'invalid');
  rejectsOption('birthMonth', 3, months(1).map(item => item.value ? option(item.value.padStart(2, '0'), item.label) : item), 'invalid');
});

test('missing, duplicate, disabled and unsupported month values preserve fixed failure codes', () => {
  rejectsOption('birthMonth', 3, [], 'missing');
  rejectsOption('birthMonth', 3, [option('', 'Choose')], 'missing');
  rejectsOption('birthMonth', 3, [...months(0), option('2', 'Other')], 'ambiguous');
  rejectsOption('birthMonth', 3, months(0).map(item => item.value === '2' ? { ...item, disabled: true } : item), 'disabled');
  for (const value of [0, 13, -1, 3.5, 'PRIVATE', null, {}]) rejectsOption('birthMonth', value, months(0), 'invalid');
  rejectsOption('birthMonth', 3, [...months(0), option('12')], 'invalid');
});

test('country, year and day keep exact value priority and unique label compatibility', () => {
  for (const [key, value] of [['country', 'TH'], ['birthYear', '1984'], ['birthDay', '29']]) {
    const exact = option(value, 'Visible translation');
    const label = option('another-value', value);
    assert.equal(resolveProfileOption(key, value, [label, exact]), exact);
    assert.equal(resolveProfileOption(key, value, [label]), label);
    rejectsOption(key, value, [exact, { ...exact }], 'ambiguous');
    rejectsOption(key, value, [label, option('different-value', value)], 'ambiguous');
    rejectsOption(key, value, [{ ...exact, disabled: true }, label], 'disabled');
  }
  rejectsOption('birthYear', 1984, [option('2527'), option('1985')], 'missing');
  rejectsOption('country', 'TH', [option('JP'), option('th', 'Thailand')], 'missing');
  rejectsOption('birthDay', 29, [option('28')], 'missing');
});

test('invalid inputs and diagnostics never reveal profile values or option labels', () => {
  const choices = Array.from({ length: 401 }, () => option('PRIVATE-VALUE', 'PRIVATE-LABEL'));
  for (const [key, value, entries] of [
    ['birthYear', 'PRIVATE-BIRTHDAY', choices], ['country', 'PRIVATE-COUNTRY', choices],
    ['PRIVATE-FIELD', 'PRIVATE-TARGET', []], ['birthDay', 32, []], ['birthYear', 1984, null],
    ['birthYear', 1984, [{ value: '1984', label: 'PRIVATE-LABEL', disabled: 'yes' }]],
  ]) {
    assert.throws(() => resolveProfileOption(key, value, entries), error => {
      assert.equal(error.code, 'profile_option_invalid');
      assert.ok(error.failureDiagnostics.option_count >= 0 && error.failureDiagnostics.option_count <= 400);
      assert.match(error.failureDiagnostics.profile_option_reason, /^(missing|incomplete|ambiguous|disabled|invalid)$/);
      assert.doesNotMatch(JSON.stringify(error), /PRIVATE/);
      if (key === 'PRIVATE-FIELD') assert.equal(Object.hasOwn(error.failureDiagnostics, 'profile_field'), false);
      return true;
    });
  }
});

function virtualClock(timeoutMs = 5000) {
  let elapsed = 0;
  const delays = [];
  return { delays, elapsed: () => elapsed, options: { timeoutMs, pollIntervalMs: 100,
    now: () => elapsed, pause: async duration => { delays.push(duration); elapsed += duration; } } };
}

test('read-only waiting handles empty and partial lists becoming complete for every month', async () => {
  for (const base of [0, 1]) {
    for (let month = 1; month <= 12; month++) {
      const clock = virtualClock();
      const complete = months(base);
      const snapshots = [[option('', 'Choose')], complete.slice(0, 5), complete];
      let reads = 0;
      const selected = await waitForProfileOption(async () => snapshots[reads++], 'birthMonth', month, clock.options);
      assert.equal(selected.value, String(month - 1 + base));
      assert.equal(reads, 3);
      assert.equal(clock.elapsed(), 200);
      assert.equal(complete.some(item => Object.hasOwn(item, 'selected')), false);
    }
  }
});

test('options that never load retain the final reason and stop within five seconds', async () => {
  for (const [key, value, choices, reason] of [
    ['country', 'TH', [option('', 'Country')], 'missing'],
    ['birthMonth', 3, [option('0'), option('1'), option('2')], 'incomplete'],
    ['birthYear', 1984, [option('2527')], 'missing'],
    ['birthDay', 29, [option('29', '29', { disabled: true })], 'disabled'],
  ]) {
    const clock = virtualClock();
    let reads = 0;
    await assert.rejects(waitForProfileOption(async () => { reads++; return choices; }, key, value, clock.options), error => {
      assert.equal(error.code, 'profile_option_invalid');
      assert.equal(error.failureDiagnostics.profile_option_reason, reason);
      assert.equal(error.failureDiagnostics.option_count, choices.length);
      return true;
    });
    assert.equal(clock.elapsed(), 5000);
    assert.equal(reads, 51);
  }
});

test('an ambiguous or disabled selection is only read again, never chosen early', async () => {
  for (const initial of [
    [option('TH'), option('TH')], [option('TH', 'Thailand', { disabled: true })],
  ]) {
    const clock = virtualClock();
    let reads = 0;
    const ready = option('TH', 'Thailand');
    assert.equal(await waitForProfileOption(async () => reads++ === 0 ? initial : [ready], 'country', 'TH', clock.options), ready);
    assert.equal(reads, 2);
  }
});

test('reader exceptions are not swallowed or retried, including unrelated registration failures', async () => {
  for (const failure of [new Error('fixture navigation failure'), new RegistrationError('field_changed'), new RegistrationError('profile_option_invalid')]) {
    const clock = virtualClock();
    let reads = 0;
    await assert.rejects(waitForProfileOption(async () => { reads++; throw failure; }, 'country', 'TH', clock.options), error => error === failure);
    assert.equal(reads, 1);
    assert.deepEqual(clock.delays, []);
  }
});

test('a stalled read is bounded and invalid input cannot invoke the reader', async () => {
  await assert.rejects(waitForProfileOption(() => new Promise(() => {}), 'country', 'TH', { timeoutMs: 10 }),
    error => error.code === 'profile_option_invalid' && error.failureDiagnostics.profile_option_reason === 'missing');
  let reads = 0;
  const reader = async () => { reads++; return months(0); };
  await assert.rejects(waitForProfileOption(reader, 'birthMonth', 0), { code: 'profile_option_invalid' });
  await assert.rejects(waitForProfileOption(reader, 'birthMonth', 1, { timeoutMs: 5001 }), { code: 'profile_option_invalid' });
  assert.equal(reads, 0);
});
