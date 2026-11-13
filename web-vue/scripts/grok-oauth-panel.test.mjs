import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import { compileScript, parse } from '@vue/compiler-sfc'
import ts from 'typescript'
import { computed, ref, watch } from 'vue'
import * as rowHelpers from '../src/views/accounts/grokOAuthCandidates.ts'

// Exercise the actual Vue setup logic with fixture-only API calls and timers.
const source = readFileSync(new URL('../src/views/accounts/GrokOAuthAccountsPanel.vue', import.meta.url), 'utf8')
const script = compileScript(parse(source).descriptor, { id: 'oauth-panel-test' })
const compiled = ts.transpileModule(script.content, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
const account = { id: 'oauth-a', email: 'a@example.test', status: 'active', models: [], has_access_token: true, has_refresh_token: true }
const candidate = { id: 'grok-a', email: account.email, login_status: 'active', oauth_status: 'pending', oauth_account_id: '', has_access_token: false, has_refresh_token: false }
const job = (fields = {}) => ({ id: 'job-a', status: 'running', stage: 'authorize', source_account_id: 'grok-a', created_at: 10, updated_at: 20, models: [], ...fields })
function panel(options = {}) {
  const scheduled = new Map(), notifications = [], calls = [], confirmations = []
  let timerId = 0, unmount
  const api = {
    qualityJobs: async () => ({ items: [] }),
    qualitySchedule: async () => ({ enabled: true, interval_hours: 24, pending: 0, running: 0, next_due_at: 0, last_checked_at: 0 }),
    saveQualitySchedule: async (enabled, interval_hours) => { calls.push(['schedule', enabled, interval_hours]); return { enabled, interval_hours, pending: 0, running: 0 } },
    checkQuality: async (...args) => { calls.push(['quality', ...args]); return { job_ids: ['quality-a'] } },
    list: async () => ({ items: [account] }),
    candidates: async () => ({ items: [candidate] }),
    getProtocolJob: async () => ({ job: job({ status: 'authorized', stage: 'completed', account: { ...account, cpa_sync: { status: 'synced', pool_id: 'cpa-a' } } }) }),
    startProtocol: async (...args) => { calls.push(['authorize', ...args]); return { job: job(), reused: false } },
    checkCPA: async (...args) => { calls.push(['check', ...args]); return { ok: true, account } },
    syncCPA: async (...args) => { calls.push(['sync', ...args]); return { ok: true, account } },
    queryQuota: async (...args) => { calls.push(['quota', ...args]); return { ok: true, account } },
    syncModels: async (...args) => { calls.push(['models', ...args]); return { account, models: [] } },
    setDisabled: async (...args) => { calls.push(['status', ...args]); return { updated: 1 } },
    remove: async (...args) => { calls.push(['remove', ...args]); return { removed: 1 } },
  }
  const imports = {
    vue: { computed, ref, watch, defineComponent: value => value, onMounted() {}, onBeforeUnmount(fn) { unmount = fn } },
    './grokOAuthCandidates': rowHelpers,
    '@/api/grokOAuthAccounts': { grokOAuthAccountsApi: api },
    '@/api/accountImports': { accountImportsApi: { listCPAPools: async () => ({ pools: [{ id: 'cpa-a', name: 'CPA A' }] }) } },
    '@/composables/useToast': { useToast: () => Object.fromEntries(['success', 'error', 'warning', 'info'].map(key => [key, message => notifications.push([key, message])])) },
    '@/composables/useConfirmDialog': { useConfirmDialog: () => ({ ask: async value => { confirmations.push(value); return options.confirmed ?? true } }) },
    '@/lib/errorMessage': { errorMessage: (error, fallback) => error?.message || fallback },
  }
  const module = { exports: {} }
  vm.runInNewContext(compiled, { module, exports: module.exports, require: name => imports[name] || {}, window: { setTimeout(fn) { const id = ++timerId; scheduled.set(id, fn); return id }, clearTimeout(id) { scheduled.delete(id) } }, console })
  const state = module.exports.default.setup({ open: true, embedded: true, focusAccountIds: options.focusAccountIds || [] }, { expose() {}, emit() {} })
  return { state, api, scheduled, notifications, calls, confirmations, unmount: () => unmount() }
}

test('row authorization pins the selected CPA target in the start request', async () => {
  const { state, api, calls } = panel()
  state.cpaPoolId.value = 'cpa-a'
  let finish
  api.startProtocol = async (...args) => { calls.push(['authorize', ...args]); return new Promise(resolve => { finish = resolve }) }
  const pending = state.startProtocolAuthorization({ ...candidate, source_account_id: candidate.id })
  state.cpaPoolId.value = 'cpa-b'
  finish({ job: job(), reused: false })
  await pending
  assert.deepEqual(calls, [['authorize', 'grok-a', 'cpa-a']])
  assert.equal(state.jobs.value['job-a'].source_account_id, 'grok-a')
})

test('quality actions use OAuth identity and scheduler validates persisted interval', async () => {
  const { state, calls } = panel()
  await state.checkQuality({ ...candidate, oauth_account_id: account.id })
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), ['quality', [account.id], false])
  state.qualityIntervalHours.value = 0
  await state.saveQualitySchedule()
  assert.equal(calls.filter(call => call[0] === 'schedule').length, 0)
  state.qualityIntervalHours.value = 12
  state.qualityScheduleEnabled.value = false
  await state.saveQualitySchedule()
  assert.deepEqual(calls.at(-1), ['schedule', false, 12])
  assert.equal(state.qualitySchedule.value.enabled, false)
})

test('saved OAuth with failed CPA delivery warns and never starts another authorization', async () => {
  const { state, api, calls, notifications } = panel()
  state.jobs.value = { 'job-a': job() }
  api.getProtocolJob = async () => ({ job: job({ status: 'authorized', stage: 'cpa_sync_failed', account: { ...account, cpa_sync: { status: 'failed' } } }) })
  await state.pollJobs()
  assert.equal(state.jobs.value['job-a'].status, 'authorized')
  assert.equal(notifications.some(([type]) => type === 'success'), false)
  assert.match(notifications.find(([type]) => type === 'warning')[1], /CPA 交付待处理/)
  assert.deepEqual(calls, [])
})

test('CPA-managed and legacy uncertain rows check their recorded target without uploading', async () => {
  const { state, calls } = panel()
  state.cpaPoolId.value = 'different-target'
  await state.handleCPA({ ...candidate, oauth_account_id: account.id, cpa_managed: true, cpa_sync: { status: 'pending', at: 20, pool_id: 'original-target' } })
  assert.deepEqual(calls, [['check', 'oauth-a', 'original-target']])
})

test('delivery retry uses saved OAuth, not a new authorization or token refresh', async () => {
  const { state, calls } = panel()
  state.cpaPoolId.value = 'cpa-a'
  await state.handleCPA({ ...candidate, oauth_account_id: account.id, has_access_token: true, has_refresh_token: true, cpa_sync: { status: 'failed' } })
  assert.deepEqual(calls, [['sync', 'oauth-a', 'cpa-a']])
})

test('poll errors stop after three attempts and manual refresh restores terminal state', async () => {
  const { state, api, scheduled } = panel()
  state.jobs.value = { 'job-a': job() }
  api.getProtocolJob = async () => { throw new Error('offline') }
  for (let i = 0; i < 3; i++) {
    scheduled.clear()
    await state.pollJobs()
  }
  assert.equal(scheduled.size, 0)
  assert.match(state.pollErrors.value['job-a'], /页面状态不代表后台仍在运行/)
  api.getProtocolJob = async () => ({ job: job({ status: 'failed' }) })
  await state.loadAccounts()
  assert.equal(state.jobs.value['job-a'].status, 'failed')
  assert.equal(state.pollErrors.value['job-a'], '')
})

test('late poll cannot replace a newer terminal read and unmount stops scheduling', async () => {
  const { state, api, scheduled, unmount } = panel()
  state.jobs.value = { 'job-a': job() }
  let finish
  api.getProtocolJob = () => new Promise(resolve => { finish = resolve })
  const old = state.readJob('job-a')
  api.getProtocolJob = async () => ({ job: job({ status: 'authorized', stage: 'completed' }) })
  await state.readJob('job-a')
  finish({ job: job() })
  await old
  assert.equal(state.jobs.value['job-a'].status, 'authorized')
  state.jobs.value = { 'job-a': job() }
  api.getProtocolJob = () => new Promise(resolve => { finish = resolve })
  const pending = state.pollJobs()
  unmount()
  finish({ job: job() })
  await pending
  assert.equal(scheduled.size, 0)
})

test('managed credentials cannot be refreshed through the details handler', async () => {
  const { state, calls } = panel()
  state.accounts.value = [{ ...account, cpa_managed: true, cpa_sync: { status: 'synced' } }]
  await state.refreshAccount(account.id)
  await state.syncModels(account.id)
  assert.deepEqual(calls, [])
})

test('account-page row selection scopes the unified panel without authorizing', () => {
  const { state, calls } = panel({ focusAccountIds: ['grok-a'] })
  state.candidates.value = [candidate, { ...candidate, id: 'grok-b' }]
  assert.deepEqual(Array.from(state.visibleRows.value, row => row.id), ['grok-a'])
  state.rowScopeIds.value = []
  assert.equal(state.visibleRows.value.length, 2)
  assert.deepEqual(calls, [])
})

test('local OAuth disable and enable retain confirmation and never refresh managed tokens', async () => {
  const { state, calls, confirmations } = panel()
  const row = { ...candidate, account: { ...account, cpa_managed: true } }
  await state.toggleAccount(row)
  await state.toggleAccount({ ...row, account: { ...row.account, status: 'disabled' } })
  assert.deepEqual(JSON.parse(JSON.stringify(calls)), [['status', ['oauth-a'], true], ['status', ['oauth-a'], false]])
  assert.equal(confirmations.length, 2)
  assert.match(confirmations[0].message, /CPA 独立管理/)
})

test('cancelled local deletion does nothing and confirmed deletion uses only local OAuth endpoint', async () => {
  const row = { ...candidate, account: { ...account, cpa_managed: true } }
  const cancelled = panel({ confirmed: false })
  await cancelled.state.removeAccount(row)
  assert.deepEqual(cancelled.calls, [])
  const accepted = panel()
  accepted.state.jobs.value = { 'job-a': job({ status: 'authorized', account }) }
  await accepted.state.removeAccount(row)
  assert.deepEqual(JSON.parse(JSON.stringify(accepted.calls)), [['remove', ['oauth-a']]])
  assert.equal(Object.keys(accepted.state.jobs.value).length, 0)
  assert.match(accepted.confirmations[0].message, /CPA 中的凭据保留/)
})

test('unmanaged account model discovery stays available in row details', async () => {
  const { state, calls } = panel()
  state.accounts.value = [account]
  await state.syncModels(account.id)
  assert.deepEqual(calls, [['models', 'oauth-a']])
})

test('failed delivery retry keeps its pinned CPA target after top selector changes', async () => {
  const { state, calls } = panel()
  state.cpaPoolId.value = 'cpa-b'
  await state.handleCPA({ ...candidate, oauth_account_id: account.id, has_access_token: true, has_refresh_token: true, cpa_sync: { status: 'failed', pool_id: 'cpa-a' } })
  assert.deepEqual(calls, [['sync', 'oauth-a', 'cpa-a']])
})

test('expired manual grant can be replaced by an explicit new row action', async () => {
  const { state, api, calls } = panel()
  state.cpaPoolId.value = 'cpa-a'
  state.device.value = { id: 'expired', expires_at: 1 }
  state.deviceTarget.value = { id: 'previous-row' }
  api.startDevice = async (...args) => { calls.push(['device', ...args]); return { id: 'fresh', expires_at: 9999999999 } }
  await state.startDeviceAuthorization({ ...candidate, source_account_id: candidate.id })
  assert.equal(state.device.value.id, 'fresh')
  assert.deepEqual(calls, [['device', 'grok-a', 'cpa-a']])
})

test('reopening a device grant restores its CPA target after another import dialog', async () => {
  const { state, api, calls } = panel()
  state.cpaPools.value = [{ id: 'cpa-a', name: 'CPA A' }, { id: 'cpa-b', name: 'CPA B' }]
  state.cpaPoolId.value = 'cpa-a'
  api.startDevice = async (...args) => { calls.push(['device', ...args]); return { id: 'fresh', expires_at: 9999999999 } }
  const row = { ...candidate, source_account_id: candidate.id }
  await state.startDeviceAuthorization(row)
  state.closeDialog()
  state.cpaPoolId.value = 'cpa-b'
  state.openImport()
  assert.equal(state.dialogPoolName.value, 'CPA B')
  state.closeDialog()
  await state.startDeviceAuthorization(row)
  assert.equal(state.dialogPoolId.value, 'cpa-a')
  assert.equal(state.dialogPoolName.value, 'CPA A')
  assert.deepEqual(calls, [['device', 'grok-a', 'cpa-a']])
})

test('legacy account row and bulk entries carry selected IDs to the unified panel without direct authorization', async () => {
  const source = readFileSync(new URL('../src/views/accounts/useGrokAccountsPage.ts', import.meta.url), 'utf8')
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
  const opened = [], apiCalls = []
  const imports = {
    vue: { computed, ref, watch() {} },
    '@/api/grokAccounts': { grokAccountsApi: new Proxy({}, { get: (_, action) => async (...args) => { apiCalls.push([action, ...args]); throw new Error('unexpected legacy API call') } }) },
    '@/composables/useToast': { useToast: () => ({ warning() {} }) },
    '@/composables/useConfirmDialog': { useConfirmDialog: () => ({ ask() { throw new Error('navigation must not ask to authorize') } }) },
    '@/composables/usePageQuery': { usePagedQuery: () => ({ total: ref(0), currentPage: ref(1), pageCount: ref(1), paginationSummary: ref('') }), usePageDebouncedAction: () => ({ schedule() {} }) },
    '@/composables/usePageRuntime': { usePageRuntime: () => ({ onActivate() {}, onShow() {}, canRun: ref(false), isActive: ref(false) }) },
  }
  const module = { exports: {} }
  vm.runInNewContext(compiled, { module, exports: module.exports, require: name => imports[name] || {} })
  const state = module.exports.useGrokAccountsPage({ onOpenOAuth: ids => opened.push(Array.from(ids)) })
  await state.authorizeOAuth([' grok-a ', 'grok-a'])
  state.accounts.value = ['grok-a', 'grok-b'].map(id => ({ id, status: 'active', has_password: true, has_sso: true }))
  state.selectedIds.value = ['grok-a', 'grok-b']
  await state.runBulkAction('authorize')
  assert.deepEqual(opened, [['grok-a'], ['grok-a', 'grok-b']])
  assert.deepEqual(apiCalls, [])
})
