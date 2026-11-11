import { launchPersistentBrowser } from './browser-runtime.mjs';
import { ensureMediaSession, isCompleteMediaSession } from './media-session.mjs';
import { waitForIdentity } from './session-refresh.mjs';
import { randomBytes } from 'node:crypto';
import { chmod, mkdir } from 'node:fs/promises';
import path from 'node:path';
import { isTrustedAdobeURL, RegistrationError } from './core.mjs';
import { NetworkDiagnostics, verificationDiagnosis, hasCaptchaRequirement } from './network-diagnostics.mjs';

const CONTINUE = /^(Continue|Next|继续|下一步)$/i;
const CREATE = /^(Create account|Create an account|Agree and create account|创建帐户|创建账户|同意并创建帐户|同意并创建账户)$/i;
const wait = ms => new Promise(resolve => setTimeout(resolve, ms));

/** Adobe 用按钮覆盖原生 select；操作关联的可见列表，再回读实际表单值。 */
export async function selectProfileOption(frame, field, option) {
  if (await field.inputValue() === option.value) return;
  const buttonId = await field.evaluate(element => {
    let parent = element.parentElement;
    for (let depth = 0; parent && depth < 4; depth++, parent = parent.parentElement) {
      const buttons = parent.querySelectorAll('button[aria-haspopup="listbox"]');
      if (buttons.length === 1 && parent.querySelectorAll('select').length === 1) return buttons[0].id;
    }
    return null;
  });
  if (buttonId === null) {
    await field.selectOption(option.value);
  } else {
    if (!buttonId) throw new RegistrationError('profile_dropdown_unrecognized');
    const button = frame.locator(`[id=${JSON.stringify(buttonId)}]`);
    if (await button.count() !== 1) throw new RegistrationError('profile_dropdown_unrecognized');
    if (await button.getAttribute('aria-expanded') !== 'true') await button.click();
    const listId = await button.getAttribute('aria-controls');
    if (!listId) throw new RegistrationError('profile_dropdown_unrecognized');
    const list = frame.getByRole('listbox').and(frame.locator(`[id=${JSON.stringify(listId)}]`));
    await list.waitFor({ state: 'visible', timeout: 5000 });
    const choice = list.getByRole('option', { name: option.label, exact: true });
    if (await choice.count() === 0) {
      // 国家列表按可视区域渲染；只滚动关联列表，不改写 select 或触发提交。
      const metrics = await list.evaluate(element => ({ height: element.clientHeight, total: element.scrollHeight }));
      if (metrics.height <= 0 || metrics.total <= metrics.height) throw new RegistrationError('profile_option_not_rendered');
      const increment = Math.max(1, Math.floor(metrics.height * 0.75));
      const end = Math.max(0, metrics.total - metrics.height);
      const positions = Array.from({ length: Math.min(80, Math.ceil(end / increment) + 1) }, (_, index) => Math.min(index * increment, end));
      for (const top of positions) {
        await list.evaluate((element, offset) => { element.scrollTop = offset; }, top);
        try { await choice.waitFor({ state: 'visible', timeout: 200 }); break; }
        catch (error) { if (error.name !== 'TimeoutError') throw error; }
      }
    }
    await choice.click({ timeout: 5000 });
  }
  const deadline = Date.now() + 3000;
  while (await field.inputValue() !== option.value) {
    if (Date.now() >= deadline) throw new RegistrationError('profile_selection_not_applied');
    await wait(50);
  }
}

export function classifyForm(form) {
  const text = `${form.headings.join(' ')} ${form.notices.join(' ')}`;
  if (form.humanVerification?.required === true) return 'needs_manual';
  if (/captcha|verify you are human|prove you.re human|人机验证|证明您是|验证您是人/i.test(text)) return 'needs_manual';
  if (/not available in your (country|region)|不适用于您所在|所在地区.*不可用/i.test(text)) return 'needs_manual';
  if (form.fields.some(f => /one-time-code/i.test(f.autocomplete) || /verification.code|验证码/i.test(f.label))) return 'awaiting_verification';
  if (/verify your email|check your (email|inbox)|verification code|验证您的电子邮件|查看您的电子邮件|验证码/i.test(text)) return 'awaiting_verification';
  if (form.fields.some(f => /first.?name|last.?name|birth|country|region|名字|姓氏|出生|国家|地区|月份|年份/i.test(`${f.name} ${f.id} ${f.label}`))) return 'awaiting_profile';
  if (form.fields.some(f => f.type === 'password')) return 'awaiting_password';
  if (form.fields.some(f => f.type === 'email' || /email|电子邮件|邮箱/i.test(`${f.name} ${f.label}`))) return 'awaiting_email';
  if (form.buttons.some(b => CREATE.test(b.text))) return 'awaiting_approval';
  return 'needs_manual';
}

/** 仅依据可见挑战和表单错误，不用整页导航文案推断注册结果。 */
export function formBlockingCode(form) {
  const text = `${form.headings.join(' ')} ${form.notices.join(' ')}`;
  if (form.humanVerification?.required === true || /captcha|verify you are human|prove you.re human|人机验证|证明您是|验证您是人/i.test(text)) return 'human_verification_required';
  if (/not available in your (country|region)|不适用于您所在|所在地区.*不可用/i.test(text)) return 'region_unavailable';
  const errors = form.notices.join(' ');
  if (/too many|rate.?limit|try again later|请求过于频繁|稍后再试/i.test(errors)) return 'upstream_rate_limited';
  if (/email.{0,60}(already in use|already registered)|account.{0,40}already exists|邮箱.{0,20}(已注册|已使用)|帐户已存在|账户已存在/i.test(errors)) return 'account_already_exists';
  if (form.fields.some(field => field.invalid) || /\b(invalid|incorrect|error|failed|unable|cannot)\b|无效|不正确|错误|失败|无法|不能/i.test(errors)) return 'form_rejected';
  return null;
}

/** 等待后置条件；只读取状态，超时或导航不确定时绝不重放提交。 */
export async function waitForFormStage(readForm, {
  fromStage,
  accept = stage => stage !== 'needs_manual' && stage !== fromStage,
  timeoutMs = 30_000,
  pollIntervalMs = 250,
  now = Date.now,
  pause = wait,
} = {}) {
  const deadline = now() + timeoutMs;
  while (true) {
    try {
      const form = await readForm();
      const code = formBlockingCode(form);
      if (code) return { form, stage: 'needs_manual', code };
      const stage = classifyForm(form);
      if (await accept(stage, form)) return { form, stage, code: null };
    } catch (error) {
      // 导航期间的 DOM 替换可以继续观察，关闭页面及未知错误必须交回调用方。
      if (!/Execution context was destroyed|Frame was detached|Cannot find context with specified id/i.test(String(error.message))) throw error;
    }
    const remaining = deadline - now();
    if (remaining <= 0) throw new RegistrationError('stage_transition_timeout');
    await pause(Math.min(pollIntervalMs, remaining));
  }
}

/** 沿用 CFM 的单次会话与提交前落盘规则；仅操作 Adobe 官方页面。 */
export class BrowserRegistration {
  constructor(store, { headless = false, onEvent = () => {} } = {}, { launchBrowser = launchPersistentBrowser } = {}) {
    this.store = store;
    this.headless = headless;
    this.onEvent = onEvent;
    this.launchBrowser = launchBrowser;
    this.context = null;
    this.page = null;
    this.network = [];
  }

  async waitForStage(options = {}) {
    return waitForFormStage(() => this.form(), { ...this.stageWaitOptions, ...options });
  }

  async open() {
    const state = await this.store.load();
    const profileDir = path.join(this.store.runDir, 'browser');
    await mkdir(profileDir, { mode: 0o700, recursive: true });
    await chmod(profileDir, 0o700);
    this.context = await this.launchBrowser(profileDir, { headless: this.headless });
    this.networkDiagnostics ??= new NetworkDiagnostics();
    this.networkDiagnostics.attach(this.context);
    this.page = this.context.pages()[0] || await this.context.newPage();
    if (state.emailSubmitted || state.passwordSubmitted || state.submission.started) {
      const restored = this.context.pages().filter(p => isTrustedAdobeURL(p.url()));
      if (restored.length) {
        this.page = restored.at(-1);
        return this.status();
      }
    }
    await this.page.goto('https://account.adobe.com/', { waitUntil: 'domcontentloaded', timeout: 45000 });
    if (state.submission.started) return this.status();
    const createLink = this.page.getByRole('link', { name: /^(Create an account|创建[帐账]户)$/i });
    try {
      try { await createLink.waitFor({state:'visible',timeout:20000}); }
      catch {
        const blockedFont = new URL(this.page.url()).hostname === 'auth.services.adobe.com' && await this.page.evaluate(() =>
          document.readyState === 'loading' && !!document.querySelector('link[rel="stylesheet"][href="https://use.typekit.net/znu8cfp.css"]') &&
          !performance.getEntriesByName('https://use.typekit.net/znu8cfp.css').length,
        );
        if (!blockedFont) throw new RegistrationError('login_ui_not_ready');
        await this.page.route('https://use.typekit.net/znu8cfp.css', route=>route.abort('failed'));
        await this.page.reload({waitUntil:'domcontentloaded',timeout:30000});
        await createLink.waitFor({state:'visible',timeout:20000});
        this.onEvent({code:'optional_font_load_skipped'});
      }
      await createLink.click();
      await this.page.locator('input[autocomplete="new-password"]').waitFor({state:'visible',timeout:20000});
      const outcome = await this.waitForStage();
      await this.store.setStage(outcome.code ? 'needs_manual' : 'form_ready', outcome.code);
    } catch {
      await this.store.setStage('needs_manual', 'login_ui_not_ready');
    }
    return this.status();
  }

  async authFrame() {
    // 显式打开 Firefly 后固定当前页；热更新前的实例缺省保持原跨页查找行为。
    const candidates = this.focusPageOnly === true ? [this.page] : [...this.context.pages()].reverse();
    for (const candidate of candidates) {
      if (!isTrustedAdobeURL(candidate.url())) continue;
      if (new URL(candidate.url()).hostname === 'auth.services.adobe.com') {
        this.page = candidate;
        return candidate.mainFrame();
      }
      for (const frame of [...candidate.frames()].reverse()) {
        if (!isTrustedAdobeURL(frame.url())) continue;
        if (await frame.locator('input:visible,select:visible').count()) {
          this.page = candidate;
          return frame;
        }
      }
    }
    if (!isTrustedAdobeURL(this.page.url())) throw new RegistrationError('untrusted_page');
    return this.page.mainFrame();
  }

  async form() {
    const frame = await this.authFrame();
    const humanVerification = await this.detectHumanVerification();
    // Adobe 登录页自身也使用 Shadow DOM；由 Playwright 穿透，不能在 body 上 querySelector。
    const [fields, headings, notices, buttons, agreements, fieldErrors] = await Promise.all([
      frame.locator('input:visible,select:visible').evaluateAll(elements => elements.map(e => ({
        tag: e.tagName.toLowerCase(), type: e.type || '', id: e.id, name: e.name || '',
        label: (e.getAttribute('aria-label') || [...(e.labels || [])].map(l => {const copy=l.cloneNode(true);copy.querySelectorAll('select,input,option').forEach(n=>n.remove());return copy.textContent.trim();}).join(' ') || e.placeholder || '').slice(0,200),
        autocomplete: e.autocomplete || '', filled: !!e.value, invalid: e.getAttribute('aria-invalid') === 'true',
        ...(e.tagName === 'SELECT' && e.name === 'month' ? {options:[...e.options].map(o=>({value:o.value,label:o.label}))}:{}),
      }))),
      frame.locator('h1:visible,h2:visible,h3:visible').evaluateAll(elements => elements.map(e => e.textContent.trim().slice(0,200))),
      frame.locator('[role="alert"]:visible,.error:visible,.alert:visible').evaluateAll(elements => elements.map(e => e.textContent.trim().slice(0,300))),
      frame.locator('button:visible,input[type="submit"]:visible').evaluateAll(elements => elements.map(e => ({text:(e.textContent || e.value || e.getAttribute('aria-label') || '').trim().slice(0,200),disabled:!!e.disabled}))),
      frame.locator('p:visible,span:visible,small:visible').evaluateAll(elements => elements.map(e => e.textContent.trim()).filter(t => /by (continuing|clicking).{0,80}(agree|accept)|继续即.{0,30}同意/is.test(t)).map(t => t.slice(0,300)).slice(0,5)),
      // 错误可能仅由输入框的 ARIA 属性关联，位于 Shadow DOM 中且没有 .error 类。
      frame.locator('input[aria-invalid="true"]:visible,select[aria-invalid="true"]:visible').evaluateAll(elements => elements.flatMap(element => {
        const ids = `${element.getAttribute('aria-describedby') || ''} ${element.getAttribute('aria-errormessage') || ''}`.trim().split(/\s+/);
        const root = element.getRootNode();
        return ids.filter(Boolean).map(id => root.getElementById?.(id) || element.ownerDocument.getElementById(id))
          .filter(node => node && node.getClientRects().length)
          .map(node => node.textContent.trim().slice(0,300)).filter(Boolean);
      })),
    ]);
    notices.push(...fieldErrors.filter(message => !notices.includes(message)));
    // 仅添加固定提示，不读取或输出挑战题目、令牌及解答。
    if (humanVerification.required) notices.unshift('人机验证需要手动完成');
    const result = {fields,headings,notices,buttons,agreements,humanVerification};
    const url = new URL(frame.url());
    const state = await this.store.load();
    // 页面错误偶尔包含邮箱；输出只保留表单结构和经脱敏的提示。
    let serialized = JSON.stringify(result).replaceAll(state.email, '[email]');
    if (state.config.password) serialized = serialized.replaceAll(state.config.password, '[password]');
    return { ...JSON.parse(serialized), host: url.hostname, path: url.pathname };
  }

  async detectHumanVerification() {
    await this.networkStatus();
    const captchaRequired = hasCaptchaRequirement(this.network);
    const instruction = /verify you are human|prove you(?:'|’)re human|not a robot|start puzzle|人机验证|验证您是人|证明您是|开始拼图|完成拼图/i;
    const provider = /(?:^|\.)(?:arkoselabs\.com|funcaptcha\.com|hcaptcha\.com|recaptcha\.net)$|^challenges\.cloudflare\.com$/i;
    // 仅检查当前表单所属页面，避免旧注册标签页残留的验证框遮盖新页面状态。
    for (const page of this.page ? [this.page] : []) {
      if (!isTrustedAdobeURL(page.url())) continue;
      // page.frames() 包含所有层级；逐层核对宿主 iframe，排除隐藏的预加载验证框。
      for (const frame of page.frames()) {
        try {
          let visible = true;
          for (let nested = frame; nested.parentFrame(); nested = nested.parentFrame()) {
            const element = await nested.frameElement();
            try {
              const box = await element.boundingBox();
              if (!await element.isVisible() || !box || box.width < 120 || box.height < 60) visible = false;
            } finally { await element.dispose(); }
            if (!visible) break;
          }
          if (!visible) continue;
          const url = new URL(frame.url());
          const knownProvider = url.hostname === 'arks-client.adobe.com' || provider.test(url.hostname) ||
            (/(?:^|\.)google\.com$/i.test(url.hostname) && /^\/recaptcha\//.test(url.pathname));
          if (frame.parentFrame() && knownProvider) {
            return { required: true, code: 'human_verification_required', source: 'visible_challenge_frame' };
          }
          // 只查询固定人工验证文案是否可见，不读取页面正文或挑战交互控件的值。
          const matches = await frame.getByText(instruction).all();
          for (const match of matches.slice(0, 10)) {
            if (await match.isVisible()) return { required: true, code: 'human_verification_required', source: 'visible_instruction' };
          }
        } catch {
          // 页面导航或 iframe 被移除时，在下一次只读状态查询中重新判定。
        }
      }
    }
    return captchaRequired
      ? { required: true, code: 'captcha_required', source: 'upstream_captcha_response' }
      : { required: false };
  }

  async networkStatus() {
    // 热更新已有实例也能回读当前页保留的请求；不导航、不发起任何额外请求。
    this.networkDiagnostics ??= new NetworkDiagnostics();
    this.networkDiagnostics.attach(this.context);
    await this.networkDiagnostics.capture(this.page);
    this.network = this.networkDiagnostics.snapshot(this.page);
    return verificationDiagnosis(this.network);
  }

  async status() {
    const state = await this.store.load();
    const form = await this.form();
    const networkDiagnosis = await this.networkStatus();
    const humanVerificationRequired = form.humanVerification?.required === true;
    const detected = humanVerificationRequired ? 'needs_manual' : classifyForm(form);
    const screenshot = this.workerMode ? undefined : path.join(this.store.runDir, 'current.png');
    if (!this.workerMode) await this.page.screenshot({ path: screenshot, timeout: 10000 }).then(() => chmod(screenshot, 0o600)).catch(() => {});
    const pages = this.context.pages().map(page => {const u = new URL(page.url() || 'about:blank'); return {host:u.hostname,path:u.pathname};});
    return { stage: humanVerificationRequired || networkDiagnosis.code ? 'needs_manual' : state.stage,
      code: networkDiagnosis.code || (humanVerificationRequired ? 'human_verification_required' : state.code),
      recordedStage: state.stage, detected, submissionStarted: state.submission.started,
      form, pages, network:this.network, networkDiagnosis, screenshot };
  }

  async field(frame, descriptor) {
    const visible = frame.locator('input:visible,select:visible');
    const items = await visible.all();
    for (const element of items) {
      const same = await element.evaluate((e, d) => e.tagName.toLowerCase() === d.tag && e.type === d.type && e.id === d.id && (e.name || '') === d.name, descriptor);
      if (same) return element;
    }
    throw new RegistrationError('field_changed');
  }

  async clickContinue(frame, beforeClick, fromStage) {
    const button = frame.getByRole('button', { name: CONTINUE });
    await button.waitFor({state:'visible',timeout:10000});
    if (await button.count() !== 1) throw new RegistrationError('continue_not_ready');
    // Playwright 的 trial 只等待可交互，不产生表单点击。
    await button.click({ trial: true, timeout: 10000 });
    await beforeClick?.();
    try {
      await button.click({ timeout: 15000 });
      return await this.waitForStage({ fromStage });
    } catch {
      await this.store.setStage('needs_manual', 'continuation_result_unknown');
      throw new RegistrationError('continuation_result_unknown');
    }
  }

  async advance(input = {}) {
    let state = await this.store.load();
    if (input.action === 'challenge_ui') {
      if (input.approved !== true || !isTrustedAdobeURL(this.page.url()) ||
          (await this.detectHumanVerification()).required !== true) throw new RegistrationError('challenge_not_ready');
      const allowedButtons = ['Start puzzle', 'Navigate to previous image', 'Navigate to next image', 'Reload Challenge', 'Submit', 'Audio', 'Restart', 'Play', 'Done', 'Visual'];
      const visibleChallengeFrame = async frame => {
        try {
          if (!frame || frame === this.page.mainFrame()) return false;
          const url = new URL(frame.url());
          if (url.protocol !== 'https:' || url.hostname !== 'arks-client.adobe.com' ||
              url.username || url.password || url.port) return false;
          let nested = frame;
          let depth = 0;
          while (nested.parentFrame()) {
            if (++depth > 16) return false;
            const element = await nested.frameElement();
            try {
              const box = await element.boundingBox();
              if (!await element.isVisible() || !box || box.width < 120 || box.height < 60) return false;
            } finally { await element.dispose(); }
            nested = nested.parentFrame();
          }
          return nested === this.page.mainFrame();
        } catch { return false; }
      };
      const frames = this.page.frames();
      if (input.button !== undefined) {
        if (!Number.isInteger(input.frame) || !frames[input.frame] || !allowedButtons.includes(input.button)) throw new RegistrationError('challenge_control_invalid');
        const frame = frames[input.frame];
        if (!await visibleChallengeFrame(frame)) throw new RegistrationError('challenge_frame_unrecognized');
        const button = frame.getByRole('button', {name:input.button,exact:true});
        if (await button.count() !== 1 || !await button.isVisible() || !await button.isEnabled()) throw new RegistrationError('challenge_control_not_ready');
        if (!await visibleChallengeFrame(frame)) throw new RegistrationError('challenge_frame_unrecognized');
        await button.click({timeout:10000});
        await wait(input.button === 'Submit' ? 2000 : 700);
      }
      const controls = [];
      for (const [index,frame] of this.page.frames().entries()) {
        if (!await visibleChallengeFrame(frame)) continue;
        const buttons = await frame.locator('button:visible,[role="button"]:visible').evaluateAll((elements,allowed)=>elements.map(e=>({text:(e.getAttribute('aria-label') || e.textContent || '').trim(),disabled:!!e.disabled})).filter(button=>allowed.includes(button.text)),allowedButtons);
        if (buttons.length) controls.push({frame:index,host:'arks-client.adobe.com',buttons});
      }
      return {controls,...await this.status()};
    }
    if (input.action === 'open_firefly') {
      this.page = await this.context.newPage();
      this.focusPageOnly = true;
      await this.page.goto('https://firefly.adobe.com/',{waitUntil:'domcontentloaded',timeout:45000});
      const outcome = await this.waitForStage({ accept: async (stage, form) => {
        if (['awaiting_email', 'awaiting_password', 'awaiting_verification'].includes(stage)) return true;
        if (form.host !== 'firefly.adobe.com') return false;
        // Firefly 初始 HTML 可先于 IMS 和登录跳转完成；只等就绪，不在这里读取身份凭据。
        return this.page.evaluate(async () => {
          const ims = window.adobeIMS;
          if (!ims || typeof ims.isSignedInUser !== 'function' ||
              typeof ims.getProfile !== 'function' || typeof ims.getAccessToken !== 'function') return false;
          try { return await ims.isSignedInUser() === true; } catch { return false; }
        });
      } });
      if (outcome.code) await this.store.setStage('needs_manual', outcome.code);
      return this.status();
    }
    if (state.submission.started) throw new RegistrationError('submission_already_started');
    if (input.action === 'retry_font_load') {
      if (new URL(this.page.url()).hostname !== 'auth.services.adobe.com') throw new RegistrationError('diagnostic_page_mismatch');
      // 仅跳过已观察到阻塞解析的可选字体 CSS；不拦截身份、验证码或验证脚本。
      await this.page.route('https://use.typekit.net/znu8cfp.css', route => route.abort('failed'));
      await this.page.reload({waitUntil:'domcontentloaded',timeout:45000});
      await this.page.locator('input:visible').first().waitFor({timeout:20000}).catch(() => {});
      return this.status();
    }
    if (input.action === 'diagnose') {
      const frame = await this.authFrame();
      const document = await frame.evaluate(() => ({
        readyState:document.readyState,
        htmlLength:document.documentElement.outerHTML.length,
        links:[...document.querySelectorAll('link[href]')].map(s=>{const u=new URL(s.href);return {rel:s.rel,host:u.hostname,path:u.pathname};}),
        scripts:[...document.scripts].filter(s=>s.src).map(s=>{const u=new URL(s.src);return {host:u.hostname,path:u.pathname};}),
        resources:performance.getEntriesByType('resource').map(r=>{const u=new URL(r.name);return {host:u.hostname,path:u.pathname,status:r.responseStatus,type:r.initiatorType,duration:Math.round(r.duration)};}).slice(-30),
      }));
      const cdp = await this.context.newCDPSession(this.page);
      const errors = [];
      const clean = value => String(value).replaceAll(state.email,'[email]').replace(/https?:\/\/\S+/g,'[url]').slice(0,350);
      cdp.on('Runtime.exceptionThrown',event=>errors.push(clean(event.exceptionDetails.exception?.description || event.exceptionDetails.text)));
      cdp.on('Runtime.consoleAPICalled',event=>{if(event.type==='error') errors.push(clean(event.args.map(a=>a.value || a.description || '').join(' ')));});
      await cdp.send('Runtime.enable');
      await wait(100);
      await cdp.detach();
      return {diagnostics:document,errors:errors.slice(-10),network:this.network};
    }
    if (input.action === 'open_account') {
      this.page = await this.context.newPage();
      await this.page.goto('https://account.adobe.com/', {waitUntil:'domcontentloaded',timeout:45000});
      const entry = this.page.getByRole('link',{name:/^(Create an account|创建帐户|创建账户)$/i});
      await entry.waitFor({state:'visible',timeout:45000}).catch(() => {});
      if (await entry.isVisible()) await entry.click();
      return this.status();
    }
    const frame = await this.authFrame();
    const form = await this.form();
    const kind = classifyForm(form);
    const blocked = formBlockingCode(form);
    if (blocked) {
      await this.store.setStage('needs_manual', blocked);
      return this.status();
    }
    const createLink = frame.getByRole('link',{name:/^(Create an account|创建[帐账]户)$/i});
    if (kind === 'awaiting_email' && /^(Sign in|登录)$/i.test(form.headings.join(' ').trim()) && await createLink.count() === 1) {
      await createLink.click();
      const outcome = await this.waitForStage({ fromStage: 'awaiting_email' });
      await this.store.setStage(outcome.code ? 'needs_manual' : 'form_ready', outcome.code);
      return this.status();
    }
    const text = [...form.headings,...form.notices,...form.agreements].join(' ');
    let outcome;
    if (kind === 'awaiting_email') {
      if (state.emailSubmitted) throw new RegistrationError('email_already_submitted');
      if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(state.email)) throw new RegistrationError('email_required');
      // 邮箱继续若同时代表同意条款，也必须留在当前页面等待确认。
      if (/by (continuing|clicking).{0,80}(agree|accept)|继续即.{0,30}同意/is.test(text)) {
        await this.store.setStage('needs_manual', 'early_agreement_requires_manual');
        return this.status();
      }
      const descriptor = form.fields.find(f => f.type === 'email' || /email|电子邮件|邮箱/i.test(`${f.name} ${f.label}`));
      await (await this.field(frame, descriptor)).fill(state.email);
      outcome = await this.clickContinue(frame, () => this.store.save({ emailSubmitted: true, emailSubmittedAt: new Date().toISOString(), stage: 'awaiting_email' }), kind);
    } else if (kind === 'awaiting_password') {
      // 不把登录已有账户或修改密码当作新账号注册。
      if (!/create.{0,20}account|sign up|创建.{0,3}[帐账]户|注册/i.test(form.headings.join(' '))) {
        await this.store.setStage('needs_manual', 'existing_account_or_password_required');
        return this.status();
      }
      if (state.passwordSubmitted) throw new RegistrationError('password_already_submitted');
      const password = state.config.password || `${randomBytes(24).toString('base64url')}aA9!`;
      state = await this.store.save({ config: { ...state.config, password } });
      for (const descriptor of form.fields.filter(f => f.type === 'email' || f.type === 'password')) {
        await (await this.field(frame, descriptor)).fill(descriptor.type === 'password' ? password : state.email);
      }
      if (form.buttons.some(b => CREATE.test(b.text))) {
        await this.store.setStage('awaiting_approval', 'account_agreement_required');
        return this.status();
      }
      if (form.agreements.length) {
        await this.store.setStage('needs_manual', 'early_agreement_requires_manual');
        return this.status();
      }
      outcome = await this.clickContinue(frame, () => this.store.save({ passwordSubmitted: true, stage: 'awaiting_password' }), kind);
    } else if (kind === 'awaiting_profile') {
      const profile = input.profile || state.config.profile;
      if (!profile) { await this.store.setStage('awaiting_profile', 'profile_required'); return this.status(); }
      const rules = [
        ['firstName', /first.?name|名字|名$/i], ['lastName', /last.?name|姓氏|^姓$/i],
        ['birthMonth', /month|月份|^月$/i], ['birthYear', /year|年份|^年$/i], ['country', /country|region|国家|地区/i],
      ];
      // Adobe currently asks only for month/year in some regions. Keep the
      // generated day in the saved profile and fill it only when requested.
      const dayPattern = /birth.?day|(^|\s)day(\s|$)|日期|^日$/i;
      const dayDescriptor = form.fields.find(f => {
        const text = `${f.name} ${f.id} ${f.label}`;
        return dayPattern.test(text) && !/month|year|月|年/i.test(text);
      });
      if (dayDescriptor) {
        const day = Number(profile.birthDay);
        const maxDay = new Date(Date.UTC(Number(profile.birthYear), Number(profile.birthMonth), 0)).getUTCDate();
        if (!Number.isInteger(day) || day < 1 || day > maxDay) throw new RegistrationError('profile_invalid');
        rules.push(['birthDay', dayPattern, dayDescriptor]);
      }
      if (!profile.firstName || !profile.lastName || !profile.birthMonth || !profile.birthYear || !profile.country) throw new RegistrationError('profile_incomplete');
      if (![profile.firstName,profile.lastName,profile.country].every(v=>typeof v==='string' && v.trim() && v.length<=120) || !/^\d{4}$/.test(String(profile.birthYear))) throw new RegistrationError('profile_invalid');
      await this.store.save({ config: { ...state.config, profile } });
      for (const [key, pattern, providedDescriptor] of rules) {
        const descriptor = providedDescriptor || form.fields.find(f => pattern.test(`${f.name} ${f.id} ${f.label}`));
        if (!descriptor) throw new RegistrationError('profile_field_unrecognized');
        const field = await this.field(frame, descriptor);
        let value = String(profile[key]);
        if (descriptor.tag === 'select') {
          const options = await field.locator('option').evaluateAll(options => options.map(o => ({value:o.value,label:o.label})));
          if (key === 'birthMonth' && /^\d+$/.test(value)) {
            const month = Number(value);
            if (month<1 || month>12) throw new RegistrationError('profile_option_invalid');
            if (options.some(o=>o.value==='0') && options.some(o=>o.value==='11') && !options.some(o=>o.value==='12')) value=String(month-1);
          }
          const match = options.find(o => o.value === value || o.label === value);
          if (!match) throw new RegistrationError('profile_option_invalid');
          await selectProfileOption(frame, field, match);
        } else await field.fill(value);
      }
      const ready = await this.waitForStage({ accept: (_stage, current) => current.buttons.some(button => CREATE.test(button.text) && !button.disabled) });
      await this.store.setStage(ready.code ? 'needs_manual' : 'awaiting_approval', ready.code || 'account_agreement_required');
      return this.status();
    } else {
      await this.store.setStage(kind, kind === 'needs_manual' ? 'unrecognized_page' : null);
      return this.status();
    }
    await this.store.setStage(outcome.stage, outcome.code);
    return this.status();
  }

  async approveCreate() {
    const state = await this.store.load();
    if (state.submission.started) throw new RegistrationError('submission_already_started');
    if (state.stage !== 'awaiting_approval') throw new RegistrationError('approval_not_ready');
    const frame = await this.authFrame();
    const button = frame.getByRole('button', { name: CREATE });
    if (await button.count() !== 1 || !await button.isEnabled()) throw new RegistrationError('create_button_not_ready');
    await this.store.markSubmissionStarted();
    try {
      await button.click({ timeout: 15000 });
      const outcome = await this.waitForStage({ accept: (stage, form) => stage === 'awaiting_verification' ||
        form.host === 'firefly.adobe.com' || form.host === 'account.adobe.com' });
      // 账户欢迎页证明页面已跳转，不证明 Firefly 身份或会话可用；成功仍由 exportSession 核对。
      const code = outcome.form.host === 'account.adobe.com' ? 'firefly_identity_required' : 'creation_needs_verification';
      await this.store.setStage(outcome.stage === 'awaiting_verification' ? outcome.stage : 'needs_manual', outcome.code || code);
      return this.status();
    } catch {
      await this.store.markOutcomeUnknown();
      throw new RegistrationError('submission_result_unknown');
    }
  }

  async verify(input = {}) {
    const state = await this.store.load();
    let actionStarted = false;
    try {
      let applyVerification;
      if (input.verificationURL) {
        if (!isTrustedAdobeURL(input.verificationURL)) throw new RegistrationError('untrusted_verification_link');
        if (state.verificationSubmitted) throw new RegistrationError('verification_already_submitted');
        await this.store.save({ verificationSubmitted: true, stage: 'verification' });
        actionStarted = true;
        applyVerification = () => this.page.goto(input.verificationURL, { waitUntil: 'domcontentloaded', timeout: 45000 });
      } else {
        if (!/^\d{6}$/.test(String(input.code || ''))) throw new RegistrationError('verification_required');
        const frame = await this.authFrame();
        const form = await this.form();
        if (classifyForm(form) !== 'awaiting_verification') throw new RegistrationError('verification_not_ready');
        if (state.verificationSubmitted) throw new RegistrationError('verification_already_submitted');
        const descriptor = form.fields.find(f => /one-time-code/i.test(f.autocomplete) || /verification.code|验证码/i.test(f.label));
        if (!descriptor) throw new RegistrationError('verification_field_unrecognized');
        // 输入六位码可能即触发校验，先保存单次提交标记。
        await this.store.save({ verificationSubmitted: true, stage: 'verification' });
        actionStarted = true;
        applyVerification = async () => (await this.field(frame, descriptor)).fill(String(input.code));
      }
      // 填完验证码可能立即导航并令 fill 抛错；动作只执行一次，结果交给只读后置观察判断。
      try { await applyVerification(); } catch { /* 不重放输入或导航，也不额外点击 Verify。 */ }
      // Adobe 可在填码时自动提交；只观察后置条件，不能因旧表单尚在而再次点 Verify。
      const outcome = await this.waitForStage({
        fromStage: 'awaiting_verification',
        accept: async (stage, form) => {
          if (stage === 'awaiting_verification') return false;
          if (form.host === 'firefly.adobe.com') {
            return this.page.evaluate(async expectedEmail => {
              const ims = window.adobeIMS;
              if (!ims || typeof ims.isSignedInUser !== 'function' ||
                  typeof ims.getProfile !== 'function' || typeof ims.getAccessToken !== 'function') return false;
              try {
                if (!await ims.isSignedInUser()) return false;
                const profile = await ims.getProfile();
                return typeof profile?.email === 'string' && profile.email.trim().toLowerCase() === expectedEmail &&
                  typeof profile.userId === 'string' && !!profile.userId.trim() && profile.emailVerified !== false;
              } catch { return false; }
            }, state.email.trim().toLowerCase());
          }
          return stage !== 'needs_manual' || form.host === 'account.adobe.com';
        },
      });
      if (outcome.code) await this.store.setStage('needs_manual', outcome.code);
    } catch (error) {
      if (!actionStarted) throw error;
      await this.store.setStage('needs_manual', 'verification_result_unknown');
      throw new RegistrationError('verification_result_unknown');
    }
    return this.status();
  }

  async exportSession() {
    const state = await this.store.load();
    if (!state.submission.started) throw new RegistrationError('registration_not_submitted');
    const nonempty = value => typeof value === 'string' && value.trim().length > 0;
    const matchingIdentity = account => account && nonempty(account.email) &&
      account.email.trim().toLowerCase() === state.email.trim().toLowerCase() &&
      nonempty(account.account_id) && nonempty(account.token);
    let result = state.result;
    if (result?.account) {
      // 恢复只复用已核对的原始快照，避免新 Token/Cookie 改变已落地文件的字节。
      if (result.fireflySessionVerified !== true || !matchingIdentity(result.account) || (!nonempty(result.account.cookie) && !isCompleteMediaSession(result.account))) {
        throw new RegistrationError('saved_identity_not_verified');
      }
      if (result.emailVerified === false) throw new RegistrationError('email_verification_required');
    } else {
      if (state.stage === 'completed') throw new RegistrationError('saved_identity_not_verified');
      const pageURL = this.page?.url();
      if (!isTrustedAdobeURL(pageURL) || new URL(pageURL).hostname !== 'firefly.adobe.com') {
        throw new RegistrationError('firefly_session_not_ready');
      }
      let identity = this.workerMode ? await waitForIdentity(this.page,{timeoutMs:10000}) : await this.page.evaluate(async () => {
        const ims = window.adobeIMS;
        if (!ims || typeof ims.isSignedInUser !== 'function' || !await ims.isSignedInUser() ||
            typeof ims.getProfile !== 'function' || typeof ims.getAccessToken !== 'function') return null;
        const profile = await ims.getProfile();
        const credential = await ims.getAccessToken();
        if (!profile?.email || !profile?.userId || !credential?.token) return null;
        return {
          email: profile.email, account_id: profile.userId, token: credential.token, user_agent: typeof navigator === 'object' ? navigator.userAgent : '',
          emailVerified: typeof profile.emailVerified === 'boolean' ? profile.emailVerified : null,
        };
      });
      if (!matchingIdentity(identity)) throw new RegistrationError('identity_not_verified');
      if (identity.emailVerified === false) throw new RegistrationError('email_verification_required');
      let media, cookie;
      if (this.workerMode) {
        const expectedAccountID = identity.account_id;
        media = await ensureMediaSession(this.page,this.context);
        if (!isCompleteMediaSession(media)) throw new RegistrationError('media_session_incomplete');
        identity = await waitForIdentity(this.page,{timeoutMs:10000});
        if (!matchingIdentity(identity) || identity.account_id !== expectedAccountID) throw new RegistrationError('identity_not_verified');
        if (identity.emailVerified === false) throw new RegistrationError('email_verification_required');
        cookie = media.cookie;
      } else {
        const cookies = await this.context.cookies('https://firefly.adobe.com/');
        cookie = cookies.filter(c => nonempty(c.name) && typeof c.value === 'string').map(c => `${c.name}=${c.value}`).join('; ');
        if (!cookie) throw new RegistrationError('firefly_cookie_missing');
      }
      const refreshCookies = await this.context.cookies("https://adobeid-na1.services.adobe.com/ims/check/v6/token");
      const refresh_cookie = refreshCookies.map(c => `${c.name}=${c.value}`).join("; ");
      const account = { email: identity.email, account_id: identity.account_id, token: identity.token, cookie, refresh_cookie, user_agent:identity.user_agent,
        ...(media?{media_session_arp_mode:media.media_session_arp_mode,media_session_arp_source:media.media_session_arp_source}:{}) };
      result = { ...result, account, fireflySessionVerified: true, emailVerified: identity.emailVerified, generationVerified: false };
      await this.store.save({ stage: 'credentials_saved', result });
    }
    let destination;
    if (!this.workerMode) try {
      // 独立模块URL兼容运行中的旧core缓存；热更新不重建浏览器或重放注册。
      const { writePrivateJSONOnce } = await import('./core.mjs?private-export-v1');
      destination = await writePrivateJSONOnce(this.store.runDir, 'adobe2api-import.json', { accounts: [result.account] });
    } catch (error) {
      const allowed = new Set(['private_file_conflict', 'private_file_too_large', 'invalid_private_filename', 'unsafe_storage', 'invalid_state', 'storage_error']);
      throw new RegistrationError(allowed.has(error?.code) ? error.code : 'private_export_failed');
    }
    if (state.stage !== 'completed') {
      await this.store.save({ stage: 'completed', result: { ...result, ...(destination?{credentialFile: destination}:{}), exportComplete: true } });
    }
    return {
      stage: 'completed', credentialFile: destination, fireflySessionVerified: true,
      emailVerified: result.emailVerified === true ? true : null, generationVerified: false,
    };
  }

  async close() {
    if (this.context) await this.context.close();
    this.context = null;
  }
}
