// Run after the production build. Every API and external request is intercepted.
import assert from 'node:assert/strict'
import http from 'node:http'
import fs from 'node:fs/promises'
import path from 'node:path'
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright')
const root = path.resolve('dist')
const server = http.createServer(async (req, res) => {
  try {
    const requested = new URL(req.url, 'http://localhost').pathname
    const filename = path.resolve(root, '.' + (requested === '/' ? '/index.html' : requested))
    if (!filename.startsWith(root + '/')) throw new Error('invalid path')
    const data = await fs.readFile(filename)
    res.setHeader('Content-Type', ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(filename)] || 'application/octet-stream')
    res.end(data)
  } catch { res.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'], executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined })
const context = await browser.newContext({ viewport: { width: 1540, height: 1060 } })
await context.addInitScript(() => localStorage.setItem('chatgpt2api.adminKey', 'fixture-only'))
const page = await context.newPage(), errors = [], unknown = [], writes = [], blocked = []
page.on('pageerror', error => errors.push(error.message))
const accounts = new Map()
const candidates = [
  { id: 'grok-new', email: 'new@example.test', oauth_status: 'pending', oauth_account_id: '', has_access_token: false, has_refresh_token: false },
  { id: 'grok-retry', email: 'retry@example.test', oauth_status: 'authorized', oauth_account_id: 'oauth-retry', has_access_token: true, has_refresh_token: true },
  { id: 'grok-managed', email: 'managed@example.test', oauth_status: 'authorized', oauth_account_id: 'oauth-managed', has_access_token: true, has_refresh_token: true },
  { id: 'grok-legacy', email: 'legacy@example.test', oauth_status: 'authorized', oauth_account_id: 'oauth-legacy', has_access_token: true, has_refresh_token: true },
].map(item => ({ ...item, login_status: 'active', updated_at: '2026-11-13T10:00:00Z' }))
function account(id, email, sync, managed = false) { return { id, email, status: 'active', models: [], has_access_token: true, has_refresh_token: true, expires_at: 1, updated_at: '2026-11-13T10:00:00Z', cpa_sync: sync, cpa_managed: managed } }
accounts.set('oauth-retry', account('oauth-retry', 'retry@example.test', { status: 'failed', pool_id: 'cpa-a', pool_name: 'O-CPA', error: 'CPA 暂时不可用' }))
accounts.set('oauth-managed', account('oauth-managed', 'managed@example.test', { status: 'synced', pool_id: 'cpa-a', pool_name: 'O-CPA', at: 100 }, true))
accounts.set('oauth-legacy', account('oauth-legacy', 'legacy@example.test', { status: 'pending', pool_id: 'cpa-a', pool_name: 'O-CPA', at: 100, error: '凭据已刷新，请重新同步' }, true))
accounts.set('oauth-orphan', account('oauth-orphan', 'orphan@example.test', { status: 'synced', pool_id: 'cpa-a', pool_name: 'O-CPA', at: 100 }, true))
let job, jobReads = 0
await context.route('**/*', async route => {
  const req = route.request(), url = new URL(req.url()), pathname = url.pathname, method = req.method()
  if (url.origin !== origin) { blocked.push(url.origin); await route.abort(); return }
  if (!pathname.startsWith('/api/') && !pathname.startsWith('/auth/') && !['/version', '/meta', '/health'].includes(pathname)) { await route.continue(); return }
  let data
  if (pathname === '/auth/status') data = { authenticated: true, role: 'admin', name: 'Fixture' }
  else if (pathname === '/api/settings') data = { config: {} }
  else if (pathname === '/api/third-party-apps') data = { apps: [] }
  else if (pathname === '/version') data = { version: 'fixture' }
  else if (pathname === '/api/cpa/pools') data = { pools: [{ id: 'cpa-a', name: 'O-CPA' }, { id: 'cpa-b', name: 'T-CPA' }] }
  else if (pathname === '/api/grok/oauth/accounts' && method === 'GET') data = { items: [...accounts.values()], total: accounts.size, available_models: [] }
  else if (pathname === '/api/grok/oauth/accounts/status' && method === 'POST') {
    const body = req.postDataJSON(); writes.push({ action: 'status', ...body })
    for (const id of body.ids) accounts.set(id, { ...accounts.get(id), status: body.disabled ? 'disabled' : 'active' })
    data = { updated: body.ids.length, disabled: body.disabled }
  } else if (pathname === '/api/grok/oauth/accounts' && method === 'DELETE') {
    const body = req.postDataJSON(); writes.push({ action: 'remove', ...body })
    for (const id of body.ids) accounts.delete(id)
    data = { removed: body.ids.length }
  }
  else if (pathname === '/api/grok/oauth/candidates') data = { items: candidates, total: candidates.length }
  else if (pathname === '/api/grok/oauth/protocol/start') {
    const body = req.postDataJSON(); writes.push({ action: 'authorize', ...body })
    assert.deepEqual(body, { account_id: 'grok-new', cpa_pool_id: 'cpa-a' })
    job = { id: 'job-new', status: 'running', stage: 'authorize', message: '正在授权', source_account_id: 'grok-new', cpa_pool_id: 'cpa-a', cpa_pool_name: 'O-CPA', created_at: 1000, updated_at: 1001, models: [] }
    candidates[0].oauth_status = 'running'; candidates[0].oauth_job_id = job.id
    data = { job, reused: false }
  } else if (pathname === '/api/grok/oauth/protocol/jobs/job-new') {
    jobReads++
    if (jobReads === 1) job = { ...job, stage: 'syncing_cpa', message: '正在同步 CPA', updated_at: 1002 }
    else {
      const saved = account('oauth-new', 'new@example.test', { status: 'synced', pool_id: 'cpa-a', pool_name: 'O-CPA', at: 1004 }, true)
      accounts.set(saved.id, saved)
      Object.assign(candidates[0], { oauth_status: 'authorized', oauth_account_id: saved.id, has_access_token: true, has_refresh_token: true })
      delete candidates[0].oauth_job_id
      job = { ...job, status: 'authorized', stage: 'completed', message: '授权并同步完成', account: saved, updated_at: 1004 }
    }
    data = { job }
  } else if (/\/accounts\/[^/]+\/cpa\/(check|sync)$/.test(pathname)) {
    const id = pathname.split('/')[5], action = pathname.endsWith('/check') ? 'check' : 'sync', body = req.postDataJSON()
    writes.push({ action, id, ...body })
    const previous = accounts.get(id)
    const saved = { ...previous, cpa_managed: true, cpa_sync: { status: 'synced', pool_id: body.cpa_pool_id, pool_name: body.cpa_pool_id === 'cpa-a' ? 'O-CPA' : 'T-CPA', at: 1005 } }
    accounts.set(id, saved); data = { ok: true, account: saved }
  } else if (pathname === '/api/grok/oauth/device/start') {
    const body = req.postDataJSON(); writes.push({ action: 'device', ...body })
    data = { id: 'fixture-device', user_code: 'TEST-CODE', verification_uri_complete: 'https://accounts.x.ai/oauth2/device?user_code=TEST-CODE', expires_at: 9999999999, interval: 5, status: 'pending' }
  } else if (pathname === '/api/grok/oauth/device/poll') {
    writes.push({ action: 'device-poll', ...req.postDataJSON() }); data = { status: 'authorized', account: accounts.get('oauth-retry'), models: [] }
  } else { unknown.push(`${method} ${pathname}`); data = {} }
  await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) })
})
const row = id => page.locator(`[data-account-row="${id}"]`)
async function selectPool(name) { await page.getByRole('button', { name: 'CPA 同步目标', exact: true }).click(); await page.getByRole('button', { name, exact: true }).click() }
try {
  await page.goto(origin + '/#/oauth?tab=xai')
  await row('grok-new').waitFor()
  assert.equal(await page.getByRole('table').count(), 1)
  assert.equal(await page.getByText('已授权凭据 · 额度与有效期', { exact: true }).count(), 0)
  assert.equal(await row('grok-new').getByRole('button', { name: '开始授权' }).isDisabled(), true)
  assert.equal(await row('grok-retry').getByRole('button', { name: '重试同步' }).isEnabled(), true, 'saved delivery target permits retry without a new top-level selection')
  assert.equal(await row('oauth:oauth-orphan').count(), 1, 'orphan OAuth credentials remain visible')
  assert.equal(await row('grok-managed').getByText('需重新授权', { exact: true }).count(), 0)
  await selectPool('O-CPA')
  await row('grok-new').getByRole('button', { name: '开始授权' }).click()
  await selectPool('T-CPA')
  await row('grok-new').getByText('正在同步 CPA', { exact: true }).first().waitFor()
  assert.equal(await row('grok-new').getByText('O-CPA', { exact: true }).count(), 1, 'target pinned while selection changes')
  await row('grok-new').getByText('已交付 CPA', { exact: true }).waitFor()
  await row('grok-retry').getByRole('button', { name: '重试同步' }).click()
  await row('grok-retry').getByText('已交付 CPA', { exact: true }).waitFor()
  await row('grok-legacy').getByRole('button', { name: '核对 CPA' }).click()
  await row('grok-legacy').getByText('已交付 CPA', { exact: true }).waitFor()
  assert.equal(writes.filter(item => item.action === 'authorize').length, 1)
  assert.deepEqual(writes.find(item => item.action === 'sync'), { action: 'sync', id: 'oauth-retry', cpa_pool_id: 'cpa-a' })
  assert.deepEqual(writes.find(item => item.action === 'check'), { action: 'check', id: 'oauth-legacy', cpa_pool_id: 'cpa-a' })
  assert.equal(await row('grok-managed').getByRole('button', { name: '额度与测试' }).count(), 0)
  await page.getByRole('button', { name: '导入凭据', exact: true }).click()
  await page.getByRole('dialog').getByText('导入后自动同步到 T-CPA', { exact: true }).waitFor()
  await page.getByRole('dialog').getByRole('button', { name: '取消', exact: true }).click()
  assert.equal(await page.locator('textarea:visible').count(), 0, 'no persistent authorization or import form below rows')
  await row('grok-retry').locator('summary').click()
  await row('grok-retry').getByRole('button', { name: '手动设备码', exact: true }).click()
  await page.getByRole('dialog').getByText('TEST-CODE', { exact: true }).waitFor()
  await page.getByRole('dialog').getByRole('button', { name: '检查授权', exact: true }).click()
  assert.deepEqual(writes.find(item => item.action === 'device'), { action: 'device', account_id: 'grok-retry', cpa_pool_id: 'cpa-b' })
  await page.reload()
  await row('grok-new').getByText('已交付 CPA', { exact: true }).waitFor()
  if (process.env.CFM_UI_SCREENSHOT_DIR) {
    await fs.mkdir(process.env.CFM_UI_SCREENSHOT_DIR, { recursive: true })
    for (const theme of ['light', 'dark']) {
      await page.emulateMedia({ colorScheme: theme })
      await page.waitForFunction(value => (document.documentElement.dataset.theme || 'light') === value, theme)
      await page.screenshot({ path: path.join(process.env.CFM_UI_SCREENSHOT_DIR, `xai-oauth-${theme}.png`), fullPage: true, animations: 'disabled' })
    }
  }
  await page.setViewportSize({ width: 390, height: 844 })
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'mobile page must not overflow')
  if (process.env.CFM_UI_SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.CFM_UI_SCREENSHOT_DIR, 'xai-oauth-mobile.png'), fullPage: true, animations: 'disabled' })
  await page.getByRole('table', { name: 'xAI OAuth 账号' }).evaluate(table => { table.parentElement.scrollLeft = table.parentElement.scrollWidth })
  const mobileActionBounds = await row('grok-managed').getByRole('button', { name: '核对 CPA', exact: true }).boundingBox()
  assert.ok(mobileActionBounds && mobileActionBounds.x >= 0 && mobileActionBounds.x + mobileActionBounds.width <= 390, 'mobile row actions remain reachable by horizontal scrolling')
  if (process.env.CFM_UI_SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.CFM_UI_SCREENSHOT_DIR, 'xai-oauth-mobile-actions.png'), fullPage: true, animations: 'disabled' })
  await page.setViewportSize({ width: 1540, height: 1060 })
  await row('grok-managed').locator('summary').click()
  await row('grok-managed').getByRole('button', { name: '禁用 OAuth', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '禁用 OAuth', exact: true }).click()
  await row('grok-managed').getByText('CFM OAuth 已禁用', { exact: true }).waitFor()
  await row('grok-managed').getByRole('button', { name: '启用 OAuth', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '启用 OAuth', exact: true }).click()
  await row('grok-managed').getByText('CFM OAuth 已禁用', { exact: true }).waitFor({ state: 'hidden' })
  await row('oauth:oauth-orphan').locator('summary').click()
  await row('oauth:oauth-orphan').getByRole('button', { name: '移除 OAuth', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '取消', exact: true }).click()
  assert.equal(writes.filter(item => item.action === 'remove').length, 0, 'cancel does not remove a local credential')
  await row('oauth:oauth-orphan').getByRole('button', { name: '移除 OAuth', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: '移除 OAuth', exact: true }).click()
  await row('oauth:oauth-orphan').waitFor({ state: 'hidden' })
  assert.deepEqual(writes.filter(item => item.action === 'status'), [{ action: 'status', ids: ['oauth-managed'], disabled: true }, { action: 'status', ids: ['oauth-managed'], disabled: false }])
  assert.deepEqual(writes.find(item => item.action === 'remove'), { action: 'remove', ids: ['oauth-orphan'] })
  assert.deepEqual(unknown, [])
  assert.deepEqual(errors, [])
  assert.ok(blocked.every(value => ['https://api.iconify.design', 'https://api.simplesvg.com', 'https://api.unisvg.com', 'https://fonts.googleapis.com', 'https://fonts.gstatic.com'].includes(value)), JSON.stringify(blocked))
  console.log(JSON.stringify({ passed: true, single_table: true, automatic_delivery: true, row_retry_without_reauthorization: true, legacy_check_without_upload: true, target_pinned: true, orphan_visible: true, managed_local_probe_absent: true, local_status_and_removal: true, reload: true, mobile_width: 390, mobile_actions_reachable: true, javascript_errors: errors.length, unknown_api_calls: unknown.length, writes: writes.length, upstream_requests: 0 }))
} finally { await browser.close(); await new Promise(resolve => server.close(resolve)) }
