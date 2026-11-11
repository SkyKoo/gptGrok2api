import test from 'node:test'
import assert from 'node:assert/strict'
import { adobeGenerationQuotaItems, adobeCreditLabel } from '../src/lib/adobeQuotas.ts'

const pools = [
 {id:'daily-image',resource_type:'firefly_complimentary_generations',total:3,used:1,available:2,refresh_period:'P1D',kinds:['image'],models:['gemini-3.1-flash-image','gpt-image-2.5-flare']},
 {id:'daily-video',resource_type:'firefly_complimentary_generations',total:1,used:0,available:1,refresh_period:'P1D',kinds:['video'],models:['veo-3.1-lite','veo-3.1-fast']},
 {id:'image-trial',resource_type:'firefly_limited_taste',total:3,used:0,available:3,kinds:['image'],models:['gpt-image-2']},
]
test('Adobe displays shared generation pools once and keeps common credits separate',()=>{
 const quota={status:'known',available:10,total:10,used:0,generation:{status:'known',pools:[...pools,pools[0]],models:[]}}
 const items=adobeGenerationQuotaItems(quota)
 assert.deepEqual(items.map(item=>[item.label,item.text]),[['每日图片','2 / 3'],['每日视频','1 / 1'],['图片体验','3 / 3']])
 assert.equal(items[0].models.length,2)
 assert.equal(adobeCreditLabel(quota),'通用积分 10')
})
test('Adobe failed or unknown generation query never presents old counts as available',()=>{
 for(const status of ['unknown','error','stale']){
  const items=adobeGenerationQuotaItems({status:'known',available:10,generation:{status,pools,models:[]}})
  assert.ok(items.every(item=>item.text==='未知'&&item.tone==='muted'))
 }
 assert.equal(adobeGenerationQuotaItems({status:'known',available:10})[0].text,'未知')
 assert.equal(adobeGenerationQuotaItems({status:'known',generation:{status:'error',pools:[],models:[]}})[0].text,'未知（查询失败）')
})
test('Adobe explicit exhaustion remains zero while missing or invalid values remain unknown',()=>{
 for(const available of [undefined,NaN,Infinity,-1]){
  const items=adobeGenerationQuotaItems({status:'known',generation:{status:'known',pools:[{...pools[1],available}],models:[]}})
  assert.equal(items[0].text,'未知')
 }
 const item=adobeGenerationQuotaItems({status:'known',generation:{status:'known',pools:[{...pools[1],available:0,used:1}],models:[]}})[0]
 assert.equal(item.text,'0 / 1');assert.equal(item.tone,'danger')
 assert.equal(adobeCreditLabel({status:'error',available:10}),'通用积分未知')
})
