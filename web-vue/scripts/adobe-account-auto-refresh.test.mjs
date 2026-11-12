import test from 'node:test'
import assert from 'node:assert/strict'
import { startAdobeAccountAutoRefresh } from '../src/views/accounts/adobeAccountAutoRefresh.ts'

function harness(refresh) {
 let visible=true,pending=true,handler,unsubscribed=false,id=0
 const timers=new Map()
 const stop=startAdobeAccountAutoRefresh(refresh,()=>pending,{
  visible:()=>visible,
  setTimer:(fn,delay)=>{assert.equal(delay,10000);timers.set(++id,fn);return id},
  clearTimer:key=>timers.delete(key),
  subscribeVisibility:fn=>{handler=fn;return()=>{unsubscribed=true}},
 })
 const flush=async()=>{await Promise.resolve();await Promise.resolve()}
 return {stop,timers,flush,setPending:v=>{pending=v},setVisible:async v=>{visible=v;handler();await flush()},isUnsubscribed:()=>unsubscribed, tick:async()=>{const [key,fn]=timers.entries().next().value;timers.delete(key);fn();await flush()}}
}
test('automatic processing refreshes only when pending and visible, with visibility resume',async()=>{
 let reads=0;const h=harness(async()=>{reads++})
 await h.tick();assert.equal(reads,1)
 await h.setVisible(false);await h.tick();assert.equal(reads,1)
 await h.setVisible(true);assert.equal(reads,2);assert.equal(h.timers.size,1)
 h.setPending(false);await h.tick();assert.equal(reads,2)
 h.stop();assert.equal(h.timers.size,0);assert.equal(h.isUnsubscribed(),true)
})
test('list reads do not overlap and unmount prevents rescheduling a pending response',async()=>{
 let reads=0,finish;const h=harness(()=>{reads++;return new Promise(resolve=>{finish=resolve})})
 await h.tick();await h.setVisible(true);assert.equal(reads,1)
 h.stop();finish();await h.flush();assert.equal(h.timers.size,0)
})
test('temporary list error keeps the next automatic observation scheduled',async()=>{
 let reads=0;const h=harness(async()=>{reads++;throw Error('temporary')})
 await h.tick();assert.equal(reads,1);assert.equal(h.timers.size,1)
 await h.tick();assert.equal(reads,2);h.stop()
})
