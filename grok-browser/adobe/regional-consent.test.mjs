import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { regionalConsentPlan, regionalConsentPreflight } from './regional-consent.mjs';
import { BrowserRegistration } from './browser.mjs';
import { PrivateRunStore } from './core.mjs';
import { runAutomaticRegistration } from './auto-runner.mjs';

const requiredKeys = ['kr_storage', 'kr_transfer'];
const consent = (regionalConsent, patch = {}) => ({
  tag: 'input', type: 'checkbox', id: regionalConsent, name: '', label: 'Consent',
  regionalConsent, checked: false, filled: false, required: false, invalid: false, ...patch,
});
const regionalForm = fields => ({ fields: fields ?? requiredKeys.map(key => consent(key)), regionalConsentGroupCount: 1 });

test('only the two observed Korean controls form a required consent plan', () => {
  const optional = { tag: 'input', type: 'checkbox', name: 'marketing', checked: false };
  const form = regionalForm([...requiredKeys.map(key => consent(key)), optional]);
  assert.deepEqual(regionalConsentPlan(form, 'KR').map(item => item.key), requiredKeys);
  for (const country of ['KR', 'JP', 'TH', 'US']) {
    assert.deepEqual(regionalConsentPlan({ fields: [optional] }, country), []);
  }
});

test('missing, duplicate, disabled, mistyped and wrong-country consent groups cannot be guessed', () => {
  const malformed = [
    regionalForm([consent('kr_storage')]),
    regionalForm([consent('kr_storage'), consent('kr_storage')]),
    regionalForm([consent('kr_storage'), consent('kr_transfer', { disabled: true })]),
    regionalForm([consent('kr_storage'), consent('kr_transfer', { type: 'text' })]),
    { ...regionalForm(), regionalConsentGroupCount: 2 },
    { ...regionalForm(), regionalConsentGroupCount: 0 },
  ];
  for (const form of malformed) assert.throws(() => regionalConsentPlan(form, 'KR'), { code: 'regional_consent_not_ready' });
  for (const country of ['JP', 'TH', 'US', undefined]) {
    assert.throws(() => regionalConsentPlan(regionalForm(), country), { code: 'regional_consent_not_ready' });
  }
});

test('approval deferral affects only the known unchecked fields and never changes their actual checked value', () => {
  const unknown = { type: 'checkbox', name: 'futureTerms', checked: false, required: true, invalid: true, validity: { valueMissing: true } };
  const optional = { type: 'checkbox', name: 'marketing', checked: false, required: false };
  const form = regionalForm([...requiredKeys.map(key => consent(key, { required: true, invalid: true, validity: { valueMissing: true } })), unknown, optional]);
  const deferred = regionalConsentPreflight(form, 'KR', true);
  assert.deepEqual(deferred.fields.slice(0, 2).map(field => [field.required, field.invalid, field.checked, field.validity.valueMissing]),
    [[false, false, false, false], [false, false, false, false]]);
  assert.equal(deferred.fields[2], unknown);
  assert.equal(deferred.fields[3], optional);
  assert.equal(form.fields[0].required, true, 'Original page evidence must stay unchanged');
  const final = regionalConsentPreflight(deferred, 'KR');
  assert.deepEqual(final.fields.slice(0, 2).map(field => [field.required, field.checked]), [[true, false], [true, false]]);
});

// 保留真实批准流程和磁盘状态，只替换页面 I/O；实际 DOM 选择另由离线浏览器测试覆盖。
async function fixture(t, { selected = [], onCheck, applyCheck = true, unknownRequired = false } = {}) {
  const directory = await mkdtemp(join(tmpdir(), 'adobe-region-consent-'));
  const store = new PrivateRunStore(join(directory, 'private'), 'one');
  const profile = { firstName: 'Fixture', lastName: 'Test', birthMonth: 2, birthYear: 1984, country: 'KR' };
  await store.init({ email: 'fixture@example.invalid', profile });
  const initial = await store.load();
  await store.save({ config: { ...initial.config, profile } });
  await store.setStage('awaiting_approval', 'account_agreement_required');
  const browser = new BrowserRegistration(store);
  browser.workerMode = true;
  browser.page = { url: () => 'https://auth.services.adobe.com/' };
  browser.context = { pages: () => [browser.page] };
  browser.networkStatus = async () => ({ code: null });
  browser.creationTracker = { begin: async () => {} };
  const state = { checked: new Set(selected), checks: [], clicks: 0, country: 'KR', groupVisible: true };
  const button = { count: async () => 1, isEnabled: async () => true, click: async () => {
    assert.equal((await store.load()).submission.started, true);
    state.clicks++;
  } };
  browser.authFrame = async () => ({ getByRole: () => button });
  browser.field = async (_frame, field) => {
    assert.equal(field.name, 'country');
    return { inputValue: async () => state.country };
  };
  browser.regionalConsentControl = async key => {
    assert.ok(requiredKeys.includes(key), 'Optional or unknown consent must never be targeted');
    return {
      isChecked: async () => state.checked.has(key),
      check: async () => {
        assert.equal((await store.load()).submission.started, false);
        state.checks.push(key);
        if (applyCheck) state.checked.add(key);
        await onCheck?.(key, state);
      },
    };
  };
  const form = () => ({ host: 'auth.services.adobe.com', headings: [], notices: [], agreements: [],
    buttons: [{ text: 'Create account', disabled: false }],
    ...(state.groupVisible ? { regionalConsentGroupCount: 1 } : {}),
    fields: [
      { tag: 'input', type: 'text', id: 'country', name: 'country', label: 'Country', filled: true, required: true },
      ...(state.groupVisible ? requiredKeys.map(key => consent(key, { checked: state.checked.has(key), filled: state.checked.has(key) })) : []),
      { tag: 'input', type: 'checkbox', id: 'marketing', name: 'marketing', checked: false, required: false, filled: false },
      ...(unknownRequired ? [{ tag: 'input', type: 'checkbox', id: 'futureTerms', name: 'futureTerms', checked: false, required: true, filled: false }] : []),
    ],
  });
  browser.form = async () => form();
  browser.waitForStage = async () => ({ stage: 'awaiting_verification', code: null, form: { ...form(), host: 'auth.services.adobe.com' } });
  t.after(async () => { await store.close(); await rm(directory, { recursive: true, force: true }); });
  return { browser, store, state };
}

test('without explicit creation approval the runner neither checks consent nor submits', async t => {
  const f = await fixture(t);
  const result = await runAutomaticRegistration(f.browser, f.store, { approveCreate: false });
  assert.equal(result.code, 'account_agreement_required');
  assert.deepEqual(f.state.checks, []);
  assert.equal(f.state.clicks, 0);
  assert.equal((await f.store.load()).submission.started, false);
});

test('approved creation checks only missing required consents and cannot replay', async t => {
  for (const selected of [[], ['kr_storage'], requiredKeys]) {
    await t.test(`selected=${selected.length}`, async t => {
      const f = await fixture(t, { selected });
      await f.browser.approveCreate();
      assert.deepEqual(f.state.checks, requiredKeys.filter(key => !selected.includes(key)));
      assert.equal(f.state.clicks, 1);
      await assert.rejects(f.browser.approveCreate(), { code: 'submission_already_started' });
      assert.deepEqual(f.state.checks, requiredKeys.filter(key => !selected.includes(key)));
      assert.equal(f.state.clicks, 1);
    });
  }
});

test('a pre-existing submission marker prevents any consent change', async t => {
  const f = await fixture(t);
  await f.store.markSubmissionStarted();
  await assert.rejects(f.browser.approveCreate(), { code: 'submission_already_started' });
  assert.deepEqual(f.state.checks, []);
  assert.equal(f.state.clicks, 0);
});

test('unknown required consent blocks before any known agreement is changed', async t => {
  const f = await fixture(t, { unknownRequired: true });
  const result = await f.browser.approveCreate();
  assert.equal(result.code, 'form_rejected');
  assert.deepEqual(result.failureDiagnostics.invalid_fields, ['terms']);
  assert.deepEqual(f.state.checks, []);
  assert.equal(f.state.clicks, 0);
  assert.equal((await f.store.load()).submission.started, false);
});

test('intermediate or ambiguous signup actions cannot accept agreements before being rejected', async t => {
  for (const actions of [['profile_continue'], ['create', 'unknown_signup'], ['create', 'continue']]) {
    const f = await fixture(t);
    const readForm = f.browser.form;
    f.browser.form = async () => ({...await readForm(), buttons:actions.map(action => ({action,text:'Create account',disabled:false}))});
    await assert.rejects(f.browser.approveCreate(), {code:'create_button_not_ready'});
    assert.deepEqual(f.state.checks, []);
    assert.equal(f.state.clicks, 0);
    assert.equal((await f.store.load()).submission.started, false);
  }
});

test('checkbox changes that do not persist cannot create or mark a submitted account', async t => {
  const f = await fixture(t, { applyCheck: false });
  const result = await f.browser.approveCreate();
  assert.equal(result.code, 'form_rejected');
  assert.deepEqual(result.failureDiagnostics.invalid_fields, ['terms']);
  assert.equal(f.state.clicks, 0);
  assert.equal((await f.store.load()).submission.started, false);
});

test('country changes during agreement handling stop before the next agreement and Create', async t => {
  const f = await fixture(t, { onCheck: (_key, state) => { state.country = 'JP'; } });
  const result = await f.browser.approveCreate();
  assert.equal(result.code, 'form_rejected');
  assert.deepEqual(result.failureDiagnostics.invalid_fields, ['country']);
  assert.deepEqual(f.state.checks, ['kr_storage']);
  assert.equal(f.state.clicks, 0);
  assert.equal((await f.store.load()).submission.started, false);
});

test('a required agreement group disappearing after selection cannot silently pass final validation', async t => {
  const f = await fixture(t, { onCheck: (key, state) => { if (key === 'kr_transfer') state.groupVisible = false; } });
  await assert.rejects(f.browser.approveCreate(), { code: 'regional_consent_not_ready' });
  assert.equal(f.state.clicks, 0);
  assert.equal((await f.store.load()).submission.started, false);
});
