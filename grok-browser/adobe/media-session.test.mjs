import test from 'node:test';
import assert from 'node:assert/strict';
import {isCompleteSherlock,mediaCookieHeader,ensureMediaSession} from './media-session.mjs';
const full=Buffer.from(JSON.stringify({sid:'s',ark:'a',bfp:'b',ftr:'f'})).toString('base64');
const partial=Buffer.from(JSON.stringify({sid:'s',ftr:'f'})).toString('base64');
test('partial Sherlock never replaces a complete media session',()=>{
 assert.equal(isCompleteSherlock(partial),false);assert.equal(isCompleteSherlock(full),true);
 assert.equal(mediaCookieHeader([{name:'sherlockToken',value:partial}]),'');
 const header=mediaCookieHeader([{name:'sherlockToken',value:partial},{name:'other',value:'x'},{name:'sherlockToken',value:full}]);
 assert.equal(header,`other=x; sherlockToken=${full}`);
});
test('editor initialization observes cookies without any Generate action',async()=>{
 let polls=0,time=0;const pages=[];
 const page={goto:async url=>pages.push(url)};
 const context={cookies:async()=>[{name:'sherlockToken',value:++polls<3?partial:full}]};
 const header=await ensureMediaSession(page,context,{now:()=>time,pause:async ms=>{time+=ms}});
 assert.equal(header,`sherlockToken=${full}`);assert.deepEqual(pages,['https://firefly.adobe.com/generate/image']);
});
test('missing media fields stop at bounded time without fabricating them',async()=>{
 let time=0;const page={goto:async()=>{}};const context={cookies:async()=>[{name:'sherlockToken',value:partial}]};
 await assert.rejects(ensureMediaSession(page,context,{timeoutMs:2000,now:()=>time,pause:async ms=>{time+=ms}}),e=>e.code==='media_session_incomplete');
});
