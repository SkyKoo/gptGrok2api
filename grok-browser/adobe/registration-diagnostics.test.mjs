import test from 'node:test';
import assert from 'node:assert/strict';
import { registrationDiagnostics } from './registration-diagnostics.mjs';

test('registration diagnostics retain the evidence and submission fence without form or network secrets', () => {
  const result = registrationDiagnostics({
    detected: 'needs_manual', code: 'human_verification_required',
    form: { humanVerification: {required:true, source:'visible_challenge_frame'}, fields: ['SECRET'], notices:['SECRET'] },
    networkDiagnosis: {code:'captcha_required', raw:'SECRET'},
    network:[{path:'SECRET', token:'SECRET'}],
  }, {submission:{started:true}, config:{password:'SECRET'}});
  assert.deepEqual(result, {submission_started:true, human_verification_required:true,
    verification_source:'visible_challenge_frame', verification_network_code:'captcha_required', detected_stage:'needs_manual'});
  assert.equal(JSON.stringify(result).includes('SECRET'), false);
});

test('untrusted diagnostics are discarded instead of copying arbitrary fields or strings', () => {
  assert.deepEqual(registrationDiagnostics({
    detected:'SECRET', form:{humanVerification:{required:'SECRET', source:'SECRET'}},
    networkDiagnosis:{code:'SECRET'},
  }, {submission:{started:'SECRET'}}), {submission_started:false,human_verification_required:false});
  assert.deepEqual(registrationDiagnostics(null, null), {submission_started:false,human_verification_required:false});
});

test('verification wait projection keeps bounded timing and fixed outcomes as a pair',()=>{
  for(const outcome of ['still_required','resolved','network_error','interrupted','observation_error']){
    const result=registrationDiagnostics({verificationWait:{elapsed_ms:15000,outcome,raw:'PRIVATE'}},null);
    assert.equal(result.verification_wait_elapsed_ms,15000);
    assert.equal(result.verification_wait_outcome,outcome);
    assert.doesNotMatch(JSON.stringify(result),/PRIVATE|raw/);
  }
  for(const value of [-1,150001,NaN,Infinity,'15000',1.5]){
    const result=registrationDiagnostics({verificationWait:{elapsed_ms:value,outcome:'still_required'}},null);
    assert.equal(result.verification_wait_elapsed_ms,undefined);
    assert.equal(result.verification_wait_outcome,undefined);
  }
  assert.equal(registrationDiagnostics({verificationWait:{elapsed_ms:0,outcome:'resolved'}},null).verification_wait_elapsed_ms,0);
  assert.equal(registrationDiagnostics({verificationWait:{elapsed_ms:10,outcome:'PRIVATE'}},null).verification_wait_elapsed_ms,undefined);
});

test('manual challenge hints retain only fixed categories and a real boolean', () => {
  const result = registrationDiagnostics({form:{humanVerification:{required:true,
    source:'visible_instruction', instruction_kind:'start_puzzle',
    frame_kind:'challenge_provider', puzzle_entry_visible:false, text:'SECRET'}}},null);
  assert.equal(result.verification_instruction_kind,'start_puzzle');
  assert.equal(result.verification_frame_kind,'challenge_provider');
  assert.equal(result.puzzle_entry_visible,false);
  const poisoned = registrationDiagnostics({form:{humanVerification:{required:true,
    instruction_kind:'SECRET',frame_kind:'SECRET',puzzle_entry_visible:'SECRET'}}},null);
  assert.equal(poisoned.verification_instruction_kind,undefined);
  assert.equal(poisoned.verification_frame_kind,undefined);
  assert.equal(poisoned.puzzle_entry_visible,undefined);
  assert.doesNotMatch(JSON.stringify([result,poisoned]),/SECRET|text/);
});

test('failure projection accepts only fixed enums and unique bounded field categories', () => {
  const result=registrationDiagnostics({code:'form_rejected',failureDiagnostics:{
    form_error:'invalid_field',invalid_fields:['email','SECRET','password','email','verification_code','profile','other'],
    failure_action:'wait_login_result',failure_phase:'submission_started',exception_type:'registration',
    message:'SECRET',stack:'SECRET',url:'SECRET',
  }},null);
  assert.deepEqual(result,{submission_started:false,human_verification_required:false,
    form_error:'invalid_field',invalid_fields:['email','password','verification_code','profile','other'],
    failure_action:'wait_login_result',failure_phase:'submission_started',exception_type:'registration'});
  assert.deepEqual(registrationDiagnostics({code:'form_rejected',failureDiagnostics:{
    form_error:'SECRET',invalid_fields:['SECRET'],failure_action:'SECRET',failure_phase:'SECRET',exception_type:'SECRET',
  }},null),{submission_started:false,human_verification_required:false});
  assert.doesNotMatch(JSON.stringify(result),/SECRET|message|stack|url/);
});

test('captured rejection evidence is not combined with unrelated later form fields', () => {
  const result=registrationDiagnostics({code:'form_rejected',
    failureDiagnostics:{form_error:'generic_notice',failure_action:'wait_login_result',failure_phase:'submission_started',exception_type:'registration'},
    form:{fields:[{type:'password',invalid:true}],notices:['The password is incorrect.']},
  },null);
  assert.equal(result.form_error,'generic_notice');assert.equal(result.invalid_fields,undefined);
  const success=registrationDiagnostics({stage:'completed',failureDiagnostics:{form_error:'invalid_password',failure_action:'fill_password'}},null);
  assert.equal(success.form_error,undefined);assert.equal(success.failure_action,undefined);
});

test('regional profile errors preserve specific safe fields and fixed reasons without raw form data', () => {
  const fields = ['email', 'password', 'verification_code', 'profile', 'first_name', 'last_name',
    'birth_day', 'birth_month', 'birth_year', 'country', 'terms', 'phone', 'other'];
  const result = registrationDiagnostics({code:'form_rejected', failureDiagnostics:{
    form_error:'invalid_field', invalid_fields:fields,
    invalid_field_reasons:[
      {field:'birth_day',reason:'required',label:'PRIVATE',id:'PRIVATE',value:'PRIVATE'},
      {field:'terms',reason:'unchecked',message:'PRIVATE'},
      {field:'email',reason:'format'}, {field:'birth_year',reason:'range'}, {field:'other',reason:'invalid'},
      {field:'PRIVATE',reason:'required'}, {field:'country',reason:'PRIVATE'},
      {field:'birth_day',reason:'range'}, null, 'PRIVATE', ['PRIVATE'],
    ],
  }}, null);
  assert.deepEqual(result.invalid_fields, fields);
  assert.deepEqual(result.invalid_field_reasons, [
    {field:'birth_day',reason:'required'}, {field:'terms',reason:'unchecked'},
    {field:'email',reason:'format'}, {field:'birth_year',reason:'range'}, {field:'other',reason:'invalid'},
  ]);
  assert.doesNotMatch(JSON.stringify(result), /PRIVATE|label|message|value/);
});

test('field reasons require the same rejection evidence and cannot classify an unrelated field', () => {
  const result = registrationDiagnostics({code:'form_rejected', failureDiagnostics:{
    form_error:'invalid_field', invalid_fields:['birth_day'],
    invalid_field_reasons:[{field:'email',reason:'required'}],
  }, form:{fields:[{type:'email',invalid:true}],notices:[]}}, null);
  assert.deepEqual(result.invalid_fields, ['birth_day']);
  assert.equal(result.invalid_field_reasons, undefined);
  const empty = registrationDiagnostics({code:'form_rejected', failureDiagnostics:{
    invalid_field_reasons:[{field:'birth_day',reason:'required'}], failure_action:'wait_login_result',
  }}, null);
  assert.equal(empty.invalid_fields, undefined);
  assert.equal(empty.invalid_field_reasons, undefined);
  const success = registrationDiagnostics({stage:'completed',failureDiagnostics:{
    invalid_fields:['birth_day'],invalid_field_reasons:[{field:'birth_day',reason:'required'}],
  }}, null);
  assert.equal(success.invalid_field_reasons, undefined);
});

test('field diagnostics bound input before filtering and retain no more than one reason per category', () => {
  const result = registrationDiagnostics({code:'form_rejected', failureDiagnostics:{
    invalid_fields:[...Array(20).fill('birth_day'),'country'],
    invalid_field_reasons:[...Array(20).fill({field:'country',reason:'required'}),{field:'birth_day',reason:'required'}],
  }}, null);
  assert.deepEqual(result.invalid_fields, ['birth_day']);
  assert.equal(result.invalid_field_reasons, undefined);
});

test('startup diagnostics retain exact fixed actions and bounded zero-inclusive step timing', () => {
  for (const action of ['starting_browser', 'adobe_loading_account_page', 'adobe_waiting_create_entry',
    'adobe_reloading_account_page', 'adobe_opening_registration', 'adobe_waiting_registration_form', 'adobe_checking_registration_form', 'adobe_restoring_registration_page']) {
    const result = registrationDiagnostics({ code: 'login_ui_not_ready', failureDiagnostics: {
      failure_action: action, failure_phase: 'before_submission', exception_type: 'timeout',
      elapsed_ms: 150_000, duration_ms: 0, message: 'PRIVATE', url: 'PRIVATE',
    } }, { submission: { started: false } });
    assert.deepEqual(result, { submission_started: false, human_verification_required: false,
      failure_action: action, failure_phase: 'before_submission', exception_type: 'timeout', elapsed_ms: 150_000, duration_ms: 0 });
    assert.doesNotMatch(JSON.stringify(result), /PRIVATE|message|url/);
  }
  for (const value of [-1, 150_001, NaN, Infinity, '100', 1.5]) {
    const result = registrationDiagnostics({ code: 'login_ui_not_ready', failureDiagnostics: {
      failure_action: 'adobe_waiting_create_entry', elapsed_ms: value, duration_ms: value,
    } }, null);
    assert.equal(result.elapsed_ms, undefined); assert.equal(result.duration_ms, undefined);
  }
  const other = registrationDiagnostics({ code: 'form_rejected', failureDiagnostics: {
    failure_action: 'submit_email', elapsed_ms: 100, duration_ms: 100,
  } }, null);
  assert.equal(other.elapsed_ms, undefined); assert.equal(other.duration_ms, undefined);
});

const start = Date.parse('2026-11-12T05:27:00Z');
const safeEvent = (patch = {}) => ({path:'https://arks-client.adobe.com/fc/gt2/public_key/:key',
  method:'POST',time:'2026-11-12T05:27:00.000Z', observedAt:'2026-11-12T05:27:02.000Z',
  elapsedMs:1_500,errorCode:'ERR_ABORTED',source:'event',...patch});

test('Worker opt-in persists bounded request timing without URLs or raw request fields', () => {
  const diagnostic = registrationDiagnostics({network:[safeEvent({token:'SECRET',body:'SECRET',headers:{authorization:'SECRET'}})]},
    {submission:{started:true}}, {now:() => start + 3_000});
  assert.deepEqual(diagnostic, {submission_started:true,human_verification_required:false,
    captured_at:'2026-11-12T05:27:03.000Z', network_events:[{resource:'verification_session',method:'POST',
      started_at:'2026-11-12T05:27:00.000Z',observed_at:'2026-11-12T05:27:02.000Z',elapsed_ms:1_500,
      error_code:'ERR_ABORTED',source:'event'}]});
  assert.doesNotMatch(JSON.stringify(diagnostic), /SECRET|https:|path|token|body|headers/);
  assert.equal(registrationDiagnostics({network:[safeEvent()]}, null).network_events, undefined);
});

test('legacy network records keep known fields without inventing observation source or times', () => {
  const legacy = safeEvent({observedAt:undefined,source:undefined,status:200,errorCode:undefined});
  const events = registrationDiagnostics({network:[legacy]}, null, {now:() => start + 3_000}).network_events;
  assert.deepEqual(events, [{resource:'verification_session',method:'POST',started_at:'2026-11-12T05:27:00.000Z',
    status:200,elapsed_ms:1_500}]);
});

test('malicious and unknown diagnostic fields are rejected at the persistence boundary', () => {
  const poisoned = [
    safeEvent({path:'https://arks-client.adobe.com/fc/gt2/public_key/SECRET?token=SECRET'}),
    safeEvent({path:'https://arks-client.adobe.com.evil.test/fc/gt2/public_key/:key'}),
    safeEvent({method:'SECRET'}), safeEvent({time:'2026-11-12T05:27:00Z SECRET'}),
    safeEvent({time:'2026-02-31T05:27:00.000Z'}), safeEvent({time:'2026-11-12T05:00:00Z'}),
    safeEvent({time:'2026-11-12T05:27:30Z'}), safeEvent({errorCode:'SECRET',status:'200'}),
    null, 'SECRET', ['SECRET'],
    safeEvent({observedAt:'SECRET',source:'SECRET',elapsedMs:'SECRET',raw:'SECRET'}),
  ];
  const result = registrationDiagnostics({network:poisoned}, null, {now:() => start + 3_000});
  assert.equal(result.network_events.length,1);
  assert.deepEqual(result.network_events[0],{resource:'verification_session',method:'POST',
    started_at:'2026-11-12T05:27:00.000Z',error_code:'ERR_ABORTED'});
  assert.doesNotMatch(JSON.stringify(result),/SECRET|evil|https:/);
  for (const now of [undefined,()=>NaN,()=>Infinity,()=>start+0.5,()=>0,()=>Date.UTC(2200,0),()=>{throw Error('SECRET');}]) {
    assert.deepEqual(registrationDiagnostics({network:poisoned},null,{now}),{submission_started:false,human_verification_required:false});
  }
});

test('timeline limits both the recent input window and the final persisted event count', () => {
  const network = Array.from({length:60}, (_,offset) => safeEvent({time:new Date(start + offset).toISOString()}));
  const result = registrationDiagnostics({network},null,{now:()=>start+3_000});
  assert.equal(result.network_events.length,8);
  assert.equal(result.network_events[0].started_at,new Date(start+52).toISOString());
  assert.equal(result.network_events[7].started_at,new Date(start+59).toISOString());
  const hiddenByWindow = [safeEvent(), ...Array.from({length:40},()=>({path:'SECRET'}))];
  assert.equal(registrationDiagnostics({network:hiddenByWindow},null,{now:()=>start+3_000}).network_events,undefined);
});
