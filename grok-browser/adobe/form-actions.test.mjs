import test from 'node:test';
import assert from 'node:assert/strict';
import { registrationButtonAction, registrationLinkAction, isRegistrationButton,
  locateRegistrationButton, locateCreateAccountLink, isSignupPasswordForm } from './form-actions.mjs';

test('the shared signup button ID distinguishes continuation from creation by its official event', () => {
  for (const [dataId, clickEvent, action] of [
    ['Signup-CreateAccountBtn', 'SignupContinueClick', 'continue'],
    ['Signup-ContinueBtn', 'CreateAccountContinueClick', 'profile_continue'],
    ['Signup-CreateAccountBtn', 'CreateAccountClick', 'create'],
    ['Signup-CreateAccountBtn', 'UNKNOWN_PRIVATE_EVENT', 'unknown_signup'],
    ['Signup-CreateAccountBtn', undefined, 'unknown_signup'],
    ['PasswordPage-ContinueButton', 'ContinueClick', null],
  ]) assert.equal(registrationButtonAction({ dataId, clickEvent }), action);
  assert.equal(registrationLinkAction({ dataId: 'EmailPage-CreateAccountLink' }), 'create_account');
  assert.equal(registrationLinkAction({ dataId: 'PasswordPage-GetHelpLink' }), null);
});

test('localized or misleading labels never override a recognized signup action', () => {
  assert.equal(isRegistrationButton({ action: 'continue', text: 'Create account' }, 'create'), false);
  assert.equal(isRegistrationButton({ action: 'continue', text: '続行' }, 'continue'), true);
  assert.equal(isRegistrationButton({ action: 'profile_continue', text: 'ดำเนินการต่อ' }, 'continue'), true);
  assert.equal(isRegistrationButton({ action: 'create', text: 'アカウントを作成' }, 'create'), true);
  assert.equal(isRegistrationButton({ action: 'unknown_signup', text: 'Create account' }, 'create'), false);
  assert.equal(isRegistrationButton({ action: 'unknown_signup', text: 'Continue' }, 'continue'), false);
  assert.equal(isRegistrationButton({ text: 'Create account' }, 'create'), true);
  assert.equal(isRegistrationButton({ text: '继续' }, 'continue'), true);
});

function locatorFixture(count = 1) {
  const calls = [];
  const locator = { count: async () => count, or: other => {
    assert.equal(other, locator);
    calls.push({ union: true });
    return locator;
  } };
  return { calls, locator, frame: {
    locator: selector => { calls.push({ selector }); return locator; },
    getByRole: (role, options) => { calls.push({ role, name: options.name.source }); return locator; },
  } };
}

test('localized signup buttons select only the corresponding official control', async () => {
  for (const [storedAction, requestedAction, event] of [
    ['continue', 'continue', 'SignupContinueClick'],
    ['profile_continue', 'continue', 'CreateAccountContinueClick'],
    ['create', 'create', 'CreateAccountClick'],
  ]) {
    const f = locatorFixture();
    assert.equal(await locateRegistrationButton(f.frame, requestedAction,
      { buttons: [{ action: storedAction, text: 'ชื่อภาษาไทย' }] }), f.locator);
    assert.equal(f.calls.length, 1);
    assert.ok(f.calls[0].selector.includes(`[data-click-event="${event}"]`));
    assert.ok(f.calls[0].selector.endsWith(':visible'));
  }
});

test('conflicting, duplicated, unknown and stale signup controls cannot fall back to text matching', async () => {
  for (const buttons of [
    [{ action: 'continue', text: 'Create account' }],
    [{ action: 'unknown_signup', text: 'Create account' }],
    [{ action: 'create' }, { action: 'create' }],
    [{ action: 'create' }, { action: 'unknown_signup' }],
    [{ action: 'create' }, { action: 'continue' }],
  ]) {
    const f = locatorFixture();
    await assert.rejects(locateRegistrationButton(f.frame, 'create', { buttons }), { code: 'create_button_not_ready' });
    assert.deepEqual(f.calls, []);
  }
  for (const count of [0, 2]) {
    const f = locatorFixture(count);
    await assert.rejects(locateRegistrationButton(f.frame, 'create', { buttons: [{ action: 'create' }] }), { code: 'create_button_not_ready' });
    assert.equal(f.calls.length, 1);
    assert.ok(f.calls[0].selector);
  }
});

test('legacy forms preserve the existing role and text lookup without selecting arbitrary submit controls', async () => {
  const f = locatorFixture();
  assert.equal(await locateRegistrationButton(f.frame, 'create', { buttons: [{ text: 'Create account' }] }), f.locator);
  assert.equal(f.calls[0].role, 'button');
  assert.ok(f.calls[0].name.includes('Create account'));
  assert.equal(f.calls[0].selector, undefined);
  const duplicate = locatorFixture(2);
  await assert.rejects(locateRegistrationButton(duplicate.frame, 'continue', {}), { code: 'continue_not_ready' });
});

test('creation links use the observed stable marker and retain the legacy fallback otherwise', () => {
  const current = locatorFixture();
  assert.equal(locateCreateAccountLink(current.frame, { links: [{ action: 'create_account' }] }), current.locator);
  assert.deepEqual(current.calls, [{ selector: '[data-id="EmailPage-CreateAccountLink"]:visible' }]);
  const legacy = locatorFixture();
  assert.equal(locateCreateAccountLink(legacy.frame, {}), legacy.locator);
  assert.equal(legacy.calls[0].role, 'link');
});

test('before the first form snapshot the creation entry unions its stable marker with the legacy link', () => {
  const f = locatorFixture();
  assert.equal(locateCreateAccountLink(f.frame), f.locator);
  assert.deepEqual(f.calls.map(call => call.selector ? 'marker' : call.role || 'union'), ['marker', 'link', 'union']);
});

test('new-password plus an official signup action identifies registration without admitting login or reset pages', () => {
  const fields = [{ type: 'password', autocomplete: 'new-password' }];
  assert.equal(isSignupPasswordForm({ fields, buttons: [{ action: 'continue' }] }), true);
  assert.equal(isSignupPasswordForm({ fields, buttons: [{ action: 'create' }] }), true);
  assert.equal(isSignupPasswordForm({ fields, buttons: [{ action: 'profile_continue' }] }), false);
  assert.equal(isSignupPasswordForm({ fields, buttons: [{ action: 'unknown_signup' }] }), false);
  assert.equal(isSignupPasswordForm({ fields, buttons: [{ text: 'Continue' }] }), false);
  assert.equal(isSignupPasswordForm({ fields: [{ type: 'password', autocomplete: 'current-password' }],
    buttons: [{ action: 'continue' }] }), false);
});
