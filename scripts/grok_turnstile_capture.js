#!/usr/bin/env node
'use strict';

// Read-only Grok/Turnstile observation harness. It never submits signup data
// and redacts token-shaped values before writing the report.
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const crypto = require('node:crypto');
const {spawn} = require('node:child_process');

function args(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i += 1) {
    if (!argv[i].startsWith('--')) continue;
    const key = argv[i].slice(2);
    const next = argv[i + 1];
    if (!next || next.startsWith('--')) out[key] = true;
    else { out[key] = next; i += 1; }
  }
  return out;
}

const cli = args(process.argv.slice(2));
const targetURL = String(cli.url || 'https://accounts.x.ai/sign-up?redirect=grok-com');
const outDir = path.resolve(String(cli.out || path.join(os.tmpdir(), 'cfm-grok-capture')));
const timeoutMs = Math.max(5000, Number(cli.timeout || 45000) * 1000);
const headless = !cli.headed;
const requestedChrome = String(cli.chrome || process.env.CHROME_PATH || '').trim();
const chromeCandidates = [
  requestedChrome,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable',
  '/usr/bin/chromium', '/usr/bin/chromium-browser',
].filter(Boolean);
const chrome = chromeCandidates.find((candidate) => fs.existsSync(candidate));
if (!chrome) throw new Error('Chrome/Chromium executable not found; pass --chrome PATH');
fs.mkdirSync(outDir, {recursive: true});
const jsDir = path.join(outDir, 'scripts');
fs.mkdirSync(jsDir, {recursive: true});
const challengeDir = path.join(outDir, 'challenge');
fs.mkdirSync(challengeDir, {recursive: true});

const events = [];
const requestMeta = new Map();
const savedScripts = [];
const startedAt = new Date().toISOString();
const redact = (value) => String(value ?? '')
  .replace(/eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}/g, '<jwt-redacted>')
  .replace(/[A-Za-z0-9_-]{120,}/g, (match) => `<long-redacted:${match.length}>`);
const short = (value, limit = 16000) => {
  const text = redact(value);
  return text.length > limit ? `${text.slice(0, limit)}<truncated:${text.length}>` : text;
};
const record = (type, data = {}) => {
  events.push({at: new Date().toISOString(), type, ...data});
};

function relevant(url) {
  return /accounts\.x\.ai|challenges\.cloudflare\.com|cloudflare|turnstile|cdn-cgi/i.test(String(url || ''));
}
function scriptLike(url, mime = '') {
  return /\.js(?:[?#]|$)|javascript/i.test(String(url || '')) || /javascript/i.test(String(mime || ''));
}
function safeName(url) {
  const hash = crypto.createHash('sha256').update(String(url)).digest('hex').slice(0, 12);
  const base = String(url).split('/').pop().split('?')[0].replace(/[^A-Za-z0-9._-]/g, '_').slice(-80) || 'script.js';
  return `${hash}-${base}`;
}

function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const port = server.address().port;
      server.close(() => resolve(port));
    });
  });
}
async function waitForJSON(url, deadline) {
  let last;
  while (Date.now() < deadline) {
    try { return await (await fetch(url)).json(); }
    catch (error) { last = error; await new Promise((resolve) => setTimeout(resolve, 150)); }
  }
  throw last || new Error(`timeout waiting for ${url}`);
}

// Keep the harness CommonJS-compatible while allowing asynchronous CDP setup.
(async () => {
const port = await freePort();
const profileDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cfm-grok-capture-'));
const chromeArgs = [
  `--remote-debugging-port=${port}`, `--user-data-dir=${profileDir}`,
  '--no-first-run', '--no-default-browser-check', '--disable-background-networking',
  '--disable-blink-features=AutomationControlled', '--disable-dev-shm-usage',
  '--window-size=1440,900', '--lang=en-US', 'about:blank',
];
if (headless) chromeArgs.unshift('--headless=new');
if (cli.proxy) chromeArgs.splice(chromeArgs.length - 1, 0, `--proxy-server=${cli.proxy}`);
const browser = spawn(chrome, chromeArgs, {stdio: ['ignore', 'ignore', 'pipe']});
browser.stderr.on('data', (chunk) => record('chrome.stderr', {value: short(chunk.toString(), 2000)}));
const cleanup = () => {
  try { browser.kill('SIGTERM'); } catch (_) {}
  try { fs.rmSync(profileDir, {recursive: true, force: true}); } catch (_) {}
};
process.on('exit', cleanup);
process.on('SIGINT', () => { cleanup(); process.exit(130); });

const deadline = Date.now() + timeoutMs;
const tabs = await waitForJSON(`http://127.0.0.1:${port}/json/list`, deadline);
const tab = tabs.find((item) => item.type === 'page');
if (!tab || !tab.webSocketDebuggerUrl) throw new Error('Chrome page target unavailable');
const ws = new WebSocket(tab.webSocketDebuggerUrl);
await new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = reject; });
let nextID = 0;
const pending = new Map();
ws.onmessage = async (message) => {
  const packet = JSON.parse(String(message.data));
  if (packet.id && pending.has(packet.id)) {
    const waiter = pending.get(packet.id); pending.delete(packet.id);
    if (packet.error) waiter.reject(new Error(JSON.stringify(packet.error)));
    else waiter.resolve(packet.result || {});
    return;
  }
  if (packet.method === 'Runtime.consoleAPICalled') {
    const values = (packet.params.args || []).map((item) => item.value ?? item.description ?? '');
    const joined = values.map((value) => String(value)).join(' ');
    const marker = '__CFM_GROK_CAPTURE__';
    const index = joined.indexOf(marker);
    if (index >= 0) {
      try { record('page.capture', JSON.parse(joined.slice(index + marker.length))); }
      catch (_) { record('page.capture.raw', {value: short(joined)}); }
    } else if (/turnstile|challenge|captcha/i.test(joined)) {
      record('page.console', {level: packet.params.type, value: short(joined, 4000)});
    }
  } else if (packet.method === 'Runtime.exceptionThrown') {
    record('page.exception', {description: short(packet.params.exceptionDetails?.text || packet.params.exceptionDetails?.exception?.description || '')});
  } else if (packet.method === 'Page.frameNavigated') {
    record('frame.navigated', {frameId: packet.params.frame.id, url: packet.params.frame.url, name: packet.params.frame.name || ''});
  } else if (packet.method === 'Network.requestWillBeSent') {
    const req = packet.params.request;
    requestMeta.set(packet.params.requestId, {url: req.url, method: req.method, type: packet.params.type || '', mime: '', postData: req.postData || ''});
    if (relevant(req.url)) record('network.request', {requestId: packet.params.requestId, url: req.url, method: req.method, resourceType: packet.params.type || '', postData: short(req.postData || '', 12000)});
  } else if (packet.method === 'Network.responseReceived') {
    const response = packet.params.response;
    const meta = requestMeta.get(packet.params.requestId) || {};
    meta.mime = response.mimeType || '';
    requestMeta.set(packet.params.requestId, meta);
    if (relevant(response.url)) record('network.response', {requestId: packet.params.requestId, url: response.url, status: response.status, mime: response.mimeType || ''});
  } else if (packet.method === 'Network.loadingFinished') {
    const meta = requestMeta.get(packet.params.requestId);
    if (!meta || !relevant(meta.url)) return;
    if (scriptLike(meta.url, meta.mime) && Number(packet.params.encodedDataLength || 0) <= 4 * 1024 * 1024) {
      try {
        const body = await command('Network.getResponseBody', {requestId: packet.params.requestId});
        const file = path.join(jsDir, safeName(meta.url));
        fs.writeFileSync(file, body.body || '', 'utf8');
        savedScripts.push({url: meta.url, file, bytes: Buffer.byteLength(body.body || '')});
        record('network.script_saved', {url: meta.url, file, bytes: Buffer.byteLength(body.body || '')});
      } catch (_) {}
    }
    if (meta.type === 'Document' && /challenges\.cloudflare\.com\/cdn-cgi\/challenge-platform|turnstile\/f\/av0/i.test(meta.url) && Number(packet.params.encodedDataLength || 0) <= 2 * 1024 * 1024) {
      try {
        const body = await command('Network.getResponseBody', {requestId: packet.params.requestId});
        const file = path.join(challengeDir, safeName(meta.url) + '.html');
        fs.writeFileSync(file, short(body.body || '', 2 * 1024 * 1024), 'utf8');
        record('network.challenge_saved', {url: meta.url, file, bytes: Buffer.byteLength(body.body || '')});
      } catch (_) {}
    }
  }
};
function command(method, params = {}) {
  return new Promise((resolve, reject) => {
    const id = ++nextID;
    pending.set(id, {resolve, reject});
    ws.send(JSON.stringify({id, method, params}));
  });
}

const instrumentation = `(() => {
  const marker = '__CFM_GROK_CAPTURE__';
  const emit = (type, data) => { try { console.debug(marker + JSON.stringify({type, ...data})); } catch (_) {} };
  const summarize = (value) => {
    if (value == null) return value;
    if (typeof value === 'string') return {kind:'string', length:value.length, prefix:value.slice(0,24)};
    if (Array.isArray(value)) return {kind:'array', length:value.length, items:value.slice(0,6).map(summarize)};
    if (typeof value === 'object') {
      const summary = {kind:'object', keys:Object.keys(value).slice(0,40)};
      for (const key of ['source','event','mode','widgetId','seq','code','rcV','action']) {
        const item = value[key];
        if (typeof item === 'string' || typeof item === 'number' || typeof item === 'boolean') {
          summary[key] = typeof item === 'string' ? item.slice(0,80) : item;
        }
      }
      return summary;
    }
    return {kind:typeof value, value:String(value).slice(0,80)};
  };
  window.addEventListener('message', (event) => emit('window.message', {origin:event.origin, data:summarize(event.data)}), true);
  const wrapped = new WeakSet();
  const wrap = () => {
    const t = window.turnstile;
    if (!t || wrapped.has(t)) return;
    wrapped.add(t);
    for (const name of ['render','execute','reset','remove','getResponse']) {
      if (typeof t[name] !== 'function') continue;
      const original = t[name];
      t[name] = function(...args) {
        const first = args[0];
        const options = (args[1] && typeof args[1] === 'object') ? args[1] : (first && typeof first === 'object' ? first : {});
        emit('turnstile.' + name, {argc:args.length, sitekey:String(options.sitekey || first?.getAttribute?.('data-sitekey') || ''), action:String(options.action || ''), cdata:String(options.cData || options.cdata || ''), callback:typeof options.callback === 'function'});
        if (args[1] && typeof args[1] === 'object' && typeof args[1].callback === 'function') {
          const callback = args[1].callback;
          args[1] = {...args[1], callback:(token) => { emit('turnstile.callback', {length:String(token || '').length, prefix:String(token || '').slice(0,16)}); return callback(token); }};
        }
        const result = original.apply(this, args);
        emit('turnstile.' + name + '.result', {resultType:typeof result, result:String(result ?? '').slice(0,80)});
        return result;
      };
    }
  };
  setInterval(wrap, 100);
  emit('instrumentation.ready', {href:location.href});
})();`;
await command('Runtime.enable');
await command('Page.enable');
await command('Network.enable');
await command('Log.enable');
await command('Page.addScriptToEvaluateOnNewDocument', {source: instrumentation});
await command('Page.navigate', {url: targetURL});
record('capture.started', {targetURL, chrome, headless, proxy: cli.proxy ? String(cli.proxy) : '', timeoutMs});

if (cli.probe) {
  // Optional diagnostic only: render a standalone widget on the loaded page.
  // It does not fill or submit the signup form and only records token metadata.
  await new Promise((resolve) => setTimeout(resolve, 3500));
  const probe = `(() => {
    const marker = '__CFM_GROK_CAPTURE__';
    const emit = (type, data) => { try { console.debug(marker + JSON.stringify({type, ...data})); } catch (_) {} };
    const html = document.documentElement?.outerHTML || '';
    const match = html.match(/0x4AAAA[A-Za-z0-9_-]+/);
    const sitekey = match ? match[0] : '';
    if (!sitekey || !window.turnstile) return {ok:false, reason:'turnstile-or-sitekey-unavailable'};
    const host = document.createElement('div');
    host.id = 'cfm-grok-turnstile-probe';
    host.style.cssText = 'position:fixed;left:8px;bottom:8px;width:320px;min-height:80px;z-index:2147483647;background:#fff';
    document.body.appendChild(host);
    try {
      const widgetId = window.turnstile.render(host, {
        sitekey,
        theme: 'light',
        callback: (token) => emit('probe.callback', {length:String(token || '').length, prefix:String(token || '').slice(0,16)}),
        'error-callback': (code) => emit('probe.error', {code:String(code || '')}),
        'expired-callback': () => emit('probe.expired', {}),
      });
      emit('probe.rendered', {sitekey, widgetId:String(widgetId ?? '')});
      if (window.__CFM_GROK_PROBE_EXECUTE__) {
        setTimeout(() => {
          try {
            const result = window.turnstile.execute(widgetId);
            emit('probe.execute.result', {resultType:typeof result, result:String(result ?? '').slice(0,80)});
          } catch (error) {
            emit('probe.execute.error', {message:String(error)});
          }
        }, 500);
      }
      return {ok:true, sitekey, widgetId:String(widgetId ?? '')};
    } catch (error) {
      emit('probe.exception', {message:String(error)});
      return {ok:false, reason:String(error)};
    }
  })()`;
  try {
    if (cli.execute) await command('Runtime.evaluate', {expression:'window.__CFM_GROK_PROBE_EXECUTE__ = true', returnByValue:true});
    const result = await command('Runtime.evaluate', {expression: probe, returnByValue: true});
    record('probe.result', {value: short(result.result?.value || '', 4000)});
  } catch (error) {
    record('probe.error', {value: String(error)});
  }
}

while (Date.now() < deadline) {
  await new Promise((resolve) => setTimeout(resolve, 1000));
  try {
    const state = await command('Runtime.evaluate', {expression:`JSON.stringify({title:document.title,href:location.href,sitekeys:[...document.querySelectorAll('[data-sitekey]')].map(x=>x.getAttribute('data-sitekey')),widgets:document.querySelectorAll('.cf-turnstile').length,inputs:document.querySelectorAll('[name="cf-turnstile-response"]').length,iframes:[...document.querySelectorAll('iframe')].map(x=>x.src).filter(Boolean)})`, returnByValue:true});
    record('page.state', {value: short(state.result?.value || '', 4000)});
  } catch (error) { record('capture.error', {value:String(error)}); break; }
}
try {
  const html = await command('Runtime.evaluate', {expression:'document.documentElement?.outerHTML || ""', returnByValue:true});
  fs.writeFileSync(path.join(outDir, 'page.html'), short(html.result?.value || '', 2 * 1024 * 1024), 'utf8');
} catch (_) {}
const report = {startedAt, finishedAt:new Date().toISOString(), targetURL, chrome, headless, proxy:cli.proxy ? String(cli.proxy) : '', savedScripts, events};
fs.writeFileSync(path.join(outDir, 'report.json'), JSON.stringify(report, null, 2));
console.log(JSON.stringify({outDir, eventCount:events.length, savedScripts:savedScripts.length, report:path.join(outDir, 'report.json')}, null, 2));
try { ws.close(); } catch (_) {}
cleanup();
})().catch((error) => {
  console.error(error?.stack || error);
  process.exitCode = 1;
});
