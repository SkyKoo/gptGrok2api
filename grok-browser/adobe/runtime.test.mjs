import test from 'node:test';
import assert from 'node:assert/strict';
import { launchPersistentBrowser, browserProxy, browserLaunchError } from './browser-runtime.mjs';

test('proxy root slash is normalized and credentials remain separate from the server', () => {
  assert.equal(browserProxy(''), undefined);
  assert.deepEqual(browserProxy('http://host.docker.internal:7897/'), { server: 'http://host.docker.internal:7897' });
  assert.deepEqual(browserProxy('http://fixture:p%40ss@proxy.example.test:8080/'), {
    server: 'http://proxy.example.test:8080', username: 'fixture', password: 'p@ss',
  });
  for (const raw of ['http://proxy.test/path', 'http://proxy.test/?secret=fixture',
    'http://proxy.test/#fragment', 'socks4://proxy.test:1080', 'not-a-url', 'http://proxy.test/\n']) {
    assert.throws(() => browserProxy(raw), error => error.code === 'browser_proxy_invalid' && !error.message.includes(raw));
  }
});

test('SOCKS5 normalizes remote DNS aliases and preserves encoded credentials and IPv6', () => {
  assert.deepEqual(browserProxy('socks5://proxy.example.test:1080/'), { server: 'socks5://proxy.example.test:1080' });
  assert.deepEqual(browserProxy('socks5h://[::1]:1080/'), { server: 'socks5://[::1]:1080' });
  assert.deepEqual(browserProxy('socks5h://fixture%40user:p%40ss%2B%3D%25@[::1]:1080/'), {
    server: 'socks5://[::1]:1080', username: 'fixture@user', password: 'p@ss+=%',
  });
  assert.deepEqual(browserProxy('socks5://fixture:%E7%A7%98%E5%AF%86@proxy.example.test:1080'), {
    server: 'socks5://proxy.example.test:1080', username: 'fixture', password: '秘密',
  });
  for (const raw of ['socks5://proxy.test/path', 'socks5://proxy.test/?secret=fixture',
    'socks5://proxy.test/#fragment', 'socks5://proxy.test:65536',
    'socks5://fixture:%ZZ@proxy.test:1080', 'socks5://:secret@proxy.test:1080']) {
    assert.throws(() => browserProxy(raw), error => error.code === 'browser_proxy_invalid' && !error.message.includes(raw));
  }
});

test('both backends receive unauthenticated SOCKS5 and CloakBrowser retains authenticated SOCKS5', async () => {
  for (const backend of ['playwright', 'cloakbrowser']) {
    for (const scheme of ['socks5', 'socks5h']) {
      let captured;
      await launchPersistentBrowser('/fixture/browser', {}, {
        env: { ADOBE_BROWSER_BACKEND: backend, ADOBE_BROWSER_PROXY: `${scheme}://[::1]:1080/` },
        importer: async () => backend === 'cloakbrowser'
          ? { launchPersistentContext: async options => { captured = options; } }
          : { chromium: { launchPersistentContext: async (_path, options) => { captured = options; } } },
      });
      assert.deepEqual(captured.proxy, { server: 'socks5://[::1]:1080' });
    }
  }
  for (const scheme of ['socks5', 'socks5h']) {
    let captured;
    await launchPersistentBrowser('/fixture/browser', {}, {
      env: { ADOBE_BROWSER_BACKEND: 'cloakbrowser', ADOBE_BROWSER_PROXY: `${scheme}://fixture:p%40ss%2B%3D%25@proxy.test:1080/` },
      importer: async () => ({ launchPersistentContext: async options => { captured = options; } }),
    });
    assert.deepEqual(captured.proxy, { server: 'socks5://proxy.test:1080', username: 'fixture', password: 'p@ss+=%' });
  }
});

test('ordinary Playwright rejects authenticated SOCKS before importing or launching', async () => {
  for (const scheme of ['socks5', 'socks5h']) {
    for (const userinfo of ['fixture:secret', 'fixture']) {
      await assert.rejects(launchPersistentBrowser('/fixture/browser', {}, {
        env: { ADOBE_BROWSER_BACKEND: 'playwright', ADOBE_BROWSER_PROXY: `${scheme}://${userinfo}@proxy.test:1080/` },
        importer: async () => assert.fail('unsupported authentication must not launch or fall back'),
      }), error => error.code === 'browser_proxy_auth_unsupported' && !error.message.includes('secret'));
    }
  }
});

test('both browser backends receive only the explicit normalized proxy', async () => {
  for (const backend of ['playwright', 'cloakbrowser']) {
    let captured;
    await launchPersistentBrowser('/fixture/browser', {}, {
      env: { ADOBE_BROWSER_BACKEND: backend, ADOBE_BROWSER_PROXY: 'http://host.docker.internal:7897/' },
      importer: async () => backend === 'cloakbrowser'
        ? { launchPersistentContext: async options => { captured = options; } }
        : { chromium: { launchPersistentContext: async (_path, options) => { captured = options; } } },
    });
    assert.deepEqual(captured.proxy, { server: 'http://host.docker.internal:7897' });
    assert.equal(captured.viewport, null);
  }
  await assert.rejects(launchPersistentBrowser('/fixture/browser', {}, {
    env: { ADOBE_BROWSER_PROXY: 'http://proxy.test/bad-path' },
    importer: async () => assert.fail('invalid proxy must fail before launching'),
  }), error => error.code === 'browser_proxy_invalid');
});

test('container Chromium retains the requested disk profile and native context lifecycle', async () => {
  let closed = false;
  const context = { close: async () => { closed = true; } };
  const calls = [];
  const result = await launchPersistentBrowser('/private-fixture/browser', { headless: false }, {
    env: { ADOBE_BROWSER_CHANNEL: 'chromium' },
    importer: async name => {
      assert.equal(name, 'playwright');
      return { chromium: { launchPersistentContext: async (...args) => { calls.push(args); return context; } } };
    },
  });
  assert.equal(result, context);
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], '/private-fixture/browser');
  const options = calls[0][1];
  assert.equal(options.channel, 'chromium');
  assert.equal(options.viewport, null);
  assert.equal(options.headless, false);
  assert.equal(options.timeout, 30_000);
  assert.ok(options.args.includes('--restore-last-session'));
  for (const key of ['userAgent', 'locale', 'timezoneId', 'deviceScaleFactor']) {
    assert.equal(Object.hasOwn(options, key), false);
  }
  await result.close();
  assert.equal(closed, true);
});

test('existing host usage still selects installed Chrome', async () => {
  await launchPersistentBrowser('/fixture/browser', { headless: true }, {
    env: {},
    importer: async () => ({ chromium: { launchPersistentContext: async (_directory, options) => {
      assert.equal(options.channel, 'chrome');
      assert.equal(options.headless, true);
    } } }),
  });
});

test('CloakBrowser uses its persistent API without passing a Chrome channel or protocol identity', async () => {
  let calls = 0;
  const context = {};
  const result = await launchPersistentBrowser('/fixture/retained-profile', {}, {
    env: { ADOBE_BROWSER_BACKEND: 'cloakbrowser', ADOBE_BROWSER_CHANNEL: 'chromium' },
    importer: async name => {
      assert.equal(name, 'cloakbrowser');
      return { launchPersistentContext: async options => {
        calls++;
        assert.equal(options.userDataDir, '/fixture/retained-profile');
        assert.equal(options.viewport, null);
        assert.equal(options.headless, false);
        assert.equal(options.humanize, true);
        assert.deepEqual(options.launchOptions, { timeout: 30_000 });
        assert.deepEqual(options.args, ['--restore-last-session', '--window-size=1440,900']);
        for (const key of ['channel', 'contextOptions', 'userAgent', 'locale', 'timezone', 'deviceScaleFactor']) {
          assert.equal(Object.hasOwn(options, key), false);
        }
        return context;
      } };
    },
  });
  assert.equal(calls, 1);
  assert.equal(result, context);
});

test('unknown backends and channels stop before importing or launching any browser', async () => {
  for (const [env, code] of [
    [{ ADOBE_BROWSER_BACKEND: 'unknown' }, 'browser_backend_invalid'],
    [{ ADOBE_BROWSER_CHANNEL: 'unknown' }, 'browser_channel_invalid'],
  ]) {
    await assert.rejects(launchPersistentBrowser('/fixture/browser', {}, {
      env, importer: async () => { assert.fail('must not load invalid runtime'); },
    }), error => error.code === code);
  }
});

test('missing or failed CloakBrowser never silently falls back to another backend', async () => {
  const imported = [];
  await assert.rejects(launchPersistentBrowser('/fixture/browser', {}, {
    env: { ADOBE_BROWSER_BACKEND: 'cloakbrowser' },
    importer: async name => { imported.push(name); throw new Error('private implementation details'); },
  }), error => error.code === 'browser_backend_unavailable' && !error.message.includes('private'));
  assert.deepEqual(imported, ['cloakbrowser']);

  const launchError = new Error('simulated browser launch failure');
  let launches = 0;
  await assert.rejects(launchPersistentBrowser('/fixture/browser', {}, {
    env: { ADOBE_BROWSER_BACKEND: 'cloakbrowser' },
    importer: async () => ({ launchPersistentContext: async () => { launches++; throw launchError; } }),
  }), error => error.code === 'browser_launch_failed' && !error.message.includes(launchError.message));
  assert.equal(launches, 1);
});

test('dedicated refresh opts out of tab restoration without changing stored identity or proxy', async () => {
  for (const backend of ['playwright','cloakbrowser']) {
    let captured, directory;
    const context = {};
    const result = await launchPersistentBrowser('/fixture/existing-profile', {restoreLastSession:false}, {
      env:{ADOBE_BROWSER_BACKEND:backend,ADOBE_BROWSER_PROXY:'http://fixture:secret@proxy.test:8080'},
      importer:async()=>backend === 'cloakbrowser'
        ? {launchPersistentContext:async options=>{captured=options;directory=options.userDataDir;return context;}}
        : {chromium:{launchPersistentContext:async(dir,options)=>{captured=options;directory=dir;return context;}}},
    });
    assert.equal(result,context);
    assert.equal(directory,'/fixture/existing-profile');
    assert.deepEqual(captured.args,['--window-size=1440,900']);
    assert.equal(captured.proxy.password,'secret');
    assert.equal(captured.viewport,null);
  }
});

test('startup failures expose fixed categories without raw browser details', () => {
  const fixtures = [
    [{name:'TimeoutError',message:'private-token'},'browser_launch_timeout'],
    [new Error('Timeout 30000ms exceeded. proxy=private-token'),'browser_launch_timeout'],
    [new Error('net::ERR_PROXY_CONNECTION_FAILED private-token'),'browser_launch_proxy_error'],
    [new Error('Failed to create ProcessSingleton private-token'),'browser_launch_profile_unavailable'],
    [new Error('Profile appears to be in use private-token'),'browser_launch_profile_locked'],
    [{name:'CloakBrowserLicenseError',message:'private-token'},'browser_launch_license_denied'],
    [new Error('Target page, context or browser has been closed private-token'),'browser_launch_process_exited'],
    [new Error('something private-token'),'browser_launch_failed'],
    [{code:'private-token'},'browser_launch_failed'],
  ];
  for(const [raw,code] of fixtures){
    const safe = browserLaunchError(raw);
    assert.equal(safe.code,code);
    assert.equal(safe.message.includes('private-token'),false);
    assert.equal(safe.cause,undefined);
  }
});
