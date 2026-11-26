#!/usr/bin/env python3
"""Fenced Worker image-only replacement; the caller owns the lock and held ingress."""
import argparse
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shutil
import subprocess
import time
import uuid
import common

os.umask(0o077)
OLD_NAME = 'cfm-browser-worker'
LEGACY_NAME = 'gptgrok2api-grok-browser-1'
NAME = 'cfm-browser-worker'
SERVICE = 'grok-browser'
CACHE = '/root/.cloakbrowser'
ENV = {'CLOAKBROWSER_CACHE_DIR': CACHE, 'CLOAKBROWSER_AUTO_UPDATE': 'false',
       'CLOAKBROWSER_RELEASE_CHANNEL': 'stable', 'CFM_BROWSER_WORKER_CONCURRENCY': '1'}
PROTECTED = ('register.json', 'grok_accounts.json', 'grok_runtime_accounts.json',
             'grok_registration_tasks.json', 'adobe_accounts.json.enc',
             'adobe_registration_tasks.enc.json', 'adobe_relogin_tasks.json.enc')


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def runtime_fingerprint(info):
    """Bind a successful probe to the same process and configuration without logging env."""
    return digest({key: info[key] for key in
                   ('Id', 'Name', 'Image', 'RestartCount', 'Config', 'HostConfig', 'Mounts', 'NetworkSettings')}
                  | {'started_at': info['State']['StartedAt']})


def idle_health(value, service):
    # bool compares equal to 1 in Python, but is not a valid capacity field.
    return (isinstance(value, dict) and type(value.get('capacity')) is int
            and value == dict(status='ok', service=service, capacity=1))


def envmap(container):
    return dict(item.split('=', 1) for item in container['Config']['Env'])


def replace_stanza(raw, service):
    """JSON flow mappings are YAML; other services remain byte-for-byte intact."""
    lines = raw.decode().splitlines(keepends=True)
    starts = [i for i, line in enumerate(lines) if re.match(r'^  grok-browser:', line)]
    if len(starts) != 1:
        raise RuntimeError('expected one worker Compose stanza')
    start, end = starts[0], len(lines)
    for i in range(start + 1, len(lines)):
        if re.match(r'^(?:  [A-Za-z0-9_-]+:|[A-Za-z0-9_-]+:)', lines[i]):
            end = i
            break
    return ''.join(lines[:start] + ['  grok-browser: ' + json.dumps(service, ensure_ascii=False) + '\n\n'] + lines[end:]).encode()


def replace_image(raw, image):
    if '\n' in image or '\r' in image:
        raise RuntimeError('invalid image')
    data, n = re.subn(rb'(?m)^GPTGROK2API_GROK_BROWSER_IMAGE=[^\r\n]*',
                     b'GPTGROK2API_GROK_BROWSER_IMAGE=' + image.encode(), raw)
    if n != 1:
        raise RuntimeError('expected one worker image variable')
    return data


def preserve_container_name(raw):
    """Validate the existing shared name and return the original Compose bytes."""
    lines = raw.decode().splitlines(keepends=True)
    starts = [i for i, line in enumerate(lines) if re.match(r'^  grok-browser:', line)]
    if len(starts) != 1:
        raise RuntimeError('expected one worker Compose stanza')
    start, end = starts[0], len(lines)
    for i in range(start + 1, len(lines)):
        if re.match(r'^(?:  [A-Za-z0-9_-]+:|[A-Za-z0-9_-]+:)', lines[i]):
            end = i
            break
    inline = lines[start].split(':', 1)[1].strip()
    if inline:
        try:
            service = json.loads(inline)
        except ValueError:
            raise RuntimeError('unsupported inline Worker Compose stanza') from None
        if not isinstance(service, dict) or service.get('container_name') != NAME:
            raise RuntimeError('existing shared worker name differs')
    else:
        fields = [i for i in range(start + 1, end) if re.match(r'^    container_name:', lines[i])]
        if len(fields) != 1 or lines[fields[0]].split(':', 1)[1].strip() not in (NAME, '"' + NAME + '"', "'" + NAME + "'"):
            raise RuntimeError('existing shared worker name differs')
    return raw


def atomic_preserving(path, raw):
    if path.read_bytes() == raw:
        return
    info = path.stat()
    temp = path.with_name(path.name + '.adobe-new')
    with temp.open('wb') as out:
        os.fchmod(out.fileno(), info.st_mode & 0o777)
        os.fchown(out.fileno(), info.st_uid, info.st_gid)
        out.write(raw)
        out.flush()
        os.fsync(out.fileno())
    os.replace(temp, path)
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


class Worker:
    def __init__(self, args):
        self.root = Path(args.root).resolve()
        self.op = Path(args.operation_dir).resolve()
        self.backup = self.op / 'worker-before'
        self.m = json.loads(Path(args.candidate_manifest).read_text())
        self.r = common.rolling(self.root)
        if self.m['commit'] != common.COMMIT:
            raise RuntimeError('worker manifest differs from pinned release plan')
        self.h = self.r.Host()
        self.cache = self.root / 'browser-cache/cloakbrowser'
        self.data = self.root / 'data/browser-worker'
        self.envfile = self.root / '.env'
        self.composefile = self.root / 'compose.yaml'
        self.timeout = args.wait_seconds
        self.phase = 'initialized'
        self.failure_code = None
        self.h.overall_deadline = time.monotonic() + common.require_pipeline_budget(self.op)
        image = self.h.image(self.m['image'])
        if (self.m['service'] != SERVICE or self.m['container'] != NAME
                or image['Id'] != self.m['image_id'] or image['Architecture'] != 'arm64'
                or image['Config'].get('Labels', {}).get('org.opencontainers.image.revision') != self.m['commit']):
            raise RuntimeError('candidate worker identity changed')

    def event(self, phase, **values):
        self.phase = phase
        state = dict(phase=phase, at=self.r.now(), image=self.m['image'], image_id=self.m['image_id'], **values)
        self.r.atomic_json(self.op / 'worker-status.json', state)
        return state

    def failure_evidence(self, error):
        """Persist only structural failure evidence; never include env or command output."""
        sample = {}
        try:
            _, info = self.current_worker()
            state = info.get('State', {})
            health = state.get('Health') or {}
            sample = {
                'container': info.get('Name', '').lstrip('/'),
                'image_id': info.get('Image'),
                'running': state.get('Running'),
                'restarts': info.get('RestartCount'),
                'oom': state.get('OOMKilled'),
                'health_status': health.get('Status'),
                'health_log_count': len(health.get('Log') or []),
            }
            try:
                value = self.health()
                sample['worker_health'] = {
                    key: value.get(key) for key in ('status', 'service', 'capacity')
                    if key in value
                }
            except Exception as health_error:
                sample['worker_health_error_type'] = type(health_error).__name__
        except Exception as inspect_error:
            sample['inspect_error_type'] = type(inspect_error).__name__
        payload = {
            'at': self.r.now(),
            'action': getattr(self, 'action', None),
            'phase': self.phase,
            'error_type': type(error).__name__,
            'reason_code': self.failure_code,
            'container': sample,
        }
        target = self.op / 'worker-error.json'
        if target.exists():
            target = self.op / ('worker-error-' + uuid.uuid4().hex + '.json')
        try:
            self.r.atomic_json(target, payload)
        except Exception:
            pass

    def compose(self, *args):
        return ['docker', 'compose', '--project-directory', str(self.root), '--env-file',
                str(self.envfile), '-f', str(self.composefile), *args]

    def config(self):
        return json.loads(self.h.run(self.compose('config', '--format', 'json')))

    def config_at(self, compose, envfile):
        return json.loads(self.h.run(['docker', 'compose', '--project-directory', str(self.root),
                                     '--env-file', str(envfile), '-f', str(compose),
                                     'config', '--format', 'json']))

    def current_worker(self, expected=None):
        names = self.h.run(['docker', 'container', 'ls', '-a', '--format', '{{.Names}}']).decode().splitlines()
        matches = [name for name in (NAME, LEGACY_NAME) if name in names]
        if len(matches) != 1 or matches[0] != NAME or (expected is not None and expected != NAME):
            raise RuntimeError('Existing shared Worker is missing, conflicting or unexpected')
        return matches[0], self.h.inspect(matches[0])

    def health(self, name=OLD_NAME):
        code = 'import urllib.request;print(urllib.request.urlopen("http://127.0.0.1:8899/health",timeout=3).read().decode())'
        return json.loads(self.h.run(['docker', 'exec', name, 'python', '-c', code], timeout=6))

    def fenced(self):
        with (self.root / 'maintenance.lock').open('a') as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                pass
            else:
                fcntl.flock(lock, fcntl.LOCK_UN)
                raise RuntimeError('caller must own maintenance.lock')
        current = self.r.load(self.root / 'current.json')
        gate = self.h.gateway(current['frontdoor'])
        state = self.h.control(current['container'])
        frozen = (state.get('phase') == 'frozen' and state.get('paused') is True
                  and state.get('writer_lease') is False and state.get('active_requests') == 0
                  and all(v == 0 for v in state.get('jobs', {}).values()))
        if not gate.get('route', {}).get('held') or not (self.r.idle(state) or frozen):
            raise RuntimeError('app must be idle/frozen and ingress held')

    def no_browsers(self, name=OLD_NAME):
        # Names only, never command lines containing credentials.
        code = """import pathlib,json
active=0;zombies=0
for p in pathlib.Path('/proc').glob('[0-9]*/comm'):
 try:
  name=p.read_text().strip()
  if not (name.startswith(('chrome','chromium')) or name=='node'):continue
  status=(p.parent/'status').read_text()
 except OSError: continue
 state=next((line.split(':',1)[1].strip()[:1] for line in status.splitlines() if line.startswith('State:')), '')
 if state=='Z':zombies+=1
 else:active+=1
print(json.dumps({'active':active,'zombies':zombies}))"""
        counts = json.loads(self.h.run(['docker', 'exec', name, 'python', '-c', code], timeout=6))
        with (self.op / 'worker-process-checks.jsonl').open('a') as record:
            record.write(json.dumps(dict(at=self.r.now(), **counts)) + '\n')
        if counts['active']:
            raise RuntimeError('worker still has browser/Node processes')
        return counts

    def protected(self):
        return {name: self.r.file_digest(self.root / 'data' / name)
                if (self.root / 'data' / name).exists() else None for name in PROTECTED}

    def service(self, original):
        out = json.loads(json.dumps(original))
        if out.get('container_name') != NAME:
            raise RuntimeError('existing worker name differs')
        # Compose versions serialize byte counts as integers or decimal strings.
        # Validate the exact size without changing the saved Compose value.
        shm_size = original.get('shm_size')
        valid_shm = ((type(shm_size) is int and shm_size == 536870912)
                     or (type(shm_size) is str and shm_size == '536870912'))
        if original.get('init') is not True or not valid_shm:
            raise RuntimeError('expected existing worker init/shared-memory settings missing')
        if any(original.get('environment', {}).get(k) != v for k, v in ENV.items()):
            raise RuntimeError('expected existing worker environment missing')
        volumes = original.get('volumes', [])
        for source, target, readonly in ((self.data, '/data', False), (self.cache, CACHE, False),
                (self.root / 'secrets/cloakbrowser-license.key', CACHE + '/license.key', True)):
            matches = [m for m in volumes if m.get('target') == target]
            if (len(matches) != 1 or matches[0].get('type') != 'bind'
                    or matches[0].get('source') != str(source)
                    or bool(matches[0].get('read_only', False)) != readonly):
                raise RuntimeError('existing worker persistent/license binding differs')
        aliases = (original.get('networks', {}).get('default') or {}).get('aliases', [])
        if not {'grok-browser', 'cfm-browser-worker'} <= set(aliases):
            raise RuntimeError('expected existing worker aliases missing')
        out['image'] = self.m['image']
        out['container_name'] = NAME
        return out

    def license_check(self):
        license_path = self.root / 'secrets/cloakbrowser-license.key'
        if license_path.is_symlink() or not license_path.is_file() or license_path.stat().st_size == 0:
            raise RuntimeError('expected nonempty regular license file is missing')
        if license_path.stat().st_mode & 0o077:
            raise RuntimeError('license file must not be group/world accessible')

    def environment_check(self, old, proposed):
        image_env = envmap(self.h.image(self.m['image']))
        # `compose config` serializes literal dollars as $$ for re-use as input.
        image_env.update({key: value.replace('$$', '$') if isinstance(value, str) else value
                          for key, value in proposed.get('environment', {}).items()})
        expected = envmap(old)
        changed = sorted(key for key in set(expected) | set(image_env) if image_env.get(key) != expected.get(key))
        if changed:
            raise RuntimeError('candidate image would change existing environment keys: ' + ','.join(changed))

    def candidate_config(self, config, original_env, original_compose):
        expected = json.loads(json.dumps(config))
        expected['services'][SERVICE] = self.service(config['services'][SERVICE])
        return expected, replace_image(original_env, self.m['image']), preserve_container_name(original_compose)

    def rollback_config(self, config, original_compose):
        """Restore this operation's exact original service and Compose bytes."""
        self.service(config['services'][SERVICE])  # Validate the existing topology.
        return json.loads(json.dumps(config)), original_compose

    def validate_config(self, expected, changed_env, changed_compose):
        # Normalize both with Compose before touching the live files (shm_size,
        # mount ordering and other documented schema normalization included).
        attempt = self.op / ('worker-config-' + uuid.uuid4().hex)
        attempt.mkdir(mode=0o700)
        (attempt / '.env').write_bytes(changed_env)
        (attempt / 'compose.yaml').write_bytes(changed_compose)
        (attempt / 'expected.json').write_text(json.dumps(expected))
        actual = self.config_at(attempt / 'compose.yaml', attempt / '.env')
        canonical = self.config_at(attempt / 'expected.json', attempt / '.env')
        if actual != canonical:
            raise RuntimeError('proposed Compose diff exceeds reviewed worker-only changes')
        return canonical

    def config_plan(self, config, original_env, original_compose):
        expected, env, compose = self.candidate_config(config, original_env, original_compose)
        canonical = self.validate_config(expected, env, compose)
        proof = {'previous_container': OLD_NAME, 'candidate_container': NAME,
                 'candidate_image_id': self.m['image_id'],
                 'original_env_sha256': common.sha(original_env),
                 'candidate_env_sha256': common.sha(env),
                 'original_compose_sha256': common.sha(original_compose),
                 'candidate_compose_sha256': common.sha(compose),
                 'resolved_config_sha256': common.sha(json.dumps(canonical, sort_keys=True).encode())}
        return proof, canonical, env, compose

    def checked_config_plan(self, config, original_env, original_compose):
        proof, canonical, env, compose = self.config_plan(config, original_env, original_compose)
        if self.r.load(self.op / 'worker-config-plan.json') != proof:
            raise RuntimeError('Worker image-only configuration plan changed')
        return canonical, env, compose

    def verify_candidate_config(self):
        config = self.r.load(self.backup / 'compose-config.json')
        canonical, env, compose = self.checked_config_plan(config, (self.backup / '.env').read_bytes(),
                                                          (self.backup / 'compose.yaml').read_bytes())
        if self.envfile.read_bytes() != env or self.composefile.read_bytes() != compose or self.config() != canonical:
            raise RuntimeError('live Worker configuration differs from the exact image-only plan')

    def snapshot(self):
        if self.backup.exists():
            required = ('.env', 'compose.yaml', 'worker-inspect.json', 'compose-config.json',
                        'protected.json', 'health.json')
            if any(not (self.backup / name).is_file() for name in required):
                raise RuntimeError('worker baseline is incomplete; inspect retained evidence before continuing')
            return self.r.load(self.backup / 'worker-inspect.json'), self.r.load(self.backup / 'compose-config.json')
        _, old = self.current_worker(OLD_NAME)
        config = self.config()
        if old['Image'] == self.m['image_id']:
            raise RuntimeError('candidate already running without operation baseline')
        if self.h.image(old['Config']['Image'])['Id'] != old['Image']:
            raise RuntimeError('old image tag changed')
        self.service(config['services'][SERVICE])
        self.backup.mkdir(mode=0o700)
        shutil.copy2(self.envfile, self.backup / '.env')
        shutil.copy2(self.composefile, self.backup / 'compose.yaml')
        self.r.atomic_json(self.backup / 'worker-inspect.json', old)
        self.r.atomic_json(self.backup / 'compose-config.json', config)
        self.r.atomic_json(self.backup / 'protected.json', self.protected())
        self.r.atomic_json(self.backup / 'health.json', self.health())
        return old, config

    def seed_cache(self, old):
        marker = self.op / 'worker-cache-seeded.json'
        matches = [m for m in old['Mounts'] if m['Destination'] == CACHE]
        if (len(matches) != 1 or matches[0]['Source'] != str(self.cache)
                or matches[0].get('Type') != 'bind' or matches[0].get('RW') is not True):
            raise RuntimeError('this image-only release requires the existing persistent cache')
        value = dict(source_container_id=old['Id'], target=str(self.cache), already_mounted=True)
        if marker.exists():
            if self.r.load(marker) != value:
                raise RuntimeError('cache provenance changed')
        else:
            self.r.atomic_json(marker, value)

    def cache_check(self):
        # Version markers only. No license key or browser launch.
        code = """import hashlib,json,os,struct,subprocess
from cloakbrowser.config import CHROMIUM_VERSION,get_chromium_version,get_effective_version,get_binary_path,get_platform_tag
v=get_effective_version(pro=True,release_channel='stable')
assert v, 'no stable Pro browser marker found'
p=get_binary_path(v,pro=True)
assert p.is_file() and os.access(p,os.X_OK), 'selected browser missing/not executable'
with p.open('rb') as f:header=f.read(64)
assert header[:5]==b'\\x7fELF\\x02' and header[5]==1 and struct.unpack('<H',header[18:20])[0]==183, 'browser must be ELF64 aarch64'
js="import fs from 'node:fs';import {CHROMIUM_VERSION,getChromiumVersion,getEffectiveVersion,getBinaryPath,getPlatformTag} from '/app/adobe/node_modules/cloakbrowser/dist/config.js';const v=getEffectiveVersion(true,'stable');if(!v||!fs.existsSync(getBinaryPath(v,true)))throw Error('missing browser');fs.accessSync(getBinaryPath(v,true),fs.constants.X_OK);console.log(JSON.stringify({version:v,platform:getPlatformTag(),path:getBinaryPath(v,true),chromium_version:CHROMIUM_VERSION,platform_chromium_version:getChromiumVersion()}));"
node=json.loads(subprocess.check_output(['node','--input-type=module','-e',js],text=True))
assert node['version']==v and node['platform']==get_platform_tag()=='linux-arm64'
assert node['path']==str(p) and node['chromium_version']==CHROMIUM_VERSION and node['platform_chromium_version']==get_chromium_version()
with p.open('rb') as f: digest=hashlib.file_digest(f,'sha256').hexdigest()
print(json.dumps(dict(version=v,platform=node['platform'],path=str(p),binary_sha256=digest,python_chromium_version=CHROMIUM_VERSION,python_platform_chromium_version=get_chromium_version(),node=node,python_node_match=True,browser_started=False)))"""
        command = ['docker', 'run', '--rm', '--network', 'none', '--cpus=1', '--memory=512m',
                   '--entrypoint', 'python', '-e', 'PYTHONDONTWRITEBYTECODE=1',
                   '-e', 'CLOAKBROWSER_CACHE_DIR=' + CACHE, '--mount',
                   'type=bind,source=' + str(self.cache) + ',target=' + CACHE + ',readonly',
                   self.m['image'], '-c', code]
        result = json.loads(self.h.run(command, timeout=20))
        record = self.op / 'worker-cache-verification.json'
        if record.exists():
            prior = self.r.load(record)
            if any(prior.get(key) != result.get(key) for key in ('version', 'platform', 'path', 'binary_sha256')):
                raise RuntimeError('prepared browser version/binary changed; inspect before switching')
        self.r.atomic_json(record, result)
        return result

    def prepare_cache(self):
        """Preseed before holding ingress; never starts a licensed session."""
        self.license_check()
        _, old = self.current_worker(OLD_NAME)
        config = self.config()
        proposed = self.service(config['services'][SERVICE])
        self.environment_check(old, proposed)
        existing = envmap(old)
        if any(existing.get(key) for key in ('CLOAKBROWSER_VERSION', 'CLOAKBROWSER_BINARY_PATH', 'CLOAKBROWSER_DOWNLOAD_URL')):
            raise RuntimeError('custom browser pin/path/download requires separate cache review')
        if self.health().get('capacity') != 1:
            raise RuntimeError('worker owns work; defer cache preparation')
        self.no_browsers()
        self.seed_cache(old)
        result = self.cache_check()
        self.rollback_cache_check(old)
        processes = self.no_browsers()
        if self.current_worker(OLD_NAME)[1]['Id'] != old['Id']:
            raise RuntimeError('source worker changed during cache preparation')
        return self.event('cache_prepared', cache=result, processes=processes)

    def rollback_cache_check(self, old):
        if envmap(old).get('CLOAKBROWSER_CACHE_DIR', CACHE) != CACHE:
            raise RuntimeError('old worker cache path differs from preserved rollback cache')
        expected = self.r.load(self.op / 'worker-cache-verification.json')
        # Verify the prior image resolver against the same unchanged binary.
        # against the same cache without a license mount or binary execution.
        code = """import hashlib,json,os,sys
from cloakbrowser.config import get_effective_version,get_binary_path,get_platform_tag
expected=json.loads(sys.argv[1]);v=get_effective_version(pro=True,release_channel='stable')
p=get_binary_path(v,pro=True)
assert v==expected['version'] and str(p)==expected['path'] and get_platform_tag()==expected['platform']
assert p.is_file() and os.access(p,os.X_OK)
with p.open('rb') as f:assert hashlib.file_digest(f,'sha256').hexdigest()==expected['binary_sha256']
print(json.dumps(dict(version=v,cache_matches=True,browser_started=False)))"""
        command = ['docker', 'run', '--rm', '--network', 'none', '--cpus=1', '--memory=512m',
                   '--entrypoint', 'python', '-e', 'PYTHONDONTWRITEBYTECODE=1',
                   '-e', 'CLOAKBROWSER_CACHE_DIR=' + CACHE, '--mount',
                   'type=bind,source=' + str(self.cache) + ',target=' + CACHE + ',readonly',
                   old['Image'], '-c', code, json.dumps(expected)]
        result = json.loads(self.h.run(command, timeout=20))
        self.r.atomic_json(self.op / 'worker-rollback-cache-verification.json', result)
        return result

    def wait_health(self, image_id, service, name=NAME):
        if not math.isfinite(self.timeout) or self.timeout <= 0:
            raise RuntimeError('worker health wait budget must be positive and finite')
        deadline = time.monotonic() + self.timeout
        observed = None
        while time.monotonic() < deadline:
            self.phase = 'wait_health:inspect'
            try:
                _, actual = self.current_worker(name)
            except (RuntimeError, self.r.ReleaseError, subprocess.SubprocessError, OSError, ValueError):
                self.failure_code = 'inspect_unavailable'
            else:
                # These facts cannot converge into the expected process merely by
                # waiting. Keep them outside the transient transport-error handler.
                if actual['Image'] != image_id:
                    self.fail_readiness('image_mismatch')
                if actual['State']['OOMKilled'] is not False:
                    self.fail_readiness('oom')
                if actual['State']['Running'] is not True:
                    self.fail_readiness('process_stopped')
                identity = runtime_fingerprint(actual)
                if observed is not None and identity != observed:
                    self.fail_readiness('process_or_configuration_changed')
                observed = identity
                self.phase = 'wait_health:health'
                try:
                    health = self.health(name)
                except (RuntimeError, self.r.ReleaseError, subprocess.SubprocessError, OSError, ValueError):
                    self.failure_code = 'health_unavailable'
                else:
                    if isinstance(health, dict) and health.get('status') == 'ok' and health.get('service') != service:
                        self.fail_readiness('health_service_mismatch')
                    if idle_health(health, service):
                        self.phase = 'wait_health:processes'
                        self.no_browsers(name)
                        _, fresh = self.current_worker(name)
                        if runtime_fingerprint(fresh) != observed:
                            self.fail_readiness('process_or_configuration_changed')
                        if not fresh['State']['Running'] or fresh['State']['OOMKilled']:
                            self.fail_readiness('process_stopped_or_oom')
                        if time.monotonic() > deadline:
                            self.fail_readiness('deadline_exceeded')
                        self.failure_code = None
                        return fresh
                    self.failure_code = 'health_not_idle_or_contract_mismatch'
            remaining = deadline - time.monotonic()
            if remaining > 0:
                time.sleep(min(.5, remaining))
        self.fail_readiness('deadline_exceeded')

    def fail_readiness(self, reason):
        self.failure_code = reason
        message = {
            'process_or_configuration_changed':
                'process_or_configuration_changed: worker process/configuration '
                'changed since full verification',
        }.get(reason, reason)
        raise RuntimeError('worker readiness failed: ' + message)

    def verification_context(self):
        return dict(version=1, operation=str(self.op), root=str(self.root), commit=common.COMMIT,
                    manifest_sha256=digest(self.m), image_id=self.m['image_id'])

    def compare(self, actual, old, candidate, rollback_cache=False):
        if actual.get('Name') != '/' + (NAME if candidate else OLD_NAME):
            raise RuntimeError('Worker container name differs')
        if envmap(actual) != envmap(old):
            raise RuntimeError('image-only worker replacement changed environment')
        if sorted(actual['Mounts'], key=lambda m: m['Destination']) != sorted(old['Mounts'], key=lambda m: m['Destination']):
            raise RuntimeError('image-only worker replacement changed mounts')
        for key in ('PortBindings', 'SecurityOpt', 'RestartPolicy', 'LogConfig', 'Init', 'ShmSize',
                    'Memory', 'NanoCpus', 'PidsLimit', 'CapAdd', 'CapDrop', 'ReadonlyRootfs'):
            if actual['HostConfig'].get(key) != old['HostConfig'].get(key):
                raise RuntimeError('worker runtime settings changed: ' + key)
        before, after = old['NetworkSettings']['Networks'], actual['NetworkSettings']['Networks']
        if set(before) != set(after):
            raise RuntimeError('worker network membership changed')
        for name, network in before.items():
            fresh = after[name]
            if fresh.get('GwPriority') != network.get('GwPriority'):
                raise RuntimeError('worker network gateway priority changed')
            expected_aliases = set(network.get('Aliases') or [])
            if set(fresh.get('Aliases') or []) != expected_aliases:
                raise RuntimeError('worker DNS aliases changed')

    def verify(self):
        self.phase = 'verify:fenced'
        self.fenced()
        receipt_path = self.op / 'worker-verified.json'
        receipt_path.unlink(missing_ok=True)
        old = self.r.load(self.backup / 'worker-inspect.json')
        self.phase = 'verify:wait_health'
        actual = self.wait_health(self.m['image_id'], 'cfm-browser-worker')
        self.phase = 'verify:compare'
        self.compare(actual, old, True)
        self.phase = 'verify:config'
        self.verify_candidate_config()
        self.phase = 'verify:protected'
        if self.protected() != self.r.load(self.backup / 'protected.json'):
            raise RuntimeError('protected account/registration files changed')
        self.phase = 'verify:rpc'
        code = """import json,os,urllib.request,urllib.error,uuid
base='http://127.0.0.1:8899';headers={'Content-Type':'application/json'}
token=os.getenv('CFM_BROWSER_WORKER_TOKEN',os.getenv('GROK_BROWSER_TOKEN','')).strip()
if token:headers['Authorization']='Bearer '+token
def call(path,data=None):
 req=urllib.request.Request(base+path,headers=headers,data=json.dumps(data).encode() if data is not None else None)
 try:
  with urllib.request.urlopen(req,timeout=4) as r:return r.status,json.load(r)
 except urllib.error.HTTPError as r:return r.code,json.load(r)
before=call('/health')[1]
for payload in ({'provider':'adobe'}, {'provider':'adobe','profile_id':'invalid/profile'}):
 key=uuid.uuid4().hex;payload['id']=key
 assert call('/v1/browser/reservations',payload)[0]==422
 assert call('/v1/browser/reservations/'+key)[0]==404
assert call('/health')[1]==before
assert '/v1/adobe/run' in call('/openapi.json')[1]['paths']
print(json.dumps(dict(adobe_route=True,invalid_admission=422,reservation_created=False,capacity_preserved=True,browser_started=False)))"""
        checks = json.loads(self.h.run(['docker', 'exec', NAME, 'python', '-c', code], timeout=20))
        if checks != dict(adobe_route=True, invalid_admission=422, reservation_created=False,
                          capacity_preserved=True, browser_started=False):
            raise RuntimeError('worker negative RPC probe result differs')
        self.no_browsers(NAME)
        _, fresh = self.current_worker(NAME)
        if runtime_fingerprint(fresh) != runtime_fingerprint(actual):
            raise RuntimeError('worker changed during full verification')
        health = self.health(NAME)
        if (not fresh['State']['Running'] or fresh['State']['OOMKilled']
                or not idle_health(health, 'cfm-browser-worker')):
            raise RuntimeError('worker no longer idle after full verification')
        receipt = dict(self.verification_context(), full_verification=True,
                       container_id=fresh['Id'], runtime_sha256=runtime_fingerprint(fresh),
                       verified_at=self.r.now(), checks=checks)
        self.r.atomic_json(receipt_path, receipt)
        return self.event('verified', container_id=fresh['Id'], container=NAME, health=health, checks=checks)

    def verify_ready(self):
        """Recheck the fenced process once after activation.

        ``switch`` already waited for the replacement to become healthy and
        wrote a full probe receipt. The candidate remains paused here, so a
        second readiness wait cannot make a failed capacity or health contract
        converge. Fail immediately and leave the coordinator to recover.
        """
        self.phase = 'verify_ready:fenced'
        self.fenced()
        receipt = self.r.load(self.op / 'worker-verified.json')
        if (receipt.get('full_verification') is not True
                or any(receipt.get(key) != value for key, value in self.verification_context().items())
                or not isinstance(receipt.get('verified_at'), str) or not receipt['verified_at']
                or receipt.get('checks') != dict(adobe_route=True, invalid_admission=422,
                    reservation_created=False, capacity_preserved=True, browser_started=False)):
            raise RuntimeError('worker has no matching full verification receipt')
        old = self.r.load(self.backup / 'worker-inspect.json')
        self.phase = 'verify_ready:health'
        try:
            _, actual = self.current_worker(NAME)
        except (RuntimeError, self.r.ReleaseError, subprocess.SubprocessError, OSError, ValueError):
            self.fail_readiness('inspect_unavailable')
        if actual['Image'] != self.m['image_id']:
            self.fail_readiness('image_mismatch')
        if actual['State']['OOMKilled'] is not False:
            self.fail_readiness('oom')
        if actual['State']['Running'] is not True:
            self.fail_readiness('process_stopped')
        if (actual['Id'] != receipt.get('container_id')
                or runtime_fingerprint(actual) != receipt.get('runtime_sha256')):
            self.fail_readiness('process_or_configuration_changed')
        self.phase = 'verify_ready:config'
        self.compare(actual, old, True)
        self.verify_candidate_config()
        try:
            health = self.health(NAME)
        except (RuntimeError, self.r.ReleaseError, subprocess.SubprocessError, OSError, ValueError):
            self.fail_readiness('health_unavailable')
        if not idle_health(health, 'cfm-browser-worker'):
            self.fail_readiness('health_not_idle_or_contract_mismatch')
        self.phase = 'verify_ready:processes'
        self.no_browsers(NAME)
        _, fresh = self.current_worker(NAME)
        if (runtime_fingerprint(fresh) != receipt['runtime_sha256']
                or not fresh['State']['Running'] or fresh['State']['OOMKilled']):
            self.fail_readiness('process_or_configuration_changed')
        return self.event('ready_verified', container_id=fresh['Id'], container=NAME, health=health,
                          full_verification_at=receipt['verified_at'])

    def verify_live(self):
        """Read-only post-resume validation permits ordinary jobs and writes."""
        old = self.r.load(self.backup / 'worker-inspect.json')
        _, actual = self.current_worker(NAME)
        health = self.health(NAME)
        if (actual['Image'] != self.m['image_id'] or not actual['State']['Running']
                or actual['State']['OOMKilled'] or health.get('status') != 'ok'
                or health.get('service') != 'cfm-browser-worker'):
            raise RuntimeError('live worker identity/health mismatch')
        self.compare(actual, old, True)
        self.verify_candidate_config()
        code = "import json,os,urllib.request; token=os.getenv('CFM_BROWSER_WORKER_TOKEN',os.getenv('GROK_BROWSER_TOKEN','')).strip(); req=urllib.request.Request('http://127.0.0.1:8899/openapi.json',headers={'Authorization':'Bearer '+token} if token else {}); print(json.dumps({'adobe_route':'/v1/adobe/run' in json.load(urllib.request.urlopen(req,timeout=3))['paths']}))"
        routes = json.loads(self.h.run(['docker', 'exec', NAME, 'python', '-c', code], timeout=6))
        if routes.get('adobe_route') is not True:
            raise RuntimeError('live worker Adobe route missing')
        return dict(phase='verified_live', container_id=actual['Id'], image_id=actual['Image'], health=health, checks=routes)

    def switch(self):
        self.phase = 'switch:fenced'
        self.fenced()
        self.phase = 'switch:snapshot'
        old, config = self.snapshot()
        self.phase = 'switch:current_worker'
        name, actual = self.current_worker()
        if name == NAME and actual['Image'] == self.m['image_id']:
            return self.verify()
        if name != OLD_NAME or actual['Id'] != old['Id'] or actual['Image'] != old['Image']:
            raise RuntimeError('worker changed since baseline')
        self.phase = 'switch:capacity'
        if self.health().get('capacity') != 1:
            raise RuntimeError('worker has outstanding ownership')
        self.phase = 'switch:no_browsers'
        self.no_browsers()
        self.phase = 'switch:license'
        self.license_check()
        if not (self.op / 'worker-cache-seeded.json').exists():
            raise RuntimeError('prepare-cache must complete before the maintenance window')
        self.phase = 'switch:seed_cache'
        self.seed_cache(old)  # Provenance check only; preparation is already complete.
        self.phase = 'switch:cache_check'
        self.cache_check()
        if self.data.is_symlink() or self.cache.is_symlink():
            raise RuntimeError('worker storage cannot be a symlink')
        if not self.data.is_dir() or not self.cache.is_dir():
            raise RuntimeError('existing persistent worker directories are required')
        original_env = (self.backup / '.env').read_bytes()
        original_compose = (self.backup / 'compose.yaml').read_bytes()
        self.phase = 'switch:config_plan'
        canonical, changed_env, changed_compose = self.checked_config_plan(config, original_env, original_compose)
        if self.envfile.read_bytes() not in (original_env, changed_env) or self.composefile.read_bytes() not in (original_compose, changed_compose):
            raise RuntimeError('configuration changed outside worker operation')
        atomic_preserving(self.envfile, changed_env)
        atomic_preserving(self.composefile, changed_compose)
        if self.config() != canonical:
            raise RuntimeError('Compose diff exceeds reviewed worker-only changes')
        self.event('configured', previous_image=old['Config']['Image'], previous_image_id=old['Image'])
        self.fenced()
        self.phase = 'switch:compose_up'
        try:
            self.h.run(self.compose('up', '-d', '--no-deps', '--pull', 'never', SERVICE), timeout=30)
        except (self.r.ReleaseError, subprocess.SubprocessError):
            self.event('switch_command_unknown')
        # Always inspect actual state; never retry a timed-out compose up.
        self.phase = 'switch:verify'
        return self.verify()

    def rollback(self):
        self.fenced()
        old = self.r.load(self.backup / 'worker-inspect.json')
        name, actual = self.current_worker()
        if name != NAME or actual['Image'] not in (old['Image'], self.m['image_id']):
            raise RuntimeError('unknown worker; do not overwrite')
        if actual['State']['Running']:
            if self.health(name).get('capacity') != 1:
                raise RuntimeError('worker may own work; rollback refused')
            self.no_browsers(name)
        if self.h.image(old['Config']['Image'])['Id'] != old['Image']:
            raise RuntimeError('rollback image tag changed')
        config = self.r.load(self.backup / 'compose-config.json')
        original_env = (self.backup / '.env').read_bytes()
        original_compose = (self.backup / 'compose.yaml').read_bytes()
        _, changed_env, changed_compose = self.candidate_config(config, original_env, original_compose)
        rollback_config, rollback_compose = self.rollback_config(config, original_compose)
        if self.envfile.read_bytes() not in (original_env, changed_env) or self.composefile.read_bytes() not in (original_compose, changed_compose, rollback_compose):
            raise RuntimeError('rollback refused: unknown external configuration changes')
        self.rollback_cache_check(old)
        canonical = self.validate_config(rollback_config, original_env, rollback_compose)
        atomic_preserving(self.envfile, original_env)
        atomic_preserving(self.composefile, rollback_compose)
        if self.config() != canonical:
            raise RuntimeError('rollback Compose differs')
        cache_mounted = any(mount.get('Destination') == CACHE and mount.get('Source') == str(self.cache)
                            for mount in actual.get('Mounts', []))
        if name != OLD_NAME or actual['Image'] != old['Image'] or not actual['State']['Running'] or not cache_mounted:
            try:
                self.h.run(self.compose('up', '-d', '--no-deps', '--pull', 'never', SERVICE), timeout=30)
            except (self.r.ReleaseError, subprocess.SubprocessError):
                self.event('rollback_command_unknown')
        service = self.r.load(self.backup / 'health.json')['service']
        restored = self.wait_health(old['Image'], service, OLD_NAME)
        self.compare(restored, old, False, rollback_cache=True)
        return self.event('rolled_back', restored_image_id=old['Image'], data_restored=False, cache_retained=True)

    def inspect(self):
        _, old = self.current_worker(OLD_NAME)
        config = self.config()
        proposed = self.service(config['services'][SERVICE])
        self.environment_check(old, proposed)
        self.license_check()
        if any(envmap(old).get(key) for key in ('CLOAKBROWSER_VERSION', 'CLOAKBROWSER_BINARY_PATH', 'CLOAKBROWSER_DOWNLOAD_URL')):
            raise RuntimeError('custom browser pin/path/download requires separate review')
        proof, _, _, _ = self.config_plan(config, self.envfile.read_bytes(), self.composefile.read_bytes())
        rollback_expected, rollback_compose = self.rollback_config(config, self.composefile.read_bytes())
        self.validate_config(rollback_expected, self.envfile.read_bytes(), rollback_compose)
        path = self.op / 'worker-config-plan.json'
        if path.exists():
            if self.r.load(path) != proof:
                raise RuntimeError('Worker image-only configuration plan already exists with different content')
        else:
            self.r.atomic_json(path, proof)
        return dict(service=SERVICE, previous_container=OLD_NAME, container=NAME, current_image_id=old['Image'],
                    candidate_image_id=self.m['image_id'], health=self.health(),
                    data_target=str(self.data), cache_target=str(self.cache),
                    licensed_browser_will_start=False)

    def prepare(self):
        """Complete both pre-handoff Worker checks in one process."""
        self.inspect()
        return self.prepare_cache()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True)
    parser.add_argument('--operation-dir', required=True)
    parser.add_argument('--candidate-manifest', required=True)
    parser.add_argument('--action', required=True,
                        choices=['inspect', 'prepare', 'prepare-cache', 'switch', 'rollback', 'verify', 'verify-ready', 'verify-live'],
                        help='verify runs full probes; verify-ready rechecks an unchanged fully verified Worker while fenced')
    parser.add_argument('--wait-seconds', type=int, default=90,
                        help='Positive readiness deadline; identity/exit/OOM failures stop immediately')
    args = parser.parse_args()
    common.host_required()
    common.require_pipeline_budget(args.operation_dir)
    worker = None
    try:
        worker = Worker(args)
        worker.action = args.action
        worker.phase = args.action + ':start'
        with common.timing_phase(args.operation_dir, 'worker-' + args.action):
            result = getattr(worker, args.action.replace('-', '_'))()
        common.require_pipeline_budget(args.operation_dir)
        print(json.dumps(result, ensure_ascii=False))
    except (Exception, KeyboardInterrupt) as error:
        if worker is not None:
            worker.failure_evidence(error)
        print(json.dumps({'ok': False, 'error_type': type(error).__name__,
                          'message': 'Operation stopped; inspect private state and evidence before retrying'}))
        raise SystemExit(1)


if __name__ == '__main__':
    main()
