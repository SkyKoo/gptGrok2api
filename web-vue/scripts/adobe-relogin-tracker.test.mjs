import test from 'node:test'
import assert from 'node:assert/strict'
import { createAdobeReloginTracker, isAdobeReloginNotStarted } from '../src/lib/adobeRelogin.ts'
const task=(id,overrides={})=>({progress_id:'task-'+id,account_ref:id,email:id+'@example.test',stage:'queued',started_at:'2026-11-11T00:00:00Z',updated_at:'2026-11-11T00:00:00Z',done:false,...overrides})
const rejected=cause=>cause?.status===400

test('one batch submission creates independent tasks and locks only those accounts',async()=>{
 const posts=[],completed=[];let state,rows=[task('a'),task('b')]
 const tracker=createAdobeReloginTracker({start:async(ids,requestId)=>{posts.push({ids,requestId});return{items:rows}},list:async()=>({items:rows}),isRejected:rejected},value=>state=value,()=>completed.push(true))
 await tracker.start(['a','b'],'batch')
 await tracker.start(['a'],'duplicate')
 assert.deepEqual(posts,[{ids:['a','b'],requestId:'batch'}])
 assert.equal(tracker.busy('a'),true);assert.equal(tracker.busy('other'),false)
 rows=[task('a',{stage:'completed',done:true,session_updated:true,updated_at:'2026-11-11T00:00:02Z'}),task('b',{stage:'failed',done:true,error:'otp',updated_at:'2026-11-11T00:00:02Z'})]
 await tracker.sync()
 assert.equal(state.tasks.length,2);assert.equal(tracker.busy('a'),false);assert.equal(completed.length,1)
})
test('lost POST response stays uncertain until GET finds the original request, never POSTs again',async()=>{
 let state,posts=0,accepted=false;const queries=[]
 const tracker=createAdobeReloginTracker({start:async()=>{posts++;throw new Error('network')},list:async id=>{queries.push(id);return{items:accepted?[task('a')]:[]}},isRejected:rejected},value=>state=value,()=>{})
 await tracker.start(['a'],'lost')
 assert.equal(state.requests[0].uncertain,true);assert.equal(tracker.busy('a'),true)
 await tracker.sync();await tracker.start(['a'],'new')
 assert.equal(posts,1)
 accepted=true;await tracker.sync()
 assert.equal(state.requests.length,0);assert.equal(state.tasks[0].account_ref,'a');assert.ok(queries.includes('lost'))
})
test('refresh restores an unresolved request, and GET failure retains current running tasks',async()=>{
 let state,fail=false;const calls=[]
 const tracker=createAdobeReloginTracker({start:async()=>{throw new Error('must never submit')},list:async id=>{calls.push(id);if(fail)throw new Error('network');return{items:[task('a')]}},isRejected:rejected},value=>state=value,()=>{},[{request_id:'saved',ids:['a'],uncertain:true}])
 assert.equal(tracker.busy('a'),true)
 await tracker.sync();assert.ok(calls.includes('saved'))
 fail=true;await tracker.sync()
 assert.equal(state.pollError,true);assert.equal(state.tasks[0].done,false);assert.equal(tracker.busy('a'),true)
})
test('explicit rejection releases local reservation and old queued snapshots cannot undo completion',async()=>{
 let state
 const tracker=createAdobeReloginTracker({start:async()=>{throw{status:400}},list:async()=>({items:[]}),isRejected:rejected},value=>state=value,()=>{})
 await assert.rejects(tracker.start(['a'],'rejected'));assert.equal(tracker.busy('a'),false);assert.equal(state.requests.length,0)
 let resolveStart,rows=[]
 const slow=createAdobeReloginTracker({start:()=>new Promise(resolve=>resolveStart=resolve),list:async()=>({items:rows}),isRejected:rejected},value=>state=value,()=>{})
 const started=slow.start(['a'],'slow')
 rows=[task('a',{done:true,stage:'completed',session_updated:true,updated_at:'2026-11-11T00:00:01Z'})]
 await slow.sync();resolveStart({items:[task('a')]});await started
 assert.equal(state.tasks[0].done,true);assert.equal(state.tasks[0].stage,'completed')
})

test('real admission classifier keeps proxy 503 uncertain and accepts only explicit local rejection',async()=>{
 for(const status of [502,503,504]){
  assert.equal(isAdobeReloginNotStarted({status}),false)
  assert.equal(isAdobeReloginNotStarted({status,data:{error:{type:'server_error'}}}),false)
 }
 assert.equal(isAdobeReloginNotStarted({status:400}),false)
 assert.equal(isAdobeReloginNotStarted({status:400,data:{error:{type:'invalid_request_error'}}}),true)
 assert.equal(isAdobeReloginNotStarted({status:409,data:{error:{type:'adobe_relogin_start_failed'}}}),true)
 let state,posts=0,visible=false;const queries=[]
 const tracker=createAdobeReloginTracker({start:async()=>{posts++;throw{status:503,data:{error:{type:'server_error'}}}},list:async id=>{queries.push(id);return{items:visible?[task('a',{request_id:'proxy-503'})]:[]}},isRejected:isAdobeReloginNotStarted},value=>state=value,()=>{})
 await tracker.start(['a'],'proxy-503')
 assert.equal(state.requests[0].uncertain,true);assert.equal(tracker.busy('a'),true)
 await tracker.sync();await tracker.start(['a'],'must-not-repost')
 assert.equal(posts,1);assert.ok(queries.includes('proxy-503'))
 visible=true;await tracker.sync()
 assert.equal(state.requests.length,0);assert.equal(state.tasks[0].request_id,'proxy-503');assert.equal(posts,1)
 const notStarted={status:503,data:{error:{type:'adobe_relogin_not_started'}}}
 assert.equal(isAdobeReloginNotStarted(notStarted),true)
 const rejectedTracker=createAdobeReloginTracker({start:async()=>{throw notStarted},list:async()=>({items:[]}),isRejected:isAdobeReloginNotStarted},()=>{},()=>{})
 await assert.rejects(rejectedTracker.start(['b'],'maintenance'),error=>error===notStarted)
 assert.equal(rejectedTracker.busy('b'),false)
})
