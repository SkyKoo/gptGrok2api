import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, realpath, mkdir, writeFile, readFile, lstat, unlink, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { spawn } from 'node:child_process';
import { acquireProfileCacheLock } from './profile-cache.mjs';

async function fixture(t) {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'profile-cache-js-')));
  t.after(() => rm(root, { recursive: true, force: true }));
  const run = join(root, 'profile-1');
  await mkdir(run, { mode: 0o700 });
  return { root, run };
}

test('profile lock is exclusive without rewriting state and releases only its own inode', async t => {
  const { run } = await fixture(t);
  const state = join(run, 'state.json');
  await writeFile(state, 'private-fixture-state', { mode: 0o600 });
  const before = await lstat(state);
  const lease = await acquireProfileCacheLock(run);
  await assert.rejects(acquireProfileCacheLock(run), { code: 'profile_busy' });
  assert.equal(await readFile(state, 'utf8'), 'private-fixture-state');
  assert.equal((await lstat(state)).mtimeMs, before.mtimeMs);
  await lease.release();
  await lease.release();
  await assert.rejects(lstat(join(run, '.lock')), { code: 'ENOENT' });
});

test('replaced profile lock remains untouched and all owned handles are closed', async t => {
  const { run } = await fixture(t);
  const lease = await acquireProfileCacheLock(run);
  await unlink(join(run, '.lock'));
  await writeFile(join(run, '.lock'), 'replacement-owner', { mode: 0o600 });
  await assert.rejects(lease.release(), { code: 'profile_lock_changed' });
  assert.equal(await readFile(join(run, '.lock'), 'utf8'), 'replacement-owner');
  assert.equal(lease.handle.fd, -1);
});

async function mockedHelper(t, scenario) {
  const { root } = await fixture(t);
  const mock = join(root, 'process.mjs');
  const loader = join(root, 'loader.mjs');
  await writeFile(mock, `
    import {EventEmitter} from 'node:events';
    import {PassThrough} from 'node:stream';
    export function spawn(){
      const child=new EventEmitter(); child.pid=${scenario === 'spawn_failure' ? 'undefined' : '12345'};
      child.stdout=new PassThrough();child.stdin=new EventEmitter();
      globalThis.fixtureState={closed:false,killed:false};
      const close=code=>{globalThis.fixtureState.closed=true;child.emit('close',code)};
      const send=row=>child.stdout.emit('data',JSON.stringify(row)+'\\n');
      const progress={progress:true,files:80,bytes:800,removedFiles:64,removedBytes:640};
      const final={status:'cleaned',code:'cache_removed',files:80,bytes:800,removedFiles:80,removedBytes:800};
      child.kill=()=>{globalThis.fixtureState.killed=true;setTimeout(()=>close(0),50);return true};
      child.stdin.end=()=>setTimeout(()=>{
        const scenario=${JSON.stringify(scenario)};
        if(scenario==='spawn_failure'){child.emit('error',new Error('fixture spawn'));return}
        if(scenario==='watchdog')return;
        if(scenario==='success'){send(final);close(0);return}
        send(progress);
        if(scenario==='error_after_progress'){child.emit('error',new Error('fixture kill failure'));return}
        if(scenario==='invalid_counts'){send({...final,status:'unknown',removedFiles:0,removedBytes:0});return}
        if(scenario==='regressing_counts'){send({...final,removedFiles:0,removedBytes:0});return}
        if(scenario==='final_then_junk')send(final);
        child.stdout.emit('data','not JSON\\n');
      },0);
      return child;
    }
  `);
  await writeFile(loader, `export async function resolve(specifier,context,next){
    if(specifier==='node:child_process'&&context.parentURL?.endsWith('/profile-cache.mjs'))
      return {url:${JSON.stringify(pathToFileURL(mock).href)},shortCircuit:true};
    return next(specifier,context);
  }`);
  const code = `
    const {cleanClosedProfileCache}=await import(${JSON.stringify(new URL('./profile-cache.mjs',import.meta.url).href)});
    const result=await cleanClosedProfileCache('/fixture/profile-1',{fd:7});
    process.stdout.write(JSON.stringify({result,state:globalThis.fixtureState}));
  `;
  const child=spawn(process.execPath,['--experimental-loader',loader,'--input-type=module','-e',code],{stdio:['ignore','pipe','pipe']});
  let stdout='',stderr='';child.stdout.on('data',chunk=>stdout+=chunk);child.stderr.on('data',chunk=>stderr+=chunk);
  const exitCode=await new Promise((resolve,reject)=>{child.on('error',reject);child.on('close',resolve)});
  assert.equal(exitCode,0,stderr);
  return JSON.parse(stdout);
}

test('helper final response returns the confirmed result',async t=>{
  const {result,state}=await mockedHelper(t,'success');
  assert.equal(result.status,'cleaned');assert.equal(result.removedFiles,80);
  assert.equal(state.closed,true);assert.equal(state.killed,false);
});

test('launched helper error retains progress and waits for close before returning',async t=>{
  const {result,state}=await mockedHelper(t,'error_after_progress');
  assert.equal(result.status,'partial');assert.equal(result.code,'cache_helper_interrupted');
  assert.equal(result.removedFiles,64);assert.equal(result.removedBytes,640);assert.equal(result.outcomeUnknown,true);
  assert.equal(state.closed,true);
});

test('invalid or regressing output cannot erase confirmed deletion counts or override failure',async t=>{
  for(const scenario of ['invalid_counts','regressing_counts','malformed','final_then_junk'])await t.test(scenario,async sub=>{
    const {result,state}=await mockedHelper(sub,scenario);
    assert.equal(result.status,'partial');assert.equal(result.code,'cache_helper_invalid_response');
    assert.equal(result.removedFiles,scenario==='final_then_junk'?80:64);assert.equal(result.outcomeUnknown,true);
    assert.equal(state.closed,true);
  });
});

test('watchdog waits for termination and classifies unconfirmed deletion as unknown',async t=>{
  const {result,state}=await mockedHelper(t,'watchdog');
  assert.equal(result.status,'partial');assert.equal(result.code,'cache_helper_interrupted');
  assert.equal(result.outcomeUnknown,true);assert.equal(state.killed,true);assert.equal(state.closed,true);
});

test('failed spawn does not claim any deletion',async t=>{
  const {result}=await mockedHelper(t,'spawn_failure');
  assert.equal(result.status,'blocked');assert.equal(result.code,'cache_helper_unavailable');
  assert.equal(result.removedFiles,0);assert.equal(result.outcomeUnknown,undefined);
});
