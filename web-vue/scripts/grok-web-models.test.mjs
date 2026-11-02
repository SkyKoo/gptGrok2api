import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'
import * as vue from 'vue'

function harness() {
 const state = { payload: {
  chat_models: ['gpt-5-3', 'grok-web-expert'], image_models: ['gpt-image-2'],
  models: [{ id: 'gpt-5-3', name: 'GPT' }, { id: 'grok-web-expert', name: 'Grok Web · 专家（深度思考 · Grok 4.7）' }],
  model_aliases: { 'grok-4.20-expert': 'grok-web-expert' },
 }, fallback: false, saved: new Map([['chat-model', 'grok-4.20-expert']]) }
 const mocks = {
  vue,
  '@/api/models': { modelsApi: { catalog: async () => { if (state.fallback) throw new Error('old endpoint'); return state.payload }, list: async () => ({ data: state.payload.models }) } },
  '@/config/modelCatalog': { isImageModelId: id => id.includes('image'), resolveImageModels: () => ['gpt-image-2'] },
  '@/lib/studioMedia': { resolveStudioImageModel: model => model },
  '@/api/imageTasks': { DEFAULT_IMAGE_MODEL: 'gpt-image-2', DEFAULT_IMAGE_QUALITY: 'high', DEFAULT_IMAGE_SIZE: '1024x1024', isImageSizeSupportedByModel: () => true },
  '@/lib/preferences': { getStringPreference: (key, fallback) => state.saved.get(key) || fallback, setStringPreference: (key, value) => state.saved.set(key, value), preferenceKeys: { studioChatModel: 'chat-model' } },
 }
 function evaluate(file) {
  const source = ts.transpileModule(fs.readFileSync(file, 'utf8'), { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
  const module = { exports: {} }
  vm.runInNewContext(source, { module, exports: module.exports, require(name) { if (mocks[name]) return mocks[name]; throw new Error(`unexpected import ${name}`) }, console }, { filename: file })
  return module.exports
 }
 mocks['@/composables/useModelCatalog'] = evaluate('src/composables/useModelCatalog.ts')
 const form = evaluate('src/views/studio/studioModelFormRuntime.ts')
 const scope = vue.effectScope()
 const runtime = scope.run(() => form.useStudioModelFormRuntime({ settingsStore: { settings: {} }, hasReferences: () => false }))
 return { state, runtime, scope }
}

test('official Web descriptions update without changing API model IDs or image choices', async t => {
 const { state, runtime, scope } = harness(); t.after(() => scope.stop())
 await runtime.loadModelCatalog(true); await vue.nextTick()
 assert.deepEqual([...runtime.chatModelOptions.value], ['gpt-5-3', 'grok-web-expert'])
 assert.match(runtime.chatModelLabels.value['grok-web-expert'], /Grok 4\.7/)
 assert.equal(runtime.chatModelLabels.value['gpt-5-3'], undefined)
 assert.ok(runtime.imageModelOptions.value.includes('gpt-image-2'))
 state.payload = { ...state.payload, models: [{ id: 'grok-web-expert', name: 'Grok Web · 专家（官方新版本）' }] }
 await runtime.loadModelCatalog(true)
 assert.match(runtime.chatModelLabels.value['grok-web-expert'], /官方新版本/)
 assert.equal(runtime.chatModelOptions.value[1], 'grok-web-expert')
})
test('saved legacy Web selection migrates using backend aliases', async t => {
 const { state, runtime, scope } = harness(); t.after(() => scope.stop())
 assert.equal(runtime.chatModel.value, 'grok-4.20-expert')
 await runtime.loadModelCatalog(true); await vue.nextTick()
 assert.equal(runtime.chatModel.value, 'grok-web-expert')
 assert.equal(state.saved.get('chat-model'), 'grok-web-expert')
})
test('models API fallback preserves official names', async t => {
 const { state, runtime, scope } = harness(); t.after(() => scope.stop());state.fallback=true
 await runtime.loadModelCatalog(true)
 assert.match(runtime.chatModelLabels.value['grok-web-expert'], /Grok 4\.7/)
 assert.ok(runtime.chatModelOptions.value.includes('grok-web-expert'))
})
