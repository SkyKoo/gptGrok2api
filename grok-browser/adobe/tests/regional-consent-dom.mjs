// 离线真实 DOM 回归：所有页面响应来自本文件的合成表单，不联系 Adobe。
import assert from 'node:assert/strict';
import { mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
import { BrowserRegistration } from '../browser.mjs';
import { PrivateRunStore } from '../core.mjs';
import { runAutomaticRegistration } from '../auto-runner.mjs';

const target = 'https://auth.services.adobe.com/offline-region-fixture';
const scenarioFilter = process.env.REGION_DOM_CASE;
const sourceFiles = (await readdir(new URL('../', import.meta.url))).filter(name => name.endsWith('.mjs') && !name.endsWith('.test.mjs'));
const sourceHashes = async () => Object.fromEntries(await Promise.all(sourceFiles.map(async name =>
  [name, createHash('sha256').update(await readFile(new URL('../' + name, import.meta.url))).digest('hex')])));
const sourceSHA = await sourceHashes();
const directory = await mkdtemp(join(tmpdir(), 'cfm-region-dom-'));
const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'],
  ...(process.env.CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.CHROMIUM_EXECUTABLE_PATH } : {}) });
const report = { scope: 'offline synthetic DOM in real Chromium', real_adobe_registration_tested: false,
  external_requests_forwarded: 0, source_sha256: sourceSHA,
  test_sha256: createHash('sha256').update(await readFile(new URL(import.meta.url))).digest('hex'),
  browser_version: await browser.version(), cases: [] };

function html(config) {
  // 覆盖 Spectrum 将 data-id 传给 input，以及旧版 wrapper 标记两种结构。
  return `<!doctype html><html><meta charset="utf-8"><style>
  label { display:block; margin:8px; }
  label.choice { position:relative; min-height:22px; }
  label.choice input { position:absolute; inset:0; width:100%; height:100%; margin:0; opacity:0; }
  </style><body>
  <h1>Create an account</h1>
  <section id="passwordStage" hidden>
    <label>Email<input name="email" type="email" autocomplete="email"></label>
    <label>Password<input name="password" type="password" autocomplete="new-password"></label>
    <button id="passwordContinue" type="button" data-id="Signup-CreateAccountBtn" data-click-event="SignupContinueClick">Continue</button>
  </section>
  <main id="profileStage">
  <label>Country/Region <select id="country" name="country" required>
    <option value="US">United States</option><option value="KR">Korea, South</option>
    <option value="JP">Japan</option><option value="TH">Thailand</option>
  </select></label>
  <section id="profile"></section><section id="consents"></section>
  <button id="create" type="button" data-id="Signup-CreateAccountBtn" data-click-event="CreateAccountClick">Create account</button>
  </main>
  <script>
  const config = ${JSON.stringify(config)};
  window.fixtureEvents = { changes: [], passwordContinues: 0, createClicks: 0, createsAccepted: 0, submitted: null, monthOptionsLoaded: false };
  document.querySelector('h1').textContent = config.country === 'JP' ? 'アカウントを作成' : config.country === 'TH' ? 'สร้างบัญชี' : 'Create an account';
  let controlSerial = 0, renderSerial = 0;
  function checkbox(id, text, suffix = '') {
    const selected = (config.selected || []).includes(id) ? ' checked' : '';
    const disabled = config.disabledRequired && id === 'SouthKorea-ServiceProviderConsent' ? ' disabled' : '';
    const inputMarker = config.markerOnInput ? ' data-id="' + id + '"' : '';
    const wrapperMarker = config.markerOnInput && !config.duplicateAncestorMarker ? '' : ' data-id="' + id + '"';
    return '<label class="choice"' + wrapperMarker + '><input type="checkbox" id="fixture-choice-' + (++controlSerial) + suffix + '"' + inputMarker + selected + disabled + '>' + text + '</label>';
  }
  function render() {
    const currentRender = ++renderSerial;
    const country = document.querySelector('#country').value;
    document.querySelector('#create').textContent = country === 'JP' ? 'アカウントを作成' : country === 'TH' ? 'สร้างบัญชี' : country === 'KR' ? '계정 만들기' : 'Create account';
    const labels = country === 'JP' ? ['名', '姓', '月', '年'] : country === 'TH' ? ['ชื่อ', 'นามสกุล', 'เดือน', 'ปี'] : ['First name', 'Last name', 'Month', 'Year'];
    const nameFields = '<label>' + labels[0] + '<input name="firstName" id="firstName" required></label>' +
      '<label>' + labels[1] + '<input name="lastName" id="lastName" required></label>';
    const monthOptions = Array.from({length:12}, (_,i) => '<option value="' + i + '">' + (i + 1) + '</option>').join('');
    const initialMonth = config.initialMonthValue === undefined ? '' : '<option value="' + config.initialMonthValue + '">' + config.initialMonthValue + '</option>';
    const month = '<label>' + labels[2] + '<select name="month" id="month" required><option value="">Choose</option>' +
      (config.monthOptionsDelayMs || config.monthOptionsNever ? initialMonth : monthOptions) + '</select></label>';
    const year = country === 'JP'
      ? '<label>' + labels[3] + '<select name="bday-year" id="year" required><option value="">Choose</option><option value="1984">1984</option></select></label>'
      : '<label>' + labels[3] + '<input name="bday-year" id="year" required pattern="[0-9]{4}"></label>';
    document.querySelector('#profile').innerHTML = nameFields + (country === 'JP' ? year + month : month + year);
    if (config.monthOptionsDelayMs && !config.monthOptionsNever) setTimeout(() => {
      if (currentRender !== renderSerial) return;
      const original = document.querySelector('[name="month"]');
      const replacement = original.cloneNode(false);
      replacement.innerHTML = '<option value="">Choose</option>' + monthOptions;
      original.replaceWith(replacement);
      window.fixtureEvents.monthOptionsLoaded = true;
    }, config.monthOptionsDelayMs);
    let markup = '';
    if (country === 'KR') {
      const group = suffix => '<div data-id="MarketingConsent-SouthKorea-Required"><p>Required</p>' +
        checkbox('SouthKorea-StorageConsent', '개인정보 수집 및 이용에 동의합니다.', suffix) +
        (config.missingRequired ? '' : checkbox('SouthKorea-ServiceProviderConsent', '개인정보의 국내외 이전에 동의합니다.', suffix)) +
        (config.duplicateMarker ? checkbox('SouthKorea-StorageConsent', 'Duplicate', suffix + 'duplicate') : '') + '</div>';
      markup = group('a') + (config.duplicateGroup ? group('b') : '');
    }
    markup += '<div data-id="MarketingConsent-Optional">' + checkbox('Optional-ReceiveEmails', 'Receive marketing emails') +
      checkbox('Optional-ThirdPartySharing', 'Share for marketing') + '</div>';
    if (config.unknownRequired) markup += '<label><input type="checkbox" name="unrecognized-required" required>Unrecognized required choice</label>';
    document.querySelector('#consents').innerHTML = markup;
    if (country === 'KR' && config.multipleInputs) document.querySelector('[data-id="SouthKorea-StorageConsent"]').insertAdjacentHTML('beforeend', '<input type="checkbox" id="duplicate-input">');
    if (country === 'KR' && config.wrongType) document.querySelector('[data-id="SouthKorea-ServiceProviderConsent"] input, input[data-id="SouthKorea-ServiceProviderConsent"]').type = 'text';
    document.querySelectorAll('#consents input').forEach(input => input.addEventListener('change', event => {
      window.fixtureEvents.changes.push({ dataId: input.closest('[data-id]')?.dataset.id || input.name,
        checked: input.checked, trusted: event.isTrusted });
    }));
  }
  document.querySelector('#country').addEventListener('change', () => {
    renderSerial++;
    document.querySelector('#profile').innerHTML = '';
    document.querySelector('#consents').innerHTML = '';
    setTimeout(render, 150);
  });
  document.querySelector('#passwordContinue').textContent = config.country === 'JP' ? '続行' : config.country === 'TH' ? 'ดำเนินการต่อ' : 'Continue';
  document.querySelector('#passwordContinue').addEventListener('click', () => {
    window.fixtureEvents.passwordContinues++;
    if (!document.querySelector('[name="email"]').value || !document.querySelector('[name="password"]').value) return;
    document.querySelector('#passwordStage').hidden = true;
    document.querySelector('#profileStage').hidden = false;
    setTimeout(render, 150);
  });
  document.querySelector('#create').addEventListener('click', () => {
    const state = window.fixtureEvents;
    state.createClicks++;
    const country = document.querySelector('#country').value;
    const controls = [...document.querySelectorAll('#profileStage input,#profileStage select')];
    const required = [...document.querySelectorAll('[data-id="MarketingConsent-SouthKorea-Required"] input')];
    if (!controls.every(input => input.checkValidity()) || required.some(input => !input.checked)) {
      required.filter(input => !input.checked).forEach(input => input.setAttribute('aria-invalid', 'true'));
      return;
    }
    state.submitted = { country, firstName: document.querySelector('[name="firstName"]').value,
      lastName: document.querySelector('[name="lastName"]').value, month: document.querySelector('[name="month"]').value,
      year: document.querySelector('[name="bday-year"]').value,
      consents: [...document.querySelectorAll('#consents input')].map(input => ({ dataId: input.closest('[data-id]')?.dataset.id || input.name, checked: input.checked })) };
    state.createsAccepted++;
    document.body.innerHTML = '<h1>Verify your email</h1><label>Verification code<input name="code" autocomplete="one-time-code"></label>';
  });
  if (config.startPasswordStage) {
    document.querySelector('#profileStage').hidden = true;
    document.querySelector('#passwordStage').hidden = false;
  } else render();
  </script></body></html>`;
}

async function fixture(name, country, config = {}) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, serviceWorkers: 'block' });
  let served = 0;
  await context.route('**/*', route => {
    assert.equal(route.request().method(), 'GET');
    assert.equal(route.request().url(), target, 'No unplanned endpoint may be accessed');
    served++;
    return route.fulfill({ status: 200, contentType: 'text/html', body: html({ ...config, country }) });
  });
  const page = await context.newPage();
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.goto(target, { waitUntil: 'load' });
  const store = new PrivateRunStore(join(directory, name), 'one');
  await store.init({ email: 'fixture@example.invalid' });
  await store.setStage('form_ready');
  const driver = new BrowserRegistration(store);
  driver.workerMode = true;
  driver.context = context;
  driver.page = page;
  driver.stageWaitOptions = { timeoutMs: 5000, pollIntervalMs: 50 };
  const profile = { firstName: 'Fixture', lastName: 'Test', country, birthMonth: config.birthMonth || 2, birthYear: 1984, birthDay: 29 };
  return { context, page, store, driver, profile, pageErrors, served: () => served,
    events: () => page.evaluate(() => window.fixtureEvents),
    close: async () => { await context.close(); await store.close(); } };
}

async function run(name, country, config, operation) {
  if (scenarioFilter && scenarioFilter !== name) return;
  const f = await fixture(name, country, config);
  try {
    await operation(f);
    assert.equal(f.served(), 1);
    assert.deepEqual(f.pageErrors, []);
    report.cases.push({ name, country, passed: true });
    console.error(`PASS ${name}`);
  } finally { await f.close(); }
}

try {
  const approvedCases = [['KR', false], ['JP', false], ['TH', false], ['JP', true], ['TH', true]]
    .map(([country, startPasswordStage]) => ({ country, startPasswordStage,
      name: (startPasswordStage ? 'password-then-profile-' : 'approved-') + country.toLowerCase() }));
  for (const markerOnInput of [false, true]) {
    for (const selected of [[], ['SouthKorea-StorageConsent'], ['SouthKorea-StorageConsent', 'SouthKorea-ServiceProviderConsent']]) {
      if (!markerOnInput && selected.length === 0) continue; // 原 approved-kr 已覆盖。
      approvedCases.push({ country: 'KR', markerOnInput, selected,
        name: `approved-kr-${markerOnInput ? 'input' : 'wrapper'}-selected-${selected.length}` });
    }
    approvedCases.push({ country: 'KR', markerOnInput,
      selected: ['Optional-ReceiveEmails', 'Optional-ThirdPartySharing'],
      name: `approved-kr-${markerOnInput ? 'input' : 'wrapper'}-marketing-preserved` });
  }
  for (const config of approvedCases) {
    const { name, country, startPasswordStage } = config;
    const selected = config.selected || [];
    await run(name, country, config, async f => {
      const paused = await runAutomaticRegistration(f.driver, f.store, {
        readInput: async () => ({ profile: f.profile }), approveCreate: false,
      });
      assert.equal(paused.code, 'account_agreement_required', JSON.stringify(paused));
      assert.equal((await f.store.load()).stage, 'awaiting_approval');
      assert.equal((await f.store.load()).submission.started, false);
      assert.deepEqual((await f.events()).changes, [], 'No consent may be toggled before approval');
      assert.equal((await f.events()).createClicks, 0);
      assert.equal((await f.events()).passwordContinues, startPasswordStage ? 1 : 0);
      const completed = await runAutomaticRegistration(f.driver, f.store, {
        readInput: async () => ({}), approveCreate: true,
      });
      assert.equal(completed.code, 'verification_required', JSON.stringify(completed));
      const events = await f.events();
      assert.equal(events.createClicks, 1);
      assert.equal(events.createsAccepted, 1);
      assert.deepEqual(events.submitted, {
        country, firstName: 'Fixture', lastName: 'Test', month: '1', year: '1984',
        consents: [
          ...(country === 'KR' ? [
            { dataId: 'SouthKorea-StorageConsent', checked: true },
            { dataId: 'SouthKorea-ServiceProviderConsent', checked: true },
          ] : []),
          { dataId: 'Optional-ReceiveEmails', checked: selected.includes('Optional-ReceiveEmails') },
          { dataId: 'Optional-ThirdPartySharing', checked: selected.includes('Optional-ThirdPartySharing') },
        ],
      });
      assert.deepEqual(events.changes, country === 'KR' ? [
        { dataId: 'SouthKorea-StorageConsent', checked: true, trusted: true },
        { dataId: 'SouthKorea-ServiceProviderConsent', checked: true, trusted: true },
      ].filter(event => !selected.includes(event.dataId)) : []);
      assert.equal((await f.store.load()).submission.started, true);
      await assert.rejects(f.driver.approveCreate(), error => error.code === 'submission_already_started');
      const repeat = await runAutomaticRegistration(f.driver, f.store, { approveCreate: true });
      assert.equal(repeat.code, 'verification_required');
      assert.equal((await f.events()).createClicks, 1, 'A later runner must not repeat the create click');
    });
  }

  for (const markerOnInput of [false, true]) {
    for (const [failure, config] of [
      ['missing-required', { missingRequired: true }],
      ['duplicate-group', { duplicateGroup: true }],
      ['duplicate-marker', { duplicateMarker: true }],
      ['disabled-required', { disabledRequired: true }],
      ['wrong-type', { wrongType: true }],
      ['unknown-required', { unknownRequired: true }],
      ...(markerOnInput ? [['duplicate-ancestor-marker', { duplicateAncestorMarker: true }]] : []),
      ...(!markerOnInput ? [['multiple-inputs', { multipleInputs: true }]] : []),
    ]) {
      const name = `kr-${markerOnInput ? 'input' : 'wrapper'}-${failure}`;
      await run(name, 'KR', { ...config, markerOnInput }, async f => {
      const result = await runAutomaticRegistration(f.driver, f.store, {
        readInput: async () => ({ profile: f.profile }), approveCreate: true,
      });
      assert.equal(result.automatic.paused, true);
      assert.equal((await f.events()).createClicks, 0, name);
      assert.deepEqual((await f.events()).changes, [], name + ': incomplete or unrelated requirements must block before consent actions');
      assert.equal((await f.store.load()).submission.started, false, name);
      });
    }
  }

  for (const markerOnInput of [false, true]) await run(`kr-${markerOnInput ? 'input' : 'wrapper'}-country-changed`, 'KR', { markerOnInput }, async f => {
    const prepared = await f.driver.advance({ profile: f.profile });
    assert.equal(prepared.stage, 'awaiting_approval');
    await f.page.locator('#country').selectOption('JP');
    await f.page.locator('[name="bday-year"]').waitFor();
    await f.page.locator('[name="firstName"]').fill('Fixture');
    await f.page.locator('[name="lastName"]').fill('Test');
    await f.page.locator('[name="month"]').selectOption({ value: '1' });
    await f.page.locator('[name="bday-year"]').selectOption({ value: '1984' });
    const result = await f.driver.approveCreate();
    assert.equal(result.code, 'form_rejected');
    assert.ok(result.failureDiagnostics?.invalid_fields.includes('country'));
    assert.equal((await f.events()).createClicks, 0);
    assert.deepEqual((await f.events()).changes, []);
    assert.equal((await f.store.load()).submission.started, false);
  });

  for (const birthMonth of [3, 4]) await run(`th-delayed-month-${birthMonth}`, 'TH', { monthOptionsDelayMs: 400, birthMonth, initialMonthValue: String(birthMonth) }, async f => {
    const result = await f.driver.advance({ profile: f.profile });
    assert.equal(result.stage, 'awaiting_approval', JSON.stringify(result));
    assert.equal(await f.page.locator('[name="month"]').inputValue(), String(birthMonth - 1));
    assert.equal((await f.events()).monthOptionsLoaded, true);
    assert.equal((await f.events()).createClicks, 0);
    assert.deepEqual((await f.events()).changes, []);
    assert.equal((await f.store.load()).submission.started, false);
  });

  await run('th-month-options-never-loaded', 'TH', { monthOptionsNever: true, birthMonth: 3 }, async f => {
    f.driver.stageWaitOptions = { timeoutMs: 1000, pollIntervalMs: 20 };
    const result = await runAutomaticRegistration(f.driver, f.store, {
      readInput: async () => ({ profile: f.profile }), approveCreate: true,
    });
    assert.equal(result.automatic.paused, true);
    assert.equal(result.code, 'profile_option_invalid');
    assert.deepEqual(result.failureDiagnostics, { profile_field: 'birth_month', profile_option_reason: 'missing', option_count: 1 });
    assert.equal((await f.events()).createClicks, 0);
    assert.deepEqual((await f.events()).changes, []);
    assert.equal((await f.store.load()).submission.started, false);
  });

  assert.deepEqual(await sourceHashes(), sourceSHA, 'Source changed while the regression was running');
  assert.ok(report.cases.length > 0, 'The requested scenario must exist');
  report.source_unchanged = true;
  report.passed = report.cases.length;
  if (process.env.REGION_DOM_REPORT) await writeFile(process.env.REGION_DOM_REPORT, JSON.stringify(report, null, 2) + '\n');
  console.log(JSON.stringify(report));
} finally {
  await browser.close();
  await rm(directory, { recursive: true, force: true });
}
