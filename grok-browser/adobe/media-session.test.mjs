import test from 'node:test';
import assert from 'node:assert/strict';
import {isUsableSherlock,mediaCookieHeader,ensureMediaSession} from './media-session.mjs';
const partial=Buffer.from(JSON.stringify({sid:'s',ftr:'f'})).toString('base64');
test('official opaque sessions do not require undocumented fields',()=>{
 assert.equal(isUsableSherlock(partial),true);assert.equal(isUsableSherlock('opaque-session'),true);
 assert.equal(mediaCookieHeader([{name:'sherlockToken',value:partial}]),`sherlockToken=${partial}`);
 for(const value of ['', 'bad\r\nheader', 'session;other=field', 'white space'])assert.equal(isUsableSherlock(value),false);
});
test('cookie path precedence selects one complete opaque value without merging',()=>{
 const header=mediaCookieHeader([{name:'sherlockToken',value:'root',path:'/'},{name:'other',value:'x'},{name:'sherlockToken',value:partial,path:'/generate'}]);
 assert.equal(header,`other=x; sherlockToken=${partial}`);
});
test('editor initialization observes a missing session without Generate actions',async()=>{
 let polls=0,time=0;const pages=[];
 const page={goto:async url=>pages.push(url)};
 const context={cookies:async()=>++polls<3?[]:[{name:'sherlockToken',value:partial}]};
 const header=await ensureMediaSession(page,context,{now:()=>time,pause:async ms=>{time+=ms}});
 assert.equal(header,`sherlockToken=${partial}`);assert.deepEqual(pages,['https://firefly.adobe.com/generate/image']);
});
test('absent media session stops at bounded time without fabrication',async()=>{
 let time=0;const page={goto:async()=>{}};const context={cookies:async()=>[]};
 await assert.rejects(ensureMediaSession(page,context,{timeoutMs:2000,now:()=>time,pause:async ms=>{time+=ms}}),e=>e.code==='media_session_incomplete');
});
