// Exercise the built console against local fixtures. Only the relogin button can submit a task.
import assert from 'node:assert/strict'
import fs from 'node:fs/promises'
import path from 'node:path'
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright')
const browser = await chromium.launch({ headless: true })
try {
  const page = await browser.newPage()
  await page.addInitScript(() => localStorage.setItem('chatgpt2api.adminKey', 'fixture'))
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  const now = Math.floor(Date.now() / 1000)
  const account = { id: 'first', email: 'first@example.test', status: 'active', has_password: true, has_sso: true }
  const previous = { id: 'old', payload: { account_id: account.id }, email: account.email, created_at: now - 90, updated_at: now - 30, status: 'failed', error: 'grok_relogin_password_request_unobserved' }
  const other = { ...previous, id: 'other', email: 'other@example.test', payload: { account_id: 'other' }, error: 'grok_relogin_age_confirmation_required' }
  const jobs = [previous, other]
  let submits = 0
  let rejectNextList = false
  await page.route('**/*', async route => {
    const url = new URL(route.request().url())
    if (url.origin !== 'https://fixture.test') return route.abort()
    const pathname = url.pathname
    const method = route.request().method()
    if (pathname === '/' || pathname.startsWith('/assets/') || pathname.startsWith('/vendor/') || pathname === '/logo.svg') {
      const file = path.resolve('dist', pathname === '/' ? 'index.html' : pathname.slice(1))
      const mime = { '.html': 'text/html', '.css': 'text/css', '.js': 'application/javascript', '.svg': 'image/svg+xml' }
      return route.fulfill({ body: await fs.readFile(file), contentType: mime[path.extname(file)] || 'application/octet-stream' })
    }
    let data = { items: [], groups: [], pools: [], servers: [], accounts: [], summary: {}, page: 1, total: 0 }
    if (pathname === '/auth/status') data = { authenticated: true, role: 'admin' }
    if (pathname === '/api/settings') data = { config: {} }
    if (pathname === '/api/register/grok/accounts') data = { items: [account], total: 1, all_total: 1, page: 1, runtime_available: true }
    if (pathname === '/api/register/grok/accounts/relogin') {
      if (method === 'GET') {
        if (rejectNextList) {
          rejectNextList = false
          return route.fulfill({ status: 503, json: {} })
        }
        data = { items: jobs }
      } else {
        assert.equal(method, 'POST')
        assert.equal(route.request().postDataJSON().account_id, account.id)
        submits++
        const job = { ...previous, id: 'new', created_at: now, updated_at: now, status: 'running', result: { stage: 'signing_in' }, error: '' }
        jobs.push(job)
        data = { job }
      }
    }
    return route.fulfill({ json: data })
  })
  await page.goto('https://fixture.test/#/accounts?platform=grok')
  const panel = page.getByRole('region', { name: 'Grok 重新登录进度' })
  const toggle = panel.getByRole('button', { name: /重新登录进度/ })
  await toggle.waitFor()
  assert.equal(await toggle.getAttribute('aria-expanded'), 'false')
  await page.getByRole('button', { name: '重新登录', exact: true }).click()
  await panel.getByText('1 个进行中 · 2 个已结束').waitFor()
  assert.equal(await toggle.getAttribute('aria-expanded'), 'false')
  await toggle.click()
  await panel.getByText('点击提交后仍停在密码页，未观察到发往认证服务的 POST 请求').waitFor()
  await panel.getByText('需确认年龄', { exact: true }).waitFor()
  await panel.getByText('请用该账号在 Grok 官网完成年龄确认，再回到这里重新登录').waitFor()
  assert.equal(await panel.getByRole('article', { name: account.email, exact: true }).count(), 2)
  await panel.locator('[aria-current="step"]').filter({ hasText: '正在登录' }).waitFor()
  rejectNextList = true
  await panel.getByText('进度暂时无法更新，正在重新连接。').waitFor()
  Object.assign(jobs.at(-1), { status: 'completed', result: { session_updated: true, text_quota: true, image_quota: true, video_quota: true } })
  await panel.getByText('会话已更新，文字、图片和视频额度已刷新').waitFor()
  await panel.getByText('0 个进行中 · 3 个已结束').waitFor()
  await panel.getByRole('button', { name: '收起 other@example.test 的登录记录' }).click()
  await page.reload()
  await panel.getByText('0 个进行中 · 2 个已结束').waitFor()
  assert.equal(await toggle.getAttribute('aria-expanded'), 'false')
  assert.equal(submits, 1)
  assert.deepEqual(errors, [])
  console.log('Grok relogin panel passed: collapse, history, stages, reconnection, completion and dismiss.')
} finally {
  await browser.close()
}
