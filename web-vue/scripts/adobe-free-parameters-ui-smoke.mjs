// Build first. Every API is mocked; this test never contacts an upstream or submits a real generation.
import assert from 'node:assert/strict'
import http from 'node:http'
import fs from 'node:fs/promises'
import path from 'node:path'
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright')
const root = path.resolve(process.env.CFM_UI_DIST || 'dist')
const server = http.createServer(async (req, res) => {
  try {
    const pathname = new URL(req.url, 'http://localhost').pathname
    const file = path.resolve(root, '.' + (pathname === '/' ? '/index.html' : pathname))
    if (!file.startsWith(root + '/')) throw Error('invalid path')
    res.setHeader('Content-Type', ({ '.html':'text/html', '.js':'text/javascript', '.css':'text/css', '.svg':'image/svg+xml' })[path.extname(file)] || 'application/octet-stream')
    res.end(await fs.readFile(file))
  } catch { res.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const origin=`http://127.0.0.1:${server.address().port}`
const browser=await chromium.launch({headless:true,args:['--no-sandbox']})
const context=await browser.newContext({viewport:{width:1440,height:1000}})
await context.addInitScript(()=>{
  localStorage.setItem('chatgpt2api.adminKey','fixture-only')
  localStorage.setItem('studio-active-mode','video')
  localStorage.setItem('studio-image-model','gemini-3.1-flash-image-2.1')
})
const page=await context.newPage(), errors=[], writes=[]
page.on('pageerror',error=>errors.push(error.message))
const lite={kind:'video',max_reference_images:2,variants:[
  {sizes:['1920x1080'],aspect_ratios:['16:9'],resolutions:['1080p'],durations:[8],audio:true,max_reference_images:2,max_count:1},
  {sizes:['1920x1080'],aspect_ratios:['16:9'],resolutions:['1080p'],durations:[4,6],audio:false,max_reference_images:0,max_count:1},
  {sizes:['1280x720'],aspect_ratios:['16:9'],resolutions:['720p'],durations:[8],audio:false,max_reference_images:0,max_count:1},
  {sizes:['720x1280'],aspect_ratios:['9:16'],resolutions:['720p'],durations:[8],audio:true,max_reference_images:0,max_count:1},
  {sizes:['1080x1920'],aspect_ratios:['9:16'],resolutions:['1080p'],durations:[8],audio:true,max_reference_images:0,max_count:1},
]}
const fast={...lite,variants:lite.variants.map(v=>({...v,audio:false}))}
const omni={kind:'video',max_reference_images:2,audio_required:true,default_audio:true,variants:[
  {sizes:['1280x720'],aspect_ratios:['16:9'],resolutions:['720p'],durations:[5],audio:true,audio_required:true,default_audio:true,max_reference_images:2,max_count:1},
  {sizes:['720x1280'],aspect_ratios:['9:16'],resolutions:['720p'],durations:[5],audio:true,audio_required:true,default_audio:true,max_reference_images:0,max_count:1},
]}
const nano={kind:'image',qualities:['auto'],max_reference_images:14,variants:[
  {sizes:['1024x1024'],aspect_ratios:['1:1'],resolutions:['1K'],qualities:['auto'],max_count:1,max_reference_images:14},
  {sizes:['1200x896'],aspect_ratios:['4:3'],resolutions:['1K'],qualities:['auto'],max_count:1,max_reference_images:0},
  {sizes:['768x1376'],aspect_ratios:['9:16'],resolutions:['1K'],qualities:['auto'],max_count:1,max_reference_images:0},
]}
const catalog={chat_models:[],image_models:['gemini-3.1-flash-image-2.1'],image_edit_models:['gemini-3.1-flash-image-2.1'],video_models:['veo-3.1-lite','veo-3.1-fast','gemini-omni-flash-1.1','gemini-omni-flash-1.2','grok-imagine-video'],models:[
  {id:'gemini-3.1-flash-image-2.1',name:'Nano Banana 2.1'},{id:'veo-3.1-lite',name:'Veo 3.1 Lite'},{id:'veo-3.1-fast',name:'Veo 3.1 Fast'},
  {id:'gemini-omni-flash-1.1',name:'Gemini Omni Flash 1.1'},{id:'gemini-omni-flash-1.2',name:'Gemini Omni Flash 1.2'},{id:'grok-imagine-video',name:'Grok Imagine Video'},
],media_capabilities:{'gemini-3.1-flash-image-2.1':nano,'veo-3.1-lite':lite,'veo-3.1-fast':fast,'gemini-omni-flash-1.1':omni,'gemini-omni-flash-1.2':omni}}
await context.route('**/*',async route=>{
  const req=route.request(), url=new URL(req.url()), pathname=url.pathname
  if(url.origin!==origin){await route.abort();return}
  if(!pathname.startsWith('/api/')&&!pathname.startsWith('/auth/')&&!pathname.startsWith('/v1/')&&!['/version','/meta','/health'].includes(pathname)){await route.continue();return}
  if(req.method()!=='GET')writes.push({path:pathname,method:req.method()})
  let data={items:[],groups:[],servers:[],pools:[],total:0,all_total:0,config:{},version:'fixture'}
  if(pathname==='/auth/status')data={authenticated:true,role:'admin',name:'Fixture'}
  else if(pathname==='/api/model-catalog')data=catalog
  else if(pathname==='/api/settings')data={config:{}}
  await route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(data)})
})
const screenshot=async name=>{
  if(!process.env.CFM_UI_SCREENSHOT_DIR)return
  await fs.mkdir(process.env.CFM_UI_SCREENSHOT_DIR,{recursive:true})
  await page.screenshot({path:path.join(process.env.CFM_UI_SCREENSHOT_DIR,name+'.png'),fullPage:true,animations:'disabled'})
}
const button=name=>page.getByRole('button',{name,exact:true})
const videoSettings=()=>page.locator('.studio-video-popover')
async function selectVideoModel(label){
  await videoSettings().locator('.grouped-select-trigger').click()
  await page.locator('.grouped-select-menu').getByRole('button',{name:label,exact:true}).click()
  if(!await videoSettings().isVisible())await button('视频设置').click()
  await page.getByText('视频设置 · '+label,{exact:true}).waitFor()
}
const fixtureFile={name:'fixture-frame.png',mimeType:'image/png',buffer:Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aS7sAAAAASUVORK5CYII=','base64')}
try{
  await page.goto(origin+'/#/studio')
  await button('视频设置').click()
  await selectVideoModel('Veo 3.1 Lite')
  await button('9:16 竖屏 · 720×1280').click()
  assert.equal(await button('720p').getAttribute('aria-pressed'),'true')
  assert.equal(await button('8 秒').getAttribute('aria-pressed'),'true')
  assert.match(await button('视频设置').innerText(),/9:16 竖屏/)
  await button('9:16 竖屏 · 1080×1920').click()
  assert.equal(await button('1080p').getAttribute('aria-pressed'),'true')
  await screenshot('lite-free-portrait-options')
  await button('16:9 横屏 · 1920×1080').click()
  await button('4 秒').waitFor();await button('6 秒').waitFor();await button('8 秒').click()
  await screenshot('lite-free-silent-options')
  await button('生成音频').click()
  assert.equal(await button('4 秒').count(),0);assert.equal(await button('6 秒').count(),0)
  await button('9:16 竖屏 · 720×1280').click()
  assert.equal(await button('720p').getAttribute('aria-pressed'),'true')
  assert.equal(await button('生成音频').getAttribute('aria-pressed'),'true')
  assert.equal(await button('上传图片').isDisabled(),true)
  await button('9:16 竖屏 · 1080×1920').click()
  assert.equal(await button('1080p').getAttribute('aria-pressed'),'true')
  assert.equal(await button('生成音频').getAttribute('aria-pressed'),'true')
  assert.equal(await button('16:9 横屏 · 1280×720').count(),0)
  await screenshot('lite-audio-combination')
  await button('16:9 横屏 · 1920×1080').click()
  await selectVideoModel('Veo 3.1 Fast')
  await videoSettings().getByText('当前参数组合不支持生成音频，请关闭音频或选择支持音频的参数。',{exact:true}).waitFor()
  assert.deepEqual(await videoSettings().locator('.studio-video-label').allTextContents(),['音频'])
  await page.locator('textarea.chat-input').fill('A paper boat on calm water')
  assert.equal(await button('提交视频任务').isDisabled(),true)
  await page.locator('textarea.chat-input').press('Enter')
  assert.deepEqual(writes,[])
  await button('无音频').waitFor()
  await screenshot('fast-audio-explicit-correction')
  await button('无音频').click()
  assert.equal(await button('提交视频任务').isDisabled(),false)
  await selectVideoModel('Veo 3.1 Lite')
  await page.locator('input[type=file]').setInputFiles(fixtureFile)
  await page.getByText('参考图 1/2 张',{exact:true}).waitFor()
  assert.equal(await button('4 秒').count(),0);assert.equal(await button('6 秒').count(),0)
  assert.equal(await button('9:16 竖屏 · 720×1280').count(),0)
  await screenshot('lite-reference-baseline-only')
  await button('移除 fixture-frame.png').click()
  for(const version of ['1.1','1.2']){
    await selectVideoModel('Gemini Omni Flash '+version)
    await button('9:16 竖屏 · 720×1280').click()
    assert.equal(await button('生成音频').getAttribute('aria-pressed'),'true')
    assert.equal(await button('无音频').count(),0)
    assert.equal(await button('1080p').count(),0)
    assert.equal(await button('5 秒').getAttribute('aria-pressed'),'true')
    assert.equal(await button('上传图片').isDisabled(),true)
    assert.match(await button('视频设置').innerText(),/9:16 竖屏/)
    await screenshot('omni-'+version+'-portrait-audio')
    await page.locator('input[type=file]').setInputFiles(fixtureFile)
    await page.getByText('当前参数不支持参考图，请调整参数或移除参考图。',{exact:true}).waitFor()
    assert.equal(await page.locator('.chat-attachment-preview').count(),0)
    await page.getByText('当前参数不支持参考图，请调整参数或移除参考图。',{exact:true}).waitFor({state:'hidden'})
    await button('16:9 横屏 · 1280×720').click()
    await page.locator('input[type=file]').setInputFiles(fixtureFile)
    await page.getByText('参考图 1/2 张',{exact:true}).waitFor()
    assert.equal(await button('9:16 竖屏 · 720×1280').count(),0)
    await button('移除 fixture-frame.png').click()
    await button('9:16 竖屏 · 720×1280').click()
  }
  await page.setViewportSize({width:390,height:844})
  const panelBounds=await videoSettings().boundingBox()
  assert.ok(panelBounds.x>=0&&panelBounds.x+panelBounds.width<=390)
  for(const bounds of await videoSettings().locator('.studio-video-choice').evaluateAll(nodes=>nodes.map(node=>{const r=node.getBoundingClientRect();return {x:r.x,right:r.right}}))){
    assert.ok(bounds.x>=0&&bounds.right<=390)
  }
  const settingsButtonBounds=await button('视频设置').boundingBox()
  const actionsBounds=await page.locator('.chat-input-action-row').boundingBox()
  const sendBounds=await button('提交视频任务').boundingBox()
  assert.ok(settingsButtonBounds.x>=actionsBounds.x)
  assert.ok(settingsButtonBounds.x+settingsButtonBounds.width<=actionsBounds.x+actionsBounds.width+1)
  assert.ok(settingsButtonBounds.x+settingsButtonBounds.width<=sendBounds.x)
  await screenshot('omni-portrait-mobile')
  await page.setViewportSize({width:1440,height:1000})
  await selectVideoModel('Grok Imagine Video')
  await button('1:1 方形').click()
  await button('480p').click();await button('6 秒').click()
  assert.equal(await button('1:1 方形').getAttribute('aria-pressed'),'true')
  assert.equal(await button('自定义').count(),1)
  assert.equal(await button('大胆').count(),1)
  assert.equal(await button('上传图片').isDisabled(),false)
  await screenshot('grok-original-video-options')
  await button('切换模式').click();await button('画图').click()
  await button('图片设置').click()
  await button('4:3').click()
  await page.getByText('当前参数仅支持文字生成',{exact:true}).waitFor()
  assert.equal(await button('2K').count(),0);assert.equal(await button('4K').count(),0)
  await page.locator('input[type=file]').setInputFiles(fixtureFile)
  await page.getByText('当前参数不支持参考图，请调整参数或移除参考图。',{exact:true}).waitFor()
  assert.equal(await page.locator('.chat-attachment-preview').count(),0)
  await screenshot('nano21-free-nonsquare-options')
  await button('1:1').click()
  await page.locator('input[type=file]').setInputFiles(fixtureFile)
  await page.getByText('参考图 1/14 张',{exact:true}).waitFor()
  assert.equal(await button('4:3').count(),0);assert.equal(await button('9:16').count(),0)
  assert.equal(await page.locator('.chat-attachment-preview').count(),1)
  await page.getByText('当前参数不支持参考图，请调整参数或移除参考图。',{exact:true}).waitFor({state:'hidden'})
  await screenshot('nano21-reference-square-only')
  assert.deepEqual(errors,[]);assert.deepEqual(writes,[])
  console.log(JSON.stringify({ok:true,checks:['lite-portrait-720p-1080p','lite-silent-4-6-8','lite-portrait-audio-720p-1080p-8-only','fast-audio-explicit-correction','invalid-enter-blocked','video-reference-baseline-only','omni-11-12-portrait-required-audio','omni-portrait-reference-rejected','omni-landscape-reference-preserved','omni-mobile-layout','grok-original-video-options','nano21-free-nonsquare','nano21-nonsquare-reference-rejected','nano21-reference-square-only'],apiWrites:writes.length,pageErrors:errors}))
}catch(error){await screenshot('failure');console.error(JSON.stringify({errors,writes,body:(await page.locator('body').innerText()).slice(-6000)}));throw error}
finally{await browser.close();await new Promise(resolve=>server.close(resolve))}
