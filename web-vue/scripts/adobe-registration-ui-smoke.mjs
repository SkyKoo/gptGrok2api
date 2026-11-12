// Local fixtures only: no real registration, mailbox allocation or challenge interaction.
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
const context = await browser.newContext({ viewport: { width: 1440, height: 1100 } })
await context.addInitScript(() => localStorage.setItem('chatgpt2api.adminKey', 'fixture-only'))
const page = await context.newPage(), errors = [], writes = []
page.on('pageerror', error => errors.push(error.message))
const waiting = { state: 'waiting', attempts: 1, max_attempts: 3, next_attempt_at: new Date(Date.now() + 300000).toISOString() }
const base = { email: 'fixture@example.test', status: 'registration_pending', stage: 'needs_manual', imported: false, verified: false, can_retry: false, can_retry_registration: false }
const jobs = [
  { ...base, id: 'waiting', recovery_mode: 'retry_registration', auto_retry: waiting, review_hint: 'STALE_REVIEW_HINT' },
  { ...base, id: 'running', recovery_mode: 'resume_session', auto_retry: { ...waiting, state: 'running', attempts: 2 } },
  { ...base, id: 'exhausted', auto_retry: { ...waiting, state: 'exhausted', attempts: 3 } },
  { ...base, id: 'resume', can_resume_registration: true, recovery_mode: 'resume_session' },
  { ...base, id: 'retry', can_retry_registration: true, recovery_mode: 'retry_registration' },
  { ...base, id: 'unknown', recovery_mode: 'result_uncertain', auto_retry: { ...waiting, state: 'manual', reason: 'registration_result_uncertain' } },
  { ...base, id: 'inflight', status: 'running', stage: 'adobe_waiting_registration_form', recovery_mode: 'result_uncertain', review_hint: 'INFLIGHT_REVIEW_HINT' },
  { ...base, id: 'saved', status: 'import_pending', can_retry: true, recovery_mode: 'saved_result' },
  { ...base, id: 'challenge', recovery_mode: 'resume_session', auto_retry: { ...waiting, state: 'manual', reason: 'human_verification_required' } },
  { ...base, id: 'parent', retry_job_id: 'child', auto_retry: { ...waiting, state: 'superseded' } },
  { ...base, id: 'child', parent_job_id: 'parent', root_job_id: 'root', recovery_mode: 'resume_session', auto_retry: waiting },
]
const schedule = { enabled: false, interval_minutes: 60 }
const register = { target: 'adobe', total: 1, threads: 1, mode: 'total', enabled: false, mail: { source_id: 'mail', providers: [{ id: 'mail', type: 'icloud_hme', enable: true, label: 'fixture' }] }, target_settings: Object.fromEntries(['openai', 'grok', 'adobe'].map(target => [target, { total: 1, threads: 1, mode: 'total', mail_source_id: 'mail', proxy: 'direct' }])), target_runtime: Object.fromEntries(['openai', 'grok', 'adobe'].map(target => [target, { enabled: false, stats: {}, logs: [], jobs: target === 'adobe' ? jobs : [], jobs_total: target === 'adobe' ? jobs.length : 0, registration_schedule: schedule }])) }
await context.route('**/*', async route => {
  const req = route.request(), url = new URL(req.url()), pathname = url.pathname
  if (url.origin !== origin) { await route.abort(); return }
  if (!pathname.startsWith('/api/') && !pathname.startsWith('/auth/') && !pathname.startsWith('/v1/') && !['/version', '/meta', '/health'].includes(pathname)) { await route.continue(); return }
  if (req.method() !== 'GET') writes.push({ path: pathname, body: req.postDataJSON() })
  let data
  if (pathname === '/auth/status') data = { authenticated: true, role: 'admin', name: 'Fixture' }
  else if (pathname === '/api/settings') data = { config: {} }
  else if (pathname === '/api/model-catalog') data = { models: [], chat_models: [], image_models: [], video_models: [] }
  else if (pathname === '/api/register/events') { await route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' }); return }
  else if (pathname === '/api/register' || ['/api/register/adobe/retry-registration', '/api/register/adobe/retry-result'].includes(pathname)) data = { register: { ...register, target: url.searchParams.get('target') || 'adobe' } }
  else if (pathname.endsWith('/sources')) data = { sources: [{ id: 'mail', type: 'icloud_hme', label: 'fixture', enable: true, status: 'available', capabilities: { openai: true, grok: true, adobe: true } }] }
  else data = { items: [], groups: [], pools: [], servers: [], accounts: [], total: 0, all_total: 0, summary: {}, config: {}, runtime_available: true }
  await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) })
})
const job = id => page.getByRole('group', { name: `注册任务 ${id}`, exact: true })
try {
  await page.goto(origin + '/#/register')
  await page.locator('.register-target-segmented').getByRole('button', { name: 'Adobe', exact: true }).click()
  await job('waiting').getByText(/等待自动重新注册.*1\/3.*分钟后.*独立浏览器环境/).waitFor()
  assert.equal(await page.getByText('STALE_REVIEW_HINT', { exact: true }).count(), 0)
  await job('running').getByText(/正在自动继续账号验证.*2\/3.*不再次创建账号/).waitFor()
  await job('exhausted').getByText(/已达上限（3\/3）/).waitFor()
  for (const id of ['waiting', 'running', 'exhausted', 'challenge', 'unknown', 'inflight', 'parent', 'child']) assert.equal(await job(id).getByRole('button').count(), 0)
  await job('inflight').getByText('fixture@example.test · 进行中', { exact: true }).waitFor()
  await job('inflight').getByText('等待 Adobe 注册表单 · 入库未完成 · 校验未通过', { exact: true }).waitFor()
  assert.equal(await job('inflight').getByText(/adobe_waiting_registration_form/).count(), 0)
  assert.equal(await page.getByText('INFLIGHT_REVIEW_HINT', { exact: true }).count(), 0)
  assert.equal(await job('inflight').getByText(/重试已暂停|暂停注册重试/).count(), 0)
  await job('unknown').getByText(/创建结果待核对，注册重试已暂停/).waitFor()
  await job('unknown').getByText(/创建结果尚未确认.*不会自动转入登录/).waitFor()
  await job('challenge').getByText(/后续仍须通过 Adobe 校验/).waitFor()
  await job('child').getByText('补偿任务 child · 来源任务 parent', { exact: true }).waitFor()
  await job('child').getByText('首次任务 root', { exact: true }).waitFor()
  await job('parent').getByText('后续补偿任务 child', { exact: true }).waitFor()
  for (const [id, label, endpoint] of [['resume', '继续验证', 'retry-registration'], ['retry', '重新注册', 'retry-registration'], ['saved', '重试入库 / 校验', 'retry-result']]) {
    const response = page.waitForResponse(result => result.url().endsWith('/api/register/adobe/' + endpoint) && result.request().method() === 'POST')
    await job(id).getByRole('button', { name: label, exact: true }).click()
    await response
  }
  assert.deepEqual(writes, [
    { path: '/api/register/adobe/retry-registration', body: { id: 'resume' } },
    { path: '/api/register/adobe/retry-registration', body: { id: 'retry' } },
    { path: '/api/register/adobe/retry-result', body: { id: 'saved' } },
  ])
  await page.setViewportSize({ width: 390, height: 844 })
  await job('child').getByText('首次任务 root', { exact: true }).waitFor()
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false)
  assert.deepEqual(errors, [])
  console.log('PASS: Adobe registration recovery capabilities, 3-attempt states, parent/child history, exact API routing, saved-result isolation, mobile layout; no real upstream calls')
} finally { await browser.close(); await new Promise(resolve => server.close(resolve)) }
