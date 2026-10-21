import assert from 'node:assert/strict'
import test from 'node:test'
import { cpaBindingRequest, duplicateCPATargets } from '../src/lib/cpaBinding.ts'

const preview = () => ({name:'selected.json', error:'', source_version:'source-hash', suggested_ref:'cfm-1', candidates:[
  {account_ref:'cfm-1', email:'person@example.test', target_version:'target-hash', has_credential:true, identical:false, conflict:''},
  {account_ref:'cfm-conflict', email:'other@example.test', target_version:'other-hash', conflict:'邮箱不一致'},
]})

test('only chosen existing target and explicit replacement enter request; tokens stay out', () => {
  const row = {...preview(), access_token:'must-not-forward', refresh_token:'must-not-forward'}
  assert.equal(cpaBindingRequest(row, '', false), null)
  assert.deepEqual(cpaBindingRequest(row, 'cfm-1', false), {name:'selected.json', account_ref:'cfm-1', source_version:'source-hash', target_version:'target-hash', replace:false})
  assert.equal(cpaBindingRequest(row, 'cfm-1', true).replace, true)
})

test('invalid, conflicting and unavailable source cannot create a bind request', () => {
  assert.equal(cpaBindingRequest({...preview(), error:'incomplete'}, 'cfm-1', true), null)
  assert.throws(() => cpaBindingRequest(preview(), 'not-in-preview', false))
  assert.throws(() => cpaBindingRequest(preview(), 'cfm-conflict', true))
  assert.throws(() => cpaBindingRequest({...preview(), source_version:''}, 'cfm-1', false))
})

test('one target per batch; skipped rows do not count as duplicates', () => {
  assert.equal(duplicateCPATargets(['', '', 'cfm-1', 'cfm-2']), false)
  assert.equal(duplicateCPATargets(['cfm-1', '', 'cfm-1']), true)
  assert.equal(duplicateCPATargets([]), false)
})
