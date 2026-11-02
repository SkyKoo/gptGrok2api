import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import vm from 'node:vm'
import ts from 'typescript'
import * as vue from 'vue'
import { parse, compileScript } from '@vue/compiler-sfc'
import { renderToString } from '@vue/server-renderer'

const root = path.resolve('src')
const clone = value => JSON.parse(JSON.stringify(value))
const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r }); return { promise, resolve } }
function harness(t) {
 const scheduled = new Map(), calls = [], notices = [], cache = new Map()
 let timer = 0
 const api = {}, revisions = new Map()
 const runtime = {
  nextRequest(key) { const value = (revisions.get(key) || 0) + 1; revisions.set(key, value); return value },
  isLatestRequest(key, value) { return revisions.get(key) === value },
  invalidateRequest(key) { this.nextRequest(key) },
 }
 const client = { get: async () => ({ groups: [] }) }
 const overrides = {
  vue,
  '@/api/client': { __esModule: true, default: client, getAuthToken: () => 'test-token' },
  '@/api/register': { registerApi: api },
 }
 function evaluate(source, file) {
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
  const module = { exports: {} }
  vm.runInNewContext(compiled, { module, exports: module.exports,
   require: name => load(name, file), console,
   setTimeout(fn) { const id = ++timer; scheduled.set(id, fn); return id },
   clearTimeout(id) { scheduled.delete(id) },
  }, { filename: file })
  return module.exports
 }
 function load(name, from = root + '/test.ts') {
  if (Object.hasOwn(overrides, name)) return overrides[name]
  const file = (name.startsWith('@/') ? path.join(root, name.slice(2)) : path.resolve(path.dirname(from), name)).replace(/\.ts$/, '') + '.ts'
  const canonical = '@/' + path.relative(root, file).replace(/\.ts$/, '')
  if (Object.hasOwn(overrides, canonical)) return overrides[canonical]
  if (!cache.has(file)) cache.set(file, evaluate(fs.readFileSync(file, 'utf8'), file))
  return cache.get(file)
 }
 const view = load('@/views/register/registerProviderView')
 const incoming = (target = 'openai', enabled = {}) => ({
  ...clone(view.defaultRegisterConfig), target,
  target_settings: {
   openai: { mode: 'total', total: 3, threads: 2, proxy: 'http://openai.example.test:8080', resin_proxy_url: '', mail_source_id: 'openai-mail' },
   grok: { mode: 'total', total: 1, threads: 1, proxy: 'direct', resin_proxy_url: '', mail_source_id: 'grok-mail' },
  },
  target_runtime: Object.fromEntries(['openai', 'grok'].map(name => [name, {
   enabled: Boolean(enabled[name]), stats: {}, logs: [{ message: name + '-log' }], jobs: [{ id: name + '-job' }], jobs_total: 1,
   registration_schedule: { enabled: false, interval_minutes: 60 },
  }])),
 })
 api.getConfig = async target => ({ register: incoming(target) })
 api.updateConfig = async (payload, target) => {
  calls.push(['save', target, clone(payload)])
  const next = incoming(target)
  Object.assign(next.target_settings[target], payload.target_settings[target])
  next.mail.source_id = payload.mail.source_id
  return { register: next }
 }
 for (const name of ['startLegacy', 'stopLegacy', 'resetLegacy']) api[name] = async target => { calls.push([name, target]); return { register: incoming(target, name === 'startLegacy' ? { [target]: true } : {}) } }
 let confirm = async () => true
 const scope = vue.effectScope()
 const state = scope.run(() => load('@/views/register/registerConfigRuntime').useRegisterConfigRuntime({
  runtime, confirm: options => confirm(options), notifySuccess: text => notices.push(text), notifyError: text => notices.push(text),
 }))
 state.applyConfig(incoming())
 t.after(() => scope.stop())
 return { state, api, incoming, calls, notices, scheduled, evaluate, overrides, setConfirm(fn) { confirm = fn } }
}

test('running channel can be left; config, logs, source, proxy and controls restore independently', async t => {
 const h = harness(t), s = h.state
 s.applyRuntimeConfig(h.incoming('openai', { openai: true }))
 s.setTarget('grok')
 assert.equal(s.config.value.target, 'grok'); assert.equal(s.config.value.enabled, false)
 assert.equal(s.config.value.logs[0].message, 'grok-log')
 assert.equal(s.config.value.jobs[0].id, 'grok-job')
 assert.equal(s.config.value.mail.source_id, 'grok-mail'); assert.equal(s.proxyMode.value, 'direct')
 assert.equal(s.isTaskEnabled(), true)
 s.setTarget('openai')
 assert.equal(s.config.value.enabled, true); assert.equal(s.config.value.total, 3)
 assert.equal(s.config.value.logs[0].message, 'openai-log')
 assert.equal(s.config.value.mail.source_id, 'openai-mail'); assert.equal(s.proxyMode.value, 'custom')
 await vue.nextTick()
 assert.equal(h.scheduled.size, 0); assert.equal(h.calls.length, 0)
})

test('in-flight save does not block another channel or overwrite its newer draft and running state', async t => {
 const h = harness(t), s = h.state, first = deferred()
 const originalSave = h.api.updateConfig
 h.api.updateConfig = async (payload, target) => target === 'openai' ? first.promise : originalSave(payload, target)
 s.config.value.total = 9
 const saving = s.flushAutosave()
 assert.equal(s.saving.value, true)
 s.setTarget('grok'); assert.equal(s.saving.value, false)
 s.config.value.total = 4; s.config.value.mail.source_id = 'another-grok-source'
 await s.flushAutosave()
 await s.toggleTask()
 assert.equal(s.config.value.enabled, true)
 const old = h.incoming('openai'); old.target_settings.openai.total = 9
 first.resolve({ register: old }); await saving
 assert.equal(s.config.value.target, 'grok'); assert.equal(s.config.value.total, 4)
 assert.equal(s.config.value.enabled, true, 'late OpenAI response rolled back Grok runtime')
 assert.equal(s.config.value.mail.source_id, 'another-grok-source')
 s.setTarget('openai'); assert.equal(s.config.value.total, 9)
 assert.equal(h.calls[0][2].target_settings.openai, undefined)
 assert.equal(h.calls[0][2].openai_free, undefined)
})

test('confirming start after tab switch still starts original channel, once', async t => {
 const h = harness(t), s = h.state, answer = deferred()
 h.setConfirm(() => answer.promise)
 const start = s.toggleTask()
 await s.toggleTask()
 s.setTarget('grok'); s.config.value.total = 6
 answer.resolve(true); await start
 assert.deepEqual(h.calls.filter(c => c[0] === 'startLegacy'), [['startLegacy', 'openai']])
 assert.equal(s.config.value.target, 'grok'); assert.equal(s.config.value.total, 6)
 assert.equal(s.config.value.enabled, false)
 s.setTarget('openai'); assert.equal(s.config.value.enabled, true)
})

test('stop and reset capture their own channel before confirmation', async t => {
 for (const [method, endpoint] of [['toggleTask', 'stopLegacy'], ['resetStats', 'resetLegacy']]) {
  const h = harness(t), s = h.state, answer = deferred()
  s.applyRuntimeConfig(h.incoming('openai', { openai: true, grok: true }))
  h.setConfirm(() => answer.promise)
  const request = s[method]()
  s.setTarget('grok'); answer.resolve(true); await request
  assert.equal(s.config.value.target, 'grok')
  assert.deepEqual(h.calls.filter(c => c[0] === endpoint), [[endpoint, 'openai']])
 }
})

test('old GET cannot undo a save that completed while the GET was in flight', async t => {
 const h = harness(t), s = h.state, read = deferred()
 h.api.getConfig = () => read.promise
 const loading = s.loadConfig()
 s.config.value.total = 11
 await s.flushAutosave()
 read.resolve({ register: h.incoming() }); await loading
 assert.equal(s.config.value.total, 11)
})

test('SSE updates both channels without flipping tabs or scheduling a config save', async t => {
 const h = harness(t), s = h.state
 s.setTarget('grok')
 s.applyRuntimeConfig(h.incoming('openai', { openai: true, grok: true }))
 assert.equal(s.config.value.target, 'grok'); assert.equal(s.config.value.enabled, true)
 s.applySchedule({ enabled: true, interval_minutes: 15 }, 'openai')
 assert.equal(s.config.value.registration_schedule.enabled, false)
 assert.equal(s.hasScheduledRegistration(), true)
 await vue.nextTick(); assert.equal(h.scheduled.size, 0)
})

test('rendered running-task settings leave channel fieldset enabled', async t => {
 const h = harness(t)
 const stub = vue.defineComponent({ inheritAttrs: false, setup(_, { slots, attrs }) { return () => vue.h('div', attrs, slots.default?.()) } })
 for (const name of ['@/views/proxy/ResinProxyFields.vue', '@/components/ai/FormSection.vue', '@/components/ui/GroupedSelectMenu.vue']) h.overrides[name] = { default: stub }
 h.overrides['@iconify/vue'] = { Icon: stub }
 h.overrides['nanocat-ui'] = { Button: stub, Checkbox: stub, Input: stub, SegmentedTabs: stub }
 h.overrides['@/api/accountImports'] = { accountImportsApi: {} }
 const filename = path.join(root, 'views/register/RegisterTaskSettingsPanel.vue')
 const { descriptor } = parse(fs.readFileSync(filename, 'utf8'), { filename })
 const script = compileScript(descriptor, { id: 'isolation', inlineTemplate: true })
 const component = h.evaluate(script.content, filename).default
 const config = h.incoming(); config.enabled = true
 const html = await renderToString(vue.createSSRApp(component, { config, proxyMode: 'direct', selectedProxyGroupId: '', customProxyInput: '', proxyGroupGroups: [], proxyHint: '' }))
 const fieldset = html.match(/<fieldset\b[^>]*class="register-target-segmented"[^>]*>/)?.[0]
 assert.ok(fieldset); assert.doesNotMatch(fieldset, /disabled/)
})

test('HTTP client sends the explicit channel for every task and schedule operation', async t => {
 const h = harness(t), sent = []
 h.overrides['@/api/client'].default.post = async (...args) => { sent.push(args); return {} }
 h.overrides['@/api/client'].default.get = async (...args) => { sent.push(args); return {} }
 const filename = path.join(root, 'api/register.ts')
 const api = h.evaluate(fs.readFileSync(filename, 'utf8'), filename).registerApi
 await api.getConfig('grok')
 await api.updateConfig({ total: 1 }, 'grok')
 await api.startLegacy('grok'); await api.stopLegacy('grok'); await api.resetLegacy('grok')
 await api.updateSchedule({ enabled: true, interval_minutes: 10 }, 'grok')
 for (const args of sent) assert.equal(args.at(-1).params.target, 'grok', args[0])
 assert.equal(sent.length, 6)
})

test('schedule confirmation saves and emits its captured channel after a tab switch', async t => {
 const h = harness(t), prepared = deferred(), sent = [], emitted = []
 h.overrides['nanocat-ui'] = {}
 h.overrides['@/composables/usePageRuntime'] = { usePageRuntime: () => ({ canRun: vue.ref(false), clearInterval() {}, setInterval() {} }) }
 h.api.updateSchedule = async (payload, target) => { sent.push([payload, target]); return { schedule: payload } }
 const filename = path.join(root, 'views/register/RegisterSchedulePanel.vue')
 const { descriptor } = parse(fs.readFileSync(filename, 'utf8'), { filename })
 const component = h.evaluate(compileScript(descriptor, { id: 'schedule-isolation' }).content, filename).default
 const props = { target: 'openai', configSaving: false, schedule: { enabled: false, interval_minutes: 60 }, prepare: target => { assert.equal(target, 'openai'); return prepared.promise } }
 const state = component.setup(props, { expose() {}, emit: (...args) => emitted.push(args) })
 state.enabled.value = true
 const request = state.save()
 props.target = 'grok'; prepared.resolve(true); await request
 assert.equal(sent[0][1], 'openai')
 assert.equal(emitted[0][2], 'openai')
})
