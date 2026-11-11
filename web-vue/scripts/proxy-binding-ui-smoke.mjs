// Run after npm run build. All network requests are local fixtures; no upstream calls.
import assert from 'node:assert/strict'
import http from 'node:http'
import fs from 'node:fs/promises'
import path from 'node:path'
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright')
const root = path.resolve('dist')
const server = http.createServer(async (req, res) => {
  try {
    const requested = new URL(req.url, 'http://localhost').pathname
    const file = path.resolve(root, '.' + (requested === '/' ? '/index.html' : requested))
    if (!file.startsWith(root + '/')) throw new Error('invalid path')
    const data = await fs.readFile(file)
    res.setHeader('Content-Type', ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream')
    res.end(data)
  } catch { res.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'], executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined })
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } })
await context.addInitScript(() => localStorage.setItem('chatgpt2api.adminKey', 'fixture-only'))
const page = await context.newPage(), errors = [], unknown = [], writes = [], blockedExternal = []
page.on('pageerror', error => errors.push(error.message))
const accounts = new Map(['openai', 'grok', 'adobe'].map(platform => [platform, { platform, account_id: `${platform}-fixture`, policy: 'inherit', label: '跟随默认出口', available: true, binding: null, history: [] }]))
const group = { id: 'fixture', name: '测试代理组', enabled: true, strategy: 'round_robin', nodes: [{ id: 'one', name: '测试节点', url: 'http://fixture.invalid:8080', enabled: true }] }
await context.route('**/*', async route => {
  const req = route.request(), url = new URL(req.url()), pathname = url.pathname
  if (url.origin !== origin) { blockedExternal.push({ origin: url.origin, type: req.resourceType() }); await route.abort(); return }
  if (!pathname.startsWith('/api/') && !pathname.startsWith('/auth/') && !['/version', '/meta', '/health'].includes(pathname)) { await route.continue(); return }
  let data
  if (pathname === '/auth/status') data = { authenticated: true, role: 'admin', name: 'Fixture' }
  else if (pathname === '/api/settings') data = { config: { proxy: 'http://default.invalid:8080' } }
  else if (pathname === '/api/proxy/groups') data = { groups: [group] }
  else if (pathname === '/api/proxy/runtime') data = { runtime: {}, status: {} }
  else if (pathname === '/api/third-party-apps' && req.method() === 'GET') data = { apps: [] }
  else if (pathname === '/version') data = { version: 'fixture' }
  else if (pathname === '/api/proxy/binding') {
    if (req.method() === 'POST') {
      const body = req.postDataJSON(), old = accounts.get(body.platform)
      assert.equal(body.account_id, `${body.platform}-fixture`)
      assert.ok(['inherit', 'direct', 'node:fixture/one'].includes(body.reference))
      assert.ok(body.reason.trim())
      writes.push(body)
      const label = body.reference === 'inherit' ? '跟随默认出口' : body.reference === 'direct' ? 'direct' : 'fixture/one'
      accounts.set(body.platform, { ...old, policy: body.reference === 'inherit' ? 'inherit' : 'explicit', label, binding: body.reference === 'inherit' ? null : { reference: body.reference, bound_at: '2026-11-11T12:00:00Z' }, history: [...old.history, { at: '2026-11-11T12:00:00Z', from: old.label, to: label, reason: body.reason }] })
      data = accounts.get(body.platform)
    } else {
      const platform = url.searchParams.get('platform')
      assert.equal(url.searchParams.get('account_id'), `${platform}-fixture`)
      data = accounts.get(platform)
    }
  } else { unknown.push(`${req.method()} ${pathname}`); data = {} }
  await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) })
})
const platformSelect = () => page.getByLabel('平台', { exact: true })
const newRoute = () => page.getByLabel('新出口', { exact: true })
async function query(platform) {
  await platformSelect().selectOption(platform)
  await page.getByLabel('账号 ID（账号列表中的 ID）', { exact: true }).fill(`${platform}-fixture`)
  await page.getByRole('button', { name: '查询绑定', exact: true }).click()
  await page.getByText(`当前出口：${accounts.get(platform).label} · 配置可用`, { exact: true }).waitFor()
}
async function save(reference) {
  await newRoute().selectOption(reference)
  await page.getByLabel('切换原因', { exact: true }).fill('验证日常出口选择')
  await page.getByRole('button', { name: '保存绑定并记录原因', exact: true }).click()
  await page.getByRole('status').filter({ hasText: '绑定已保存，切换原因已记录。' }).waitFor()
}
try {
  await page.goto(origin + '/#/proxy')
  await page.getByText('账号日常出口', { exact: true }).waitFor()
  await page.getByText('账号未单独设置出口时，日常请求跟随下方默认出口。', { exact: false }).waitFor()
  for (const platform of ['openai', 'grok', 'adobe']) {
    await query(platform)
    await save('direct')
    assert.equal(accounts.get(platform).policy, 'explicit')
    await page.reload()
    await query(platform)
    await save('inherit')
    assert.equal(accounts.get(platform).policy, 'inherit')
    await page.reload()
    await query(platform)
    assert.equal(accounts.get(platform).binding, null)
  }
  await query('openai')
  await save('node:fixture/one')
  await page.reload()
  await query('openai')
  assert.equal(accounts.get('openai').binding.reference, 'node:fixture/one')
  await query('adobe')
  if (process.env.CFM_UI_SCREENSHOT_DIR) {
    await fs.mkdir(process.env.CFM_UI_SCREENSHOT_DIR, { recursive: true })
    for (const theme of ['dark', 'light']) {
      await page.emulateMedia({ colorScheme: theme })
      await page.waitForFunction(theme => (document.documentElement.dataset.theme || 'light') === theme, theme)
      await page.screenshot({ path: path.join(process.env.CFM_UI_SCREENSHOT_DIR, `proxy-binding-${theme}.png`), fullPage: true, animations: 'disabled' })
    }
  }
  await page.setViewportSize({ width: 390, height: 844 })
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'proxy page must fit mobile')
  assert.equal(writes.length, 7)
  assert.deepEqual(unknown, [])
  assert.deepEqual(errors, [])
  assert.ok(blockedExternal.every(request => ['https://api.iconify.design', 'https://api.simplesvg.com', 'https://api.unisvg.com', 'https://fonts.googleapis.com', 'https://fonts.gstatic.com'].includes(request.origin)), JSON.stringify(blockedExternal))
  console.log('Proxy account UI passed: all three platforms, inherit/direct/node save and reload, audit payloads, mobile layout; no upstream requests.')
} finally {
  await browser.close()
  await new Promise(resolve => server.close(resolve))
}
