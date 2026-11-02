import test from 'node:test'
import assert from 'node:assert/strict'
import { normalizeRegistrationTargetSettings, registrationTargetSettingsPayload, switchRegistrationTarget } from '../src/lib/registrationTargetSettings.ts'

const legacy = () => ({ target: 'openai', mode: 'quota', total: 20, threads: 4, target_quota: 100, target_available: 12, check_interval: 60, proxy: 'http://openai.example.test:8080', resin_proxy_url: 'http://P:fixture@resin.example.test:2260' })

test('legacy settings seed independent profiles without changing proxy', () => {
 const input=legacy(); const profiles=normalizeRegistrationTargetSettings(input)
 assert.equal(profiles.openai.mode,'quota'); assert.equal(profiles.grok.mode,'total')
 assert.equal(profiles.grok.proxy,input.proxy)
 profiles.grok.total=1; assert.equal(profiles.openai.total,20); assert.equal(input.total,20)
})

test('switching saves current drafts and restores every task field', () => {
 const config=legacy(); config.target_settings=normalizeRegistrationTargetSettings(config)
 switchRegistrationTarget(config,'grok')
 Object.assign(config,{proxy:'direct',resin_proxy_url:'',total:2,threads:1,target_quota:7,target_available:3,check_interval:90})
 switchRegistrationTarget(config,'openai')
 for (const [key,value] of Object.entries(legacy())) assert.equal(config[key],value,key)
 config.mode='available'; config.total=30
 switchRegistrationTarget(config,'grok')
 assert.equal(config.proxy,'direct');assert.equal(config.total,2);assert.equal(config.threads,1);assert.equal(config.check_interval,90);assert.equal(config.resin_proxy_url,'')
 const saved=JSON.parse(JSON.stringify({...config,target_settings:registrationTargetSettingsPayload(config)}))
 const restored=normalizeRegistrationTargetSettings(saved)
 assert.equal(restored.openai.mode,'available');assert.equal(restored.openai.total,30);assert.equal(restored.grok.proxy,'direct')
 switchRegistrationTarget(saved,'openai');assert.equal(saved.mode,'available');assert.equal(saved.proxy,legacy().proxy)
})

test('active edits replace only their own persisted snapshot', () => {
 const config=legacy();config.target_settings=normalizeRegistrationTargetSettings(config)
 config.total=9
 const payload=registrationTargetSettingsPayload(config)
 assert.equal(payload.openai.total,9);assert.equal(payload.grok.total,20)
 payload.openai.total=100;assert.equal(config.total,9)
})
