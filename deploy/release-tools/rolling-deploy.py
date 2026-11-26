#!/usr/bin/env python3
"""CFM blue/green release with a stable ingress and fenced JSON writer.

Build images separately. This tool never restores data, restarts dependencies,
replays application requests or kills an admitted task. Run on the Docker host.
"""
from __future__ import annotations

import argparse
import ast
import datetime as dt
import fcntl
from decimal import Decimal, InvalidOperation
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import tarfile
import time

PROTOCOL = 'cfm-single-writer-v1'
STRATEGY = 'rolling-single-writer-v1'
BACKUP_SOURCE = 'deploy/backup/cfm_core_backup.py'
BACKUP_LOCAL_SOURCE = 'deploy/backup/local'
SNAPSHOT_ROOT_MODE = 0o700
MIN_FREE_DURING_SNAPSHOT = 256 * 1024 ** 2
DATABASE_DUMP_RESERVE = 128 * 1024 ** 2
BACKUP_LOCAL_FILES = ('backup_catalog.py', 'backup_retention.py',
                      'oracle-backup-daily.py', 'oracle-backup-maintenance.py')
BACKUP_LOCAL_SERVICE = 'gptgrok2api-backup-retention.service'
SLOTS = {'blue': ('gptgrok2api-blue-1', 'http://cfm-blue:8080'),
         'green': ('gptgrok2api-green-1', 'http://cfm-green:8080')}
RUNTIME_FILES = {'.cfm-writer.lock', '.cfm-legacy-instance.json', '.cfm-gateway.json',
                 'cfm-maintenance'}
EPHEMERAL_BROWSER_LINKS = {'SingletonCookie', 'SingletonLock', 'SingletonSocket'}
EPHEMERAL_BROWSER_DIRS = {'BrowserMetrics'}
EPHEMERAL_BROWSER_FILES = {'.lock'}
CONTRACT_INPUTS = ('internal/mailbox/store.go', 'internal/register/store.go',
    'internal/register/free_engine.go', 'internal/register/grok_engine.go',
    'internal/register/compensation.go', 'internal/register/registration_attempt.go',
    'internal/oauth/store.go', 'internal/tasks/queue.go', 'internal/tasks/redis.go',
    'internal/httpapi/admin_extra.go', 'internal/httpapi/image_tasks.go', 'internal/httpapi/video.go')


class ReleaseError(RuntimeError):
    pass


def dependency_names(metadata):
    worker = metadata.get('grok_browser_container', 'gptgrok2api-grok-browser-1')
    if worker not in ('gptgrok2api-grok-browser-1', 'cfm-browser-worker'):
        raise ReleaseError('Unrecognized shared browser container; review deployment metadata')
    return {'postgres': 'gptgrok2api-postgres-1', 'redis': 'gptgrok2api-redis-1',
            'grok-browser': worker}


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def is_ephemeral_browser_file(root, path):
    try:
        path.relative_to(Path(root) / 'data' / 'browser-worker')
    except ValueError:
        return False
    return path.name in EPHEMERAL_BROWSER_FILES


def file_digest(path, host=None):
    result=hashlib.sha256()
    with Path(path).open('rb') as stream:
        while True:
            if host: host.checkpoint()
            block=stream.read(1024*1024)
            if not block: break
            result.update(block)
    return result.hexdigest()


def atomic_json(path, value):
    path = Path(path)
    temporary = path.with_name(path.name+'.tmp')
    with temporary.open('w') as stream:
        os.fchmod(stream.fileno(), 0o600)
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def backup_tool_bytes(host, release):
    expected = release.get('backup_tool_sha256')
    if not expected:
        return None
    source = Path(release['source']).resolve()
    raw = host.run(['git', '-c', 'safe.directory='+str(source), '-C', str(source),
                    'show', release['commit']+':'+BACKUP_SOURCE])
    if len(raw) > 256*1024 or digest(raw) != expected:
        raise ReleaseError('Backup tool differs from the fixed release commit')
    ast.parse(raw)
    return raw


def install_backup_tool(root, raw):
    if raw is None:
        return
    target = Path(root)/'tools'/'backup-core.py'
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.with_suffix('.py.new')
    with temporary.open('wb') as stream:
        os.fchmod(stream.fileno(), 0o700)
        stream.write(raw); stream.flush(); os.fsync(stream.fileno())
    os.replace(temporary, target)


def canonical_json(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()


def read_backup_local(host, source, commit):
    """Read the entire project-owned package from one immutable Git commit."""
    source = Path(source).resolve()
    if not re.fullmatch('[0-9a-f]{40}', commit):
        raise ReleaseError('Local backup package requires a full source commit')
    prefix = ['git', '-c', 'safe.directory='+str(source), '-C', str(source), 'show']
    raw = host.run(prefix+[commit+':'+BACKUP_LOCAL_SOURCE+'/manifest.json'])
    if len(raw) > 16*1024:
        raise ReleaseError('Local backup package manifest is oversized')
    manifest = json.loads(raw)
    if (manifest.get('format') != 'ogg-backup-local-package-v1'
            or manifest.get('service') != 'gptgrok2api'
            or set(manifest.get('files', {})) != set(BACKUP_LOCAL_FILES)):
        raise ReleaseError('Local backup package has a different service or incomplete file set')
    files = {}
    for name in BACKUP_LOCAL_FILES:
        content = host.run(prefix+[commit+':'+BACKUP_LOCAL_SOURCE+'/'+name])
        if len(content) > 256*1024 or digest(content) != manifest['files'][name]:
            raise ReleaseError('Local backup package differs from its fixed source manifest')
        ast.parse(content)
        files[name] = content
    return {'manifest': manifest, 'files': files, 'package_id': digest(canonical_json(manifest))}


def backup_local_bytes(host, release):
    expected = release.get('backup_local_sha256')
    if expected is None:
        return None
    prepared = read_backup_local(host, release['source'], release['commit'])
    if prepared['package_id'] != expected:
        raise ReleaseError('Local backup package differs from the prepared release')
    return prepared


def local_backup_configured(root):
    current = Path(root)/'tools'/'backup-local'/'current'
    return current.exists() or current.is_symlink()


def prepare_backup_local_source(host, root, source, commit):
    """Legacy entry accepts an explicit source but still reads committed bytes only."""
    if source is None:
        if local_backup_configured(root):
            raise ReleaseError('Configured local backup requires --source for the fixed release commit')
        return None
    source = Path(source).resolve()
    local = source/BACKUP_LOCAL_SOURCE
    if not local.exists():
        if local_backup_configured(root):
            raise ReleaseError('Source commit is missing the configured local backup package')
        return None
    return read_backup_local(host, source, commit)


def install_backup_local(root, prepared):
    """Install immutable code then atomically switch current; config is deploy-owned."""
    if prepared is None:
        return None
    root = Path(root)
    if root.resolve() != root:
        raise ReleaseError('Local backup installation root is linked')
    manifest, files = prepared['manifest'], prepared['files']
    package_id = digest(canonical_json(manifest))
    if (prepared['package_id'] != package_id or manifest.get('format') != 'ogg-backup-local-package-v1'
            or manifest.get('service') != 'gptgrok2api'
            or set(files) != set(BACKUP_LOCAL_FILES) or set(manifest.get('files', {})) != set(BACKUP_LOCAL_FILES)
            or any(digest(files[name]) != manifest['files'][name] for name in BACKUP_LOCAL_FILES)):
        raise ReleaseError('Local backup package changed before installation')
    base = root/'tools'/'backup-local'
    versions = base/'versions'
    versions.mkdir(mode=0o700, parents=True, exist_ok=True)
    if versions.resolve() != versions or not versions.is_dir():
        raise ReleaseError('Local backup versions directory is linked')
    current = base/'current'
    if (current.exists() or current.is_symlink()) and (
            not current.is_symlink() or current.resolve().parent != versions):
        raise ReleaseError('Unexpected local backup current pointer')
    if current.is_symlink() and manifest.get('release_media_policy') != 'daily':
        active_manifest = load(current / 'manifest.json')
        if active_manifest.get('release_media_policy') == 'daily':
            # A rollback to older application source must still understand newer core-only archives.
            manifest = active_manifest
            package_id = digest(canonical_json(manifest))
            if current.resolve().name != package_id:
                raise ReleaseError('Active daily media policy identity differs')
            files = {name: (current / name).read_bytes() for name in BACKUP_LOCAL_FILES}
    target = versions/package_id
    if target.exists():
        if target.is_symlink() or not target.is_dir():
            raise ReleaseError('Local backup version target is not an ordinary directory')
        for name in BACKUP_LOCAL_FILES:
            path = target/name
            if path.is_symlink() or not path.is_file() or file_digest(path) != manifest['files'][name]:
                raise ReleaseError('Installed local backup version differs from immutable manifest')
        if load(target/'manifest.json') != manifest:
            raise ReleaseError('Installed local backup manifest changed')
    else:
        stage = versions/('.'+package_id+'.'+str(os.getpid())+'.new')
        stage.mkdir(mode=0o700)
        try:
            for name, raw in files.items():
                ast.parse(raw)
                descriptor = os.open(stage/name, os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW, 0o700)
                with os.fdopen(descriptor, 'wb') as stream:
                    stream.write(raw); stream.flush(); os.fsync(stream.fileno())
            atomic_json(stage/'manifest.json', manifest)
            os.replace(stage, target)
        finally:
            if stage.exists():
                shutil.rmtree(stage)
    core = root/'tools'/'backup-core.py'
    if core.is_symlink() or not core.is_file():
        raise ReleaseError('Local backup package requires the reviewed core backup helper')
    locks = sorted(str(p) for p in root.glob('*.lock') if p.is_file() and not p.is_symlink())
    core_lock = Path('/opt/1panel/backup/ogg-cfm-core/backup.lock')
    if core_lock.is_file() and not core_lock.is_symlink():
        locks = sorted(set(locks) | {str(core_lock)})
    if str(root/'maintenance.lock') not in locks:
        raise ReleaseError('Local backup package requires the maintenance lock')
    dependencies = {'format': 'ogg-daily-dependencies-v1', 'services': {
        'gptgrok2api': {'helpers': {str(core): file_digest(core)}, 'locks': locks}}}
    # This is deployment configuration, not immutable package source. A later
    # release may change the core helper while the four local modules stay equal.
    atomic_json(target/'dependencies.json', dependencies)
    policy = root/'backup-policy'
    if policy.is_symlink() or (policy.exists() and not policy.is_dir()):
        raise ReleaseError('Local backup policy state must be an ordinary directory')
    policy.mkdir(mode=0o700, exist_ok=True)
    state_info = policy.stat()
    if policy.resolve() != policy or stat.S_IMODE(state_info.st_mode) != 0o700 or state_info.st_uid != os.geteuid():
        raise ReleaseError('Local backup policy state ownership or permissions differ')
    pointer = base/('.current.'+str(os.getpid())+'.new')
    os.symlink('versions/'+package_id, pointer)
    try:
        os.replace(pointer, current)
    finally:
        if pointer.is_symlink():
            pointer.unlink()
    return {'package_id': package_id, 'source_files': manifest['files']}


def trigger_backup_local(root, release_id):
    """Queue the service-owned post-success job only after releasing deployment locks."""
    if not local_backup_configured(root):
        return {'status': 'not_configured'}
    try:
        result = subprocess.run(['systemctl', 'start', '--no-block', BACKUP_LOCAL_SERVICE],
                                capture_output=True, timeout=20, check=False)
    except (OSError, subprocess.SubprocessError):
        return {'status': 'deferred', 'service': BACKUP_LOCAL_SERVICE, 'release': release_id}
    # A scheduling failure must not turn a healthy release into a rollback.
    return {'status': 'queued' if result.returncode == 0 else 'deferred',
            'service': BACKUP_LOCAL_SERVICE, 'release': release_id,
            'returncode': result.returncode}


def verify_backup_local_ready(host, root, release):
    """Confirm the switched writer and live route before admitting pruning."""
    root = Path(root)
    current, status = load(root/'current.json'), load(root/'rolling-status.json')
    backup = Path(current['backup'])
    if (current.get('status') != 'healthy' or current.get('commit') != release['commit']
            or current.get('image_id') != release['image_id']
            or status.get('status') != 'success' or status.get('phase') != 'success'
            or status.get('backup') != str(backup) or backup.parent != root/'backups'
            or backup.resolve() != backup or not (backup/'complete').is_file()):
        raise ReleaseError('Final local backup admission has no matching successful release')
    state = host.control(current['container'])
    route = host.gateway(current['frontdoor'])['route']
    if (state.get('protocol') != PROTOCOL or state.get('phase') != 'active'
            or state.get('paused') is not False or state.get('writer_lease') is not True
            or route.get('held') is not False or route.get('upstream') != current['upstream']):
        raise ReleaseError('Final writer/route verification failed; retention remains blocked')
    candidate = host.inspect(current['container'])
    if candidate.get('Image') != release['image_id']:
        raise ReleaseError('Final live image differs; retention remains blocked')
    host.run(['docker', 'exec', current['container'], 'wget', '-q', '-T', '3', '-O', '/dev/null',
              'http://127.0.0.1:8080/readyz'])
    for name in dependency_names(current).values():
        if host.inspect(name)['State'].get('Health', {}).get('Status') != 'healthy':
            raise ReleaseError('Final dependency health failed; retention remains blocked')
    atomic_json(backup/'retention-ready.json', {'status': 'verified', 'commit': release['commit'],
        'backup': str(backup), 'verified_at': now(), 'writer_verified': True, 'route_verified': True,
        'readyz_verified': True, 'dependencies_healthy': True})


def load(path):
    return json.loads(Path(path).read_text())


def database_backup_reserve(root):
    """Reserve the largest known database dump set plus a reviewed floor.

    The live dump size is only known after PostgreSQL and Redis export. Existing
    completed backups provide a useful lower bound before the writer fence,
    while the floor covers a first deployment and small metadata files.
    """
    largest = 0
    backups = Path(root) / 'backups'
    if backups.is_dir() and not backups.is_symlink():
        for work in backups.iterdir():
            if not work.is_dir() or work.is_symlink():
                continue
            paths = [work / name for name in
                     ('postgres.dump', 'postgres-globals.sql', 'redis.rdb')]
            if all(path.is_file() and not path.is_symlink() for path in paths):
                largest = max(largest, sum(path.stat().st_size for path in paths))
    return max(DATABASE_DUMP_RESERVE, largest)


def idle(state):
    return (state.get('protocol') == PROTOCOL and state.get('phase') == 'active'
        and state.get('paused') is True and state.get('writer_lease') is True
        and state.get('active_requests') == 0
        and isinstance(state.get('jobs'), dict)
        and all(isinstance(v, int) and v == 0 for v in state['jobs'].values()))


def business_files(root, host):
    rows = {}
    data = Path(root)/'data'
    for path in sorted(data.rglob('*')):
        host.checkpoint()
        relative = path.relative_to(data)
        if (relative.parts and relative.parts[0] == 'browser-worker'
                and any(part in EPHEMERAL_BROWSER_DIRS for part in relative.parts)):
            continue
        if (relative.parts and relative.parts[0] == 'browser-worker'
                and relative.parts[-1] in EPHEMERAL_BROWSER_LINKS
                and relative.parts[-2:-1] == ('browser',)):
            if path.is_symlink():
                continue
        if path.is_symlink():
            raise ReleaseError('Business guard refuses symlinks')
        if path.is_file() and str(relative) not in RUNTIME_FILES:
            rows[str(relative)] = file_digest(path,host)
    return rows


def database_guard(host, container):
    # No rows/credentials leave this boundary, only table names and hashes.
    tables = ('mailbox_schema_migrations', 'mailboxes', 'mailbox_registration_blocks',
        'mailbox_claims', 'mailbox_events', 'mailbox_registration_retirements', 'mailbox_sources')
    sql = ' UNION ALL '.join("SELECT '%s', count(*), md5(coalesce(string_agg(to_jsonb(t)::text, E'\\n' ORDER BY to_jsonb(t)::text), '')) FROM %s t" % (name,name) for name in tables)
    rows = host.run(['docker','exec','-i',container,'sh','-c',
        'exec psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At'], input=(sql+';').encode())
    schema = host.run(['docker','exec',container,'sh','-c',
        'exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --schema-only --no-owner --no-privileges'])
    schema = b'\n'.join(line for line in schema.splitlines() if not line.startswith((b'\\restrict ',b'\\unrestrict ')))
    return {'rows': digest(rows), 'schema': digest(schema)}


def require_legacy(root):
    path = Path(root)/'current.json'
    if (path.exists() and load(path).get('release_strategy') == STRATEGY) or (Path(root)/'data'/'.cfm-gateway.json').exists():
        raise ReleaseError('Rolling layout is active or switching; legacy deploy/apply/rollback is disabled')


class Host:
    def __init__(self):
        self.deadline = None
        self.overall_deadline = None

    def checkpoint(self):
        deadlines = [value for value in (self.deadline, self.overall_deadline)
                     if value is not None]
        if deadlines and time.monotonic() >= min(deadlines):
            raise ReleaseError('Deployment deadline exhausted; keep ingress state inspectable')

    def run(self, args, *, output=None, timeout=120, input=None):
        self.checkpoint()
        deadlines = [value for value in (self.deadline, self.overall_deadline)
                     if value is not None]
        if deadlines:
            timeout = min(timeout, max(.1, min(deadlines)-time.monotonic()))
        result = subprocess.run(args, input=input, stdout=output or subprocess.PIPE,
            stderr=subprocess.PIPE, timeout=timeout)
        if result.returncode:
            # Docker/Compose/SQL stderr can contain expanded credentials.
            raise ReleaseError('Command failed: '+args[0]+'; details withheld')
        return result.stdout

    def inspect(self, name):
        return json.loads(self.run(['docker', 'inspect', name]))[0]

    def image(self, name):
        return json.loads(self.run(['docker', 'image', 'inspect', name]))[0]

    def control(self, name, path='/status', body=None):
        args = ['docker', 'exec', name, 'wget', '-q', '-T', '20', '-O', '-']
        if body is not None:
            args += ['--header=Content-Type: application/json', '--post-data='+json.dumps(body)]
        return json.loads(self.run(args+['http://127.0.0.1:9091'+path], timeout=25))

    def action(self, name, action):
        try:
            return self.control(name, '/'+action, {})
        except (ReleaseError, subprocess.TimeoutExpired):
            # A missing reply can follow a completed irreversible transition.
            state = self.control(name)
            completed = {'pause': state.get('paused') is True,
                'resume': state.get('phase') == 'active' and state.get('paused') is False,
                'freeze': state.get('phase') in ('frozen', 'gateway') and not state.get('writer_lease'),
                'activate': state.get('phase') == 'active' and state.get('writer_lease') is True}
            if completed.get(action):
                return state
            raise

    def gateway(self, front, action=None, target=''):
        state = self.control(front, '/gateway/status')
        if action is None:
            return state
        route = state['route']; epoch = route['epoch']
        try:
            return self.control(front, '/gateway/'+action, {'epoch': epoch, 'upstream': target})
        except (ReleaseError, subprocess.TimeoutExpired):
            actual = self.control(front, '/gateway/status')
            expected = dict(route, epoch=epoch+1)
            expected.update({'held': True} if action == 'hold' else
                {'held': False} if action == 'release' else {'upstream': target})
            if actual.get('route') == expected:
                return actual
            raise

    def stop_empty(self, name):
        state = self.control(name)
        if state.get('phase') == 'active':
            self.action(name, 'pause')
            if not idle(self.control(name)):
                raise ReleaseError('Candidate owns unfinished work; do not stop it')
            self.action(name, 'freeze')
        state = self.control(name)
        initializing = state.get('phase') in ('starting', 'failed', 'recovery_required') and not state.get('jobs')
        if (state.get('writer_lease') and not initializing) or state.get('active_requests'):
            raise ReleaseError('Container has not safely retired')
        # No SIGKILL deadline. A lost/slow stop is inspected, never forced.
        try:
            self.run(['docker', 'stop', '--timeout', '-1', name], timeout=20)
        except subprocess.TimeoutExpired:
            if self.inspect(name)['State']['Running']:
                raise ReleaseError('Container stop result unknown; preserve it')
        if self.inspect(name)['State']['Running']:
            raise ReleaseError('Container still running')


def source_contract(source):
    source = Path(source)
    files = list(CONTRACT_INPUTS)
    files += sorted(str(p.relative_to(source)) for p in (source/'internal/store').glob('*.go') if not p.name.endswith('_test.go'))
    rows = {name: digest((source/name).read_bytes()) for name in files}
    tracked = subprocess.check_output(['git','-c','safe.directory='+str(source),'-C',str(source),'ls-files','-z','grok-browser']).decode().split('\0')
    browser = {name: digest((source/name).read_bytes()) for name in tracked if name}
    return {'state': digest(json.dumps(rows, sort_keys=True).encode()),
        'browser': digest(json.dumps(browser, sort_keys=True).encode())}


def check_contract(previous, candidate, approval=None):
    if previous == candidate:
        return
    if approval != {'previous': previous, 'candidate': candidate}:
        raise ReleaseError('State/browser contract changed; exact reviewed compatibility manifest required')


def slot_config(old, image, slot, runtime, assets):
    if old['Config'].get('User') != '10001:10001':
        raise ReleaseError('Unexpected application UID/GID')
    env = dict(value.split('=', 1) for value in old['Config']['Env'])
    if env.get('GO_LISTEN_ADDR') != ':8080' or env.get('GROK_DATA_DIR') != '/app/data':
        raise ReleaseError('Unexpected application paths/listener')
    env.update(CFM_ROLLING_CONTROL_ADDR='127.0.0.1:9091',
        CFM_WRITER_LEASE_FILE='/app/data/.cfm-writer.lock',
        CFM_ROLLING_STATE_FILE='/app/runtime/instance.json',
        CFM_ROLLING_BOOTSTRAP_ACTIVE='false', CFM_GATEWAY_STATE_FILE='')
    if any(m['Destination'] not in ('/app/data','/app/logs','/app/runtime','/app/web_dist/assets') for m in old['Mounts']):
        raise ReleaseError('Unknown application mount; review before changing topology')
    mounts = []
    for target in ('/app/data', '/app/logs'):
        matches = [m for m in old['Mounts'] if m['Destination'] == target]
        if len(matches) != 1 or matches[0]['Type'] != 'bind' or not matches[0]['RW']:
            raise ReleaseError('Expected original data/log bind mounts')
        mounts.append({'type': 'bind', 'source': matches[0]['Source'], 'target': target})
    mounts += [{'type': 'bind', 'source': str(runtime), 'target': '/app/runtime'},
        {'type': 'bind', 'source': str(assets), 'target': '/app/web_dist/assets', 'read_only': True}]
    networks = {name: {'external': True, 'name': name} for name in old['NetworkSettings']['Networks']}
    if 'gptgrok2api_default' not in networks:
        raise ReleaseError('Primary Docker network missing')
    service = {'image': image, 'container_name': SLOTS[slot][0], 'platform': 'linux/arm64',
        'restart': 'unless-stopped', 'user': '10001:10001', 'cap_drop': ['ALL'],
        'security_opt': ['no-new-privileges:true'], 'stop_grace_period': '20m',
        'environment': env, 'volumes': mounts,
        'networks': {name: ({'aliases': ['cfm-'+slot], 'gw_priority': 100}
            if name == 'gptgrok2api_default' else {}) for name in networks},
        'labels': {'io.cfm.rolling.slot': slot, 'io.cfm.rolling.protocol': PROTOCOL, 'io.cfm.rolling.instance': runtime.name},
        'healthcheck': {'test': ['CMD', 'wget', '-q', '-T', '3', '-O', '/dev/null', 'http://127.0.0.1:9091/status'],
            'interval': '5s', 'timeout': '4s', 'retries': 12, 'start_period': '5s'},
        'logging': {'driver': 'json-file', 'options': {'max-size': '10m', 'max-file': '3'}}}
    return {'name': 'cfm-rolling', 'services': {slot: service}, 'networks': networks}


def merge_assets(host, image, target):
    if target.is_symlink():
        raise ReleaseError('Static directory must not be a symlink')
    target.mkdir(parents=True, exist_ok=True, mode=0o755)
    target.chmod(0o755)  # Override the private process umask for public assets.
    raw = host.run(['docker', 'run', '--rm', '--read-only', '--network', 'none',
        '--entrypoint', 'tar', image, '-C', '/app/web_dist/assets', '-cf', '-', '.'])
    with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
        for member in archive.getmembers():
            if member.isdir():
                continue
            path = Path(member.name)
            if not member.isfile() or path.is_absolute() or '..' in path.parts:
                raise ReleaseError('Unexpected static archive member')
            destination = target/path
            current=target
            for part in path.parent.parts:
                current=current/part
                if current.is_symlink():
                    raise ReleaseError('Static directory contains a symlink')
                current.mkdir(exist_ok=True,mode=0o755)
                current.chmod(0o755)
            if destination.is_symlink():
                raise ReleaseError('Static file must not be a symlink')
            data = archive.extractfile(member).read()
            if destination.exists() and destination.read_bytes() != data:
                raise ReleaseError('Immutable asset collision')
            if not destination.exists():
                destination.write_bytes(data); destination.chmod(0o644)


def daily_media_file(name):
    suffix = Path(name).suffix.lower()
    if name.startswith(('data/files/images/', 'data/log_input_images/')):
        return suffix in {'.png', '.jpg', '.jpeg', '.webp', '.gif', '.avif'}
    return name.startswith('data/files/videos/') and suffix in {'.mp4', '.webm'}


class Snapshot:
    """Create the release snapshot without retaining a second expanded tree.

    The legacy mode remains available for the local fault-injection tests. The
    normal rolling release uses direct mode, which writes and verifies the
    archive from the frozen source while the writer fence is held. This keeps
    one durable copy of business files instead of source plus expanded copy
    plus archive.
    """
    def __init__(self, root, work, host, direct=False):
        self.root, self.work, self.host = Path(root), Path(work), host
        self.direct = direct
        self.receipts = {}
        self.frozen_manifest = None
        self.direct_identities = None
        self.direct_archive_identity = None
        policy = self.root / 'tools/backup-local/current/manifest.json'
        self.media_included = not (direct and policy.is_file() and load(policy).get('release_media_policy') == 'daily')
        self.source_manifest = None
        if not direct:
            (self.work/'snapshot').mkdir(mode=0o700)

    def _source_paths(self):
        """Yield the private archive paths in a stable parent-first order."""
        paths = []
        for folder in ('data', 'logs'):
            base = self.root / folder
            if not base.is_dir() or base.is_symlink():
                raise ReleaseError('Snapshot source directory is invalid')
            paths.append((folder, base))
            for directory, dirs, files in os.walk(base, followlinks=False):
                directory = Path(directory)
                dirs[:] = [name for name in dirs
                           if not (directory / name).relative_to(self.root).parts[-1] in EPHEMERAL_BROWSER_DIRS
                           or (directory / name).relative_to(self.root).parts[0] != 'data'
                           or (directory / name).relative_to(self.root).parts[1] != 'browser-worker']
                for name in sorted(dirs):
                    paths.append((str((directory / name).relative_to(self.root)), directory / name))
                for name in sorted(files):
                    candidate = directory / name
                    relative_parts = candidate.relative_to(self.root).parts
                    if (len(relative_parts) >= 5
                            and relative_parts[0] == 'data'
                            and relative_parts[1] == 'browser-worker'
                            and relative_parts[-2] == 'browser'
                            and name in EPHEMERAL_BROWSER_LINKS
                            and candidate.is_symlink()):
                        continue
                    if is_ephemeral_browser_file(self.root, candidate):
                        continue
                    paths.append((str((directory / name).relative_to(self.root)), directory / name))
        for name in ('.env', 'compose.yaml', 'current.json'):
            path = self.root / name
            if path.exists() or path.is_symlink():
                if path.is_symlink() or not path.is_file():
                    raise ReleaseError('Snapshot refuses symlinks or special files')
                paths.append((name, path))
        return paths

    @staticmethod
    def _identity(info):
        return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
                info.st_size, info.st_mtime_ns, info.st_ctime_ns)

    def _capture_manifest(self, checkpoint, paths=None):
        # Walk the source tree once per capture. Both the manifest and the
        # identity map use the same stable path list, avoiding a second full
        # directory traversal before the archive is written.
        paths = list(paths) if paths is not None else self._source_paths()
        root_info = self.root.stat()
        manifest = {'.': {'type': 'directory', 'uid': root_info.st_uid, 'gid': root_info.st_gid,
                          # The live runtime root may use 0710. Backup roots
                          # are private restore trees and follow the 0700
                          # contract enforced by snapshot_integrity.
                          'mode': SNAPSHOT_ROOT_MODE, 'mtime_ns': root_info.st_mtime_ns}}
        for relative, path in paths:
            checkpoint()
            try:
                before = path.lstat()
            except FileNotFoundError:
                if is_ephemeral_browser_file(self.root, path):
                    continue
                raise
            if path.is_symlink() or not (path.is_file() or path.is_dir()):
                raise ReleaseError('Snapshot refuses symlinks or special files')
            row = {'type': 'directory' if path.is_dir() else 'file',
                   'uid': before.st_uid, 'gid': before.st_gid,
                   'mode': stat.S_IMODE(before.st_mode), 'mtime_ns': before.st_mtime_ns}
            if path.is_file():
                row['size'] = before.st_size
            manifest[relative] = row
        identities = {}
        for relative, path in paths:
            try:
                identities[relative] = self._identity(path.lstat())
            except FileNotFoundError:
                if is_ephemeral_browser_file(self.root, path):
                    continue
                raise
        identities['.'] = self._identity(root_info)
        return manifest, identities

    @staticmethod
    def _tar_info(name, row):
        member = tarfile.TarInfo('snapshot' if name == '.' else 'snapshot/' + name)
        member.type = tarfile.DIRTYPE if row['type'] == 'directory' else tarfile.REGTYPE
        member.uid, member.gid, member.mode = row['uid'], row['gid'], row['mode']
        member.mtime = row['mtime_ns'] // 1_000_000_000
        member.pax_headers['mtime'] = format(Decimal(row['mtime_ns']) / 1_000_000_000, '.9f')
        if row['type'] == 'file':
            member.size = row['size']
        return member

    def _write_direct_archive(self, manifest, checkpoint, archive_path=None, paths=None):
        archive_path = archive_path or (self.work / 'snapshot.tar.gz')
        partial = archive_path.with_name(archive_path.name + '.writing')
        if partial.exists():
            partial.unlink()
        digest_by_name = {}
        committed = False
        def write_members(archive):
            source_paths = list(paths) if paths is not None else self._source_paths()
            for name, path in [('.', self.root), *source_paths]:
                checkpoint()
                row = manifest[name]
                if row['type'] == 'directory':
                    archive.addfile(self._tar_info(name, row))
                    continue
                before = path.stat()
                digest = hashlib.sha256()
                count = 0
                with path.open('rb') as stream:
                    class Reader:
                        def read(_, size=-1):
                            nonlocal count
                            checkpoint()
                            block = stream.read(size)
                            digest.update(block)
                            count += len(block)
                            return block
                    archive.addfile(self._tar_info(name, row), Reader())
                after = path.stat()
                if self._identity(before) != self._identity(after) or count != row['size']:
                    raise ReleaseError('File changed during frozen archive')
                digest_by_name[name] = digest.hexdigest()
                manifest[name]['sha256'] = digest_by_name[name]
                if shutil.disk_usage(self.root).free < MIN_FREE_DURING_SNAPSHOT:
                    raise ReleaseError('Insufficient space while writing frozen archive')
        try:
            pigz = shutil.which('pigz')
            if pigz:
                with partial.open('wb') as output:
                    # There is no diagnostic consumer for compressor stderr.  A
                    # PIPE can fill while a large archive is being written and
                    # then stall the writer before wait() is reached.  Release
                    # errors intentionally omit command output, so discard it
                    # instead of creating a back-pressure pipe.
                    compressor = subprocess.Popen(
                        [pigz, '-1', '-p', str(max(1, min(os.cpu_count() or 1, 4))), '-c'],
                        stdin=subprocess.PIPE, stdout=output, stderr=subprocess.DEVNULL)
                    try:
                        with tarfile.open(fileobj=compressor.stdin, mode='w|', format=tarfile.PAX_FORMAT) as archive:
                            write_members(archive)
                        compressor.stdin.close()
                        if compressor.wait() != 0:
                            raise ReleaseError('Archive compressor failed')
                    except BaseException:
                        if compressor.poll() is None:
                            compressor.kill()
                        compressor.wait()
                        raise
            else:
                with tarfile.open(partial, mode='w:gz', compresslevel=1) as archive:
                    write_members(archive)
            os.chmod(partial, 0o600)
            with partial.open('rb') as stream:
                os.fsync(stream.fileno())
            os.replace(partial, archive_path)
            committed = True
        finally:
            if not committed:
                try:
                    partial.unlink()
                except FileNotFoundError:
                    pass
        directory = os.open(self.work, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        self._verify_direct_archive(archive_path, manifest, checkpoint, verify_content=False)
        self.direct_archive_identity = self._identity(archive_path.stat())

    def _verify_direct_archive(self, path, manifest, checkpoint, verify_content=True):
        seen = set()
        try:
            with tarfile.open(path, mode='r:gz') as archive:
                for member in archive:
                    checkpoint()
                    name = '.' if member.name == 'snapshot' else member.name.removeprefix('snapshot/')
                    if name not in manifest or name in seen:
                        raise ReleaseError('Frozen archive member set differs')
                    row = manifest[name]
                    if (member.isdir()) != (row['type'] == 'directory'):
                        raise ReleaseError('Frozen archive metadata differs')
                    member_mtime = Decimal(member.pax_headers.get('mtime', str(member.mtime))) * 1_000_000_000
                    if (member.uid != row['uid'] or member.gid != row['gid']
                            or member.mode != row['mode']
                            or member_mtime != row['mtime_ns']):
                        raise ReleaseError('Frozen archive metadata differs')
                    if row['type'] == 'file':
                        digest = hashlib.sha256(); count = 0
                        stream = archive.extractfile(member)
                        while True:
                            checkpoint(); block = stream.read(1024 * 1024)
                            if not block: break
                            count += len(block)
                            if verify_content:
                                digest.update(block)
                        if count != row['size'] or (verify_content and digest.hexdigest() != row['sha256']):
                            raise ReleaseError('Frozen archive content differs')
                    seen.add(name)
        except (tarfile.TarError, OSError, EOFError, InvalidOperation) as error:
            raise ReleaseError('Frozen archive verification failed') from error
        if seen != set(manifest):
            raise ReleaseError('Frozen archive member set differs')

    def business_files(self):
        if self.direct:
            if self.frozen_manifest is None:
                raise ReleaseError('Frozen archive manifest is missing')
            return {name.removeprefix('data/'): row['sha256']
                    for name, row in (self.source_manifest or self.frozen_manifest).items()
                    if name.startswith('data/') and row['type'] == 'file'
                    and name.removeprefix('data/') not in RUNTIME_FILES}
        return business_files(self.work/'snapshot', self.host)

    def sync(self, frozen=False):
        if self.direct:
            if not frozen:
                # Online scanning rejects links and special files without
                # allocating an expanded copy. The frozen pass reads content
                # once and writes the only archive copy.
                self._capture_manifest(self.host.checkpoint)
                return
            paths = self._source_paths()
            self.source_manifest, identities = self._capture_manifest(self.host.checkpoint, paths)
            self.direct_identities = identities
            self.frozen_manifest = self.source_manifest.copy()
            archive_paths = []
            for name, path in paths:
                row = self.source_manifest.get(name)
                if row is None and is_ephemeral_browser_file(self.root, path):
                    continue
                if row is None:
                    raise ReleaseError('Snapshot source disappeared')
                if not self.media_included and row['type'] == 'file' and daily_media_file(name):
                    # The cutover guard covers shared media even when its bytes live in the daily backup.
                    row['sha256'] = file_digest(path, self.host)
                    del self.frozen_manifest[name]
                else:
                    archive_paths.append((name, path))
            self._write_direct_archive(self.frozen_manifest, self.host.checkpoint, paths=archive_paths)
            _, stable_identities = self._capture_manifest(self.host.checkpoint)
            if stable_identities != self.direct_identities:
                raise ReleaseError('Frozen source changed during archive')
            atomic_json(self.work/'snapshot-metadata.json', self.frozen_manifest)
            return
        seen = set()
        for folder in ('data', 'logs'):
            for directory, dirs, files in os.walk(self.root/folder, followlinks=False):
                for name in dirs+files:
                    source = Path(directory)/name
                    if source.is_symlink():
                        raise ReleaseError('Snapshot refuses symlinks')
                for name in files:
                    self.host.checkpoint()
                    source = Path(directory)/name; relative = str(source.relative_to(self.root))
                    try:
                        before = source.stat()
                        if not stat.S_ISREG(before.st_mode):
                            raise ReleaseError('Snapshot refuses non-regular files')
                        signature = (before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns)
                        seen.add(relative)
                        if self.receipts.get(relative) == signature:
                            continue
                        target = self.work/'snapshot'/relative
                        target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                        shutil.copy2(source, target)
                        after = source.stat()
                        if (after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns) != signature:
                            if frozen:
                                raise ReleaseError('File changed inside writer fence')
                            continue
                        self.receipts[relative] = signature
                    except FileNotFoundError:
                        if frozen:
                            raise ReleaseError('File disappeared inside writer fence')
        for folder in ('data', 'logs'):
            for target in (self.work/'snapshot'/folder).rglob('*'):
                relative = str(target.relative_to(self.work/'snapshot'))
                if target.is_file() and relative not in seen:
                    target.unlink()
                    self.receipts.pop(relative, None)
        for name in ('.env', 'compose.yaml', 'current.json'):
            if (self.root/name).is_file():
                shutil.copy2(self.root/name, self.work/'snapshot'/name)

    def database(self, containers):
        for name, command in [('postgres.dump', 'exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc'),
            ('postgres-globals.sql', 'exec pg_dumpall -U "$POSTGRES_USER" --globals-only')]:
            with (self.work/name).open('wb') as output:
                self.host.run(['docker', 'exec', containers['postgres'], 'sh', '-c', command], output=output)
            if shutil.disk_usage(self.root).free < MIN_FREE_DURING_SNAPSHOT:
                raise ReleaseError('Insufficient space after PostgreSQL snapshot')
        self.host.run(['docker', 'exec', '-i', containers['postgres'], 'pg_restore', '-l'], input=(self.work/'postgres.dump').read_bytes())
        if self.host.run(['docker', 'exec', containers['redis'], 'redis-cli', '--raw', 'SAVE']).strip() != b'OK':
            raise ReleaseError('Redis snapshot failed')
        self.host.run(['docker','exec',containers['redis'],'redis-check-rdb','/data/dump.rdb'])
        self.host.run(['docker', 'cp', containers['redis']+':/data/dump.rdb', str(self.work/'redis.rdb')])
        os.chmod(self.work/'redis.rdb', 0o600)
        if shutil.disk_usage(self.root).free < MIN_FREE_DURING_SNAPSHOT:
            raise ReleaseError('Insufficient space after Redis snapshot')

    def finish(self):
        if self.direct:
            if self.frozen_manifest is None or not (self.work/'snapshot.tar.gz').is_file():
                raise ReleaseError('Frozen archive is missing')
            archive_identity = self._identity((self.work/'snapshot.tar.gz').stat())
            if self.direct_archive_identity is None or archive_identity != self.direct_archive_identity:
                raise ReleaseError('Frozen archive identity changed')
            metadata = self.work/'snapshot-metadata.json'
            if not metadata.is_file() or json.loads(metadata.read_text()) != self.frozen_manifest:
                raise ReleaseError('Frozen archive metadata changed')
            checks = {name: file_digest(self.work/name, self.host) for name in
                      ('snapshot.tar.gz', 'snapshot-metadata.json', 'postgres.dump',
                       'postgres-globals.sql', 'redis.rdb')}
            self._verify_direct_archive(self.work/'snapshot.tar.gz', self.frozen_manifest,
                                        self.host.checkpoint, verify_content=True)
            for name in checks:
                self.host.checkpoint()
                with (self.work/name).open('rb') as stream:
                    os.fsync(stream.fileno())
            atomic_json(self.work/'checksums.json', checks)
            self.host.checkpoint()
            atomic_json(self.work/'complete', {'verified_at':now(),'protocol':PROTOCOL,
                                               'archive_mode':'direct',
                                               'media_included':self.media_included,
                                               'manifest_sha256':file_digest(self.work/'snapshot-metadata.json', self.host)})
            return
        with tarfile.open(self.work/'snapshot.tar.gz', 'w:gz', compresslevel=1) as archive:
            archive.add(self.work/'snapshot', arcname='snapshot')
        with tarfile.open(self.work/'snapshot.tar.gz') as archive:
            for member in archive:
                if member.isfile():
                    with archive.extractfile(member) as source:
                        while source.read(1024*1024):
                            pass
        checks = {name: file_digest(self.work/name, self.host) for name in
            ('snapshot.tar.gz', 'postgres.dump', 'postgres-globals.sql', 'redis.rdb')}
        for name in checks:
            self.host.checkpoint()
            with (self.work/name).open('rb') as stream:
                os.fsync(stream.fileno())
        atomic_json(self.work/'checksums.json', checks)
        atomic_json(self.work/'complete', {'verified_at':now(),'protocol':PROTOCOL})


def retire_rolled_back_payload(root, work):
    """Retire only this attempt's direct backup after the old service is back."""
    receipt = {'status': 'checking', 'removed': []}
    try:
        backups = root / 'backups'
        if work.parent != backups or work.resolve(strict=True) != work:
            return
        current = load(root / 'current.json')
        previous = load(work / 'previous.json')
        failed = load(work / 'release.json')
        restored_field = {'program_rolled_back': 'data_restored',
                          'old_program_recovered': 'data_restore_performed'}.get(failed.get('phase'))
        if failed.get('status') != 'rolled_back' or not restored_field or failed.get(restored_field) is not False:
            return
        if any(os.path.lexists(work / name) for name in ('complete', 'retention-ready.json', 'snapshot')):
            raise ReleaseError('Completed or legacy backup requires separate review')
        payload = ('snapshot.tar.gz', 'postgres.dump', 'postgres-globals.sql', 'redis.rdb')
        targets = []
        for name in payload:
            p = work / name
            if os.path.lexists(p):
                if not stat.S_ISREG(p.lstat().st_mode) or os.path.ismount(p):
                    raise ReleaseError('Unexpected backup payload type')
                targets.append(p)
        if not targets:
            return
        if (current.get('status') != 'healthy' or current.get('commit') != previous.get('commit')
                or current.get('backup') != previous.get('backup') or not current.get('backup')):
            raise ReleaseError('Previous recovery point is not active')
        prior = Path(current['backup'])
        if (prior == work or prior.parent != backups or prior.resolve(strict=True) != prior
                or load(prior / 'release.json').get('status') != 'success'
                or not (prior / 'complete').is_file()):
            raise ReleaseError('Previous successful release backup is unavailable')
        checks = load(prior / 'checksums.json')
        for name in payload:
            p = prior / name
            if (not re.fullmatch('[0-9a-f]{64}', str(checks.get(name, '')))
                    or not stat.S_ISREG(p.lstat().st_mode) or file_digest(p) != checks[name]):
                raise ReleaseError('Previous recovery point is incomplete')
        # Both deploy entry points create a new private batch and hold the maintenance lock.
        receipt.update(status='removing', files=[p.name for p in targets])
        atomic_json(work / 'rollback-cleanup.json', receipt)
        for p in targets:
            p.unlink()
            receipt['removed'].append(p.name)
        receipt['status'] = 'completed'
    except (OSError, ValueError, TypeError, AttributeError, ReleaseError) as error:
        receipt.update(status='deferred', reason=str(error))
    try:
        atomic_json(work / 'rollback-cleanup.json', receipt)
    except OSError as error:
        print('Could not record rollback backup cleanup:', type(error).__name__, flush=True)


class RollingRelease:
    def __init__(self, root, host=None, drain_timeout=600, handoff_timeout=25):
        self.root, self.host = Path(root), host or Host()
        self.drain_timeout, self.handoff_timeout = drain_timeout, handoff_timeout
        self.started_monotonic = time.monotonic()

    def journal(self, phase, **extra):
        elapsed = round(time.monotonic() - self.started_monotonic, 3)
        timing = {'elapsed_seconds': elapsed}
        if self.host.overall_deadline is not None:
            timing['remaining_seconds'] = round(
                max(0, self.host.overall_deadline - time.monotonic()), 3)
        self.report.update(phase=phase, updated_at=now(), **timing, **extra)
        self.report['events'].append({'phase': phase, 'time': now(), **timing})
        atomic_json(self.work/'release.json', self.report)
        atomic_json(self.root/'rolling-status.json', self.report)
        print(json.dumps({'phase': phase, **timing, **extra}, ensure_ascii=False), flush=True)

    def start_slot(self, old, image, slot, suffix):
        name = SLOTS[slot][0]
        present = self.host.run(['docker', 'ps', '-a', '--filter', 'name=^/'+name+'$', '--format', '{{.ID}}']).strip()
        if present:
            existing = self.host.inspect(name)
            if existing['State']['Running'] or existing['Config'].get('Labels', {}).get('io.cfm.rolling.slot') != slot:
                raise ReleaseError('Inactive slot is not safely reusable')
            self.host.run(['docker', 'rm', name])
        runtime = self.root/'instances'/(self.work.name+'-'+suffix)
        runtime.mkdir(parents=True, mode=0o700); os.chown(runtime, 10001, 10001)
        config = self.root/('slot-'+slot+'.json')
        atomic_json(config, slot_config(old, image, slot, runtime, self.root/'static-assets'))
        self.owned_candidate = name
        self.owned_runtime = runtime.name
        self.host.run(['docker', 'compose', '-f', str(config), 'up', '-d', '--no-build', '--pull', 'never', '--wait', '--wait-timeout', '60', slot], timeout=75)
        state = self.host.control(name)
        if state.get('phase') != 'standby' or state.get('writer_lease') or state.get('protocol') != PROTOCOL:
            raise ReleaseError('Candidate opened shared state before activation')
        return name

    def stop_candidate(self, name):
        info = self.host.inspect(name)
        if info['Config'].get('Labels',{}).get('io.cfm.rolling.instance') != self.owned_runtime:
            raise ReleaseError('Candidate ownership cannot be confirmed; preserve container')
        self.host.stop_empty(name)

    def metadata(self, base, release, slot, status='healthy', *, recovered=False):
        metadata = dict(base, release_strategy=STRATEGY, status=status, frontdoor=self.front,
            slot=slot, container=SLOTS[slot][0], upstream=SLOTS[slot][1],
            image=release['image'], image_id=release['image_id'], commit=release['commit'],
            source=release['source'], contract=release['contract'], deployed_at=now())
        if not recovered:
            metadata['backup'] = str(self.work)
        return metadata

    def deploy(self, release, bootstrap=False, approval=None):
        old_meta = load(self.root/'current.json')
        backup_tool = backup_tool_bytes(self.host, release)
        backup_local = backup_local_bytes(self.host, release)
        if (self.root/'tools'/'backup-core.py').exists() and backup_tool is None:
            raise ReleaseError('Configured backup requires a reviewed project script in this release')
        if local_backup_configured(self.root) and backup_local is None:
            raise ReleaseError('Configured local backup requires its reviewed project package in this release')
        if old_meta.get('release_strategy') == STRATEGY:
            if bootstrap or old_meta.get('status') != 'healthy':
                raise ReleaseError('Finish the previous release; bootstrap only once')
            old_name, self.front = old_meta['container'], old_meta['frontdoor']
            slot = 'green' if old_meta['slot'] == 'blue' else 'blue'
            check_contract(old_meta['contract'], release['contract'], approval)
        else:
            if not bootstrap:
                raise ReleaseError('Legacy layout requires the explicit bootstrap action')
            old_name = self.front = 'gptgrok2api-app-1'; slot = 'blue'
            if release['commit'] != old_meta['commit'] or release['image'] != old_meta['image']:
                raise ReleaseError('Bootstrap uses the currently running protocol-capable image only')
        if release['base_commit'] != old_meta['commit']:
            raise ReleaseError('Release base differs from current version')
        old = self.host.inspect(old_name); image = self.host.image(release['image'])
        labels = image['Config'].get('Labels', {})
        if image['Id'] != release['image_id'] or image['Architecture'] != 'arm64' or image['Os'] != 'linux' or labels.get('org.opencontainers.image.revision') != release['commit'] or labels.get('io.cfm.rolling.protocol') != PROTOCOL:
            raise ReleaseError('Candidate image identity/protocol mismatch')
        if old['Config']['Image'] not in (old_meta['image'],old_meta.get('image_id')) or old['State'].get('Health', {}).get('Status') != 'healthy':
            raise ReleaseError('Active image/health differs from current metadata')
        if self.host.image(old_meta['image'])['Id'] != old['Image'] or (old_meta.get('image_id') and old_meta['image_id'] != old['Image']):
            raise ReleaseError('Previous image tag changed; retain the verified immutable rollback image')
        active = self.host.control(old_name)
        if active.get('protocol') != PROTOCOL or active.get('phase') != 'active' or not active.get('writer_lease'):
            raise ReleaseError('Active app lacks the writer handoff protocol; perform the one-time protocol upgrade first')
        dependency_containers = dependency_names(old_meta)
        dependencies = {name: self.host.inspect(container)['Id'] for name, container in dependency_containers.items()}
        for container in dependency_containers.values():
            if self.host.inspect(container)['State'].get('Health',{}).get('Status')!='healthy':
                raise ReleaseError('Shared dependency is not healthy')
        if self.host.run(['docker','exec',dependencies['redis'],'redis-cli','--raw','PING']).strip()!=b'PONG':
            raise ReleaseError('Redis backup authentication/topology unsupported; review before handoff')
        for folder in ('data','logs'):
            mounts=[m for m in old['Mounts'] if m['Destination']=='/app/'+folder]
            if len(mounts)!=1 or Path(mounts[0]['Source']).resolve() != (self.root/folder).resolve():
                raise ReleaseError('Snapshot path differs from the active bind mount')
        needed = sum(p.stat().st_size for folder in ('data','logs')
                     for p in (self.root/folder).rglob('*') if p.is_file())
        database_reserve = database_backup_reserve(self.root)
        # Direct archive mode keeps only the source tree and one streaming
        # archive. The previous three-copy estimate was for the legacy
        # expanded snapshot plus archive and needlessly blocked releases with
        # large, mostly incompressible media. Keep a fixed reserve for
        # temporary metadata and interrupted writes, plus the largest known
        # PostgreSQL/Redis dump set. Snapshot.database() also checks the live
        # free space after each export.
        if shutil.disk_usage(self.root).free < needed + 4*1024**3 + database_reserve:
            raise ReleaseError('Insufficient space for snapshot, archive and safety reserve')
        ingress_wait_budget = 60
        if not bootstrap:
            gate = self.host.gateway(self.front)
            ingress_wait_budget = int(gate.get('wait_budget_seconds', 0))
            if gate['route']['held'] or ingress_wait_budget < self.handoff_timeout+30:
                raise ReleaseError('Ingress held or queue wait budget too small')
        self.work = self.root/'backups'/('rolling-'+dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ')+'-'+release['commit'][:12])
        self.work.mkdir(parents=True, mode=0o700)
        self.report = {'status': 'running', 'commit': release['commit'],
                       'previous_commit': old_meta['commit'], 'events': [],
                       'backup': str(self.work), 'started_at': now()}
        atomic_json(self.work/'previous.json', old_meta)
        self.journal('prepare')
        snapshot = Snapshot(self.root, self.work, self.host, direct=True)
        candidate = None; frozen = False; released = False; held = False; release_attempted = False; data_changed = False; activation_attempted = False
        self.owned_candidate = None
        try:
            merge_assets(self.host, old_meta['image'], self.root/'static-assets')
            merge_assets(self.host, release['image'], self.root/'static-assets')
            candidate = self.start_slot(old, release['image_id'], slot, 'candidate')
            self.journal('standby_ready', candidate=candidate)
            self.host.action(old_name, 'pause')
            # Direct mode captures the authoritative manifest and archive only
            # after the writer is fenced. An online metadata-only walk is not
            # reused and needlessly delays the handoff on large browser trees.
            if not snapshot.direct:
                snapshot.sync()
            deadline = time.monotonic()+self.drain_timeout
            while True:
                state = self.host.control(old_name)
                if idle(state):
                    if bootstrap:
                        self.host.control(self.front, '/gateway/init', {})
                        held = True
                        break
                    if self.host.gateway(self.front)['route']['upstream'] != old_meta['upstream']:
                        raise ReleaseError('Ingress changed concurrently')
                    try:
                        self.host.gateway(self.front, 'hold'); held = True; break
                    except ReleaseError:
                        if self.host.gateway(self.front)['route']['held']:
                            raise
                if time.monotonic() >= deadline:
                    raise ReleaseError('Drain deadline reached; retain active service, do not cancel work')
                time.sleep(.2)
            self.journal('handoff', old_requests_and_jobs_drained=True)
            # Keep the control handoff within the reviewed 30-second budget.
            # The data fence below has its own bounded window because copying
            # and hashing a large, already-paused data directory can exceed
            # the control RPC budget while the ingress queue remains held.
            self.host.deadline = time.monotonic()+self.handoff_timeout
            # The marker precedes the irreversible seal so a crash blocks the
            # legacy deploy entry even if the seal's HTTP reply is lost.
            atomic_json(self.root/'current.json', dict(old_meta, release_strategy=STRATEGY,
                status='switching', frontdoor=self.front, candidate=candidate, candidate_slot=slot))
            retired = self.host.action(old_name, 'freeze'); frozen = True
            if retired.get('writer_lease') or retired.get('phase') not in ('frozen', 'gateway'):
                raise ReleaseError('Writer lease retirement unconfirmed')
            # Frozen media and browser state can take longer than the control
            # handoff itself. Keep the data fence separate from the short
            # activation budget so a complete snapshot cannot consume the
            # time needed to admit the candidate.
            data_budget = max(300, ingress_wait_budget - 5)
            self.host.deadline = time.monotonic()+data_budget
            data_guard_started = time.monotonic()
            snapshot.sync(frozen=True); snapshot.database(dependencies)
            before_files = snapshot.business_files()
            if snapshot.business_files() != before_files:
                raise ReleaseError('Frozen backup differs from current business files')
            before_db = database_guard(self.host, dependencies['postgres'])
            atomic_json(self.work/'business-before.json', {'files':before_files,'database':before_db})
            self.journal('snapshot_frozen', data_guard_elapsed_seconds=round(time.monotonic()-data_guard_started, 3),
                         data_guard_budget_seconds=data_budget)
            self.host.deadline = time.monotonic()+max(self.handoff_timeout, 30)
            activation_attempted = True
            self.host.action(candidate, 'activate')
            state = self.host.control(candidate)
            if not idle(state):
                raise ReleaseError('Activated candidate is not paused/ready')
            self.host.run(['docker', 'exec', candidate, 'wget', '-q', '-T', '3', '-O', '/dev/null', 'http://127.0.0.1:8080/readyz'])
            for name, ident in dependencies.items():
                if self.host.inspect(dependency_containers[name])['Id'] != ident or self.host.inspect(dependency_containers[name])['State'].get('Health',{}).get('Status') != 'healthy':
                    raise ReleaseError('Shared dependency was replaced')
            if business_files(self.root, self.host) != before_files or database_guard(self.host, dependencies['postgres']) != before_db:
                data_changed = True
                raise ReleaseError('Initialization changed business data/schema; keep ingress held and inspect')
            self.host.gateway(self.front, 'route', SLOTS[slot][1])
            atomic_json(self.root/'current.json', self.metadata(old_meta, release, slot, 'switching'))
            release_attempted = True
            self.host.gateway(self.front, 'release'); released = True
            self.host.action(candidate, 'resume')
            self.host.deadline = None
            atomic_json(self.root/'current.json', self.metadata(old_meta, release, slot))
            install_backup_tool(self.root, backup_tool)
            self.journal('traffic_switched')
            if old_name != self.front:
                self.host.stop_empty(old_name)
            snapshot.finish()
            local_install = install_backup_local(self.root, backup_local)
            self.journal('success', status='success', shared_dependencies_retained=True,
                previous_stopped=(old_name != self.front), bootstrap_front_preserved=bootstrap,
                local_backup_package=local_install,
                retention_requires_final_verification=backup_local is not None)
        except BaseException:
            budget_exhausted = (self.host.overall_deadline is not None
                                and time.monotonic() >= self.host.overall_deadline)
            # The release deadline bounds the forward path.  Recovery must
            # remain possible after that bound, otherwise an expired budget
            # could strand a frozen writer or a held ingress indefinitely.
            self.host.deadline = None
            self.host.overall_deadline = None
            if budget_exhausted:
                self.report['budget_exhausted'] = True
            candidate = candidate or self.owned_candidate
            # Never stop a version that may already own accepted business work.
            if release_attempted and not released:
                try:
                    released = self.host.gateway(self.front)['route']['held'] is not True
                except BaseException:
                    self.journal('needs_attention', status='needs_attention', traffic_result='unknown', instances_preserved=True)
                    raise
            if data_changed:
                self.journal('needs_attention', status='needs_attention', business_data_changed=True, ingress_held=True)
                raise
            if released:
                self.journal('needs_attention', status='needs_attention', traffic_switched=True)
                raise
            try:
                if activation_attempted:
                    # Even an initializer that returned an error may have run a
                    # migration. Stop only its never-admitted private runtime,
                    # then compare stable data before choosing an old program.
                    self.stop_candidate(candidate)
                    candidate = None; self.owned_candidate = None
                    if business_files(self.root,self.host) != before_files or database_guard(self.host,dependencies['postgres']) != before_db:
                        self.journal('needs_attention', status='needs_attention', business_data_changed=True, ingress_held=True)
                        raise ReleaseError('Failed initialization changed data; automatic rollback refused')
                actual = self.host.control(old_name)
                frozen = frozen or actual.get('phase') in ('frozen', 'gateway', 'sealing')
                if not frozen:
                    self.host.action(old_name, 'resume')
                    if held and not bootstrap:
                        self.host.gateway(self.front, 'route', old_meta['upstream'])
                        self.host.gateway(self.front, 'release')
                    atomic_json(self.root/'current.json', old_meta)
                    if candidate:
                        self.stop_candidate(candidate)
                    self.journal('aborted_before_seal', status='aborted', old_service_retained=True)
                else:
                    # Rebuild the previous version from current data, never reuse
                    # a retired in-memory store or restore the backup over new data.
                    if candidate:
                        self.stop_candidate(candidate)
                    recovery_slot = 'blue' if bootstrap else old_meta['slot']
                    if not bootstrap and self.host.inspect(old_name)['State']['Running']:
                        self.host.stop_empty(old_name)
                    recovery = self.start_slot(old, old['Image'], recovery_slot, 'recovery')
                    self.host.action(recovery, 'activate')
                    if not idle(self.host.control(recovery)):
                        raise ReleaseError('Recovery did not become ready; keep ingress held')
                    self.host.gateway(self.front, 'route', SLOTS[recovery_slot][1])
                    self.host.gateway(self.front, 'release')
                    self.host.action(recovery, 'resume')
                    prior = dict(release, image=old_meta['image'], image_id=old['Image'],
                        commit=old_meta['commit'], contract=old_meta.get('contract', release['contract']),
                        source=old_meta.get('source', release['source']))
                    atomic_json(self.root/'current.json', self.metadata(old_meta, prior, recovery_slot, recovered=True))
                    self.journal('program_rolled_back', status='rolled_back', data_restored=False)
                    retire_rolled_back_payload(self.root, self.work)
            except BaseException:
                self.journal("needs_attention", status="needs_attention", traffic_switched=False, instances_preserved=True)
                raise

            raise


def prepare(host, root, source, image, output):
    source = Path(source).resolve()
    prefix = ['git', '-c', 'safe.directory='+str(source), '-C', str(source)]
    commit = host.run(prefix+['rev-parse', 'HEAD']).decode().strip()
    if not re.fullmatch('[0-9a-f]{40}', commit) or host.run(prefix+['status', '--porcelain']).strip():
        raise ReleaseError('Prepare requires a clean fixed-commit source tree')
    info = host.image(image)
    if info['Config'].get('Labels', {}).get('org.opencontainers.image.revision') != commit:
        raise ReleaseError('Image revision differs from reviewed source')
    meta = load(Path(root)/'current.json')
    value = {'commit': commit, 'base_commit': meta['commit'], 'image': image,
        'image_id': info['Id'], 'source': str(source), 'contract': source_contract(source)}
    if (source/BACKUP_SOURCE).is_file():
        raw = host.run(prefix+['show', commit+':'+BACKUP_SOURCE])
        ast.parse(raw)
        value['backup_tool_sha256'] = digest(raw)
    elif (Path(root)/'tools'/'backup-core.py').exists():
        raise ReleaseError('Source commit is missing the configured project backup script')
    prepared_local = prepare_backup_local_source(host, root, source, commit)
    if prepared_local is not None:
        value['backup_local_sha256'] = prepared_local['package_id']
    atomic_json(output, value)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    tools_dir = Path(__file__).resolve().parent
    installed_root = tools_dir.parent if tools_dir.name == 'tools' else None
    parser.add_argument('--root', type=Path, default=installed_root, required=installed_root is None)
    sub = parser.add_subparsers(dest='action', required=True)
    prep = sub.add_parser('prepare'); prep.add_argument('--source', type=Path, required=True)
    prep.add_argument('--image', required=True); prep.add_argument('--output', type=Path, required=True)
    for name in ('deploy', 'bootstrap'):
        command = sub.add_parser(name); command.add_argument('--release', type=Path, required=True)
        command.add_argument('--compatible-contract', type=Path)
        command.add_argument('--drain-timeout', type=int, default=600)
        command.add_argument('--handoff-timeout', type=int, default=25)
        command.add_argument('--total-timeout', type=int, default=1200,
                             help='safe end-to-end budget for the rolling handoff')
    sub.add_parser('status')
    sub.add_parser('guard-legacy')
    sub.add_parser('layout')
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise ReleaseError('Run this host tool as root')
    if os.uname().sysname != 'Linux' or os.uname().machine not in ('aarch64','arm64'):
        raise ReleaseError('This host tool requires the reviewed Linux ARM64 deployment host')
    os.umask(0o077)
    if args.action == 'layout':
        meta=load(args.root/'current.json')
        print(('rolling' if meta.get('status')=='healthy' else 'switching') if meta.get('release_strategy')==STRATEGY else 'legacy'); return
    if args.action == 'guard-legacy':
        require_legacy(args.root); return
    if args.action == 'status':
        print(json.dumps({'current': load(args.root/'current.json'),
            'rolling': load(args.root/'rolling-status.json') if (args.root/'rolling-status.json').exists() else None}, indent=2)); return
    host = Host()
    if args.action == 'prepare':
        prepare(host, args.root, args.source, args.image, args.output); return
    if (not 1 <= args.handoff_timeout <= 30 or args.drain_timeout < 1
            or not 60 <= args.total_timeout <= 1200):
        raise ReleaseError('Release deadlines must fit the 60-to-1200-second end-to-end budget')
    host.overall_deadline = time.monotonic() + args.total_timeout
    with (args.root/'maintenance.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX|fcntl.LOCK_NB)
        def interrupted(*_):
            raise ReleaseError('Interrupted; inspect durable ingress/instance state before continuing')
        signal.signal(signal.SIGTERM, interrupted); signal.signal(signal.SIGHUP, interrupted)
        release = load(args.release)
        RollingRelease(args.root, host, args.drain_timeout, args.handoff_timeout).deploy(
            release, args.action == 'bootstrap', load(args.compatible_contract) if args.compatible_contract else None)
        if local_backup_configured(args.root):
            verify_backup_local_ready(host, args.root, release)
    queued = trigger_backup_local(args.root, load(args.release)['commit'])
    print(json.dumps({'event': 'local_backup_maintenance', **queued}))


if __name__ == '__main__':
    try:
        main()
    except (ReleaseError, OSError, subprocess.SubprocessError, ValueError):
        print('Release stopped; inspect rolling-status.json and live control state. Command details withheld.', flush=True)
        raise SystemExit(1)
