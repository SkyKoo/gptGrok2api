// Isolated browser fixtures; no real registration, login or generation request.
import assert from 'node:assert/strict'
import http from 'node:http'
import fs from 'node:fs/promises'
import path from 'node:path'
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright')
const root=path.resolve('dist')
const server=http.createServer(async(req,res)=>{try{const pathname=new URL(req.url,'http://localhost').pathname,file=path.resolve(root,'.'+(pathname==='/'?'/index.html':pathname));if(!file.startsWith(root+'/'))throw Error('invalid path');const data=await fs.readFile(file);res.setHeader('Content-Type',({'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml'})[path.extname(file)]||'application/octet-stream');res.end(data)}catch{res.writeHead(404).end()}})
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve))
const origin=`http://127.0.0.1:${server.address().port}`
const browser=await chromium.launch({headless:true,args:['--no-sandbox']})
const context=await browser.newContext({viewport:{width:1440,height:1000}})
await context.addInitScript(()=>localStorage.setItem('chatgpt2api.adminKey','fixture-only'))
const page=await context.newPage(),errors=[],writes=[],unexpected=[],jobs=[],batches=new Map()
page.on('pageerror',error=>errors.push(error.message))
const base={provider:'adobe',disabled:false,has_profile:true,media_session_status:'preserved',expires_at:Math.floor(Date.now()/1000)+3600,quota:{status:'unknown'}}
const accounts=[{...base,id:'unknown',account_id:'unknown',email:'unknown@example.test',status:'submission_unknown',submission_unknown:true,pending:true,in_flight:true},{...base,id:'ready',account_id:'ready',email:'ready@example.test',status:'active'},{...base,id:'otp',account_id:'otp',email:'otp@example.test',status:'expired'},{...base,id:'busy',account_id:'busy',email:'busy@example.test',status:'busy',pending:true,in_flight:true}]
let loseResponse=false,rejectNext=false,lateBatch
function acceptBatch(body){const items=body.ids.map(id=>({progress_id:`job-${jobs.length}-${id}`,request_id:body.request_id,account_ref:id,email:accounts.find(account=>account.id===id).email,stage:'running',started_at:new Date().toISOString(),updated_at:new Date().toISOString(),done:false,submission_unknown:id==='unknown'}));jobs.push(...items);batches.set(body.request_id,items);return items}
function finish(items){for(const task of items){task.done=true;task.finished_at=task.updated_at=new Date().toISOString();if(task.account_ref==='otp'){task.stage='failed';task.error='adobe_relogin_email_verification_required'}else{task.stage='completed';task.session_updated=true;const account=accounts.find(a=>a.id===task.account_ref);account.media_session_status='refreshed';account.media_session_refreshed=true}}}
await context.route('**/*',async route=>{
 const req=route.request(),url=new URL(req.url()),pathname=url.pathname
 if(url.origin!==origin){await route.abort();return}
 if(!pathname.startsWith('/api/')&&!pathname.startsWith('/auth/')&&!pathname.startsWith('/v1/')&&!['/version','/meta','/health'].includes(pathname)){await route.continue();return}
 let data,status=200
 if(req.method()!=='GET'&&pathname!=='/api/register/grok/accounts/runtime/snapshot')writes.push({path:pathname,method:req.method(),body:req.postDataJSON()})
 if(pathname==='/auth/status')data={authenticated:true,role:'admin',name:'Fixture'}
 else if(pathname==='/api/settings')data={config:{}}
 else if(pathname==='/api/model-catalog')data={models:[],chat_models:[],image_models:[],video_models:[]}
 else if(pathname==='/api/adobe/accounts'&&req.method()==='GET')data={items:accounts,total:accounts.length,all_total:accounts.length,page:1,page_size:25}
 else if(pathname==='/api/adobe/accounts/relogin'&&req.method()==='POST'){
  const body=req.postDataJSON();assert.equal(typeof body.request_id,'string');assert.ok(body.request_id.length>20);assert.ok(body.ids.length)
  assert.ok(body.ids.every(id=>id!=='busy'))
  if(rejectNext){rejectNext=false;await route.fulfill({status:400,contentType:'application/json',body:JSON.stringify({error:{message:'fixture request rejected',type:'invalid_request_error'}})});return}
  if(loseResponse){loseResponse=false;lateBatch=body;await route.abort('failed');return}
  data={request_id:body.request_id,items:acceptBatch(body)};status=202
 }
 else if(pathname==='/api/adobe/accounts/relogin'&&req.method()==='GET')data={items:url.searchParams.has('request_id')?batches.get(url.searchParams.get('request_id'))||[]:jobs}
 else if(pathname==='/api/register/grok/accounts/runtime/snapshot')data={accounts:[],runtime_available:true}
 else if(req.method()!=='GET'){unexpected.push(pathname);data={error:'unexpected mutation'}}
 else data={items:[],groups:[],pools:[],servers:[],accounts:[],total:0,all_total:0,page:1,page_size:25,summary:{},config:{},runtime_available:true}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)})
})
const row=email=>page.getByRole('row').filter({hasText:email})
const progress=()=>page.getByRole('region',{name:'Adobe 重新登录进度',exact:true})
async function clearSelection(){await page.locator('.selection-bulk-bar-host:not(.selection-bulk-bar-leave-active)').getByRole('button',{name:'取消选择',exact:true}).click();await page.locator('.selection-bulk-bar-host').waitFor({state:'detached'})}
try{
 await page.goto(origin+'/#/accounts?platform=adobe')
 const unknown=row('unknown@example.test'),busy=row('busy@example.test')
 await unknown.getByText('提交结果未知，原任务保留；重新登录仅更新会话。',{exact:true}).waitFor()
 assert.equal(await unknown.getByText('生成中',{exact:true}).count(),0)
 assert.equal(await unknown.getByRole('button',{name:'刷新额度',exact:true}).isDisabled(),true)
 assert.equal(await unknown.getByRole('button',{name:'重新登录',exact:true}).isDisabled(),false)
 assert.equal(await busy.getByRole('button',{name:'重新登录',exact:true}).isDisabled(),true)
 const fits=await unknown.locator('td').last().evaluate(cell=>{const boundary=cell.closest('table').parentElement.getBoundingClientRect();return [...cell.querySelectorAll('button')].every(button=>{const box=button.getBoundingClientRect();return box.left>=boundary.left&&box.right<=boundary.right})});assert.equal(fits,true)
 await page.getByRole('checkbox',{name:'选择 ready@example.test'}).check()
 await unknown.getByRole('button',{name:'重新登录',exact:true}).click()
 await progress().getByText('浏览器登录中',{exact:false}).waitFor()
 assert.deepEqual(writes[0].body.ids,['unknown'])
 assert.equal(await unknown.getByRole('button',{name:'登录中…',exact:true}).isDisabled(),true)
 assert.equal(await unknown.getByRole('button',{name:'更多',exact:true}).isDisabled(),true)
 assert.equal(await row('ready@example.test').getByRole('button',{name:'重新登录',exact:true}).isDisabled(),false)
 finish(jobs)
 await progress().getByText('重新登录完成，媒体会话已确认。',{exact:true}).waitFor()
 await progress().getByText('待核对任务仍保留，未重新提交。',{exact:true}).waitFor()
 assert.equal(await page.getByRole('checkbox',{name:'选择 ready@example.test'}).isChecked(),true)
 await clearSelection()
 await page.getByRole('checkbox',{name:'选择 unknown@example.test'}).check()
 await page.getByRole('checkbox',{name:'选择 otp@example.test'}).check()
 await page.getByRole('button',{name:'批量操作',exact:true}).click()
 await page.locator('.floating-action-menu-panel').getByRole('button',{name:'重新登录',exact:true}).click()
 await page.waitForFunction(()=>[...document.querySelectorAll('button')].filter(button=>button.textContent.trim()==='登录中…').length===2)
 assert.equal(writes.length,2);assert.deepEqual(writes[1].body.ids,['unknown','otp'])
 finish(jobs.filter(task=>!task.done))
 await progress().getByRole('alert').filter({hasText:'Adobe 要求邮件验证码'}).waitFor()
 assert.equal(accounts[0].in_flight,true)
 await clearSelection()
 // Lost batch response: no re-POST, unresolved account lock survives a reload.
 loseResponse=true
 await row('ready@example.test').getByRole('button',{name:'重新登录',exact:true}).click()
 await progress().getByText('启动结果待确认',{exact:false}).waitFor()
 assert.equal(writes.length,3)
 await page.reload()
 await progress().getByText('启动结果待确认',{exact:false}).waitFor()
 assert.equal(await row('ready@example.test').getByRole('button',{name:'登录中…',exact:true}).isDisabled(),true)
 assert.equal(writes.length,3)
 const late=acceptBatch(lateBatch)
 await progress().getByRole('article',{name:'ready@example.test 的重新登录进度',exact:true}).getByText('浏览器登录中',{exact:false}).waitFor()
 finish(late)
 await progress().getByRole('article',{name:'ready@example.test 的重新登录进度',exact:true}).getByText('重新登录完成，媒体会话已确认。',{exact:true}).waitFor()
 assert.equal(writes.length,3)
 // Explicit rejection is terminal and must release the local account lock.
 rejectNext=true
 await row('ready@example.test').getByRole('button',{name:'重新登录',exact:true}).click()
 await page.getByRole('alert').filter({hasText:'fixture request rejected'}).waitFor()
 assert.equal(await row('ready@example.test').getByRole('button',{name:'重新登录',exact:true}).isDisabled(),false)
 assert.equal(await progress().getByText('启动结果待确认',{exact:false}).count(),0)
 assert.equal(writes.length,4)
 await page.getByRole('button',{name:'卡片视图',exact:true}).click()
 const card=page.getByRole('article',{name:'Adobe 账号 unknown@example.test',exact:true})
 await card.getByText('待核对',{exact:true}).waitFor()
 assert.equal(await card.getByRole('button',{name:'重新登录',exact:true}).isDisabled(),false)
 if(process.env.CFM_UI_SCREENSHOT_DIR){await fs.mkdir(process.env.CFM_UI_SCREENSHOT_DIR,{recursive:true});await page.screenshot({path:path.join(process.env.CFM_UI_SCREENSHOT_DIR,'adobe-relogin-desktop.png'),fullPage:true})}
 await page.setViewportSize({width:390,height:844});await page.waitForTimeout(250)
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false)
 if(process.env.CFM_UI_SCREENSHOT_DIR)await page.screenshot({path:path.join(process.env.CFM_UI_SCREENSHOT_DIR,'adobe-relogin-mobile.png'),fullPage:true})
 assert.deepEqual(errors,[]);assert.deepEqual(unexpected,[])
 console.log('PASS: Adobe async single/batch relogin, per-account locking, OTP/unknown feedback, lost POST reconciliation and reload, cards/mobile; exactly 4 intentional POSTs')
}catch(error){console.error(JSON.stringify({errors,writes,body:(await page.locator('body').innerText()).slice(0,4000)}));throw error}
finally{await browser.close();await new Promise(resolve=>server.close(resolve))}
