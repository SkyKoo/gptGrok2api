import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'
import { resolveStudioImageModel, buildVideoForm } from '../src/lib/studioMedia.ts'
test('saved legacy image models migrate to Pro and editing switches back to Pro',()=>{
 for (const old of ['grok-imagine-image','grok-imagine-image-lite','grok-imagine-image-edit']) {
  assert.equal(resolveStudioImageModel(old,false,['gpt-image-2','grok-imagine-image-pro']),'grok-imagine-image-pro')
 }
 assert.equal(resolveStudioImageModel('grok-imagine-image-pro',true,['gpt-image-2','grok-imagine-image-edit']),'grok-imagine-image-edit')
})
test('video options preserve the requested resolution',()=>{
 for (const resolution of ['480p','720p']) assert.equal(buildVideoForm('test',{seconds:6,size:'720x1280',resolution},[]).get('resolution_name'),resolution)
})

async function imageSizes() {
 const source=await readFile(new URL('../src/api/imageTasks.ts',import.meta.url),'utf8')
 const js=ts.transpileModule(source.replace("import apiClient from './client'",'const apiClient = {}'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
 return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`)
}
test('Grok generation keeps Pro 1K ratios and hides unverified 2K/4K choices',async()=>{
 const {resolveImageSizePresets,isImageSizeSupportedByModel}=await imageSizes()
 const presets=resolveImageSizePresets('grok-imagine-image-pro')
 for(const ratio of ['1:1','2:3','3:2','9:16','16:9']){
  assert.ok(presets.some(p=>p.ratio===ratio&&p.resolution==='1K'),`${ratio} 1K`)
 }
 assert.equal(isImageSizeSupportedByModel('2560x1440','grok-imagine-image-pro'),false)
 assert.equal(isImageSizeSupportedByModel('2048x2048','grok-imagine-image-pro'),false)
 assert.equal(isImageSizeSupportedByModel('auto','grok-imagine-image-pro'),true)
 assert.equal(isImageSizeSupportedByModel('2048x2048','codex-gpt-image-2'),true)
 assert.equal(isImageSizeSupportedByModel('3840x2160','grok-imagine-image-pro'),false)
 assert.equal(isImageSizeSupportedByModel('2048x2048','grok-imagine-image-edit'),false)
 assert.deepEqual(resolveImageSizePresets('grok-imagine-image-edit').map(p=>p.value),['auto','1024x1024'])
})

// Verify that all controls reach the multipart request without fixed defaults.
test('video settings serialize duration, ratio, resolution and style', () => {
 const form = buildVideoForm('paper plane', { seconds: 6, size: '1792x1024', resolution: '480p', preset: 'fun' }, [])
 assert.equal(form.get('seconds'), '6')
 assert.equal(form.get('size'), '1792x1024')
 assert.equal(form.get('resolution_name'), '480p')
 assert.equal(form.get('preset'), 'fun')
 for (const options of [{seconds: 5}, {seconds: 10}, {seconds: 15}, {size: '1920x1080'}, {resolution: '1080p'}, {preset: 'unknown'}]) {
  assert.throws(() => buildVideoForm('test', {seconds: 6, size: '720x1280', resolution: '720p', ...options}, []))
 }
})
