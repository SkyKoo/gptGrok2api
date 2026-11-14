import { RegistrationError } from './core.mjs';
import { REGISTRATION_TIMEOUTS } from './registration-timeouts.mjs';

const LAUNCH_TIMEOUT_MS = REGISTRATION_TIMEOUTS.launch;

// Playwright errors can contain the full launch command, proxy credentials and
// profile paths. Convert them to fixed categories before crossing the RPC pipe.
export function browserLaunchError(error) {
  const message = typeof error?.message === 'string' ? error.message : '';
  let code = 'browser_launch_failed';
  if (error?.name === 'CloakBrowserLicenseError') code = 'browser_launch_license_denied';
  else if (/ERR_PROXY_CONNECTION_FAILED|ERR_TUNNEL_CONNECTION_FAILED|ERR_NO_SUPPORTED_PROXIES|ERR_PROXY_AUTH_UNSUPPORTED/.test(message)) code = 'browser_launch_proxy_error';
  else if (/profile.*(?:in use|already in use)/i.test(message)) code = 'browser_launch_profile_locked';
  else if (/ProcessSingleton|SingletonLock/.test(message)) code = 'browser_launch_profile_unavailable';
  else if (error?.name === 'TimeoutError' || /Timeout \d+ms exceeded/.test(message)) code = 'browser_launch_timeout';
  else if (/Target page, context or browser has been closed|Browser closed|process did exit|Failed to launch the browser process/i.test(message)) code = 'browser_launch_process_exited';
  return new RegistrationError(code);
}

/** 显式代理配置，不继承宿主环境；去掉 URL 根斜线，凭据与 server 分开。 */
export function browserProxy(raw) {
  if (!raw) return undefined;
  try {
    if (typeof raw !== 'string' || /[\r\n\0]/.test(raw)) throw new Error();
    const url = new URL(raw);
    if (!['http:', 'https:', 'socks5:', 'socks5h:'].includes(url.protocol) || !url.hostname ||
        !['', '/'].includes(url.pathname) || url.search || url.hash ||
        (url.password && !url.username)) throw new Error();
    // Chromium 的 SOCKS5 在代理端解析域名；socks5h 使用相同入口。
    // 非 HTTP 协议的 URL.origin 为 null，必须保留实际 host（含 IPv6/端口）。
    const protocol = url.protocol === 'socks5h:' ? 'socks5:' : url.protocol;
    return { server: `${protocol}//${url.host}`,
      ...(url.username ? { username: decodeURIComponent(url.username) } : {}),
      ...(url.password ? { password: decodeURIComponent(url.password) } : {}),
    };
  } catch { throw new RegistrationError('browser_proxy_invalid'); }
}

/** 同一任务始终使用磁盘 profile；Docker 仅替换浏览器来源，不覆盖网页环境。 */
export async function launchPersistentBrowser(profileDir, { headless = false, restoreLastSession = true } = {}, dependencies = {}) {
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
  // CloakBrowser 的 Chromium 支持 RFC 1929；普通 Playwright 不支持认证 SOCKS。
  // 在启动前明确拒绝，不能丢弃凭据或退回直连。
  if (backend === 'playwright' && proxy?.server.startsWith('socks5:') &&
      (proxy.username || proxy.password)) {
    throw new RegistrationError('browser_proxy_auth_unsupported');
  }

  let runtime;
  try { runtime = await importer(backend); }
  catch { throw new RegistrationError('browser_backend_unavailable'); }

  // Dedicated refresh workers explicitly navigate one page. Restoring previous
  // pages makes Playwright wait for their initialization before returning the
  // context, too early for post-launch page cleanup to prevent that workload.
  const args = [...(restoreLastSession ? ['--restore-last-session'] : []), '--window-size=1440,900'];
  if (backend === 'cloakbrowser') {
    if (typeof runtime.launchPersistentContext !== 'function') {
      throw new RegistrationError('browser_backend_incompatible');
    }
    // npm 0.6.4 的持久接口将 timeout 放在 launchOptions；不复用 Grok 的平台指纹参数。
    try { return await runtime.launchPersistentContext({
      userDataDir: profileDir,
      headless,
      humanize: true,
      viewport: null,
      args,
      ...(proxy ? { proxy } : {}),
      launchOptions: { timeout: LAUNCH_TIMEOUT_MS },
    }); } catch (error) { throw browserLaunchError(error); }
  }
  if (typeof runtime.chromium?.launchPersistentContext !== 'function') {
    throw new RegistrationError('browser_backend_incompatible');
  }
  try { return await runtime.chromium.launchPersistentContext(profileDir, {
    channel,
    headless,
    viewport: null,
    args,
    ...(proxy ? { proxy } : {}),
    timeout: LAUNCH_TIMEOUT_MS,
  }); } catch (error) { throw browserLaunchError(error); }
}
