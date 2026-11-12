import test from 'node:test';
import assert from 'node:assert/strict';
import { formFailureDiagnostics } from './browser.mjs';
import { registrationDiagnostics } from './registration-diagnostics.mjs';
import { BrowserRegistration } from './browser.mjs';
import { PrivateRunStore } from './core.mjs';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

test('native English birthday controls retain exact fixed categories and missing-value reasons', () => {
  const form={fields:['day','month','year'].map(name=>({name,type:'text',invalid:true,validity:{valueMissing:true}}))};
  assert.deepEqual(formFailureDiagnostics(form),{
    form_error:'invalid_field',invalid_fields:['birth_day','birth_month','birth_year'],
    invalid_field_reasons:['birth_day','birth_month','birth_year'].map(field=>({field,reason:'required'})),
  });
});

test('birthday month and year labels never turn into birthday day', () => {
  for(const [label,field] of [['Birthday month','birth_month'],['Birthday year','birth_year'],['Birthday day','birth_day'],['出生年份','birth_year']]) {
    assert.deepEqual(formFailureDiagnostics({fields:[{label,invalid:true}]}).invalid_fields,[field]);
  }
});

test('required unchecked agreements and phone controls are classified without guessing from a notice', () => {
  const diagnostic=formFailureDiagnostics({fields:[
    {type:'checkbox',name:'termsConsent',required:true,checked:false,invalid:true},
    {type:'tel',name:'contact',invalid:true,validity:{patternMismatch:true}},
    {name:'year',invalid:true,validity:{rangeUnderflow:true}},
  ]});
  assert.deepEqual(diagnostic.invalid_fields,['terms','phone','birth_year']);
  assert.deepEqual(diagnostic.invalid_field_reasons,[{field:'terms',reason:'unchecked'},{field:'phone',reason:'format'},{field:'birth_year',reason:'range'}]);
  assert.deepEqual(formFailureDiagnostics({fields:[{name:'terms',type:'checkbox',checked:false,required:false}]}),{});
});

test('an unknown invalid field remains unknown and never persists its identity, value or error text', () => {
  const form={fields:[{name:'PRIVATE_FIELD',label:'PRIVATE_LABEL',value:'PRIVATE_VALUE',invalid:true}],notices:[]};
  const result=registrationDiagnostics({code:'form_rejected',form},null);
  assert.deepEqual(result.invalid_fields,['other']);
  assert.equal(result.invalid_field_reasons,undefined);
  assert.doesNotMatch(JSON.stringify(result),/PRIVATE/);
});

test('non-boolean native validity data cannot claim a rejection reason', () => {
  const result=formFailureDiagnostics({fields:[{name:'day',invalid:true,validity:{valueMissing:'true',rangeOverflow:1}}]});
  assert.deepEqual(result.invalid_fields,['birth_day']);
  assert.equal(result.invalid_field_reasons,undefined);
});

async function beforeCreate(t, fields) {
  const directory=await mkdtemp(join(tmpdir(),'adobe-form-preflight-'));
  const store=new PrivateRunStore(join(directory,'private'),'one');
  await store.init({email:'fixture@example.invalid'});await store.setStage('awaiting_approval');
  const browser=new BrowserRegistration(store);let clicks=0;
  browser.form=async()=>({fields,headings:[],notices:[],buttons:[{text:'Create account',disabled:false}],agreements:[]});
  browser.status=async()=>{const state=await store.load();return {stage:state.stage,code:state.code};};
  browser.authFrame=async()=>({getByRole:()=>({count:async()=>1,isEnabled:async()=>true,click:async()=>{clicks++;}})});
  browser.waitForStage=async()=>({stage:'needs_manual',form:{host:'account.adobe.com'}});
  t.after(async()=>{await store.close();await rm(directory,{recursive:true,force:true});});
  return {browser,store,clicks:()=>clicks};
}

test('a late native birthday requirement blocks before the durable creation marker and gives an exact diagnostic',async t=>{
  const f=await beforeCreate(t,[{name:'day',type:'number',required:true,filled:false,invalid:false,validity:{valueMissing:true}}]);
  const result=await f.browser.approveCreate();
  assert.equal(result.code,'form_rejected');assert.equal(f.clicks(),0);
  assert.deepEqual(result.failureDiagnostics,{form_error:'invalid_field',invalid_fields:['birth_day'],invalid_field_reasons:[{field:'birth_day',reason:'required'}]});
  assert.equal((await f.store.load()).submission.started,false);
});

test('native range and format errors cannot create an account before Adobe adds aria-invalid',async t=>{
  for(const [validity,reason] of [[{rangeOverflow:true},'range'],[{patternMismatch:true},'format']]){
    const f=await beforeCreate(t,[{name:'year',type:'text',filled:true,invalid:false,validity}]);
    const result=await f.browser.approveCreate();assert.equal(result.code,'form_rejected');assert.equal(f.clicks(),0);
    assert.deepEqual(result.failureDiagnostics.invalid_field_reasons,[{field:'birth_year',reason}]);
    assert.equal((await f.store.load()).submission.started,false);
  }
});

test('ARIA required radio groups need one selection; optional consent and disabled inputs do not block',async t=>{
  const fields=[{name:'requiredChoice',type:'radio',required:true,filled:false,checked:false,validity:{valueMissing:false}},
    {name:'requiredChoice',type:'radio',required:false,filled:false,checked:false,validity:{valueMissing:false}}];
  const blocked=await beforeCreate(t,fields);assert.equal((await blocked.browser.approveCreate()).code,'form_rejected');
  assert.equal(blocked.clicks(),0);assert.equal((await blocked.store.load()).submission.started,false);
  const accepted=await beforeCreate(t,[fields[0],{...fields[1],checked:true,filled:true},
    {name:'marketingConsent',type:'checkbox',required:false,filled:false,checked:false},
    {name:'disabled',type:'text',disabled:true,required:true,filled:false,validity:{valueMissing:true}}]);
  await accepted.browser.approveCreate();assert.equal(accepted.clicks(),1);assert.equal((await accepted.store.load()).submission.started,true);
});

test('creation preflight compares the saved country against the actual native option value', async t => {
  for (const [requested, actual, rejected] of [
    ['KR', 'US', true], ['South Korea', 'KR', false], ['Not an option', 'US', true],
  ]) await t.test(`${requested} to ${actual}`, async t => {
    const f = await beforeCreate(t, [{ tag: 'select', type: 'select-one', name: 'country', label: 'Country', filled: true, invalid: false }]);
    await f.store.save({ config: { ...(await f.store.load()).config, profile: { country: requested } } });
    f.browser.field = async () => ({
      inputValue: async () => actual,
      locator: name => { assert.equal(name, 'option'); return { evaluateAll: async fn => fn([
        { value: 'US', label: 'United States' }, { value: 'KR', label: 'South Korea' },
      ]) }; },
    });
    const result = await f.browser.approveCreate();
    assert.equal(f.clicks(), rejected ? 0 : 1);
    assert.equal((await f.store.load()).submission.started, !rejected);
    if (rejected) {
      assert.equal(result.code, 'form_rejected');
      assert.deepEqual(result.failureDiagnostics, { form_error: 'invalid_field', invalid_fields: ['country'] });
    }
  });
});

test('known form rejections never re-read a navigating page or replace evidence with network status', async t => {
  for (const submitted of [false, true]) await t.test(submitted ? 'after create' : 'before create', async t => {
    const invalid = { name: 'day', type: 'number', required: true, filled: false, invalid: true, validity: { valueMissing: true } };
    const f = await beforeCreate(t, submitted ? [] : [invalid]);
    const readForm = f.browser.form;
    let reads = 0;
    f.browser.form = async () => {
      if (++reads > 1) throw Error('Execution context was destroyed');
      return readForm();
    };
    f.browser.status = async () => {
      await f.browser.form();
      return { code: 'verification_request_aborted' };
    };
    const failure = { fields: [invalid], headings: [], notices: [], agreements: [], buttons: [] };
    f.browser.waitForStage = async () => ({ code: 'form_rejected', stage: 'needs_manual', form: failure });
    f.browser.network = { pages: [] };
    f.browser.verificationWait = { elapsed_ms: 300, outcome: 'resolved' };
    const result = await f.browser.approveCreate();
    assert.equal(reads, 1);
    assert.equal(result.code, 'form_rejected');
    assert.equal(result.submissionStarted, submitted);
    assert.equal(f.clicks(), submitted ? 1 : 0);
    assert.deepEqual(result.failureDiagnostics, { form_error: 'invalid_field', invalid_fields: ['birth_day'],
      invalid_field_reasons: [{ field: 'birth_day', reason: 'required' }] });
    assert.deepEqual(result.network, f.browser.network);
    assert.deepEqual(result.verificationWait, f.browser.verificationWait);
    assert.equal((await f.store.load()).submission.started, submitted);
    assert.equal((await f.store.load()).code, 'form_rejected');
  });
});
