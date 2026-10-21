import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import ts from 'typescript'

// 加载真实 API 模块，仅替换 HTTP 传输，不连接真实账号或上游。
async function fixture() {
  const row = { account_ref: 'public-account-ref', access_token: 'fixture-real-token', email: 'person@example.test', two_factor_secret: '', status: '正常' }
  const calls = []
  let mode = ''
  let hideRow = false
  const publicRow = () => {
    const {access_token, two_factor_secret, ...rest} = row
    return {...rest, has_access_token: true, has_two_factor_secret: Boolean(two_factor_secret)}
  }
  const client = {
    async get(url) {
      assert.equal(url, '/api/accounts')
      return {items: hideRow ? [] : [publicRow()]}
    },
    async post(url, body) {
      calls.push({url, body})
      if (url === '/api/accounts/token') {
        assert.equal(body.access_token, row.account_ref)
        return {...row}
      }
      if (url === '/api/accounts/update') {
        assert.equal(body.access_token, row.account_ref)
        if (mode === 'fail') throw new Error('fixture storage failure')
        if (mode === 'empty') return {}
        const {access_token, ...changes} = body
        Object.assign(row, changes)
        return {item: publicRow()}
      }
      assert.equal(url, '/api/accounts')
      return {added: 0, skipped: 1, items: [publicRow()]}
    },
  }
  const key = `__accountEditFixture${Math.random().toString(16).slice(2)}`
  globalThis[key] = client
  const source = await readFile(new URL('../src/api/accounts.ts', import.meta.url), 'utf8')
  const js = ts.transpileModule(source.replace("import apiClient from './client'", `const apiClient = globalThis[${JSON.stringify(key)}]`), {
    compilerOptions: {target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext},
  }).outputText
  const {accountsApi} = await import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`)
  delete globalThis[key]
  return {api: accountsApi, row, calls, setMode: value => {mode = value}, hide: () => {hideRow = true}}
}

test('redacted list → credentials → edit → reload persists 2FA seed and other fields', async () => {
  const {api, row, calls} = await fixture()
  const account = (await api.list()).accounts[0]
  assert.ok(!account.access_token)
  assert.equal(account.has_two_factor_secret, false)
  const secrets = await api.getAccountSecrets(account.id)
  const saved = await api.upsert({...account, ...secrets, email: 'edited@example.test', login_password: 'fixture-password', two_factor_secret: 'JBSWY3DPEHPK3PXP'})
  assert.equal(saved.account.has_two_factor_secret, true)
  assert.equal(row.two_factor_secret, 'JBSWY3DPEHPK3PXP')
  assert.equal(row.email, 'edited@example.test')
  assert.equal(row.access_token, 'fixture-real-token')
  assert.equal(row.login_password, 'fixture-password')
  assert.equal(calls.filter(c => c.url === '/api/accounts').length, 0)
  assert.equal(calls.at(-1).body.access_token, 'public-account-ref')
  const reloaded = (await api.list()).accounts[0]
  assert.equal(reloaded.has_two_factor_secret, true)
  assert.equal((await api.getAccountSecrets(reloaded.id)).two_factor_secret, 'JBSWY3DPEHPK3PXP')
})

test('list refresh removing cached row cannot turn an edit into an add', async () => {
  const f = await fixture()
  const account = (await f.api.list()).accounts[0]
  const secrets = await f.api.getAccountSecrets(account.id)
  f.hide()
  await f.api.list()
  const saved = await f.api.upsert({id: account.id, ...secrets, two_factor_secret: 'JBSWY3DPEHPK3PXP'})
  assert.equal(saved.account.has_two_factor_secret, true)
  assert.equal(f.calls.at(-1).url, '/api/accounts/update')
})

test('duplicate add reports unsaved changes instead of success', async () => {
  const {api} = await fixture()
  await assert.rejects(api.upsert({access_token: 'fixture-real-token', two_factor_secret: 'JBSWY3DPEHPK3PXP'}), /已存在.*尚未保存/)
})

test('storage failures and missing update confirmation cannot produce success', async () => {
  for (const mode of ['fail', 'empty']) {
    const f = await fixture()
    const account = (await f.api.list()).accounts[0]
    const secrets = await f.api.getAccountSecrets(account.id)
    f.setMode(mode)
    await assert.rejects(f.api.upsert({...account, ...secrets, two_factor_secret: 'JBSWY3DPEHPK3PXP'}))
    assert.equal(f.row.two_factor_secret, '')
  }
})
