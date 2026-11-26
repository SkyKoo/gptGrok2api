"""Reviewed app and Worker release checks, bound to one generated private bundle."""
from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import re
from pathlib import Path
import shutil
import stat
import subprocess
import time
from contextlib import contextmanager
from datetime import datetime, timezone
from paired_phases import ReleaseInputError, verified_bundle_hashes, verify_rolling_helper

PLAN = json.loads(Path(__file__).with_name('release-plan.json').read_text())
PIPELINE_BUDGET_SECONDS = 1200
# The archive-only continuation marker is retained for resumability, but it is
# bounded by the same init-to-finish deadline. It never creates a second
# release window after traffic has been restored.
FINALIZATION_BUDGET_SECONDS = 1200
COMMIT = PLAN['candidate_commit']
BASE_COMMIT = PLAN['base_commit']
BRANCH = PLAN['branch']
PROTOCOL = 'cfm-single-writer-v1'
ROLLING_SHA256 = PLAN['rolling_sha256']
BASE_WORKER = 'cfm-browser-worker'
WORKER = 'cfm-browser-worker'
DEPENDENCIES = {'postgres': 'gptgrok2api-postgres-1', 'redis': 'gptgrok2api-redis-1'}
HELPERS = ('common.py', 'release.py', 'preflight.py', 'verify.py',
           'state-guard.py', 'coordinator.py', 'worker-release.py', 'release-plan.json',
           'build.py', 'build-worker.py', 'worker-image-probe.py', 'snapshot.py',
           'capture-services.py', 'verify-other-services.py',
           'snapshot_integrity.py', 'paired_phases.py', 'bundle-manifest.json')


def require(value, message):
    if not value:
        raise RuntimeError(message)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def _timing_path(operation_dir):
    operation_dir = Path(operation_dir)
    require(operation_dir.is_dir() and not operation_dir.is_symlink(),
            'Operation directory must be a real directory')
    path = operation_dir / 'pipeline-events.jsonl'
    require(not path.is_symlink(), 'Pipeline timing file must not be linked')
    return path


def _pipeline_started_epoch(operation_dir):
    """Return the persisted start time for this complete release attempt."""
    path = _timing_path(operation_dir)
    require(path.is_file(), 'Pipeline timing file is missing')
    first = path.read_text().splitlines()[0]
    record = json.loads(first)
    require(record.get('phase') == 'operation-created'
            and record.get('status') == 'started'
            and record.get('budget_seconds') == PIPELINE_BUDGET_SECONDS,
            'Pipeline start record is invalid')
    started = record.get('epoch')
    require(isinstance(started, (int, float)) and started > 0,
            'Pipeline start time is invalid')
    return float(started)


def pipeline_remaining(operation_dir):
    """Return the seconds left in the one budget shared by every phase."""
    return PIPELINE_BUDGET_SECONDS - (time.time() - _pipeline_started_epoch(operation_dir))


def require_pipeline_budget(operation_dir, minimum=1):
    remaining = pipeline_remaining(operation_dir)
    require(remaining >= minimum,
            'The 20-minute release budget is exhausted; preserve the operation evidence')
    return remaining


def _first_finalization_event(operation_dir):
    path = _timing_path(operation_dir)
    if not path.exists():
        return None
    for line in path.read_text().splitlines():
        if not line.strip():
            continue
        record = json.loads(line)
        if (record.get('phase') == 'archive-finalization'
                and record.get('status') == 'started'
                and record.get('budget_seconds') == FINALIZATION_BUDGET_SECONDS):
            return record
    return None


def ensure_finalization_budget(operation_dir):
    """Record one archive checkpoint inside the shared release window."""
    # Bind the continuation to a valid generated operation. This prevents a
    # hand-created timing file from becoming a way around the normal plan.
    _pipeline_started_epoch(operation_dir)
    if _first_finalization_event(operation_dir) is None:
        timing_event(operation_dir, 'archive-finalization', 'started',
                     budget_seconds=FINALIZATION_BUDGET_SECONDS,
                     shared_pipeline_budget=True)


def finalization_remaining(operation_dir):
    ensure_finalization_budget(operation_dir)
    record = _first_finalization_event(operation_dir)
    require(record is not None, 'Archive finalization start record is missing')
    started = record.get('epoch')
    require(isinstance(started, (int, float)) and started > 0,
            'Archive finalization start time is invalid')
    # Both clocks must have time remaining. The persisted archive marker is
    # useful for proving that finalization was attempted, while the operation
    # start record remains the authoritative end-to-end deadline.
    return min(pipeline_remaining(operation_dir),
               FINALIZATION_BUDGET_SECONDS - (time.time() - float(started)))


def require_finalization_budget(operation_dir, minimum=1):
    remaining = finalization_remaining(operation_dir)
    require(remaining >= minimum,
            'The archive-only continuation budget is exhausted; preserve the operation evidence')
    return remaining


def bounded_timeout(operation_dir, requested, minimum=1):
    """Bound a subprocess timeout by the remaining complete-pipeline budget."""
    remaining = require_pipeline_budget(operation_dir, minimum)
    return max(minimum, min(float(requested), remaining))


def bounded_finalization_timeout(operation_dir, requested, minimum=1):
    """Bound an archive-only helper by the persisted continuation window."""
    remaining = require_finalization_budget(operation_dir, minimum)
    return max(minimum, min(float(requested), remaining))


def timing_event(operation_dir, phase, status, *, started_at=None, elapsed_seconds=None, **values):
    """Append one private, machine-readable event for the full release timeline."""
    require(isinstance(phase, str) and phase and '\n' not in phase, 'Invalid timing phase')
    require(status in ('started', 'finished', 'failed', 'skipped'), 'Invalid timing status')
    record = {'at': datetime.now(timezone.utc).isoformat(), 'epoch': time.time(),
              'phase': phase, 'status': status}
    if started_at is not None:
        record['started_at'] = started_at
    if elapsed_seconds is not None:
        record['elapsed_seconds'] = round(float(elapsed_seconds), 3)
    record.update(values)
    raw = (json.dumps(record, sort_keys=True, separators=(',', ':')) + '\n').encode()
    path = _timing_path(operation_dir)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600)
    try:
        os.write(descriptor, raw)
    finally:
        os.close(descriptor)
    return record


@contextmanager
def timing_phase(operation_dir, phase, **values):
    started = time.monotonic()
    event = timing_event(operation_dir, phase, 'started', **values)
    try:
        yield event
    except BaseException as error:
        timing_event(operation_dir, phase, 'failed', started_at=event['at'],
                     elapsed_seconds=time.monotonic() - started,
                     error_type=type(error).__name__)
        raise
    else:
        timing_event(operation_dir, phase, 'finished', started_at=event['at'],
                     elapsed_seconds=time.monotonic() - started)


def write_timing_summary(operation_dir, status):
    """Write a compact total from the append-only events without overwriting evidence."""
    path = _timing_path(operation_dir)
    records = []
    if path.exists():
        for line in path.read_text().splitlines():
            if line.strip():
                records.append(json.loads(line))
    now = time.time()
    epochs = [float(item['epoch']) for item in records if isinstance(item.get('epoch'), (int, float))]
    summary = {'status': status, 'finished_at': datetime.now(timezone.utc).isoformat(),
               'total_elapsed_seconds': round(now - min(epochs), 3) if epochs else 0.0,
               'events': len(records), 'phases': {}}
    for item in records:
        phase = item.get('phase')
        if not isinstance(phase, str):
            continue
        bucket = summary['phases'].setdefault(phase, {'runs': 0, 'elapsed_seconds': 0.0,
                                                      'last_status': None})
        if item.get('status') == 'finished':
            bucket['runs'] += 1
            bucket['elapsed_seconds'] = round(bucket['elapsed_seconds'] +
                                               float(item.get('elapsed_seconds', 0)), 3)
        bucket['last_status'] = item.get('status')
    target = Path(operation_dir) / 'pipeline-timing.json'
    require(not target.is_symlink(), 'Pipeline timing summary must not be linked')
    tmp = target.with_name(target.name + '.writing')
    tmp.write_text(json.dumps(summary, indent=2) + '\n')
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)
    return summary


def file_sha(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as stream:
        while block := stream.read(1024 * 1024):
            digest.update(block)
    return digest.hexdigest()


def copy_private_data(source, destination):
    """Clone authentication state without following source or copied symlinks."""
    source, destination = Path(source), Path(destination)
    paths = [source, *source.rglob('*')]
    for path in paths:
        mode = path.lstat().st_mode
        require(stat.S_ISDIR(mode) or stat.S_ISREG(mode), 'Snapshot refuses linked or special source files')
    shutil.copytree(source, destination, symlinks=True,
                    copy_function=lambda src, dst: shutil.copy2(src, dst, follow_symlinks=False))
    # Recheck the completed copy before reading it or changing ownership. A link
    # introduced during copying remains a link and is rejected, never followed.
    for target in [destination, *destination.rglob('*')]:
        mode = target.lstat().st_mode
        require(stat.S_ISDIR(mode) or stat.S_ISREG(mode), 'Snapshot copy contains linked or special files')
        original = source / target.relative_to(destination)
        info = original.lstat()
        require(stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode), 'Snapshot source changed into a link')
        os.chown(target, info.st_uid, info.st_gid, follow_symlinks=False)


def load(path):
    return json.loads(Path(path).read_text())


def rolling(root):
    path = verify_rolling_helper(root, ROLLING_SHA256)
    spec = importlib.util.spec_from_file_location('adobe_rolling', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def host_required():
    require(__debug__, 'Optimized Python must not disable release assertions')
    require(isinstance(COMMIT, str) and re.fullmatch(r'[0-9a-f]{40}', COMMIT)
            and (COMMIT != BASE_COMMIT
                 or PLAN.get('previous_worker', {}).get('commit') != COMMIT),
            'Pin the reviewed new feature commit in release-plan.json first')
    require(PLAN['schema_migration'] is False and PLAN['worker_change'] == 'image_only',
            'Only the reviewed no-migration/image-only plan is supported')
    require(os.geteuid() == 0, 'Run on the deployment host as root')
    require(os.uname().sysname == 'Linux' and os.uname().machine in ('aarch64', 'arm64'),
            'Only the reviewed Linux ARM64 host is supported')
    os.umask(0o077)


def helper_hashes(directory):
    directory = Path(directory)
    require(directory.resolve() == Path(__file__).resolve().parent,
            'Operation directory must match this frozen bundle')
    return verified_bundle_hashes(directory, PLAN, set(HELPERS) - {'bundle-manifest.json'})


def healthy(info):
    state = info['State']
    return (state['Running'] and not state.get('OOMKilled')
            and state.get('Health', {}).get('Status') == 'healthy')


def container_summary(info):
    return {'id': info['Id'], 'image_id': info['Image'],
            'restarts': info['RestartCount'], 'running': info['State']['Running'],
            'health': info['State'].get('Health', {}).get('Status'),
            'oom': info['State'].get('OOMKilled'),
            'environment_sha256': sha(json.dumps(sorted(info['Config']['Env'])).encode()),
            'mounts': sorted(info['Mounts'], key=lambda m: m['Destination']),
            'networks': sorted(info['NetworkSettings']['Networks']),
            'ports': info['HostConfig'].get('PortBindings')}


def worker_health(host, name=BASE_WORKER):
    raw = host.run(['docker', 'exec', name, 'python', '-c',
                   "import urllib.request;print(urllib.request.urlopen('http://127.0.0.1:8899/health',timeout=3).read().decode())"], timeout=8)
    return json.loads(raw)


def saved_switches(root):
    value = load(Path(root) / 'data/register.json')
    return {key: {field: item[field] for field in ('enabled', 'refresh_codex_rt') if field in item}
            for key, item in value.items() if isinstance(item, dict)}


def verify_source(host, release):
    source = Path(release['source']).resolve()
    git = ['git', '--no-optional-locks', '-c', 'safe.directory=' + str(source), '-C', str(source)]
    require(host.run(git + ['rev-parse', 'HEAD']).decode().strip() == COMMIT,
            'Source HEAD differs from fixed feature commit')
    branch = host.run(git + ['branch', '--show-current']).decode().strip()
    require(branch in ('', BRANCH), 'Source must be detached or on the pinned main branch')
    require(not host.run(git + ['status', '--porcelain']).strip(), 'Source tree must be clean')
    tree = host.run(git + ['ls-tree', '-r', '-z', '--full-tree', COMMIT])
    return {'commit': COMMIT, 'branch': branch,
            'tree': host.run(git + ['rev-parse', COMMIT + '^{tree}']).decode().strip(),
            'tracked_tree_sha256': sha(tree),
            'state_contract_sha256': reviewed_state_contract(source)}


def reviewed_state_contract(source):
    source = Path(source)
    rows = {}
    for name in PLAN['state_contract_inputs']:
        target = source / name
        require(target.exists() and not target.is_symlink(), 'Reviewed contract input missing')
        paths = sorted(target.rglob('*.go')) if target.is_dir() else [target]
        for path in paths:
            if not path.name.endswith('_test.go'):
                rows[path.relative_to(source).as_posix()] = file_sha(path)
    return sha(json.dumps(rows, sort_keys=True).encode())


def check_image(host, name, ident, commit, app=False):
    image = host.image(name)
    labels = image['Config'].get('Labels', {})
    require(image['Id'] == ident and image['Architecture'] == 'arm64' and image['Os'] == 'linux',
            'Image architecture or immutable identity mismatch')
    require(labels.get('org.opencontainers.image.revision') == commit, 'Image revision mismatch')
    if app:
        require(labels.get('io.cfm.rolling.protocol') == PROTOCOL, 'App image lacks writer protocol')
    return image



def backup_policy_identity(root):
    """Return the installed project-owned backup package identity and contents."""
    root = Path(root)
    base = root / 'tools/backup-local'
    current = base / 'current'
    require(current.is_symlink(), 'Existing local backup policy pointer is missing')
    resolved = current.resolve(strict=True)
    require(resolved.parent == base / 'versions' and resolved.name == PLAN['backup_policy_package'],
            'Local backup policy package differs from reviewed deployment')
    files = {}
    for item in sorted(resolved.rglob('*')):
        require(not item.is_symlink(), 'Local backup package contains an unexpected link')
        if item.is_file():
            files[item.relative_to(resolved).as_posix()] = file_sha(item)
    require('manifest.json' in files, 'Local backup manifest is missing')
    manifest = load(resolved / 'manifest.json')
    require(sha(json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()) == resolved.name,
            'Local backup package manifest identity differs')
    for name, expected in manifest['files'].items():
        require(files.get(name) == expected, 'Local backup package content differs')
    dependencies = resolved / 'dependencies.json'
    require(dependencies.is_file() and not dependencies.is_symlink(), 'Backup dependency configuration missing')
    return {'pointer': os.readlink(current), 'resolved': str(resolved), 'files': files,
            'dependencies_sha256': file_sha(dependencies),
            'core_sha256': file_sha(root / 'tools/backup-core.py')}

def xai_authorization_queue_idle(host, root, container, *, require_idle=True):
    """Read the writer queue, optionally enforcing an empty handoff boundary.

    The initial preflight records the queue without blocking the release. The
    frozen handoff performs the authoritative empty-queue check after pause and
    ingress hold, when no new authorization can be admitted.
    """
    info = host.inspect(container)
    env = dict(value.split('=', 1) for value in info['Config']['Env'] if '=' in value)
    config = load(Path(root) / 'data/config.json')
    admin = (env.get('CHATGPT2API_ADMIN_KEY') or config.get('app_key')
             or config.get('admin_key') or env.get('CHATGPT2API_AUTH_KEY')
             or config.get('auth-key') or config.get('api_key'))
    require(isinstance(admin, str) and admin.strip() and '\n' not in admin and '\r' not in admin,
            'Configured admin key unavailable for authorization queue observation')
    # The host argv contains only this fixed shell text. The key travels through
    # private stdin, never through persisted files, command logging or output.
    raw = host.run(['docker', 'exec', '-i', container, 'sh', '-c',
                    'IFS= read -r key; exec wget -q -T 10 -O - --header="Authorization: Bearer $key" '
                    'http://127.0.0.1:8080/api/grok/oauth/protocol/status'],
                   input=(admin.strip() + '\n').encode(), timeout=15)
    queue = json.loads(raw).get('queue', {})
    require(queue.get('runtime') == 'go' and isinstance(queue.get('items'), list),
            'Authorization queue read-only projection differs')
    counts = {}
    for row in queue['items']:
        require(isinstance(row, dict) and row.get('status') in
                ('pending', 'queued', 'running', 'authorized', 'failed', 'manual_action_required'),
                'Authorization queue status differs')
        state = row['status']
        counts[state] = counts.get(state, 0) + 1
    if require_idle:
        require(not any(counts.get(state, 0) for state in ('pending', 'queued', 'running')),
                'Existing xAI authorization work must finish before release')
    active = sum(counts.get(state, 0) for state in ('pending', 'queued', 'running'))
    return {'read_only': True, 'active': active, 'status_counts': counts}


def preflight(root, release_path, worker_path, operation_dir, *, scan_business=True,
              required_business_bytes=None, check_browser_locks=True):
    root = Path(root)
    r = rolling(root)
    host = r.Host()
    # Host.run and host.checkpoint must observe the same persisted deadline as
    # the command wrapper. A single Docker or Git probe cannot outlive the
    # complete 20-minute release budget.
    host.overall_deadline = time.monotonic() + require_pipeline_budget(operation_dir)
    current = load(root / 'current.json')
    release, worker = load(release_path), load(worker_path)
    require(current.get('release_strategy') == r.STRATEGY and current.get('status') == 'healthy',
            'Current release must be a healthy rolling deployment')
    require(release['commit'] == worker['commit'] == COMMIT, 'Unexpected candidate commit')
    require(release['base_commit'] == current['commit'] == BASE_COMMIT, 'Release base changed')
    require(worker['container'] == WORKER and worker['service'] == 'grok-browser',
            'Worker must use the existing shared container name and service')
    source = verify_source(host, release)
    require(r.source_contract(release['source']) == release['contract'], 'Legacy source contract changed')
    require(current.get('contract') == PLAN['compatible_contract']['previous']
            and release['contract'] == PLAN['compatible_contract']['candidate'],
            'Exact reviewed browser/state compatibility contract changed')
    r.check_contract(current['contract'], release['contract'], PLAN['compatible_contract'])
    backup_tool = r.backup_tool_bytes(host, release)
    require(backup_tool is not None, 'Candidate backup coverage is required')
    backup_local = r.backup_local_bytes(host, release)
    require(backup_local is not None, 'Candidate local backup package is required')
    keep_daily_policy = (backup_local['package_id'] != PLAN['backup_policy_package']
                         and hasattr(r, 'daily_media_file')
                         and backup_local.get('manifest', {}).get('release_media_policy') != 'daily'
                         and load(root / 'tools/backup-local/current/manifest.json').get('release_media_policy') == 'daily')
    require(backup_local['package_id'] == PLAN['backup_policy_package'] or keep_daily_policy,
            'Candidate local backup package differs from the reviewed policy')
    check_image(host, release['image'], release['image_id'], COMMIT, app=True)
    check_image(host, worker['image'], worker['image_id'], COMMIT)
    check_image(host, current['image'], current['image_id'], current['commit'], app=True)
    names = [current['frontdoor'], current['container'], BASE_WORKER, *DEPENDENCIES.values()]
    containers = {name: host.inspect(name) for name in names}
    require(all(healthy(item) for item in containers.values()), 'A production container is unhealthy')
    require(containers[current['container']]['Image'] == current['image_id'], 'Active app identity changed')
    worker_names = host.run(['docker', 'container', 'ls', '-a', '--format', '{{.Names}}']).decode().splitlines()
    require(worker_names.count(WORKER) == 1 and 'gptgrok2api-grok-browser-1' not in worker_names, 'Existing shared Worker is missing or a legacy-name Worker is present')
    previous_worker = PLAN['previous_worker']
    require(containers[BASE_WORKER]['Image'] == previous_worker['image_id']
            and current.get('grok_browser_commit') == previous_worker['commit'],
            'Production Worker differs from the fixed base')
    check_image(host, previous_worker['image'], previous_worker['image_id'], previous_worker['commit'])
    control = host.control(current['container'])
    require(control.get('phase') == 'active' and control.get('writer_lease') is True
            and control.get('paused') is False and control.get('protocol') == PROTOCOL,
            'Writer is not in normal active service')
    gateway = host.gateway(current['frontdoor'])
    require(gateway['route']['held'] is False and gateway['route']['upstream'] == current['upstream'],
            'Gateway is held or routes elsewhere')
    require(gateway.get('wait_budget_seconds', 0) >= 60, 'Unexpected ingress wait budget')
    health = worker_health(host)
    require(health.get('status') == 'ok' and health.get('service') in ('grok-browser', 'cfm-browser-worker'),
            'Worker health contract mismatch')
    require(health.get('capacity') in (0, 1), 'Worker concurrency differs from single licensed slot')
    require(host.run(['docker', 'exec', DEPENDENCIES['redis'], 'redis-cli', '--raw', 'PING']).strip() == b'PONG',
            'Redis backup authentication/topology is unsupported')
    for folder in ('data', 'logs'):
        mounts = [m for m in containers[current['container']]['Mounts'] if m['Destination'] == '/app/' + folder]
        require(len(mounts) == 1 and mounts[0]['Type'] == 'bind' and mounts[0]['RW']
                and Path(mounts[0]['Source']).resolve() == (root / folder).resolve(), 'Business bind path mismatch')
    # Completed and failed relogin journals already exist on this base. They
    # remain business data and are protected by the strict frozen state guard.
    if scan_business:
        total = 0
        for folder in ('data', 'logs'):
            for path in (root / folder).rglob('*'):
                require(not path.is_symlink(), 'Business files contain a symlink')
                host.checkpoint()
                if path.is_file():
                    total += path.stat().st_size
    else:
        require(type(required_business_bytes) is int and required_business_bytes >= 0,
                'A reviewed business size is required for the fast preflight')
        total = required_business_bytes
    free = shutil.disk_usage(root).free
    # Paired snapshots use the streaming direct archive. The source tree stays
    # in place, so the old expanded-copy estimate (three times the business
    # size) blocked releases without reflecting the actual peak. Keep a fixed
    # archive margin and the largest known database dump set. The snapshot
    # writer still checks free space after every export and while writing.
    archive_reserve = 4 * 1024 ** 3
    database_reserve = r.database_backup_reserve(root)
    required_space = total + archive_reserve + database_reserve
    require(free >= required_space, 'Insufficient direct snapshot space')
    if check_browser_locks:
        require(not list((root / 'data/browser-worker').rglob('.lock'))
                if (root / 'data/browser-worker').exists() else True,
                'A browser profile appears active')
    config_plan = load(Path(operation_dir) / 'worker-config-plan.json')
    require(config_plan.get('candidate_image_id') == worker['image_id']
            and config_plan.get('previous_container') == BASE_WORKER and config_plan.get('candidate_container') == WORKER
            and config_plan.get('original_compose_sha256') == file_sha(root / 'compose.yaml')
            and config_plan.get('original_env_sha256') == file_sha(root / '.env'), 'Worker configuration plan differs from live base')
    return {'at': r.now(), 'current': current, 'source': source, 'candidate': release,
            'worker': {key: worker[key] for key in ('commit', 'image', 'image_id', 'container', 'service')},
            'worker_config_plan_sha256': file_sha(Path(operation_dir) / 'worker-config-plan.json'),
            'release_sha256': file_sha(release_path), 'worker_manifest_sha256': file_sha(worker_path),
            'helpers': helper_hashes(operation_dir), 'containers': {name: container_summary(info) for name, info in containers.items()},
            'switches': saved_switches(root), 'control': control, 'gateway': gateway, 'worker_health': health,
            'production_files': {name: file_sha(root / name) for name in ('.env', 'compose.yaml', 'current.json')},
            'backup_tool_sha256': file_sha(root / 'tools/backup-core.py'),
            'backup_policy': backup_policy_identity(root),
            'backup_local_sha256': backup_local['package_id'],
            # Record the initial queue without making a business-polling
            # snapshot a second release gate. The frozen handoff checks it
            # once more after pause and ingress hold.
            'xai_authorization_queue': xai_authorization_queue_idle(
                host, root, current['container'], require_idle=False),
            'disk_free_bytes': free, 'business_bytes': total,
            'snapshot_space_required_bytes': required_space,
            'snapshot_archive_reserve_bytes': archive_reserve,
            'snapshot_database_reserve_bytes': database_reserve,
            'maintenance_may_exceed_gateway_wait_budget': True}


def run_helper(host, operation_dir, name, args, timeout=120):
    # Host.run suppresses stderr because SQL/Compose failures may contain credentials.
    timeout = bounded_timeout(operation_dir, timeout)
    return host.run(['python3', str(Path(operation_dir) / name), *map(str, args)], timeout=timeout)


def safe_main(main):
    try:
        main()
    except (Exception, KeyboardInterrupt) as error:
        # Do not echo exception strings from subprocesses, HTTP or parsed private data.
        print(json.dumps({'ok': False, 'error_type': type(error).__name__,
                          'message': str(error) if isinstance(error, ReleaseInputError) else
                          'Operation stopped; inspect private state and evidence before retrying'}))
        raise SystemExit(1)
