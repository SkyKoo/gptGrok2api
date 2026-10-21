import assert from 'node:assert/strict'
import test from 'node:test'
import { parseSessionJsonAccount } from '../src/lib/sessionImport.ts'

test('maps real session identity and expiry while excluding arbitrary credentials/settings', () => {
  const value = parseSessionJsonAccount(JSON.stringify({
    accessToken: ' fixture-token ', user: {email:' person@example.test ', id:'user-fixture'},
    expires:'2027-01-01T00:00:00Z', refresh_token:'do-not-import', two_factor_secret:'do-not-import',
    enabled:false, status:'正常', proxy:'do-not-import', user_id:'user-fixture',
  }))
  assert.deepEqual(value, {access_token:'fixture-token', email:'person@example.test', user_id:'user-fixture', expired:'2027-01-01T00:00:00Z'})
})

test('supports normalized session input and an AT-only session for upstream identity discovery', () => {
  assert.equal(parseSessionJsonAccount('{"access_token":"fixture","email":"person@example.test"}').email, 'person@example.test')
  assert.equal(parseSessionJsonAccount('{"accessToken":"fixture"}').email, '')
})

test('rejects invalid JSON, wrong token types, and conflicting identities', () => {
  for (const value of ['', '{', '[]', 'null', 'false', '{"accessToken":{}}', '{"accessToken":123}']) {
    assert.throws(() => parseSessionJsonAccount(value))
  }
  assert.throws(() => parseSessionJsonAccount(JSON.stringify({accessToken:'fixture', email:'wrong@example.test', user:{email:'actual@example.test'}})), /身份/)
  assert.throws(() => parseSessionJsonAccount(JSON.stringify({accessToken:'fixture', user_id:'wrong', user:{id:'actual'}})), /身份/)
  assert.equal(parseSessionJsonAccount(JSON.stringify({accessToken:'fixture', email:'Person@example.test', user:{email:'person@example.test'}})).email, 'Person@example.test')
})
