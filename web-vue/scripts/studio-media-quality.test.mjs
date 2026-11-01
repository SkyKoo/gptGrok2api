import test from 'node:test'
import assert from 'node:assert/strict'
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
