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
test('legacy video options always submit 720p',()=>{
 for (const resolution of ['480p','720p']) assert.equal(buildVideoForm('test',{seconds:6,size:'720x1280',resolution},[]).get('resolution_name'),'720p')
})

async function imageSizes() {
 const source=await readFile(new URL('../src/api/imageTasks.ts',import.meta.url),'utf8')
 const js=ts.transpileModule(source.replace("import apiClient from './client'",'const apiClient = {}'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
 return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`)
}
test('Grok generation offers 1K and 2K for each ratio but no unsupported 4K',async()=>{
 const {resolveImageSizePresets,isImageSizeSupportedByModel}=await imageSizes()
 const presets=resolveImageSizePresets('grok-imagine-image-pro')
 for(const ratio of ['1:1','2:3','3:2','9:16','16:9']){
  for(const resolution of ['1K','2K']) assert.ok(presets.some(p=>p.ratio===ratio&&p.resolution===resolution),`${ratio} ${resolution}`)
 }
 assert.equal(isImageSizeSupportedByModel('2560x1440','grok-imagine-image-pro'),true)
 assert.equal(isImageSizeSupportedByModel('3840x2160','grok-imagine-image-pro'),false)
 assert.equal(isImageSizeSupportedByModel('2048x2048','grok-imagine-image-edit'),false)
 assert.deepEqual(resolveImageSizePresets('grok-imagine-image-edit').map(p=>p.value),['auto','1024x1024'])
})
