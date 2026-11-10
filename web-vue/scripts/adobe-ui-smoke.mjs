// Run after npm run build. PLAYWRIGHT_MODULE can point to an existing Docker
// image's Playwright installation; every application API is a local fixture.
import assert from 'node:assert/strict'
import http from 'node:http'
import fs from 'node:fs/promises'
import path from 'node:path'
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright')
const root = path.resolve('dist')
const server = http.createServer(async (req, res) => {
  try {
    const requested = new URL(req.url, 'http://localhost').pathname
    const file = path.resolve(root, '.' + (requested === '/' ? '/index.html' : requested))
    if (!file.startsWith(root + '/')) throw new Error('invalid path')
    const data = await fs.readFile(file)
    res.setHeader('Content-Type', ({'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml'})[path.extname(file)] || 'application/octet-stream')
    res.end(data)
  } catch { res.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({headless:true,args:['--no-sandbox']})
const context = await browser.newContext({viewport:{width:1440,height:1100}})
await context.addInitScript(() => localStorage.setItem('chatgpt2api.adminKey','fixture-only'))
const page = await context.newPage(), errors=[], requests=[], unknown=[]
page.on('pageerror',error=>errors.push(error.message))
const schedule={enabled:false,interval_minutes:60,next_run_at:'',last_attempt_at:'',last_started_at:'',last_result:'',last_error:''}
const register={target:'openai',total:1,threads:1,mode:'total',enabled:false,mail:{source_id:'mail',providers:[{id:'mail',type:'icloud_hme',enable:true,label:'fixture'}]},target_settings:Object.fromEntries(['openai','grok','adobe'].map(target=>[target,{total:1,threads:1,mode:'total',mail_source_id:'mail',proxy:'direct'}])),target_runtime:Object.fromEntries(['openai','grok','adobe'].map(target=>[target,{enabled:false,stats:{},logs:[],jobs:[],registration_schedule:schedule}]))}
const image={kind:'image',sizes:['1024x1024'],qualities:['auto'],max_reference_images:0,output_formats:['png'],variants:[{sizes:['1024x1024'],qualities:['auto'],max_count:1,max_reference_images:0}]}
const video={kind:'video',sizes:['1920x1080'],resolutions:['1080p'],durations:[8],audio:false,max_reference_images:0,variants:[{sizes:['1920x1080'],resolutions:['1080p'],durations:[8],max_count:1,max_reference_images:0,audio:false}]}
const catalog={chat_models:['gpt-test'],image_models:['gemini-3.1-flash-image'],image_edit_models:[],video_models:['veo-3.1-lite'],models:[{id:'gpt-test',name:'GPT fixture'},{id:'gemini-3.1-flash-image',name:'Gemini 3.1 Flash Image (Nano Banana 2)'},{id:'veo-3.1-lite',name:'Veo 3.1 Lite'}],media_capabilities:{'gemini-3.1-flash-image':image,'veo-3.1-lite':video}}
async function screenshot(name) { if (process.env.CFM_UI_SCREENSHOT_DIR) { await fs.mkdir(process.env.CFM_UI_SCREENSHOT_DIR, {recursive:true}); await page.screenshot({path:path.join(process.env.CFM_UI_SCREENSHOT_DIR, name+'.png'),fullPage:true}) } }
let priorities=['chatgpt','adobe'], multi=false
const account={id:'adobe:fixture',account_id:'fixture',email:'fixture@example.test',provider:'adobe',status:'active',disabled:false,media_session_status:'imported',media_session_updated_at:Math.floor(Date.now()/1000),expires_at:Math.floor(Date.now()/1000)+3600,quota:{status:'known',total:10,used:0,available:10,checked_at:new Date().toISOString()}}
await context.route('**/*',async route=>{
  const req=route.request(), url=new URL(req.url()), pathname=url.pathname
  if(url.origin!==origin){await route.abort();return}
  if(!pathname.startsWith('/api/')&&!pathname.startsWith('/auth/')&&!pathname.startsWith('/v1/')&&!['/version','/meta','/health'].includes(pathname)){await route.continue();return}
  requests.push({path:pathname,method:req.method(),body:req.postData()})
  let data
  if(pathname==='/auth/status')data={authenticated:true,role:'admin',name:'Fixture'}
  else if(pathname==='/api/settings')data={config:{}}
  else if(pathname==='/api/model-catalog')data=catalog
  else if(pathname==='/api/adobe/accounts')data={items:[{...account,media_session_status:'refreshed',media_session_refreshed:true},
    {...account,id:'adobe:zero',account_id:'zero',email:'zero@example.test',status:'exhausted',quota:{status:'known',total:10,used:10,available:0}},
    {...account,id:'adobe:unknown',account_id:'unknown',email:'unknown@example.test',media_session_status:'preserved',media_session_refreshed:false,quota:{status:'unknown'}},
    {...account,id:'adobe:stale',account_id:'stale',email:'stale@example.test',quota:{status:'stale',available:10}},
    {...account,id:'adobe:error',account_id:'error',email:'error@example.test',quota:{status:'error',available:8,error:'Fixture query error'}},
  ],total:5,all_total:5,page:1,page_size:25}
  else if(pathname==='/api/adobe/accounts/refresh')data={results:[{id:account.id,ok:true}]}
  else if(pathname==='/api/model-routing'){
    if(req.method()==='PUT')priorities=JSON.parse(req.postData()).priorities['gpt-image-2.5-sunburst']
    data={models:multi?[{model:'gpt-image-2.5-sunburst',name:'Sunburst fixture',channels:['chatgpt','adobe'],priority:priorities}]:[{model:'gemini-3.1-flash-image',channels:['adobe'],priority:['adobe']}]}
  }
  else if(pathname==='/api/openai/routing')data={image_conversation_model:'gpt-test',image_model_choices:['gpt-test']}
  else if(pathname==='/api/register')data={register:{...register,target:url.searchParams.get('target')||'openai'}}
  else if(pathname==='/api/register/events'){await route.fulfill({status:200,contentType:'text/event-stream',body:''});return}
  else if(pathname.endsWith('/sources'))data={items:[{id:'mail',type:'icloud_hme',label:'fixture',enable:true,capabilities:{openai:true,grok:true,adobe:true}}]}
  else if(['/api/accounts','/api/account-groups','/api/accounts/twofa','/api/accounts/relogin','/api/register/grok/accounts','/api/proxy/groups','/api/prompts','/api/accounts/import/cpa/pools','/api/accounts/import/sub2api/servers','/api/cpa/pools','/api/grok/oauth/accounts','/api/grok/oauth/candidates','/api/proxy/runtime','/api/register/grok/accounts/runtime/snapshot','/api/sub2api/servers','/api/third-party-apps','/version'].includes(pathname))data={items:[],groups:[],pools:[],servers:[],accounts:[],total:0,all_total:0,page:1,page_size:25,summary:{},runtime_available:true}
  else {unknown.push(pathname);data={items:[],groups:[],servers:[],pools:[],total:0,all_total:0,config:{},version:'fixture'}}
  await route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(data)})
})
try {
  await page.goto(origin+'/#/accounts?platform=adobe')
  await page.getByRole('heading',{name:'Adobe 账号',exact:true}).waitFor()
  await page.getByText('可用 10',{exact:true}).waitFor()
  await page.getByText('额度已用完（0）',{exact:true}).waitFor()
  await page.getByText('额度未知',{exact:true}).waitFor()
  await page.getByText('已过期 · 上次可用 10',{exact:true}).waitFor()
  await page.getByText('查询失败 · 上次可用 8',{exact:true}).waitFor()
  await page.getByRole('row').filter({hasText:'fixture@example.test'}).getByText('媒体会话已更新',{exact:true}).waitFor()
  await page.getByRole('row').filter({hasText:'unknown@example.test'}).getByText('凭据已刷新，保留原媒体会话',{exact:true}).waitFor()
  await screenshot('adobe-accounts')
  await page.getByRole('checkbox',{name:'选择 fixture@example.test'}).check()
  await page.getByRole('button',{name:'刷新额度',exact:true}).click()
  await page.getByText('操作已完成',{exact:true}).waitFor()
  assert.ok(requests.some(req=>req.path==='/api/adobe/accounts/refresh'&&JSON.parse(req.body).ids[0]==='adobe:fixture'))
  await page.goto(origin+'/#/register')
  await page.getByText('Adobe Firefly',{exact:true}).click()
  await page.getByText('Adobe 任务参数',{exact:true}).waitFor()
  await page.getByText('Adobe Firefly 注册',{exact:true}).waitFor()
  await page.getByText('Adobe 定时启动',{exact:true}).waitFor()
  await screenshot('adobe-registration')
  await page.goto(origin+'/#/studio')
  await page.getByRole('button',{name:'切换模式',exact:true}).click()
  await page.getByRole('button',{name:'视频',exact:true}).click()
  await page.getByRole('button',{name:'视频设置'}).click()
  await page.getByText('视频设置 · Veo 3.1 Lite',{exact:true}).waitFor()
  await page.getByRole('button',{name:'8 秒',exact:true}).waitFor()
  assert.equal(await page.getByRole('button',{name:'720p',exact:true}).count(),0)
  await screenshot('veo-options')
  assert.equal(await page.getByText('渠道',{exact:true}).count(),0)
  await page.getByRole('button',{name:'切换模式',exact:true}).click()
  await page.getByRole('button',{name:'画图',exact:true}).click()
  await page.getByRole('button',{name:'图片设置',exact:true}).click()
  await page.getByText('模型默认画质',{exact:true}).waitFor()
  assert.equal(await page.getByRole('button',{name:'2 张',exact:true}).count(),0)
  await page.goto(origin+'/#/settings')
  await page.getByText('当前没有已确认可用的多渠道模型，无需设置优先级。',{exact:true}).waitFor()
  multi=true
  await page.getByRole('button',{name:'刷新渠道',exact:true}).click()
  await page.getByText('Sunburst fixture',{exact:true}).waitFor()
  await page.getByRole('button',{name:'提高 Adobe 优先级',exact:true}).click()
  await page.getByRole('button',{name:'保存优先级',exact:true}).click()
  await page.getByText('渠道优先级已保存；已有任务继续使用原渠道。',{exact:true}).waitFor()
  assert.deepEqual(priorities,['adobe','chatgpt'])
  await screenshot('model-priority')
  assert.deepEqual(errors,[])
  assert.deepEqual(unknown,[], 'Unexpected API fixture path')
  assert.deepEqual(requests.filter(req=>req.method!=='GET'&&!['/api/adobe/accounts/refresh','/api/model-routing','/api/register/grok/accounts/runtime/snapshot'].includes(req.path)),[], 'Unexpected mutation')
  console.log(JSON.stringify({ok:true,checks:['adobe-media-session-status','adobe-account-quota','adobe-account-refresh','third-registration-tab','adobe-schedule','veo-free-parameters','gemini-free-parameters','single-channel-empty-state','multi-channel-admin-priority'],fixtureEndpoints:[...new Set(unknown)].sort()}))
} catch(error){console.log(JSON.stringify({errors,body:(await page.locator('body').innerText()).slice(-6000),fixtureEndpoints:[...new Set(unknown)].sort()}));throw error}
finally{await browser.close();await new Promise(resolve=>server.close(resolve))}
