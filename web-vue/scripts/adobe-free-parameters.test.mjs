import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'
import * as capabilities from '../src/lib/mediaCapabilities.ts'
import { buildVideoForm, normalizeStudioVideoOptions, studioVideoSelection, studioVideoSettingValues, studioVideoAspectLabel, studioVideoSizeLabel, studioReferenceLimit, studioReferenceCountError, VIDEO_SIZE_OPTIONS } from '../src/lib/studioMedia.ts'

const base = { model:'veo-3.1-lite', size:'1920x1080', resolution:'1080p', seconds:8, audio:false }
const lite = { kind:'video', max_reference_images:2, variants:[
  { sizes:['1920x1080'], aspect_ratios:['16:9'], resolutions:['1080p'], durations:[8], audio:true, max_reference_images:2 },
  { sizes:['1920x1080'], aspect_ratios:['16:9'], resolutions:['1080p'], durations:[4,6], audio:false, max_reference_images:0 },
  { sizes:['1280x720'], aspect_ratios:['16:9'], resolutions:['720p'], durations:[8], audio:false, max_reference_images:0 },
  { sizes:['720x1280'], aspect_ratios:['9:16'], resolutions:['720p'], durations:[8], audio:true, max_reference_images:0 },
  { sizes:['1080x1920'], aspect_ratios:['9:16'], resolutions:['1080p'], durations:[8], audio:true, max_reference_images:0 },
] }
const fast = { ...lite, variants:lite.variants.map(variant => ({...variant,audio:false})) }
const omni = { kind:'video', max_reference_images:2, audio_required:true, default_audio:true, variants:[
  { sizes:['1280x720'], aspect_ratios:['16:9'], resolutions:['720p'], durations:[5], audio:true, audio_required:true, default_audio:true, max_reference_images:2 },
  { sizes:['720x1280'], aspect_ratios:['9:16'], resolutions:['720p'], durations:[5], audio:true, audio_required:true, default_audio:true, max_reference_images:0 },
] }
const ref = new File(['frame'], 'frame.png', {type:'image/png'})
function catalog(t) {
  capabilities.setMediaCapabilities({'veo-3.1-lite':lite,'veo-3.1-fast':fast,'gemini-omni-flash-1.1':omni,'gemini-omni-flash-1.2':omni})
  t.after(()=>capabilities.setMediaCapabilities())
}

test('video labels show declared aspect ratios and pixels without guessing missing or ambiguous ratios',()=>{
  assert.equal(studioVideoSizeLabel('1920x1080',lite),'16:9 横屏 · 1920×1080')
  assert.equal(studioVideoSizeLabel('720x1280',lite),'9:16 竖屏 · 720×1280')
  assert.equal(studioVideoAspectLabel('1080x1920',lite),'9:16 竖屏')
  const rounded={kind:'video',variants:[{sizes:['1376x768'],aspect_ratios:['16:9']}]}
  assert.equal(studioVideoSizeLabel('1376x768',rounded),'16:9 横屏 · 1376×768')
  assert.equal(studioVideoSizeLabel('1280x720',{kind:'video',sizes:['1280x720']}),'1280×720')
  assert.equal(studioVideoSizeLabel('1280x720',{kind:'video',sizes:['1280x720'],aspect_ratios:['16:9','9:16']}),'1280×720')
  assert.equal(studioVideoAspectLabel('1024x1024',lite),'')
})

test('Omni portrait keeps required audio and excludes unverified reference or resolution combinations',t=>{
  catalog(t)
  for(const model of ['gemini-omni-flash-1.1','gemini-omni-flash-1.2']) {
    const landscape={model,size:'1280x720',resolution:'720p',seconds:5,audio:true}
    const portrait=normalizeStudioVideoOptions({...landscape,size:'720x1280'},omni,'size',0)
    assert.equal(portrait.size,'720x1280');assert.equal(portrait.audio,true)
    assert.deepEqual(studioVideoSettingValues(portrait,omni),{
      sizes:['1280x720','720x1280'],resolutions:['720p'],durations:[5],audio:[true],hasAudio:true,
    })
    const form=buildVideoForm('a paper boat',portrait,[])
    assert.equal(form.get('size'),'720x1280');assert.equal(form.get('generate_audio'),'true')
    assert.equal(form.get('seconds'),'5');assert.equal(form.get('resolution_name'),'720p')
    assert.equal(form.has('provider'),false);assert.equal(form.has('channel'),false)
    assert.equal(studioReferenceLimit('video',model,studioVideoSelection(portrait)),0)
    assert.deepEqual(studioVideoSettingValues(landscape,omni,1).sizes,['1280x720'])
    assert.deepEqual(normalizeStudioVideoOptions(portrait,omni,'size',1),landscape)
    assert.equal(buildVideoForm('a paper boat',landscape,[ref]).getAll('input_reference').length,1)
    for(const invalid of [{...portrait,audio:false},{...portrait,resolution:'1080p'},{...portrait,size:'1024x1024'},{...portrait,seconds:8}]) {
      assert.throws(()=>buildVideoForm('a paper boat',invalid,[]),/参数组合/)
    }
    assert.throws(()=>buildVideoForm('a paper boat',portrait,[ref]),/参考图/)
  }
})

test('Grok retains its existing ratio labels and video form parameters',t=>{
  catalog(t)
  for(const option of VIDEO_SIZE_OPTIONS) assert.equal(studioVideoAspectLabel(option.value),option.label)
  const form=buildVideoForm('a paper boat',{model:'grok-imagine-video',size:'720x1280',resolution:'720p',seconds:6,preset:'custom'},[ref])
  assert.equal(form.get('size'),'720x1280');assert.equal(form.get('preset'),'custom')
  assert.equal(form.getAll('input_reference').length,1);assert.equal(form.has('generate_audio'),false)
})

test('Veo choices use complete free variants and the actual reference count',t=>{
  catalog(t)
  const noRefs=studioVideoSettingValues(base,lite,0)
  assert.deepEqual(noRefs.sizes,['1920x1080','1280x720','720x1280','1080x1920'])
  assert.deepEqual(noRefs.durations,[4,6,8])
  for(const refs of [1,2]) {
    assert.deepEqual(studioVideoSettingValues(base,lite,refs),{
      sizes:['1920x1080'],resolutions:['1080p'],durations:[8],audio:[false,true],hasAudio:true,
    })
  }
  const portrait={...base,size:'720x1280',resolution:'720p'}
  assert.deepEqual(studioVideoSettingValues(portrait,lite).durations,[8])
  assert.deepEqual(studioVideoSettingValues(portrait,lite).audio,[false,true])
  assert.equal(studioReferenceLimit('video',base.model,studioVideoSelection(portrait)),0)
  assert.equal(studioReferenceLimit('video',base.model,studioVideoSelection(base)),2)
  assert.match(studioReferenceCountError('video',base.model,1,studioVideoSelection(portrait)),/当前参数不支持参考图/)
  assert.equal(buildVideoForm('a paper boat',portrait,[]).get('size'),'720x1280')
  assert.throws(()=>buildVideoForm('a paper boat',portrait,[ref]),/参考图/)
  assert.throws(()=>buildVideoForm('a paper boat',{...portrait,seconds:4},[]),/参数组合/)
  assert.equal(buildVideoForm('a paper boat',{...portrait,audio:true},[]).get('generate_audio'),'true')
})

test('enabling audio constrains Lite choices and parameter changes never silently disable it',t=>{
  catalog(t)
  const withAudio={...base,audio:true}
  assert.deepEqual(studioVideoSettingValues(withAudio,lite).sizes,['1920x1080','720x1280','1080x1920'])
  assert.deepEqual(studioVideoSettingValues(withAudio,lite).durations,[8])
  const next=normalizeStudioVideoOptions({...withAudio,size:'720x1280'},lite,'size',0)
  assert.deepEqual(next,{...withAudio,size:'720x1280',resolution:'720p'})
  const portrait1080=normalizeStudioVideoOptions({...next,size:'1080x1920'},lite,'size',0)
  assert.deepEqual(portrait1080,{...withAudio,size:'1080x1920'})
  const shortened=normalizeStudioVideoOptions({...withAudio,seconds:4},lite,'seconds',0)
  assert.deepEqual(shortened,withAudio)
  assert.equal(buildVideoForm('a paper boat',next,[]).get('generate_audio'),'true')
  assert.equal(buildVideoForm('a paper boat',portrait1080,[]).get('generate_audio'),'true')
  assert.throws(()=>buildVideoForm('a paper boat',{...withAudio,size:'1280x720',resolution:'720p'},[]),/音频/)
  assert.throws(()=>buildVideoForm('a paper boat',portrait1080,[ref]),/参考图/)
})

test('switching to Fast preserves an explicit audio request and blocks it until disabled',t=>{
  catalog(t)
  const switched=normalizeStudioVideoOptions({...base,model:'veo-3.1-fast',audio:true},fast,'model',0)
  assert.equal(switched.audio,true)
  assert.match(capabilities.mediaCombinationError(fast,{...studioVideoSelection(switched),references:0}),/关闭音频/)
  assert.throws(()=>buildVideoForm('a paper boat',switched,[]),/关闭音频/)
  assert.deepEqual(studioVideoSettingValues(switched,fast).audio,[false])
  assert.equal(studioVideoSettingValues(switched,fast).hasAudio,true)
  const corrected=normalizeStudioVideoOptions({...switched,audio:false},fast,'audio',0)
  const form=buildVideoForm('a paper boat',corrected,[ref])
  assert.equal(form.get('generate_audio'),'false')
  assert.equal(form.getAll('input_reference').length,1)
  assert.equal(form.has('provider'),false);assert.equal(form.has('channel'),false)
})

test('normalization keeps existing references and never invents a valid over-limit combination',t=>{
  catalog(t)
  const portrait={...base,size:'720x1280',resolution:'720p'}
  assert.deepEqual(normalizeStudioVideoOptions(portrait,lite,undefined,1),base)
  assert.deepEqual(normalizeStudioVideoOptions(portrait,lite,undefined,3),portrait)
  assert.deepEqual(studioVideoSettingValues(base,lite,3).sizes,[])
  assert.throws(()=>buildVideoForm('a paper boat',base,[ref,ref,ref]),/参考图/)
})

test('image size controls respect reference-specific variants and never add 2K or 4K',t=>{
  const nano={kind:'image',qualities:['auto'],max_reference_images:14,sizes:['1024x1024','1200x896','768x1376'],variants:[
    {sizes:['1024x1024'],aspect_ratios:['1:1'],resolutions:['1K'],max_reference_images:14,max_count:1},
    {sizes:['1200x896'],aspect_ratios:['4:3'],resolutions:['1K'],max_reference_images:0,max_count:1},
    {sizes:['768x1376'],aspect_ratios:['9:16'],resolutions:['1K'],max_reference_images:0,max_count:1},
  ]}
  capabilities.setMediaCapabilities({nano});t.after(()=>capabilities.setMediaCapabilities())
  const js=ts.transpileModule(fs.readFileSync(new URL('../src/api/imageTasks.ts',import.meta.url),'utf8'),{compilerOptions:{module:ts.ModuleKind.CommonJS,target:ts.ScriptTarget.ES2022}}).outputText
  const module={exports:{}}
  vm.runInNewContext(js,{module,exports:module.exports,require(name){if(name==='@/lib/mediaCapabilities')return capabilities;if(name==='./client')return {default:{}};throw Error(name)}})
  const presets=module.exports.resolveImageSizePresets
  assert.deepEqual(Array.from(presets('nano'),p=>p.ratio),['1:1','4:3','9:16'])
  assert.ok(presets('nano').every(p=>p.resolution==='1K'))
  assert.deepEqual(Array.from(presets('nano',1),p=>p.value),['1024x1024'])
  assert.equal(presets('nano',15).length,0)
  assert.equal(studioReferenceLimit('image','nano',{size:'1200x896'}),0)
  assert.equal(studioReferenceLimit('image','nano',{size:'1024x1024'}),14)
  for(const size of ['2048x2048','4096x4096']) assert.throws(()=>capabilities.validateMediaCombination('nano',{size}),/参数组合/)
})
