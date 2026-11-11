// Run after npm run build, inside Docker. All API responses are local fixtures.
// PLAYWRIGHT_MODULE may point to the browser image's installed Playwright module.
import assert from 'node:assert/strict'
import http from 'node:http'
import fs from 'node:fs/promises'
import path from 'node:path'

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright')
const root = path.resolve('dist')
const server = http.createServer(async (req, res) => {
  try {
    const pathname = new URL(req.url, 'http://localhost').pathname
    const file = path.resolve(root, '.' + (pathname === '/' ? '/index.html' : pathname))
    if (!file.startsWith(root + '/')) throw new Error('invalid path')
    const body = await fs.readFile(file)
    res.setHeader('Content-Type', ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream')
    res.end(body)
  } catch { res.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] })
const context = await browser.newContext({ viewport: { width: 1440, height: 1100 } })
await context.addInitScript(() => localStorage.setItem('chatgpt2api.adminKey', 'fixture-only'))
const page = await context.newPage()
const errors = [], unknown = [], requests = []
page.on('pageerror', error => errors.push(error.message))
const mailboxes = [
  { id: 'shared', email: 'shared@example.test', source_id: 'fixture-source', status: 'active', openai_claimed: true, grok_reserved: true, adobe_claimed: true },
  { id: 'unused', email: 'unused@example.test', source_id: 'fixture-source', status: 'available' },
  { id: 'retired', email: 'retired@example.test', source_id: 'fixture-source', status: 'available', adobe_retired: true },
]
const events = [{ id: 'link-event', email: 'shared@example.test', platform: 'adobe', event_type: 'account_mailbox_linked', result: 'success', created_at: '2026-11-11T11:00:00Z' }]
await context.route('**/*', async route => {
  const req = route.request(), url = new URL(req.url()), pathname = url.pathname
  if (url.origin === 'https://fonts.googleapis.com' && url.pathname === '/css2' && req.method() === 'GET') { await route.fulfill({ status: 200, contentType: 'text/css', body: '' }); return }
  if (url.origin !== origin) { unknown.push(`external:${url.origin}`); await route.abort(); return }
  const isAPI = pathname.startsWith('/api/') || pathname.startsWith('/auth/') || ['/version', '/meta', '/health'].includes(pathname)
  if (!isAPI) { await route.continue(); return }
  requests.push({ path: pathname, query: Object.fromEntries(url.searchParams), method: req.method(), body: req.postData() })
  let data
  if (pathname === '/auth/status' && req.method() === 'GET') data = { authenticated: true, role: 'admin', name: 'Fixture' }
  else if (pathname === '/api/settings' && req.method() === 'GET') data = { config: {} }
  else if (pathname === '/api/third-party-apps' && req.method() === 'GET') data = { items: [] }
  else if (pathname === '/version' && req.method() === 'GET') data = { version: 'fixture' }
  else if (pathname === '/api/icloud/bridge-status' && req.method() === 'GET') data = { enabled: true, reachable: true, database_configured: true, hme_configured: true }
  else if (pathname === '/api/icloud/status' && req.method() === 'GET') data = { counts: { mailboxes: 3, active: 1, available: 2, retired: 1, events: events.length } }
  else if (pathname === '/api/icloud/sources' && req.method() === 'GET') data = { sources: [{ id: 'fixture-source', type: 'icloud_hme', label: 'Fixture HME', enable: true, status: 'available', capabilities: { openai: true, grok: true, adobe: true }, config: {} }] }
  else if (pathname === '/api/icloud/events' && req.method() === 'GET') data = { events }
  else if (pathname === '/api/icloud/mailboxes' && req.method() === 'GET') {
    let items = mailboxes
    if (url.searchParams.get('platform') === 'adobe') items = items.filter(box => box.adobe_claimed)
    if (url.searchParams.get('status') === 'disabled_adobe') items = items.filter(box => box.adobe_disabled)
    if (url.searchParams.get('status') === 'retired_adobe') items = items.filter(box => box.adobe_retired)
    data = { mailboxes: items, pagination: { page: 1, page_size: 20, total: items.length, total_all: items.length, total_pages: 1 } }
  } else if (pathname === '/api/icloud/mailboxes/unused/status' && req.method() === 'POST') {
    const payload = JSON.parse(req.postData())
    assert.equal(payload.platform, 'adobe')
    assert.ok(['available', 'disabled'].includes(payload.status))
    mailboxes[1].adobe_disabled = payload.status === 'disabled'
    events.unshift({ id: `event-${events.length}`, email: mailboxes[1].email, platform: 'adobe', event_type: payload.status === 'disabled' ? 'mailbox_disabled' : 'mailbox_enabled', result: 'success', created_at: '2026-11-11T12:00:00Z' })
    data = { success: true, mailbox: mailboxes[1] }
  } else if (pathname === '/api/icloud/reconcile' && req.method() === 'POST') data = { success: true, aliases: 3, openai_claims: 1, grok_claims: 0, adobe_claims: 2, released_claims: 0 }
  else {
    unknown.push(`${req.method()} ${pathname}`)
    await route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: 'Unexpected fixture request' }) })
    return
  }
  await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) })
})
const card = email => page.locator('div.min-w-0.rounded-lg').filter({ hasText: email })
async function filter(ariaLabel, option, key, value) {
  await page.getByRole('button', { name: ariaLabel, exact: true }).click()
  await Promise.all([
    page.waitForResponse(response => {
      const url = new URL(response.url())
      return url.pathname === '/api/icloud/mailboxes' && (url.searchParams.get(key) || '') === value
    }),
    page.getByRole('button', { name: option, exact: true }).click(),
  ])
}
async function screenshot(name) {
  if (!process.env.CFM_UI_SCREENSHOT_DIR) return
  await fs.mkdir(process.env.CFM_UI_SCREENSHOT_DIR, { recursive: true })
  await page.screenshot({ path: path.join(process.env.CFM_UI_SCREENSHOT_DIR, name + '.png'), fullPage: true, animations: 'disabled' })
}
try {
  await page.goto(origin + '/#/icloud')
  await page.getByRole('heading', { name: '邮箱管理', exact: true }).waitFor()
  await page.getByText('关联 Adobe 邮箱资产', { exact: true }).waitFor()
  const shared = card('shared@example.test'), unused = card('unused@example.test'), retired = card('retired@example.test')
  for (const label of ['GPT 已占用', 'Grok 注册预留，未占用', 'Adobe 已占用', 'GPT 允许注册', 'Grok 注册预留中', 'Adobe 允许注册']) await shared.getByText(label, { exact: true }).waitFor()
  assert.equal(await shared.getByRole('button', { name: '删除邮箱', exact: true }).isDisabled(), true)
  for (const platform of ['GPT', 'Grok', 'Adobe']) {
    await unused.getByText(`${platform} 未占用`, { exact: true }).waitFor()
    await unused.getByText(`${platform} 允许注册`, { exact: true }).waitFor()
  }
  await retired.getByText('Adobe 已使用，待清理', { exact: true }).waitFor()
  assert.equal(await retired.getByRole('button', { name: '禁用Adobe', exact: true }).isDisabled(), true)
  for (const theme of ['dark', 'light']) {
    await page.emulateMedia({ colorScheme: theme })
    await page.waitForFunction(theme => (document.documentElement.dataset.theme || 'light') === theme, theme)
    await screenshot(`mailbox-adobe-${theme}`)
  }
  await unused.getByRole('button', { name: '禁用Adobe', exact: true }).click()
  await page.getByText('unused@example.test 已禁用 Adobe 注册。', { exact: true }).waitFor()
  await unused.getByText('Adobe 已禁用注册', { exact: true }).waitFor()
  await page.getByText('禁用 Adobe 注册', { exact: true }).waitFor()
  assert.deepEqual(JSON.parse(requests.filter(req => req.method === 'POST').at(-1).body), { status: 'disabled', platform: 'adobe' })
  await filter('邮箱状态筛选', 'Adobe 已禁用', 'status', 'disabled_adobe')
  await shared.waitFor({ state: 'detached' })
  await unused.waitFor()
  await filter('邮箱状态筛选', 'Adobe 已使用，待清理', 'status', 'retired_adobe')
  await unused.waitFor({ state: 'detached' })
  await retired.waitFor()
  await filter('邮箱状态筛选', '全部状态', 'status', '')
  await unused.waitFor()
  await filter('平台占用筛选', 'Adobe 已占用', 'platform', 'adobe')
  await unused.waitFor({ state: 'detached' })
  await shared.waitFor()
  await filter('平台占用筛选', '全部占用', 'platform', '')
  await unused.getByRole('button', { name: '启用Adobe', exact: true }).click()
  await page.getByText('unused@example.test 已启用 Adobe 注册。', { exact: true }).waitFor()
  await unused.getByText('Adobe 允许注册', { exact: true }).waitFor()
  await page.getByText('启用 Adobe 注册', { exact: true }).waitFor()
  assert.deepEqual(JSON.parse(requests.filter(req => req.method === 'POST').at(-1).body), { status: 'available', platform: 'adobe' })
  await page.getByRole('button', { name: '核对现有邮箱', exact: true }).click()
  await page.getByText('核对完成：发现 3 个上游邮箱，关联 GPT 1 个、Grok 0 个、Adobe 2 个，修复历史占用 0 个。', { exact: true }).waitFor()
  assert.deepEqual(errors, [])
  assert.deepEqual(unknown, [], 'Unexpected API or external request')
  assert.deepEqual(requests.filter(req => req.method !== 'GET').map(req => req.path), ['/api/icloud/mailboxes/unused/status', '/api/icloud/mailboxes/unused/status', '/api/icloud/reconcile'])
  console.log(JSON.stringify({ ok: true, checks: ['three-platform-occupancy-and-registration-chips', 'adobe-filter-parameters', 'adobe-toggle-notices-and-events', 'adobe-reconcile-count', 'adobe-link-audit-label', 'dark-light-rendering'] }))
} catch (error) {
  console.log(JSON.stringify({ errors, unknown, body: (await page.locator('body').innerText()).slice(-6000) }))
  throw error
} finally {
  await browser.close()
  await new Promise(resolve => server.close(resolve))
}
