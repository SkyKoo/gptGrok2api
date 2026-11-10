import { RegistrationError } from './core.mjs';

const LAUNCH_TIMEOUT_MS = 30_000;

/** 显式代理配置，不继承宿主环境；去掉 URL 根斜线，凭据与 server 分开。 */
export function browserProxy(raw) {
  if (!raw) return undefined;
  try {
    if (typeof raw !== 'string' || /[\r\n\0]/.test(raw)) throw new Error();
    const url = new URL(raw);
    if (!['http:', 'https:'].includes(url.protocol) || !url.hostname ||
        !['', '/'].includes(url.pathname) || url.search || url.hash) throw new Error();
    return { server: url.origin,
      ...(url.username ? { username: decodeURIComponent(url.username) } : {}),
      ...(url.password ? { password: decodeURIComponent(url.password) } : {}),
    };
  } catch { throw new RegistrationError('browser_proxy_invalid'); }
}

/** 同一任务始终使用磁盘 profile；Docker 仅替换浏览器来源，不覆盖网页环境。 */
export async function launchPersistentBrowser(profileDir, { headless = false } = {}, dependencies = {}) {
  const env = dependencies.env ?? process.env;
  const importer = dependencies.importer ?? (specifier => import(specifier));
  const backend = env.ADOBE_BROWSER_BACKEND || 'playwright';
  if (!['playwright', 'cloakbrowser'].includes(backend)) {
    throw new RegistrationError('browser_backend_invalid');
  }
  const channel = env.ADOBE_BROWSER_CHANNEL || 'chrome';
  if (backend === 'playwright' && !['chrome', 'chromium'].includes(channel)) {
    throw new RegistrationError('browser_channel_invalid');
  }
  const proxy = browserProxy(env.ADOBE_BROWSER_PROXY);

  let runtime;
  try { runtime = await importer(backend); }
  catch { throw new RegistrationError('browser_backend_unavailable'); }

  const args = ['--restore-last-session', '--window-size=1440,900'];
  if (backend === 'cloakbrowser') {
    if (typeof runtime.launchPersistentContext !== 'function') {
      throw new RegistrationError('browser_backend_incompatible');
    }
    // npm 0.6.4 的持久接口将 timeout 放在 launchOptions；不复用 Grok 的平台指纹参数。
    return runtime.launchPersistentContext({
      userDataDir: profileDir,
      headless,
      humanize: true,
      viewport: null,
      args,
      ...(proxy ? { proxy } : {}),
      launchOptions: { timeout: LAUNCH_TIMEOUT_MS },
    });
  }
  if (typeof runtime.chromium?.launchPersistentContext !== 'function') {
    throw new RegistrationError('browser_backend_incompatible');
  }
  return runtime.chromium.launchPersistentContext(profileDir, {
    channel,
    headless,
    viewport: null,
    args,
    ...(proxy ? { proxy } : {}),
    timeout: LAUNCH_TIMEOUT_MS,
  });
}
