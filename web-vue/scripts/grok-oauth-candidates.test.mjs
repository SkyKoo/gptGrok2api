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

test('background progress and attention states remain searchable after reloading the inventory', () => {
 const items=[
  {id:'grok-queued',email:'queued@example.test',login_status:'active',oauth_status:'queued',oauth_job_id:'job-1'},
  {id:'grok-running',email:'running@example.test',login_status:'active',oauth_status:'running',oauth_job_id:'job-2'},
  {id:'grok-attention',email:'attention@example.test',login_status:'active',oauth_status:'manual_action_required'},
 ]
 assert.deepEqual(filterGrokOAuthCandidates(items,'排队中','').map(i=>i.id),['grok-queued'])
 assert.deepEqual(filterGrokOAuthCandidates(items,'授权中','running').map(i=>i.oauth_job_id),['job-2'])
 assert.equal(filterGrokOAuthCandidates(items,'需处理','manual_action_required').length,1)
 assert.equal(filterGrokOAuthCandidates(items,'','pending').length,0)
})

const { mergeGrokOAuthRows, cpaAction, cpaStatusText, oauthProgressText } = await import('../src/views/accounts/grokOAuthCandidates.ts')

test('quality risk survives a failed retry and prevents CPA delivery actions', () => {
  const quality = { status: 'error', blocked: true, confirmed_at: 1, removal: { status: 'manual_required', file_name: 'xai-one.json' } }
  const rows = mergeGrokOAuthRows([{ id: 'source', email: 'one@example.test', oauth_account_id: 'one', oauth_status: 'authorized' }], [
    { id: 'one', email: 'one@example.test', has_access_token: true, has_refresh_token: true, quality, cpa_managed: true, cpa_sync: { status: 'synced' } },
  ])
  assert.equal(rows[0].quality, quality)
  assert.equal(cpaAction(rows[0]), null)
  assert.equal(cpaStatusText(rows[0]), '降智凭据待移除')
  assert.equal(filterGrokOAuthCandidates(rows, '', 'quality_risk').length, 1)
  assert.equal(filterGrokOAuthCandidates(rows, '', 'quality_error').length, 1)
  assert.equal(cpaAction({ ...rows[0], quality: { status: 'normal', blocked: false }, cpa_sync: { status: 'quality_removed' } }), null, 'a passed recheck must not upload the old local copy')
})

test('one row combines SSO authorization and CPA state, preserving orphan credentials', () => {
  const candidates = [{ id: 'grok-one', email: 'One@Example.Test', login_status: 'active', oauth_status: 'requires_renewal', oauth_account_id: 'oauth-one' }]
  const accounts = [
    { id: 'oauth-one', email: 'one@example.test', cpa_managed: true, cpa_sync: { status: 'synced', pool_id: 'target' } },
    { id: 'oauth-orphan', email: 'orphan@example.test', status: 'active' },
  ]
  const rows = mergeGrokOAuthRows(candidates, accounts)
  assert.equal(rows.length, 2)
  assert.equal(rows[0].oauth_status, 'authorized', 'stale local access expiry must not demand reauthorization after handoff')
  assert.equal(rows[0].account.id, 'oauth-one')
  assert.equal(rows[1].source_account_id, undefined)
  assert.equal(rows[1].login_status, 'unlinked')
})

test('legacy pending and unknown delivery check CPA without claiming successful delivery', () => {
  const legacy = { oauth_account_id: 'oauth-one', cpa_sync: { status: 'failed', pool_id: 'target', at: 100 } }
  assert.equal(cpaAction(legacy), 'check')
  assert.equal(cpaStatusText(legacy), '待核对 CPA')
  assert.equal(cpaAction({ oauth_account_id: 'oauth-two', cpa_sync: { status: 'unknown' } }), 'check')
  assert.equal(cpaAction({ oauth_account_id: 'oauth-three', has_access_token: true, has_refresh_token: true, cpa_sync: { status: 'failed' } }), 'sync')
})

test('latest job progress belongs to its row and distinguishes CPA syncing', () => {
  const rows = mergeGrokOAuthRows([{ id: 'grok-one', email: 'one@example.test', oauth_status: 'authorized' }], [], [
    { id: 'old', source_account_id: 'grok-one', created_at: 1, updated_at: 30, status: 'failed' },
    { id: 'new', source_account_id: 'grok-one', created_at: 10, updated_at: 20, status: 'running', stage: 'syncing_cpa', message: '正在同步 CPA' },
  ])
  assert.equal(rows[0].job.id, 'new')
  assert.equal(oauthProgressText(rows[0]), '正在同步 CPA')
  assert.equal(cpaStatusText(rows[0]), '正在同步')
})

test('terminal task corrects an inventory read captured before completion', () => {
  const rows = mergeGrokOAuthRows([{ id: 'grok-one', email: 'one@example.test', oauth_status: 'running', oauth_job_id: 'job-one' }], [], [
    { id: 'job-one', source_account_id: 'grok-one', status: 'authorized', stage: 'cpa_sync_failed', created_at: 10, updated_at: 20, account: { id: 'oauth-one', has_access_token: true, has_refresh_token: true, cpa_sync: { status: 'failed', pool_id: 'cpa-a' } } },
  ])
  assert.equal(rows[0].oauth_status, 'authorized')
  assert.equal(rows[0].oauth_account_id, 'oauth-one')
  assert.equal(cpaAction(rows[0]), 'sync')
  assert.equal(cpaStatusText(rows[0]), '同步失败')
})

const { grokOAuthImportPayload } = await import('../src/views/accounts/grokOAuthCandidates.ts')
test('CPA JSON import uses the real endpoint top-level credential shape and pinned target', () => {
  const payload = grokOAuthImportPayload({ type: 'xai', email: 'fixture@example.test', access_token: 'fixture-access', refresh_token: 'fixture-refresh', sub: 'fixture-sub', expired: '2026-11-13T12:00:00Z' }, 'cpa-a', Date.parse('2026-11-13T11:00:00Z'))
  assert.equal(payload.credential, undefined)
  assert.equal(payload.access_token, 'fixture-access')
  assert.equal(payload.refresh_token, 'fixture-refresh')
  assert.equal(payload.subject, 'fixture-sub')
  assert.equal(payload.cpa_pool_id, 'cpa-a')
  assert.equal(payload.expires_in, 3600)
  assert.throws(() => grokOAuthImportPayload({ ...payload, type: 'codex' }, 'cpa-a'), /xAI/)
  assert.throws(() => grokOAuthImportPayload({ ...payload, expired: '2020-01-01T00:00:00Z' }, 'cpa-a'), /已过期/)
})
