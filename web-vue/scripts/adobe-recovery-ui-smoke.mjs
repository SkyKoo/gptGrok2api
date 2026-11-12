// Isolated API fixtures: no real Adobe login, registration or generation.
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
    if (!file.startsWith(root + '/')) throw Error('invalid path')
    const data = await fs.readFile(file)
    res.setHeader('Content-Type', ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream')
    res.end(data)
  } catch { res.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] })
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } })
await context.addInitScript(() => localStorage.setItem('chatgpt2api.adminKey', 'fixture-only'))
const page = await context.newPage(), errors = [], writes = [], unexpected = []
page.on('pageerror', error => errors.push(error.message))
const base = { provider: 'adobe', disabled: false, login_status: 'valid', login_updated_at: Math.floor(Date.now() / 1000), has_profile: true, media_session_status: 'refreshed', expires_at: Math.floor(Date.now() / 1000) + 3600, credential_revision: 3, quota: { status: 'unknown' } }
const held = { ...base, id: 'held', account_id: 'held', email: 'held@example.test', status: 'submission_unknown', submission_unknown: true, task_status: 'submission_unknown', task_key: 'image:old-task', in_flight: true, pending: true, last_error: 'adobe_submission_unknown', can_query_quota: true, can_resolve_submission: false, task_reconciliation: { state:'waiting', release_after:new Date(Date.now()+1800000).toISOString(), next_check_at:new Date(Date.now()+60000).toISOString() } }
const accounts = [held, { ...base, id: 'running', account_id: 'running', email: 'running@example.test', status: 'busy', task_status: 'running', in_flight: true, can_query_quota: false, can_resolve_submission: false }]
let accountReads = 0
const records = []
await context.route('**/*', async route => {
  const req = route.request(), url = new URL(req.url()), pathname = url.pathname
  if (url.origin !== origin) { await route.abort(); return }
  if (!pathname.startsWith('/api/') && !pathname.startsWith('/auth/') && !pathname.startsWith('/v1/') && !['/version', '/meta', '/health'].includes(pathname)) { await route.continue(); return }
  let data, status = 200
  if (req.method() !== 'GET' && pathname !== '/api/register/grok/accounts/runtime/snapshot') writes.push({ path: pathname, body: req.postDataJSON() })
  if (pathname === '/auth/status') data = { authenticated: true, role: 'admin', name: 'Fixture' }
  else if (pathname === '/api/settings') data = { config: {} }
  else if (pathname === '/api/model-catalog') data = { models: [], chat_models: [], image_models: [], video_models: [] }
  else if (pathname === '/api/adobe/accounts' && req.method() === 'GET') { accountReads++; data = { items: accounts, total: accounts.length, all_total: accounts.length, page: 1, page_size: 25 } }
  else if (pathname === '/api/adobe/accounts/refresh') {
    assert.deepEqual(req.postDataJSON(), { ids: ['held'], credentials: false })
    data = { results: [{ id: 'held', ok: true, account: held }] }
  } else if (pathname === '/api/adobe/accounts/submission-resolutions' && req.method() === 'GET') {
    assert.equal(url.searchParams.get('id'),'held'); data = { items:records }
  } else if (req.method() !== 'GET' && pathname !== '/api/register/grok/accounts/runtime/snapshot') { unexpected.push(pathname); data = {} }
  else data = { items: [], groups: [], pools: [], servers: [], accounts: [], total: 0, all_total: 0, page: 1, page_size: 25, summary: {}, config: {}, runtime_available: true }
  await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(data) })
})
const row = email => page.getByRole('row').filter({ hasText: email })
async function screenshot(name) {
  if (!process.env.CFM_UI_SCREENSHOT_DIR) return
  await fs.mkdir(process.env.CFM_UI_SCREENSHOT_DIR, { recursive: true })
  await page.screenshot({ path: path.join(process.env.CFM_UI_SCREENSHOT_DIR, name + '.png'), fullPage: true })
}
try {
  await page.goto(origin + '/#/accounts?platform=adobe')
  const unknown = row('held@example.test'), running = row('running@example.test')
  await unknown.getByText('登录态有效', { exact: true }).waitFor()
  await unknown.getByText('系统处理中', { exact: true }).waitFor()
  assert.equal(await page.getByText('adobe_submission_unknown', { exact: true }).count(), 0)
  assert.equal(await unknown.getByRole('button', { name: '刷新额度', exact: true }).isDisabled(), false)
  assert.equal(await running.getByRole('button', { name: '刷新额度', exact: true }).isDisabled(), true)
  assert.equal(await running.getByRole('button', { name: '核对任务', exact: true }).count(), 0)
  await screenshot('adobe-recovery-desktop')
  await unknown.getByRole('button', { name: '刷新额度', exact: true }).click()
  await page.getByRole('status').filter({ hasText: '操作已完成' }).waitFor()
  assert.equal(held.in_flight, true)
  await page.getByRole('checkbox', { name: '选择 held@example.test' }).check()
  await page.getByRole('button', { name: '批量操作', exact: true }).click()
  assert.equal(await page.locator('.floating-action-menu-panel').getByRole('button', { name: '刷新额度', exact: true }).isDisabled(), false)
  await page.keyboard.press('Escape')
  assert.equal(await page.getByRole('button', { name: '核对任务', exact: true }).count(), 0)
  await unknown.getByRole('button', { name: '处理进度', exact: true }).click()
  let dialog = page.getByRole('dialog')
  await dialog.getByText('系统处理中，无需手动确认。', { exact: true }).waitFor()
  assert.equal(await dialog.getByRole('textbox').count(), 0)
  assert.equal(await dialog.getByRole('checkbox').count(), 0)
  assert.equal(await dialog.getByRole('button', { name: '确认并解除占用', exact: true }).count(), 0)
  await screenshot('adobe-automatic-progress-desktop')
  await dialog.getByRole('button', { name: '关闭', exact: true }).last().click()
  assert.equal(writes.length, 1)
  await page.getByRole('button', { name: '卡片视图', exact: true }).click()
  await page.setViewportSize({ width: 390, height: 844 })
  const card = page.getByRole('article', { name: 'Adobe 账号 held@example.test', exact: true })
  await card.getByText('系统处理中', { exact: true }).waitFor()
  await card.getByRole('button', { name: '处理进度', exact: true }).click()
  dialog = page.getByRole('dialog')
  await dialog.getByText('系统处理中，无需手动确认。', { exact: true }).waitFor()
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false)
  await screenshot('adobe-automatic-progress-mobile')
  const readsBefore = accountReads
  records.push({ task_key:'image:old-task', resolved_at:new Date().toISOString(), reviewed_by:'system', method:'automatic_no_handle', outcome:'unknown', quota_status:'known', reason:'等待期限已结束，系统已查询额度并结束本地占用；原生成和扣费结果仍未知。', task_record_state:'terminal' })
  Object.assign(held, { task_status:'settling', status:'active', submission_unknown:false, task_key:'', pending:true, in_flight:false, task_reconciliation:undefined })
  await dialog.getByText('系统自动处理', { exact: true }).waitFor({timeout:15000})
  assert.ok(accountReads>readsBefore)
  await dialog.getByText('本地占用已结束；原任务生成及扣费结果仍未知。', { exact: true }).waitFor()
  assert.equal(await dialog.getByText('系统处理中，无需手动确认。', { exact: true }).count(), 0)
  await screenshot('adobe-automatic-history-mobile')
  await dialog.getByRole('button', { name:'关闭', exact:true }).last().click()
  await card.getByText('等待结算', { exact:true }).waitFor()
  assert.equal(await card.getByRole('button', { name:'处理进度', exact:true }).count(),0)
  await card.getByRole('button', { name:'更多', exact:true }).click()
  await page.locator('.floating-action-menu-panel').getByRole('button', { name:'查看处理记录', exact:true }).click()
  await page.getByRole('dialog').getByText('系统自动处理', { exact:true }).waitFor()
  assert.equal(writes.length,1)
  assert.deepEqual(errors, []); assert.deepEqual(unexpected, [])
  console.log('PASS: automatic reconciliation progress and read-only history, GET-only polling, preserved unknown outcome, desktop/mobile layouts; no generation, relogin or release requests')
} catch (error) { console.error(JSON.stringify({ errors, writes, body: (await page.locator('body').innerText()).slice(0, 5000) })); throw error }
finally { await browser.close(); await new Promise(resolve => server.close(resolve)) }
