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
