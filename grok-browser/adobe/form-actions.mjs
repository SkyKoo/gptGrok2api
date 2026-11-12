import { RegistrationError } from './core.mjs';

const CONTINUE_TEXT = /^(Continue|Next|继续|下一步)$/i;
const CREATE_TEXT = /^(Create account|Create an account|Agree and create account|创建帐户|创建账户|同意并创建帐户|同意并创建账户)$/i;
const CREATE_LINK_TEXT = /^(Create an account|创建[帐账]户)$/i;

// Adobe 的同一 Signup 按钮在第一步与最终创建复用 data-id，事件标记才区分操作。
const BUTTON_RULES = Object.freeze([
  { dataId: 'Signup-CreateAccountBtn', clickEvent: 'SignupContinueClick', action: 'continue' },
  { dataId: 'Signup-ContinueBtn', clickEvent: 'CreateAccountContinueClick', action: 'profile_continue' },
  { dataId: 'Signup-CreateAccountBtn', clickEvent: 'CreateAccountClick', action: 'create' },
]);
const KNOWN_BUTTON_IDS = new Set(BUTTON_RULES.map(rule => rule.dataId));

/** 将已证实的官方控件标记投影为固定动作，不把原始 DOM 属性带入持久诊断。 */
export function registrationButtonAction({ dataId, clickEvent } = {}) {
  return BUTTON_RULES.find(rule => rule.dataId === dataId && rule.clickEvent === clickEvent)?.action
    || (KNOWN_BUTTON_IDS.has(dataId) ? 'unknown_signup' : null);
}

export function registrationLinkAction({ dataId } = {}) {
  return dataId === 'EmailPage-CreateAccountLink' ? 'create_account' : null;
}

function matchesAction(value, action) {
  return action === 'continue' ? value === 'continue' || value === 'profile_continue' : value === 'create';
}

/** 固定动作优先；已知控件的未知事件不能因文案恰巧匹配而变成创建请求。 */
export function isRegistrationButton(button, action) {
  if (!['continue', 'create'].includes(action) || !button) return false;
  if (button.action) return matchesAction(button.action, action);
  return (action === 'continue' ? CONTINUE_TEXT : CREATE_TEXT).test(button.text || '');
}

/** 仅定位控件；调用方仍负责阶段、批准、提交标记和点击后观察。 */
export async function locateRegistrationButton(frame, action, form) {
  if (!['continue', 'create'].includes(action)) throw new RegistrationError('registration_action_invalid');
  const error = action === 'continue' ? 'continue_not_ready' : 'create_button_not_ready';
  const known = (form?.buttons || []).filter(button => button.action);
  let button;
  if (known.length) {
    const matches = known.filter(candidate => matchesAction(candidate.action, action));
    if (known.length !== 1 || matches.length !== 1) throw new RegistrationError(error);
    const rule = BUTTON_RULES.find(candidate => candidate.action === matches[0].action);
    if (!rule) throw new RegistrationError(error);
    // 所有选择器片段来自固定规则；页面属性、文案或调用参数不拼入选择器。
    button = frame.locator(`button[data-id="${rule.dataId}"][data-click-event="${rule.clickEvent}"]:visible`);
  } else {
    button = frame.getByRole('button', { name: action === 'continue' ? CONTINUE_TEXT : CREATE_TEXT });
  }
  if (await button.count() !== 1) throw new RegistrationError(error);
  return button;
}

/** 登录页的创建入口使用独立标记，不把登录密码页当作注册密码页。 */
export function locateCreateAccountLink(frame, form) {
  if (form === undefined) {
    // 首次导航还没有表单快照；union 对同一控件去重，调用方仍须拒绝多个入口。
    return frame.locator('[data-id="EmailPage-CreateAccountLink"]:visible')
      .or(frame.getByRole('link', { name: CREATE_LINK_TEXT }));
  }
  return (form?.links || []).some(link => link.action === 'create_account')
    ? frame.locator('[data-id="EmailPage-CreateAccountLink"]:visible')
    : frame.getByRole('link', { name: CREATE_LINK_TEXT });
}

export function isSignupPasswordForm(form) {
  return (form?.fields || []).some(field => field.type === 'password' && field.autocomplete === 'new-password')
    && (form?.buttons || []).some(button => button.action === 'continue' || button.action === 'create');
}
