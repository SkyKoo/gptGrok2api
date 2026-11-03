import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'
import * as media from '../src/lib/studioMedia.ts'

function load(path, dependencies = {}, globals = {}) {
  const source = fs.readFileSync(new URL(path, import.meta.url), 'utf8')
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS } }).outputText
  const exports = {}
  vm.runInNewContext(js, { exports, require: name => dependencies[name] || {}, ...globals })
  return exports
}

function referenceRuntime(validateCount, readerOverride) {
  let id = 0
  class Reader {
    readAsDataURL(file) {
      queueMicrotask(() => {
        this.result = `data:image/png;base64,${file.name}`
        this.onload()
      })
    }
  }
  const { useStudioReferenceRuntime } = load('../src/views/studio/studioReferenceRuntime.ts', {
    vue: { ref: value => ({ value }) },
    './studioConversationState': { createStudioId: () => String(++id) },
  }, { FileReader: readerOverride || Reader })
  return useStudioReferenceRuntime({ validateCount })
}
const files = count => Array.from({ length: count }, (_, i) => ({ name: `ref${i}.png`, type: 'image/png', size: 1 }))

test('reference limits follow mode and provider without affecting chat or search', () => {
  for (const model of ['grok-imagine-image-pro', 'grok-imagine-image-edit']) {
    for (const mode of ['image']) {
      assert.equal(media.studioReferenceLimit(mode, model), 5)
      assert.doesNotThrow(() => media.validateStudioReferenceCount(mode, model, 5))
      assert.throws(() => media.validateStudioReferenceCount(mode, model, 6), /Grok 生图最多支持 5 张/)
    }
  }
  assert.equal(media.studioReferenceLimit('video', 'gpt-image-2'), 7)
  assert.equal(media.studioReferenceLimit('image', 'gpt-image-2'), 8)
  assert.equal(media.studioReferenceLimit('chat', 'grok-imagine-image-edit'), 8)
  assert.equal(media.studioReferenceLimit('search', 'grok-imagine-image-edit'), 8)
})

test('switching seven video references to Grok image retains every original file and preview', async () => {
  let mode = 'video'
  const runtime = referenceRuntime(count => media.validateStudioReferenceCount(mode, 'grok-imagine-image-pro', count))
  const originals = files(7)
  assert.equal(await runtime.append(originals), true)
  mode = 'image'
  assert.equal(runtime.selectedFiles().length, 7)
  originals.forEach((file, i) => assert.equal(runtime.selectedFiles()[i], file))
  assert.equal(runtime.messageReferenceImages().length, 7)
  assert.match(media.studioReferenceCountError(mode, 'grok-imagine-image-edit', runtime.selectedFiles().length), /当前 7 张/)
  runtime.remove(6); runtime.remove(5)
  assert.equal(media.studioReferenceCountError(mode, 'grok-imagine-image-edit', runtime.selectedFiles().length), '')
})

test('oversized batches reject atomically and concurrent additions cannot exceed the limit', async () => {
  const runtime = referenceRuntime(count => media.validateStudioReferenceCount('image', 'grok-imagine-image-edit', count))
  await runtime.append(files(4))
  await assert.rejects(runtime.append(files(2)), /最多支持 5 张/)
  assert.equal(runtime.selectedFiles().length, 4)
  assert.equal(runtime.references.value.length, 4)
  const results = await Promise.allSettled([runtime.append(files(1)), runtime.append(files(1))])
  assert.equal(results[0].status, 'fulfilled')
  assert.equal(results[1].status, 'rejected')
  assert.equal(runtime.selectedFiles().length, 5)
})

test('a mode switch during preview loading revalidates the entire batch', async () => {
  let finish
  class SlowReader {
    readAsDataURL() { finish = () => { this.result = 'data:image/png;base64,eA=='; this.onload() } }
  }
  let mode = 'video'
  const runtime = referenceRuntime(count => media.validateStudioReferenceCount(mode, 'grok-imagine-image-edit', count), SlowReader)
  const pending = runtime.append(files(6))
  for (let i = 0; i < 6; i++) {
    await new Promise(resolve => setImmediate(resolve))
    finish()
  }
  mode = 'image'
  await assert.rejects(pending, /最多支持 5 张/)
  assert.equal(runtime.selectedFiles().length, 0)
})

test('using generated output as a reference validates the destination image mode', async () => {
  const runtime = referenceRuntime(count => media.validateStudioReferenceCount('video', '', count))
  await runtime.append(files(5))
  await assert.rejects(runtime.append(files(1), count => media.validateStudioReferenceCount('image', 'grok-imagine-image-edit', count)), /最多支持 5 张/)
  assert.equal(runtime.selectedFiles().length, 5)
})

test('video form includes all seven files and rejects eight', () => {
  const references = files(7).map(file => new File(['image'], file.name, { type: file.type }))
  assert.equal(media.buildVideoForm('test', media.DEFAULT_VIDEO_OPTIONS, references).getAll('input_reference').length, 7)
  assert.throws(() => media.buildVideoForm('test', media.DEFAULT_VIDEO_OPTIONS, [...references, references[0]]), /最多支持 7 张/)
})

test('over-limit send, edited send and explicit edit preserve prompt and do not create messages or requests', async () => {
  const { useStudioSendRuntime } = load('../src/views/studio/studioSendRuntime.ts', {
    '@/lib/studioMedia': media,
    './studioRequestView': { studioErrorMessage: error => error.message },
  })
  for (const action of ['send', 'edited', 'explicit']) {
    const errors = []
    const runtime = referenceRuntime(() => {})
    await runtime.append(files(7))
    const composerRuntime = {
      composerText: { value: 'paper plane' }, isSending: { value: false },
      editingMessageId: { value: action === 'edited' ? 'edit' : '' }, composeMode: { value: 'image' },
    }
    const mustNotRun = () => assert.fail('request or conversation mutation before reference validation')
    const send = useStudioSendRuntime({
      composerRuntime, referenceRuntime: runtime, imageForm: { model: 'grok-imagine-image-edit' },
      chatStreamRuntime: { isStreaming: { value: false } },
      messageRuntime: { findMessage: () => ({ conversation: {}, message: { role: 'user' } }), addMessage: mustNotRun },
      hooks: { ensureConversation: mustNotRun }, toast: { error: message => errors.push(message) },
    })
    if (action === 'explicit') await send.sendImageEditRequest({ prompt: 'edit', files: files(6) })
    else await send.sendMessage()
    assert.equal(errors.length, 1)
    assert.match(errors[0], /最多支持 5 张/)
    assert.equal(composerRuntime.composerText.value, 'paper plane')
    assert.equal(runtime.selectedFiles().length, 7)
  }
})

test('direct image task submission validates reference count before calling the API', async () => {
  const { createStudioImageTask } = load('../src/views/studio/studioRequestView.ts', {
    '@/lib/studioMedia': media,
    '@/api/imageTasks': { DEFAULT_IMAGE_MODEL: 'gpt-image-2' },
  })
  await assert.rejects(createStudioImageTask({ prompt: 'edit', files: files(6), imageForm: { model: 'grok-imagine-image-edit' } }), /最多支持 5 张/)
})
