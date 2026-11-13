import test from 'node:test'
import assert from 'node:assert/strict'
import { setMediaCapabilities, validateMediaCombination } from '../src/lib/mediaCapabilities.ts'
import { buildVideoForm, studioReferenceLimit, resolveStudioImageModel, normalizeStudioVideoOptions, studioVideoSettingValues } from '../src/lib/studioMedia.ts'
import { mailboxDeletionBlocked, mailboxRegistrationDisabled, mailboxRegistrationRetired } from '../src/views/mailboxRegistrationPolicy.ts'

const image = { kind: 'image', qualities: ['auto'], sizes: ['1024x1024'], max_count: 1, max_reference_images: 0, variants: [{ qualities: ['auto'], sizes: ['1024x1024'], max_count: 1, max_reference_images: 0 }] }
const video = { kind: 'video', sizes: ['1920x1080'], resolutions: ['1080p'], durations: [8], audio: false, max_reference_images: 0, variants: [{ sizes: ['1920x1080'], resolutions: ['1080p'], durations: [8], audio: false, max_reference_images: 0, max_count: 1 }] }

test('Veo free tier sends logical model and supported options without channel or Grok preset', t => {
 setMediaCapabilities({ 'veo-3.1-lite': video }); t.after(() => setMediaCapabilities())
 const form = buildVideoForm('paper plane', { model: 'veo-3.1-lite', seconds: 8, size: '1920x1080', resolution: '1080p', audio: false, preset: 'spicy' }, [])
 assert.equal(form.get('model'), 'veo-3.1-lite')
 assert.equal(form.get('seconds'), '8'); assert.equal(form.get('resolution_name'), '1080p')
 assert.equal(form.get('generate_audio'), 'false')
 assert.equal(form.has('provider'), false); assert.equal(form.has('channel'), false); assert.equal(form.has('preset'), false)
 assert.throws(() => buildVideoForm('test', { model: 'veo-3.1-lite', seconds: 4, size: '1920x1080', resolution: '1080p' }, []), /参数组合/)
 assert.throws(() => buildVideoForm('test', { model: 'veo-3.1-lite', seconds: 8, size: '1920x1080', resolution: '1080p', audio: true }, []), /参数组合/)
})
test('free image enforces count and excludes unverified references', t => {
 setMediaCapabilities({ 'gemini-3.1-flash-image': image }); t.after(() => setMediaCapabilities())
 assert.equal(studioReferenceLimit('image', 'gemini-3.1-flash-image'), 0)
 assert.doesNotThrow(() => validateMediaCombination('gemini-3.1-flash-image', {size:'1024x1024',count:1,references:0}))
 for (const request of [{quality:'high'}, {count:2}, {references:1}, {size:'2048x2048'}]) assert.throws(() => validateMediaCombination('gemini-3.1-flash-image', {size:'1024x1024',count:1,...request}), /参数组合/)
})
test('union of capabilities does not create unsupported cross-channel parameter combinations', t => {
 setMediaCapabilities({ combined: { kind: 'video', variants: [
  { sizes: ['1280x720'], resolutions: ['720p'], durations: [6], max_reference_images: 2 },
  { sizes: ['1920x1080'], resolutions: ['1080p'], durations: [8], max_reference_images: 0 },
 ] } }); t.after(() => setMediaCapabilities())
 assert.throws(() => validateMediaCombination('combined',{size:'1920x1080',resolution:'1080p',seconds:6}), /参数组合/)
 assert.throws(() => validateMediaCombination('combined',{size:'1920x1080',resolution:'1080p',seconds:8,references:1}), /参数组合/)
 assert.doesNotThrow(() => validateMediaCombination('combined',{size:'1280x720',resolution:'720p',seconds:6,references:2}))
})
test('Adobe mailbox claims and reservations prevent deletion and policies stay independent', () => {
 for (const mailbox of [{adobe_claimed:true}, {adobe_reserved:true}]) assert.equal(mailboxDeletionBlocked(mailbox), true)
 assert.equal(mailboxRegistrationDisabled({ adobe_disabled:true },'adobe'), true)
 assert.equal(mailboxRegistrationDisabled({ adobe_disabled:true },'openai'), false)
 assert.equal(mailboxRegistrationRetired({ adobe_retired:true },'adobe'), true)
 assert.equal(mailboxRegistrationRetired({ adobe_retired:true },'grok'), false)
})

test('Nano Banana references preserve the selected logical model and accept six references', t => {
 const nano = { ...image, max_reference_images: 6, variants: [{ qualities:['auto'], sizes:['1200x896'], aspect_ratios:['4:3'], resolutions:['1K'], max_count:1, max_reference_images:6 }] }
 setMediaCapabilities({ 'gemini-3.1-flash-image':nano }); t.after(() => setMediaCapabilities())
 assert.equal(studioReferenceLimit('image','gemini-3.1-flash-image'),6)
 assert.equal(resolveStudioImageModel('gemini-3.1-flash-image',true,['gpt-image-2','gemini-3.1-flash-image']),'gemini-3.1-flash-image')
 assert.doesNotThrow(()=>validateMediaCombination('gemini-3.1-flash-image',{size:'1200x896',references:6,count:1}))
 assert.throws(()=>validateMediaCombination('gemini-3.1-flash-image',{size:'1200x896',references:7,count:1}),/参数组合/)
})

test('video option changes select one complete combination and constrain dependent choices', () => {
 const capability = {kind:'video', variants:[
  {sizes:['1280x720'],resolutions:['720p'],durations:[4,6],audio:false,max_reference_images:1},
  {sizes:['1920x1080'],resolutions:['1080p'],durations:[8],audio:true,max_reference_images:2},
 ]}
 const old = {model:'veo-3.1-lite',size:'1280x720',resolution:'720p',seconds:6,audio:false}
 const next = normalizeStudioVideoOptions({...old,size:'1920x1080'},capability,'size')
 assert.deepEqual(next,{...old,size:'1920x1080',resolution:'1080p',seconds:8})
 assert.deepEqual(studioVideoSettingValues(next,capability),{sizes:['1280x720','1920x1080'],resolutions:['1080p'],durations:[8],audio:[false,true],hasAudio:true})
 const back = normalizeStudioVideoOptions({...next,size:'1280x720',audio:true},capability,'size')
 assert.equal(back.size,'1920x1080');assert.equal(back.resolution,'1080p');assert.equal(back.seconds,8);assert.equal(back.audio,true)
})

test('required video audio defaults to true and cannot be disabled or omitted in serialization', t => {
 const omni = {...video,audio:true,audio_required:true,default_audio:true,variants:[{...video.variants[0],audio:true,audio_required:true,default_audio:true}]}
 setMediaCapabilities({'gemini-omni-flash-1.1':omni});t.after(()=>setMediaCapabilities())
 const options={model:'gemini-omni-flash-1.1',size:'1920x1080',resolution:'1080p',seconds:8}
 assert.equal(normalizeStudioVideoOptions({...options,audio:false},omni,'model').audio,true)
 assert.deepEqual(studioVideoSettingValues({...options,audio:true},omni).audio,[true])
 assert.equal(buildVideoForm('wind through leaves',options,[]).get('generate_audio'),'true')
 assert.throws(()=>buildVideoForm('wind through leaves',{...options,audio:false},[]),/参数组合/)
})

test('Veo reference frame and generated audio are both serialized through the unified API', t => {
 const enabled={...video,audio:true,max_reference_images:2,variants:[{...video.variants[0],audio:true,max_reference_images:2}]}
 setMediaCapabilities({'veo-3.1-lite':enabled});t.after(()=>setMediaCapabilities())
 const file=new File([new Uint8Array([137,80,78,71])],'frame.png',{type:'image/png'})
 const form=buildVideoForm('steam rises',{model:'veo-3.1-lite',size:'1920x1080',resolution:'1080p',seconds:8,audio:true},[file])
 assert.equal(form.get('generate_audio'),'true');assert.equal(form.getAll('input_reference').length,1)
 assert.equal(form.has('provider'),false);assert.equal(form.has('channel'),false)
})

test('image size presets use official ratio and resolution instead of reducing pixel dimensions', async t => {
 const [{default:ts},{default:vm},{default:fs}]=await Promise.all([import('typescript'),import('node:vm'),import('node:fs')])
 const capabilities=await import('../src/lib/mediaCapabilities.ts')
 setMediaCapabilities({nano:{kind:'image',sizes:['1200x896','896x1200','768x2752'],variants:[
  {sizes:['1200x896'],aspect_ratios:['4:3'],resolutions:['1K']},
  {sizes:['896x1200'],aspect_ratios:['3:4'],resolutions:['1K']},
  {sizes:['768x2752'],aspect_ratios:['1:4'],resolutions:['1K']},
 ]}});t.after(()=>setMediaCapabilities())
 const compiled=ts.transpileModule(fs.readFileSync(new URL('../src/api/imageTasks.ts',import.meta.url),'utf8'),{compilerOptions:{module:ts.ModuleKind.CommonJS,target:ts.ScriptTarget.ES2022}}).outputText
 const module={exports:{}}
 vm.runInNewContext(compiled,{module,exports:module.exports,require(name){if(name==='@/lib/mediaCapabilities')return capabilities;if(name==='./client')return {default:{}};throw Error(name)}},{filename:'imageTasks.ts'})
 const presets=module.exports.resolveImageSizePresets('nano')
 assert.equal(presets[0].ratio,'4:3');assert.equal(presets[0].label,'4:3 · 1K · 1200x896')
 assert.equal(presets[1].ratio,'3:4');assert.equal(presets[2].resolution,'1K')
 assert.equal(presets[2].width,768);assert.equal(presets[2].height,2752)
})

test('base64 image previews preserve safe MIME metadata and legacy PNG behavior',async()=>{
 const [{default:ts},{default:vm},{default:fs}]=await Promise.all([import('typescript'),import('node:vm'),import('node:fs')])
 const capabilities=await import('../src/lib/mediaCapabilities.ts')
 const compiled=ts.transpileModule(fs.readFileSync(new URL('../src/api/imageTasks.ts',import.meta.url),'utf8'),{compilerOptions:{module:ts.ModuleKind.CommonJS,target:ts.ScriptTarget.ES2022}}).outputText
 const module={exports:{}}
 vm.runInNewContext(compiled,{module,exports:module.exports,require(name){if(name==='@/lib/mediaCapabilities')return capabilities;if(name==='./client')return {default:{}};throw Error(name)}},{filename:'imageTasks.ts'})
 const url=module.exports.imageAssetUrl
 for(const [fields,mime] of [[{mime:'image/jpeg'},'image/jpeg'],[{mime_type:'image/webp'},'image/webp'],[{output_format:'gif'},'image/gif'],[{mime:'IMAGE/JPEG ; charset=binary'},'image/jpeg'],[{},'image/png']]){
  assert.equal(url({b64_json:'YWJj',...fields}),`data:${mime};base64,YWJj`)
 }
 assert.equal(url({b64_json:'YWJj',mime:'text/html'}),'')
 assert.equal(url({b64_json:'YWJj',output_format:'svg'}),'')
 assert.equal(url({url:'/images/result.webp',b64_json:'YWJj',mime:'image/png'}),'/images/result.webp')
})
