import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import { compileScript, parse } from '@vue/compiler-sfc'
import ts from 'typescript'
import { computed, ref } from 'vue'

// Compile the actual panel so tests exercise its refresh/poll paths, with only
// network, timers and lifecycle hooks replaced. No live authorization is sent.
const source = readFileSync(new URL('../src/views/accounts/GrokOAuthAccountsPanel.vue', import.meta.url), 'utf8')
const { descriptor } = parse(source)
const script = compileScript(descriptor, { id: 'oauth-panel-test' })
const compiled = ts.transpileModule(script.content, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
function panel() {
  const scheduled = new Map()
  let timerId = 0, unmount
  const notifications = []
  const api = {
    list: async () => ({ items: [{ id: 'oauth-a', status: 'active', models: [], quota: { remaining_percent: 0 } }] }),
    candidates: async () => ({ items: [] }),
    getProtocolJob: async () => ({ job: { id: 'job-a', status: 'authorized', models: [] } }),
  }
  const imports = {
    vue: { computed, ref, defineComponent: value => value, onMounted() {}, onBeforeUnmount(fn) { unmount = fn } },
    '@/api/grokOAuthAccounts': { grokOAuthAccountsApi: api },
    '@/api/accountImports': { accountImportsApi: { listCPAPools: async () => ({ pools: [{ id: 'cpa-a', name: 'CPA' }] }) } },
    '@/composables/useToast': { useToast: () => Object.fromEntries(['success', 'error', 'warning', 'info'].map(key => [key, message => notifications.push([key, message])])) },
    '@/lib/errorMessage': { errorMessage: (error, fallback) => error?.message || fallback },
  }
  const module = { exports: {} }
  vm.runInNewContext(compiled, {
    module, exports: module.exports, require: name => imports[name] || {},
    window: { setTimeout(fn) { const id = ++timerId; scheduled.set(id, fn); return id }, clearTimeout(id) { scheduled.delete(id) } },
    console,
  })
  const state = module.exports.default.setup({ open: true, embedded: true }, { expose() {}, emit() {} })
  return { state, api, scheduled, notifications, unmount: () => unmount() }
}

test('manual refresh resolves a stale running job and loads the saved quota', async () => {
  const { state, scheduled } = panel()
  state.protocolJob.value = { id: 'job-a', status: 'running', models: [] }
  state.scheduleProtocolPoll()
  await state.loadAccounts()
  assert.equal(state.protocolJob.value.status, 'authorized')
  assert.equal(state.protocolRunning.value, false)
  assert.equal(state.accounts.value[0].quota.remaining_percent, 0)
  assert.equal(state.protocolPollError.value, '')
  assert.equal(state.cpaPoolId.value, 'cpa-a')
  assert.equal(scheduled.size, 0)
})

test('temporary polling errors recover without reauthorizing the account', async () => {
  const { state, api } = panel()
  state.protocolJob.value = { id: 'job-a', status: 'running', models: [] }
  let reads = 0
  api.getProtocolJob = async () => { if (++reads === 1) throw new Error('offline'); return { job: { id: 'job-a', status: 'authorized', models: [] } } }
  await state.pollProtocolAuthorization()
  assert.match(state.protocolPollError.value, /重新查询/)
  await state.pollProtocolAuthorization()
  assert.equal(state.protocolJob.value.status, 'authorized')
  assert.equal(state.protocolPollError.value, '')
  assert.equal(state.accounts.value.length, 1)
})

test('repeated polling errors stop after three attempts and manual refresh can recover', async () => {
  const { state, api, scheduled } = panel()
  state.protocolJob.value = { id: 'job-a', status: 'running', models: [] }
  api.getProtocolJob = async () => { throw new Error('offline') }
  await state.pollProtocolAuthorization()
  for (let i = 0; i < 2; i++) {
    const [id, callback] = scheduled.entries().next().value
    scheduled.delete(id)
    await callback()
  }
  assert.equal(scheduled.size, 0)
  assert.match(state.protocolPollError.value, /页面状态不代表后台仍在运行/)
  api.getProtocolJob = async () => ({ job: { id: 'job-a', status: 'failed', models: [] } })
  await state.loadAccounts()
  assert.equal(state.protocolJob.value.status, 'failed')
  assert.equal(state.protocolPollError.value, '')
})

test('a late response cannot overwrite a different job or restart polling after unmount', async () => {
  const { state, api, scheduled, unmount } = panel()
  state.protocolJob.value = { id: 'job-a', status: 'running', models: [] }
  let resolve
  api.getProtocolJob = () => new Promise(done => { resolve = done })
  const pending = state.pollProtocolAuthorization()
  state.protocolJob.value = { id: 'job-b', status: 'running', models: [] }
  resolve({ job: { id: 'job-a', status: 'authorized', models: [] } })
  await pending
  assert.equal(state.protocolJob.value.id, 'job-b')
  const afterUnmount = state.pollProtocolAuthorization()
  unmount()
  resolve({ job: { id: 'job-b', status: 'running', models: [] } })
  await afterUnmount
  assert.equal(scheduled.size, 0)
})
