import test from 'node:test'
import assert from 'node:assert/strict'
import { buildResinProxyURL, readResinProxyURL } from '../src/views/proxy/resinProxyForm.ts'

test('Resin fields round trip reserved token characters without changing platform', () => {
  const fields = { address: 'http://resin:2260', platform: 'CFM-Grok', token: 'p@ss:/?#%word' }
  const raw = buildResinProxyURL(fields)
  assert.deepEqual(readResinProxyURL(raw), fields)
  assert.equal(new URL(raw).hostname, 'resin')
})
test('empty and incomplete endpoint can be edited without throwing', () => {
  assert.deepEqual(readResinProxyURL(''), { address: '', platform: '', token: '' })
  assert.equal(buildResinProxyURL({ address: '', platform: 'CFM-Grok', token: 'token' }), '')
  assert.equal(buildResinProxyURL({ address: 'http://', platform: '', token: '' }), 'http://')
})

test('registration errors explain cooldown and preserve unrelated upstream diagnostics', async () => {
  const { registrationProxyFailureText, registrationResinChannel } = await import('../src/lib/registrationProxy.ts')
  assert.equal(registrationResinChannel, 'resin:registration')
  const cooldown = registrationProxyFailureText('proxy: registration_cooldown retry_after="2026-10-29T10:56:03Z"')
  assert.match(cooldown, /冷却中/)
  assert.match(cooldown, /2026/)
  assert.match(registrationProxyFailureText('proxy: binding_unavailable'), /绑定不可用/)
  assert.match(registrationProxyFailureText('config: invalid_resin_channel'), /Platform/)
  const upstream = 'send_code: browser_worker_rejected upstream_status=400'
  assert.equal(registrationProxyFailureText(upstream), upstream)
})
