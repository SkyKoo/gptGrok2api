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
