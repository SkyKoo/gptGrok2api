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
const page = await context.newPage(), errors = [], writes = [], dialogs = []
page.on('pageerror', error => errors.push(error.message))
page.on('dialog', async dialog => { dialogs.push(dialog.type()); await dialog.dismiss() })
const screenshot = async name => {
  if (!process.env.CFM_UI_SCREENSHOT_DIR) return
  await fs.mkdir(process.env.CFM_UI_SCREENSHOT_DIR, { recursive: true })
  await page.screenshot({ path: path.join(process.env.CFM_UI_SCREENSHOT_DIR, `${name}.png`), fullPage: true, animations: 'disabled' })
}
const waiting = { state: 'waiting', attempts: 1, max_attempts: 3, next_attempt_at: new Date(Date.now() + 300000).toISOString() }
const base = { email: 'fixture@example.test', status: 'registration_pending', stage: 'needs_manual', imported: false, verified: false, can_retry: false, can_retry_registration: false }
const jobs = [
  { ...base, id: 'waiting', can_reregister: true, error: 'adobe: verification_request_timeout', recovery_mode: 'retry_registration', auto_retry: waiting, review_hint: 'STALE_REVIEW_HINT' },
  { ...base, id: 'running', recovery_mode: 'resume_session', auto_retry: { ...waiting, state: 'running', attempts: 2 } },
  { ...base, id: 'exhausted', can_reregister: true, error: 'adobe: verification_dns_failed', auto_retry: { ...waiting, state: 'exhausted', attempts: 3 }, review_hint: 'STALE_EXHAUSTED_HINT' },
  { ...base, id: 'resume', can_reregister: true, stage: 'adobe_restoring_registration_page', error: 'adobe: recovery_page_observation_timeout', can_resume_registration: true, recovery_mode: 'resume_session', creation_outcome: 'accepted' },
  { ...base, id: 'retry', can_reregister: true, can_retry_registration: true, recovery_mode: 'retry_registration' },
  { ...base, id: 'unknown', can_reregister: true, creation_outcome: 'unknown', error: 'adobe: human_verification_required', recovery_mode: 'result_uncertain', auto_retry: { ...waiting, state: 'manual', reason: 'registration_result_uncertain' }, review_hint: 'STALE_UNKNOWN_HINT' },
  { ...base, id: 'unknown-synthetic', error: 'task: registration_result_uncertain', recovery_mode: 'result_uncertain', auto_retry: { ...waiting, state: 'manual', reason: 'registration_result_uncertain' } },
  { ...base, id: 'cancelled', status: 'cancelled', auto_retry: { ...waiting, state: 'manual', reason: 'cancelled' } },
  { ...base, id: 'inflight', status: 'running', stage: 'adobe_waiting_registration_form', recovery_mode: 'result_uncertain', review_hint: 'INFLIGHT_REVIEW_HINT' },
  { ...base, id: 'saved', status: 'import_pending', can_retry: true, can_reregister: true, recovery_mode: 'saved_result' },
  { ...base, id: 'challenge', error: 'adobe: human_verification_required', recovery_mode: 'resume_session', auto_retry: { ...waiting, state: 'manual', reason: 'human_verification_required' } },
  { ...base, id: 'parent', can_reregister: true, can_retry: true, can_retry_registration: true, retry_job_id: 'child', auto_retry: { ...waiting, state: 'superseded' }, review_hint: 'STALE_PARENT_HINT' },
  { ...base, id: 'child', parent_job_id: 'parent', root_job_id: 'root', recovery_mode: 'resume_session', auto_retry: waiting },
  { ...base, id: 'cooldown', can_reregister: true, reregister_not_before: waiting.next_attempt_at, recovery_mode: 'result_uncertain' },
  { ...base, id: 'cooldown-expiry', can_reregister: true, reregister_not_before: new Date(Date.now() + 60000).toISOString(), recovery_mode: 'retry_registration' },
]
let releaseManualRequest
const manualRequestGate = new Promise(resolve => { releaseManualRequest = resolve })
const schedule = { enabled: false, interval_minutes: 60 }
const register = { target: 'adobe', total: 1, threads: 1, mode: 'total', enabled: false, mail: { source_id: 'mail', providers: [{ id: 'mail', type: 'icloud_hme', enable: true, label: 'fixture' }] }, target_settings: Object.fromEntries(['openai', 'grok', 'adobe'].map(target => [target, { total: 1, threads: 1, mode: 'total', mail_source_id: 'mail', proxy: 'direct' }])), target_runtime: Object.fromEntries(['openai', 'grok', 'adobe'].map(target => [target, { enabled: false, stats: {}, logs: [], jobs: target === 'adobe' ? jobs : [], jobs_total: target === 'adobe' ? jobs.length : 0, registration_schedule: schedule }])) }
register.target_runtime.adobe.mailbox_cleanup_error = 'mailbox: reservation_release_failed'
register.target_runtime.adobe.logs = [
  { action: 'manual_reregistration', detail: '原邮箱启动新流程', time: new Date().toISOString(), level: 'info' },
  { action: 'adobe_restoring_registration_page', detail: '继续原账号验证', time: new Date().toISOString(), level: 'info' },
]
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
  else if (pathname === '/api/register/adobe/reregister') {
    if (req.postDataJSON().id === 'unknown') await manualRequestGate
    const original = jobs.find(item => item.id === req.postDataJSON().id)
    original.retry_job_id = original.id + '-new'
    original.can_reregister = false
    jobs.push({ ...base, id: original.retry_job_id, parent_job_id: original.id, root_job_id: original.root_job_id || original.id, status: 'queued', stage: 'queued' })
    data = { register }
  }
  else if (pathname === '/api/register' || ['/api/register/adobe/retry-registration', '/api/register/adobe/retry-result'].includes(pathname)) data = { register: { ...register, target: url.searchParams.get('target') || 'adobe' } }
  else if (pathname.endsWith('/sources')) data = { sources: [{ id: 'mail', type: 'icloud_hme', label: 'fixture', enable: true, status: 'available', capabilities: { openai: true, grok: true, adobe: true } }] }
  else data = { items: [], groups: [], pools: [], servers: [], accounts: [], total: 0, all_total: 0, summary: {}, config: {}, runtime_available: true }
  await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) })
})
const job = id => page.getByRole('group', { name: `注册任务 ${id}`, exact: true })
try {
  await page.clock.install()
  await page.goto(origin + '/#/register')
  await page.locator('.register-target-segmented').getByRole('button', { name: 'Adobe', exact: true }).click()
  await page.getByText('地区按本次注册出口自动选择，定位失败时使用美国；生日按 25–50 岁随机生成。验证码由所选邮箱来源自动接收，注册成功后自动加入 Adobe 账号池。', { exact: true }).waitFor()
  await page.getByText('手动重新注册 · 原邮箱启动新流程', { exact: true }).waitFor()
  await page.getByText('恢复原注册页面 · 继续原账号验证', { exact: true }).waitFor()
  await page.getByText(/部分失败任务的邮箱预占用尚未释放.*核对现有邮箱/).waitFor()
  await job('waiting').getByText('fixture@example.test · 注册失败', { exact: true }).waitFor()
  await job('waiting').getByText(/等待自动重新注册.*1\/3.*分钟后执行/).waitFor()
  await job('waiting').getByText('人机验证组件加载失败：请求超时。', { exact: true }).waitFor()
  for (const hint of ['STALE_REVIEW_HINT', 'STALE_EXHAUSTED_HINT', 'STALE_UNKNOWN_HINT', 'STALE_PARENT_HINT']) assert.equal(await page.getByText(hint, { exact: true }).count(), 0)
  await job('running').getByText(/正在自动继续账号验证.*2\/3/).waitFor()
  await job('running').getByText('fixture@example.test · 注册未完成', { exact: true }).waitFor()
  await job('exhausted').getByText('fixture@example.test · 注册失败', { exact: true }).waitFor()
  await job('exhausted').getByText('自动重试次数已用完（3/3），已停止自动重试。', { exact: true }).waitFor()
  await job('exhausted').getByText('人机验证组件加载失败：域名解析失败。', { exact: true }).waitFor()
  for (const id of ['running', 'challenge', 'unknown-synthetic', 'inflight', 'parent', 'child']) assert.equal(await job(id).getByRole('button').count(), 0)
  for (const id of ['waiting', 'exhausted', 'retry', 'unknown']) {
    assert.equal(await job(id).getByRole('button').count(), 1)
    assert.equal(await job(id).getByRole('button', { name: '手动重新注册', exact: true }).isEnabled(), true)
  }
  await job('cooldown').getByText(/邮箱注册冷却中.*本地时间.*剩余约 \d+ 分钟/).waitFor()
  assert.equal(await job('cooldown').getByRole('button', { name: '手动重新注册', exact: true }).isDisabled(), true)
  assert.equal(await job('cooldown-expiry').getByRole('button', { name: '手动重新注册', exact: true }).isDisabled(), true)
  assert.equal(await job('waiting').getByText(/邮箱注册冷却|Adobe 限流/).count(), 0)
  await job('inflight').getByText('fixture@example.test · 进行中', { exact: true }).waitFor()
  await job('inflight').getByText('等待 Adobe 注册表单 · 入库未完成 · 校验未通过', { exact: true }).waitFor()
  assert.equal(await job('inflight').getByText(/adobe_waiting_registration_form/).count(), 0)
  assert.equal(await page.getByText('INFLIGHT_REVIEW_HINT', { exact: true }).count(), 0)
  assert.equal(await job('inflight').getByText(/重试已暂停|暂停注册重试/).count(), 0)
  for (const id of ['unknown', 'unknown-synthetic']) {
    await job(id).getByText('fixture@example.test · 注册失败', { exact: true }).waitFor()
    await job(id).getByText('未收到 Adobe 创建结果，已停止自动重试。', { exact: true }).waitFor()
    await job(id).getByText('本次流程已结束 · 入库未完成 · 校验未通过', { exact: true }).waitFor()
    assert.equal(await job(id).getByRole('status').count(), 1)
  }
  await job('unknown').getByText('Adobe 人机验证未完成。', { exact: true }).waitFor()
  assert.equal(await job('unknown-synthetic').getByText('task: registration_result_uncertain', { exact: true }).count(), 0)
  await job('challenge').getByText('fixture@example.test · 注册未完成', { exact: true }).waitFor()
  await job('challenge').getByText('Adobe 人机验证未完成。', { exact: true }).waitFor()
  await job('challenge').getByText('当前错误不支持自动重试。', { exact: true }).waitFor()
  await job('cancelled').getByText('fixture@example.test · 已取消', { exact: true }).waitFor()
  await job('cancelled').getByText('已停止自动重试。', { exact: true }).waitFor()
  assert.equal(await page.getByText(/注册尚未完成|创建结果尚未确认|自动流程已暂停/).count(), 0)
  await job('child').getByText('补偿任务 child · 来源任务 parent', { exact: true }).waitFor()
  await job('child').getByText('首次任务 root', { exact: true }).waitFor()
  await job('parent').getByText('后续补偿任务 child', { exact: true }).waitFor()
  await job('parent').getByText('fixture@example.test · 已发起后续重试', { exact: true }).waitFor()
  assert.equal(await job('parent').getByRole('status').count(), 0)
  await job('saved').getByText('fixture@example.test · 等待入库', { exact: true }).waitFor()
  await job('resume').getByText('fixture@example.test · 注册未完成', { exact: true }).waitFor()
  await job('resume').getByText('结束于：恢复原注册页面 · 入库未完成 · 校验未通过', { exact: true }).waitFor()
  await job('resume').getByText('恢复原浏览器页面超时，未能继续账号验证。', { exact: true }).waitFor()
  assert.equal(await job('resume').getByText(/注册失败|创建失败/).count(), 0)
  assert.equal(await job('resume').getByRole('button', { name: '手动重新注册', exact: true }).count(), 0)
  assert.equal(await job('resume').getByRole('button', { name: '继续验证', exact: true }).isEnabled(), true)
  await screenshot('adobe-registration-failure-desktop')
  assert.deepEqual(writes, [])
  await page.clock.runFor(61000)
  assert.equal(await job('cooldown-expiry').getByRole('button', { name: '手动重新注册', exact: true }).isEnabled(), true)
  assert.deepEqual(writes, [], 'cooldown expiry must not start registration')
  const manualStarted = page.waitForRequest(req => req.url().endsWith('/api/register/adobe/reregister') && req.method() === 'POST')
  const manualResponse = page.waitForResponse(response => response.url().endsWith('/api/register/adobe/reregister') && response.request().method() === 'POST')
  await job('unknown').getByRole('button', { name: '手动重新注册', exact: true }).click()
  await manualStarted
  for (const id of ['unknown', 'retry', 'resume', 'saved']) assert.equal(await job(id).getByRole('button').isDisabled(), true)
  releaseManualRequest()
  await manualResponse
  await job('unknown').getByText('后续补偿任务 unknown-new', { exact: true }).waitFor()
  assert.equal(await job('unknown').getByRole('button').count(), 0)
  await job('unknown-new').getByText('补偿任务 unknown-new · 来源任务 unknown', { exact: true }).waitFor()
  for (const [id, label, endpoint] of [['resume', '继续验证', 'retry-registration'], ['retry', '手动重新注册', 'reregister'], ['saved', '重试入库 / 校验', 'retry-result']]) {
    const response = page.waitForResponse(result => result.url().endsWith('/api/register/adobe/' + endpoint) && result.request().method() === 'POST')
    await job(id).getByRole('button', { name: label, exact: true }).click()
    await response
  }
  assert.deepEqual(writes, [
    { path: '/api/register/adobe/reregister', body: { id: 'unknown' } },
    { path: '/api/register/adobe/retry-registration', body: { id: 'resume' } },
    { path: '/api/register/adobe/reregister', body: { id: 'retry' } },
    { path: '/api/register/adobe/retry-result', body: { id: 'saved' } },
  ])
  register.target_runtime.adobe.enabled = true
  await page.reload()
  await page.locator('.register-target-segmented').getByRole('button', { name: 'Adobe', exact: true }).click()
  await job('waiting').getByRole('button', { name: '手动重新注册', exact: true }).waitFor()
  for (const id of ['waiting', 'exhausted', 'cooldown-expiry']) assert.equal(await job(id).getByRole('button').isDisabled(), true)
  assert.equal(writes.length, 4, 'global running state must not start additional tasks')
  await page.setViewportSize({ width: 390, height: 844 })
  await job('child').getByText('首次任务 root', { exact: true }).waitFor()
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false)
  await screenshot('adobe-registration-failure-mobile')
  assert.deepEqual(errors, [])
  assert.deepEqual(dialogs, [])
  console.log('PASS: Adobe explicit manual reregistration, unknown results, mailbox cooldown expiry without auto-submit, busy/running locks, parent/child history, exact API routing, accepted/saved isolation, recovery-page diagnostics, mobile layout; no real upstream calls')
} finally { await browser.close(); await new Promise(resolve => server.close(resolve)) }
