import test from 'node:test';
import assert from 'node:assert/strict';
import { sanitizeMediaDiagnostics } from './media-diagnostics.mjs';
import { registrationDiagnostics } from './registration-diagnostics.mjs';

const diagnostic = () => ({
  media_runtime:{state:'pending',readiness_reason:'vendors_pending',value_kind:'null',vendors_started:3,vendors_complete:2},
  media_page:{route:'editor',matching_host_count:2,authentication_count:1,arp_observer_count:1,scan_complete:true},
  media_timing:{candidate_changes:0,source_changes:1,authentication_changes:1,emissions:8,
    value_changes:2,proof_changes:0,first_source_ms:0,first_nonempty_ms:16,vendors_ready_ms:1000,last_candidate_ms:1200},
});

test('media diagnostic projection retains only bounded evidence, with no credential-bearing runtime objects',()=>{
  const input=diagnostic();
  input.media_runtime.token='PRIVATE';input.media_runtime.runtime_proof={instance:'PRIVATE'};
  input.media_page.url='PRIVATE';input.media_page.authentication={accessToken:'PRIVATE'};
  input.media_timing.raw='PRIVATE';input.cookie='PRIVATE';input.session_diagnostics={raw:'PRIVATE'};
  const projected=sanitizeMediaDiagnostics(input);
  assert.deepEqual(projected,diagnostic());assert.doesNotMatch(JSON.stringify(projected),/PRIVATE|token|proof":|url|cookie/);
  input.media_runtime.state='invalid';input.media_timing.emissions=20;
  assert.deepEqual(projected,diagnostic());
});

test('media diagnostic projection rejects arrays, unknown categories, invalid counts and inconsistent vendor totals',()=>{
  for(const value of [null,undefined,'PRIVATE',42,[],['PRIVATE']]) assert.deepEqual(sanitizeMediaDiagnostics(value),{});
  assert.deepEqual(sanitizeMediaDiagnostics({media_runtime:[],media_page:'PRIVATE',media_timing:[]}),{});
  assert.deepEqual(sanitizeMediaDiagnostics({media_runtime:{state:'PRIVATE',readiness_reason:'PRIVATE',value_kind:'PRIVATE'},
    media_page:{route:'PRIVATE',scan_complete:'PRIVATE'},media_timing:{raw:'PRIVATE'}}),{});
  for(const value of [-1,NaN,Infinity,'0',0.5]) {
    assert.deepEqual(sanitizeMediaDiagnostics({media_runtime:{vendors_started:value,vendors_complete:value},
      media_page:{matching_host_count:value,authentication_count:value,arp_observer_count:value},
      media_timing:{emissions:value,first_source_ms:value}}),{});
  }
  assert.deepEqual(sanitizeMediaDiagnostics({media_runtime:{vendors_started:4,vendors_complete:4},
    media_page:{matching_host_count:33},media_timing:{emissions:65536,first_source_ms:60001}}),{});
  assert.deepEqual(sanitizeMediaDiagnostics({media_runtime:{state:'pending',vendors_started:1,vendors_complete:2}}),
    {media_runtime:{state:'pending'}});
  for(const counts of [{matching_host_count:1,authentication_count:2,arp_observer_count:1},
    {matching_host_count:2,authentication_count:1,arp_observer_count:2}]) {
    assert.deepEqual(sanitizeMediaDiagnostics({media_page:{route:'editor',...counts}}),{media_page:{route:'editor'}});
  }
  assert.deepEqual(sanitizeMediaDiagnostics({media_runtime:{vendors_started:0,vendors_complete:0},
    media_page:{matching_host_count:32,scan_complete:false},media_timing:{emissions:65535,first_source_ms:60000}}),
    {media_runtime:{vendors_started:0,vendors_complete:0},media_page:{matching_host_count:32,scan_complete:false},
      media_timing:{emissions:65535,first_source_ms:60000}});
});

test('registration persistence revalidates media diagnostics only for an incomplete media failure',()=>{
  const input=diagnostic();input.media_runtime.token='PRIVATE';
  const projected=registrationDiagnostics({code:'media_session_incomplete',diagnostics:input},null);
  assert.deepEqual(projected,{submission_started:false,human_verification_required:false,...diagnostic()});
  for(const result of [{stage:'completed'}, {stage:'completed',code:'media_session_incomplete'},
    {code:'identity_not_verified'}, {code:'automatic_interrupted'}, {code:'PRIVATE'}]) {
    const output=registrationDiagnostics({...result,diagnostics:input},null);
    assert.equal(output.media_runtime,undefined);assert.equal(output.media_page,undefined);assert.equal(output.media_timing,undefined);
  }
  assert.deepEqual(registrationDiagnostics({code:'media_session_incomplete',diagnostics:{media_runtime:{state:'PRIVATE'}}},null),
    {submission_started:false,human_verification_required:false});
  assert.deepEqual(registrationDiagnostics({code:'media_session_incomplete',diagnostics:{media_runtime:{
    state:'pending',value_kind:'null',readiness_reason:'token_pending',vendors_started:3,vendors_complete:3}}},null).media_runtime,
    {state:'pending',value_kind:'null',readiness_reason:'token_pending',vendors_started:3,vendors_complete:3});
});
