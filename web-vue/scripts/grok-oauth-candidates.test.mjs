import test from 'node:test'
import assert from 'node:assert/strict'
import { filterGrokOAuthCandidates } from '../src/views/accounts/grokOAuthCandidates.ts'

test('pending inventory remains visible with zero OAuth credentials; filters and search are independent', () => {
 const items=[
  {id:'grok-a',email:'Pending@Example.Test',login_status:'active',oauth_status:'pending'},
  {id:'grok-b',email:'known@example.test',login_status:'异常',oauth_status:'authorized'},
  {id:'grok-c',email:'expired@example.test',login_status:'active',oauth_status:'requires_renewal'},
 ]
 assert.equal(filterGrokOAuthCandidates(items,'','').length,3)
 assert.equal(filterGrokOAuthCandidates(items,'','pending')[0].id,'grok-a')
 assert.equal(filterGrokOAuthCandidates(items,' PENDING@EXAMPLE.TEST ','pending').length,1)
 assert.equal(filterGrokOAuthCandidates(items,'grok-b','authorized').length,1)
 assert.equal(filterGrokOAuthCandidates(items,'异常','pending').length,0)
 assert.equal(filterGrokOAuthCandidates(items,'重新授权','').length,1)
})
