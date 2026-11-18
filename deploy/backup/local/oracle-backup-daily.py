#!/usr/bin/env python3
"""Create a verified gptgrok2api local daily recovery point without stopping this service.

The root-owned dependency JSON pins reviewed installed project backup helpers and
coordination locks. This program does not upload, prune, change cloud jobs, or
restore production data. Scheduling, today's-point selection and catalog merging
belong to the retention coordinator. Without --execute only configuration is read.
"""
import argparse
import contextlib
import datetime as dt
from decimal import Decimal
import fcntl
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import sys
import tarfile
import time

sys.dont_write_bytecode = True

GIB = 1024 ** 3
CHUNK = 1024 ** 2
MAX_TOTAL = 8 * GIB
MAX_FILE = 2 * GIB
CAPTURE_ATTEMPTS = 3
SERVICE = 'gptgrok2api'
ROOT = Path('/opt/gptgrok2api')
HELPERS = ['/opt/gptgrok2api/tools/backup-core.py']


class Refused(RuntimeError):
    """An expected safety refusal; messages never contain business contents."""


class SnapshotChanged(Refused):
    """An observed source mutation; only a fresh complete window may retry."""


def unchanged(condition, message):
    if not condition:
        raise SnapshotChanged(message)


def selected_stat(path):
    try:
        return path.lstat()
    except FileNotFoundError:
        raise SnapshotChanged('selected source disappeared during inspection') from None


def require(condition, message):
    if not condition:
        raise Refused(message)


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def sha_file(path):
    value = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(CHUNK), b''):
            value.update(block)
    return value.hexdigest()


def stamp(info):
    return (info.st_dev, info.st_ino, info.st_size, info.st_mode, info.st_uid,
            info.st_gid, info.st_mtime_ns, info.st_ctime_ns)


def safe_path(root, relative):
    part = PurePosixPath(relative)
    require(part.parts and not part.is_absolute() and '..' not in part.parts,
            'unsafe relative path')
    return root.joinpath(*part.parts)


def json_bytes(value):
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n").encode()


def atomic_json(path, value):
    temporary = path.with_name(path.name + '.tmp')
    descriptor = os.open(temporary, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, 'wb') as stream:
        stream.write(json_bytes(value)); stream.flush(); os.fsync(stream.fileno())
    os.replace(temporary, path)


def entry(path):
    before = selected_stat(path)
    require(stat.S_ISDIR(before.st_mode) or stat.S_ISREG(before.st_mode),
            'linked or special source member')
    result = {'type': 'directory' if stat.S_ISDIR(before.st_mode) else 'file',
              'mode': stat.S_IMODE(before.st_mode), 'uid': before.st_uid,
              'gid': before.st_gid, 'mtime_ns': before.st_mtime_ns}
    if result['type'] == 'file':
        require(before.st_size <= MAX_FILE, 'source file exceeds capacity bound')
        try:
            descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        except FileNotFoundError:
            raise SnapshotChanged('selected source disappeared before read') from None
        with os.fdopen(descriptor, 'rb') as source:
            require(stat.S_ISREG(os.fstat(source.fileno()).st_mode), 'special source member')
            unchanged(stamp(os.fstat(source.fileno())) == stamp(before), 'source changed before inventory')
            digest = hashlib.sha256(); size = 0
            for block in iter(lambda: source.read(CHUNK), b''):
                size += len(block)
                require(size <= MAX_FILE, 'source file exceeds capacity bound')
                unchanged(size <= before.st_size, 'source grew during inventory')
                digest.update(block)
            unchanged(stamp(os.fstat(source.fileno())) == stamp(before)
                      and size == before.st_size, 'source changed during inventory')
        result.update(size=size, sha256=digest.hexdigest())
    after = selected_stat(path)
    require(stat.S_ISDIR(after.st_mode) or stat.S_ISREG(after.st_mode), 'linked or special source member')
    unchanged(stamp(after) == stamp(before), 'source changed during inventory')
    return result, stamp(before)


def inventory(roots, destination=None):
    """Bounded stream copy; return content/metadata and identities for comparison."""
    rows, identities = {}, {}
    total = 0
    for label, root in sorted(roots.items()):
        root = Path(root)
        require(root.exists() and not root.is_symlink(), 'missing or linked source root')
        paths = [root, *sorted(root.rglob('*'))] if root.is_dir() else [root]
        for path in paths:
            relative = (PurePosixPath(label) / path.relative_to(root).as_posix()).as_posix()
            row, identity = entry(path)
            require(relative not in rows, 'overlapping capture roots')
            rows[relative], identities[relative] = row, identity
            total += row.get('size', 0)
            require(total <= MAX_TOTAL and len(rows) <= 200000, 'snapshot exceeds capacity bound')
            if destination:
                target = safe_path(destination, relative)
                if row['type'] == 'directory':
                    target.mkdir(parents=True, exist_ok=True, mode=0o700)
                else:
                    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                    try:
                        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                    except FileNotFoundError:
                        raise SnapshotChanged('selected source disappeared before copy') from None
                    with os.fdopen(fd, 'rb') as source, target.open('xb') as output:
                        require(stat.S_ISREG(os.fstat(source.fileno()).st_mode), 'special source member')
                        unchanged(stamp(os.fstat(source.fileno())) == identity, 'source changed before copy')
                        digest = hashlib.sha256(); copied = 0
                        for block in iter(lambda: source.read(CHUNK), b''):
                            copied += len(block); digest.update(block); output.write(block)
                            unchanged(copied <= row['size'], 'source grew during copy')
                        unchanged(digest.hexdigest() == row['sha256'] and copied == row['size'],
                                'copied content differs from inventory')
                        unchanged(stamp(os.fstat(source.fileno())) == identity, 'source changed during copy')
                        output.flush(); os.fsync(output.fileno())
                    after = selected_stat(path)
                    require(stat.S_ISREG(after.st_mode), 'linked or special source member')
                    unchanged(stamp(after) == identity, 'source changed after copy')
    if destination:
        apply_metadata(destination, rows)
    return rows, identities


def stat_inventory(roots):
    """Compare copied media identities without re-reading all media bytes.

    The earlier streaming copy already checked SHA-256 and fstat before/after.
    Device/inode, size, mode, owner, nanosecond mtime and ctime detect replacement
    or modification across the subsequent core snapshot, including mtime resets.
    """
    identities = {}
    total = 0
    for label, root in sorted(roots.items()):
        root = Path(root)
        require(root.exists() and not root.is_symlink(), 'missing or linked source root')
        paths = [root, *sorted(root.rglob('*'))] if root.is_dir() else [root]
        for path in paths:
            info = selected_stat(path)
            require(stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode),
                    'linked or special media member')
            relative = (PurePosixPath(label) / path.relative_to(root).as_posix()).as_posix()
            require(relative not in identities, 'overlapping media roots')
            if stat.S_ISREG(info.st_mode):
                require(info.st_size <= MAX_FILE, 'media file exceeds capacity bound')
                total += info.st_size
            identities[relative] = stamp(info)
            require(total <= MAX_TOTAL and len(identities) <= 200000, 'media exceeds capacity bound')
    return identities


def directory_window(core, root):
    paths = core.source_paths(root)
    directories = business_directories(core, root, [path.relative_to(root).as_posix() for path in paths])
    identities = {}
    for name, row in directories.items():
        actual, identity = entry(safe_path(root, name))
        unchanged(actual == row, 'business directory changed during inspection')
        identities[name] = identity
    return directories, identities


def apply_metadata(root, rows):
    for name, row in sorted(rows.items(), key=lambda pair: len(PurePosixPath(pair[0]).parts), reverse=True):
        path = safe_path(root, name)
        os.chown(path, row['uid'], row['gid'], follow_symlinks=False)
        os.chmod(path, row['mode'], follow_symlinks=False)
        os.utime(path, ns=(row['mtime_ns'], row['mtime_ns']), follow_symlinks=False)


def parent_directories(root, names):
    result = {}
    for name in names:
        path = safe_path(root, name)
        parent = path.parent
        while parent != root:
            relative = parent.relative_to(root).as_posix()
            if relative in result:
                # Its full ancestor chain was captured on the first visit.
                break
            result[relative] = entry(parent)[0]
            require(result[relative]['type'] == 'directory', 'non-directory source parent')
            parent = parent.parent
    return result


def business_directories(core, root, names):
    """Preserve allowed empty business/authentication dirs as well as file parents."""
    directories = parent_directories(root, names)
    trees = [root / name for name in core.TOP if (root / name).is_dir()]
    trees += [root / 'data' / name for name in core.DATA_DIRS if (root / 'data' / name).exists()]
    browser_worker = root / 'data/browser-worker'
    if browser_worker.exists():
        # The core helper validates provider identities, active locks and auth files.
        core.browser_profile_paths(browser_worker)
        cache_names = getattr(core, 'BROWSER_REBUILDABLE_DIRS', {'cloakbrowser'})
        shallow = [browser_worker]
        for provider in browser_worker.iterdir():
            # Re-creatable Chromium runtime; the core helper validates its
            # bounded, link-free tree and excludes its files.
            if provider.name in cache_names:
                continue
            require(provider.is_dir() and not provider.is_symlink(),
                    'invalid browser provider directory')
            shallow.append(provider)
            for profile in provider.iterdir():
                shallow.append(profile)
                browser = profile / 'browser'
                if browser.exists():
                    shallow.append(browser)
                    if (browser / 'Default').exists():
                        shallow.append(browser / 'Default')
                    for name in ('Default/Local Storage', 'Default/Session Storage', 'Default/IndexedDB'):
                        if (browser / name).exists():
                            trees.append(browser / name)
        for path in shallow:
            relative = path.relative_to(root).as_posix()
            require(stat.S_ISDIR(selected_stat(path).st_mode), 'invalid authentication directory')
            if relative not in directories:
                row = entry(path)[0]
                require(row['type'] == 'directory', 'invalid authentication directory')
                directories[relative] = row
    for tree in trees:
        for path in [tree, *sorted(tree.rglob('*'))]:
            info = selected_stat(path)
            require(stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode), 'linked or special business member')
            relative = path.relative_to(root).as_posix()
            if stat.S_ISDIR(info.st_mode) and relative not in directories:
                directories[relative] = entry(path)[0]
    data = root / 'data'
    if data.exists() and 'data' not in directories:
        directories['data'] = entry(data)[0]
    return directories


def preserve_root(source, destination):
    info = source.lstat()
    require(stat.S_ISDIR(info.st_mode), 'service root is not a directory')
    os.chown(destination, info.st_uid, info.st_gid)
    os.chmod(destination, stat.S_IMODE(info.st_mode))


def write_core(stage, contents, receipt, directories):
    service = stage / 'service'; service.mkdir(mode=0o700, exist_ok=True)
    require(service.is_dir() and not service.is_symlink(), 'private service stage is linked')
    (stage / 'database').mkdir(mode=0o700)
    for name in sorted(directories, key=lambda item: len(PurePosixPath(item).parts)):
        safe_path(service, name).mkdir(parents=True, exist_ok=True, mode=0o700)
    for name, raw in contents.items():
        target_name = 'core-receipt.json' if name == 'manifest.json' else name if name.startswith('database/') else 'service/' + name
        target = safe_path(stage, target_name)
        target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        with target.open('xb') as stream:
            stream.write(raw); stream.flush(); os.fsync(stream.fileno())
        item = receipt['files'].get(name, {'mode': 0o600, 'uid': 0, 'gid': 0})
        os.chown(target, item['uid'], item['gid']); os.chmod(target, item['mode'])
    apply_metadata(service, directories)


def capture_cfm_once(core, work):
    root = ROOT
    stage = work / '.staging'; stage.mkdir(mode=0o700)
    media = {'data/files': root / 'data/files', 'data/log_input_images': root / 'data/log_input_images'}
    # Bulk copying happens before the core consistency window. The application
    # may legitimately update core JSON while these media bytes are being read.
    _, copied_media_identities = inventory(media, stage / 'service')
    unchanged(stat_inventory(media) == copied_media_identities, 'media changed while preparing core snapshot')
    directories, directory_identities = directory_window(core, root)
    # The project's helper checks persisted core files and SQL before/after its
    # own collection and rejects a mixed snapshot. Do not weaken that check.
    contents, receipt = core.snapshot_once()
    unchanged(directory_window(core, root) == (directories, directory_identities),
            'business directories changed during core snapshot')
    unchanged(stat_inventory(media) == copied_media_identities, 'media changed around core snapshot')
    write_core(stage, contents, receipt, directories)
    preserve_root(root, stage / 'service')
    return stage, receipt, {'media_included': True, 'authentication_state_included': True,
        'excluded': ['logs', 'data/logs.jsonl', 'data/video_logs', 'browser_cache_and_history', 'live_lock_and_gateway_files'],
        'consistency': 'media SHA-verified before core collection; exact media identities and business directories stable across project core/SQL/Redis snapshot; no application pause'}


def capture_cfm(core, work):
    # A successful inner retry cannot repair stale outer media/directory baselines.
    # Own the sole retry loop here and discard only this invocation's partial stage.
    stage = work / '.staging'
    require(not stage.exists() and not stage.is_symlink(), 'capture stage already exists')
    for attempt in range(1, CAPTURE_ATTEMPTS + 1):
        try:
            result = capture_cfm_once(core, work)
            result[2]['capture_attempt'] = attempt
            return result
        except (SnapshotChanged, core.SnapshotChanged) as error:
            if stage.exists():
                require(stage.is_dir() and not stage.is_symlink(), 'capture stage changed type')
                shutil.rmtree(stage)
            if attempt == CAPTURE_ATTEMPTS:
                raise SnapshotChanged('complete capture remained unstable after bounded retries') from error
            time.sleep(1)



def archive_metadata(stage):
    rows = {'.': entry(stage)[0]}
    for path in sorted(stage.rglob('*')):
        rows[path.relative_to(stage).as_posix()] = entry(path)[0]
    require(sum(row.get('size', 0) for row in rows.values()) <= MAX_TOTAL, 'staged snapshot exceeds capacity')
    return rows


def verify_archive(path, expected, collect=()):
    seen, collected = {}, {}
    with gzip.open(path, 'rb') as compressed:
        with tarfile.open(fileobj=compressed, mode='r|') as archive:
            for member in archive:
                parts = PurePosixPath(member.name)
                require(parts.parts and parts.parts[0] == 'payload' and not parts.is_absolute()
                        and '..' not in parts.parts, 'unsafe archive path')
                name = PurePosixPath(*parts.parts[1:]).as_posix()
                require(name in expected and name not in seen and (member.isdir() or member.isfile()),
                        'unexpected, duplicate or linked archive member')
                row = {'type': 'directory' if member.isdir() else 'file', 'mode': member.mode & 0o7777,
                       'uid': member.uid, 'gid': member.gid,
                       'mtime_ns': int(Decimal(member.pax_headers.get('mtime', str(member.mtime))) * 1000000000)}
                if member.isfile():
                    require(member.size <= MAX_FILE, 'archive member exceeds bound')
                    digest = hashlib.sha256(); count = 0; chunks = []
                    with archive.extractfile(member) as source:
                        for block in iter(lambda: source.read(CHUNK), b''):
                            count += len(block); digest.update(block)
                            require(count <= member.size, 'archive member grew')
                            if name in collect:
                                require(count <= 64 * CHUNK, 'restore input exceeds bound')
                                chunks.append(block)
                    row.update(size=count, sha256=digest.hexdigest())
                    if name in collect:
                        collected[name] = b''.join(chunks)
                require(row == expected[name], 'archive content or metadata differs from staged snapshot')
                seen[name] = row
        # tar EOF precedes gzip CRC/trailer. Consume the full stream before accepting it.
        while compressed.read(CHUNK):
            pass
    require(seen == expected, 'archive missing staged members')
    require(set(collected) == set(collect), 'archive missing database restore input')
    return collected


def pack(stage, destination, manifest):
    def metadata(member):
        name = PurePosixPath(*PurePosixPath(member.name).parts[1:]).as_posix()
        member.pax_headers['mtime'] = format(Decimal(manifest[name]['mtime_ns']) / 1000000000, '.9f')
        return member
    with tarfile.open(destination, 'x:gz', compresslevel=1, dereference=True) as archive:
        archive.add(stage, arcname='payload', filter=metadata)
    os.chmod(destination, 0o600)
    with destination.open('rb') as stream:
        os.fsync(stream.fileno())


def checked_config(path):
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o022,
            'dependency config must be root-owned and not group/world writable')
    data = json.loads(path.read_text())
    require(data.get('format') == 'ogg-daily-dependencies-v1', 'unknown dependency config format')
    require(set(data['services']) == {SERVICE}, 'dependency config must describe only this project')
    config = data['services'][SERVICE]
    require(set(config['helpers']) == set(HELPERS), 'dependency scope differs from fixed helpers')
    for helper, digest in config['helpers'].items():
        p = Path(helper); info = p.lstat()
        require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o022,
                'helper must be root-owned and not group/world writable')
        require(sha_file(p) == digest, 'installed backup helper changed; review and repin dependency')
    require(str(ROOT / 'maintenance.lock') in config['locks'], 'service maintenance lock missing')
    require(config['locks'] and len(config['locks']) <= 20, 'invalid coordination locks')
    for lock in config['locks']:
        path = Path(lock)
        require(path.is_absolute() and '..' not in path.parts and str(path).startswith(('/opt/', '/home/ubuntu/')),
                'unexpected coordination lock path')
        require(stat.S_ISREG(path.lstat().st_mode) and not path.is_symlink(), 'coordination lock missing or linked')
    return config


@contextlib.contextmanager
def locks(paths):
    handles = []
    try:
        for path in sorted(paths):
            before = Path(path).lstat()
            fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
            handle = os.fdopen(fd, 'r+'); handles.append(handle)
            require(stamp(os.fstat(fd)) == stamp(before), 'coordination lock identity changed')
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        for handle in reversed(handles):
            handle.close()


def load_helper():
    path = HELPERS[0]
    specification = importlib.util.spec_from_file_location('ogg_daily_' + SERVICE, path)
    module = importlib.util.module_from_spec(specification); specification.loader.exec_module(module)
    return module


def container_identity():
    ids = subprocess.run(['docker', 'ps', '-aq'], capture_output=True, timeout=30, check=True).stdout.split()
    require(ids, 'no production containers found')
    raw = subprocess.run(['docker', 'inspect', *[item.decode() for item in ids]],
                         capture_output=True, timeout=30, check=True).stdout
    require(len(raw) <= 16 * CHUNK, 'container inventory exceeds bound')
    return {row['Id']: {'image': row['Image'], 'status': row['State']['Status'],
            'started': row['State']['StartedAt'], 'health': row['State'].get('Health', {}).get('Status'),
            'restarts': row['RestartCount'], 'mounts': row['Mounts'],
            'networks': {name: item['NetworkID'] for name, item in row['NetworkSettings']['Networks'].items()}}
            for row in json.loads(raw)}


RETRY_FORMAT = 'ogg-daily-retry-v1'
RETRY_LIMIT = 6
RETRY_COOLDOWN = dt.timedelta(hours=1)
LOCAL_TZ = dt.timezone(dt.timedelta(hours=8))


def policy_time(value):
    require(isinstance(value, str), 'invalid backup policy timestamp')
    try:
        result = dt.datetime.fromisoformat(value.replace('Z', '+00:00'))
    except ValueError as error:
        raise Refused('invalid backup policy timestamp') from error
    require(result.tzinfo is not None, 'backup policy timestamp lacks timezone')
    return result


def private_json(path):
    """Read a small policy record without accepting links or writable evidence."""
    require(path.resolve() == path, 'linked backup policy evidence')
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
            and not info.st_mode & 0o077 and info.st_nlink == 1 and info.st_size <= 65536,
            'unsafe backup policy evidence')
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor, 'rb') as stream:
        require(stamp(os.fstat(descriptor)) == stamp(info), 'backup policy evidence changed')
        value = json.loads(stream.read(65537))
    require(stamp(path.lstat()) == stamp(info) and isinstance(value, dict),
            'backup policy evidence changed or invalid')
    return value, stamp(info)


def retry_state():
    path = ROOT / 'backup-policy' / 'daily-retry.json'
    if not path.exists() and not path.is_symlink():
        return {'format': RETRY_FORMAT, 'service': SERVICE, 'failures': []}
    value, _ = private_json(path)
    require(set(value) == {'format', 'service', 'failures'} and value.get('format') == RETRY_FORMAT
            and value.get('service') == SERVICE and isinstance(value.get('failures'), list)
            and len(value['failures']) <= RETRY_LIMIT, 'invalid daily retry state')
    previous = None
    for failure in value['failures']:
        require(isinstance(failure, dict) and set(failure) == {'at', 'work', 'reason'}
                and failure.get('reason') == 'snapshot_changed', 'unknown daily retry failure')
        moment = policy_time(failure.get('at'))
        require(previous is None or moment >= previous, 'unordered daily retry evidence')
        previous = moment
        require(isinstance(failure.get('work'), str) and failure['work'].startswith('daily-')
                and Path(failure['work']).name == failure['work'], 'invalid daily retry work identity')
    return value


def retry_status(at=None):
    """A bounded retry journal applies to future capture attempts, never old backups."""
    at = at or dt.datetime.now(dt.timezone.utc)
    state = retry_state()
    failures = state['failures']
    if not failures:
        return None
    last = policy_time(failures[-1]['at'])
    require(last <= at, 'daily retry clock moved backwards')
    local = at.astimezone(LOCAL_TZ)
    count = sum(policy_time(item['at']).astimezone(LOCAL_TZ).date() == local.date()
                for item in failures)
    retry_after = last + RETRY_COOLDOWN
    reason = 'cooldown'
    if count >= RETRY_LIMIT:
        tomorrow = local.date() + dt.timedelta(days=1)
        retry_after = max(retry_after, dt.datetime.combine(tomorrow, dt.time(), LOCAL_TZ))
        reason = 'daily_failure_limit'
    if at < retry_after:
        return {'event': 'daily_backup_deferred', 'service': SERVICE, 'reason': reason,
                'retry_after': retry_after.isoformat(), 'failed_attempts_today': count}
    return None


def record_retry_failure(work, at=None):
    at = at or dt.datetime.now(dt.timezone.utc)
    require(retry_status(at) is None, 'daily retry budget is exhausted')
    state = retry_state()
    state['failures'] = (state['failures'] + [{'at': at.isoformat(), 'work': work.name,
                                            'reason': 'snapshot_changed'}])[-RETRY_LIMIT:]
    directory = ROOT / 'backup-policy'
    directory.mkdir(mode=0o700, exist_ok=True)
    info = directory.lstat()
    require(directory.resolve() == directory and stat.S_ISDIR(info.st_mode)
            and info.st_uid == os.geteuid() and not info.st_mode & 0o022,
            'unsafe backup policy directory')
    atomic_json(directory / 'daily-retry.json', state)
    # Persist the journal before discarding this invocation's transient scratch.
    descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def policy_modules():
    modules = []
    for name in ('backup_catalog', 'backup_retention'):
        spec = importlib.util.spec_from_file_location('daily_' + name, Path(__file__).with_name(name + '.py'))
        module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
        modules.append(module)
    return modules


def verified_newer_recovery(backup_root, failed_at):
    """Re-read actual success proofs and complete archives; cached booleans are unused."""
    catalog, retention = policy_modules()
    external = catalog.external_proofs(backup_root.parent)
    candidates = []
    for path in backup_root.iterdir():
        if not path.is_dir() or path.resolve() != path or not catalog.NAME.fullmatch(path.name):
            continue
        try:
            created = policy_time(catalog.stamp(path.name))
        except (Refused, catalog.CatalogError, ValueError):
            continue
        if created > failed_at and not (path / 'failed.json').exists() and not (path / 'failed.json').is_symlink():
            candidates.append((created, path))
    for _, path in sorted(candidates, reverse=True):
        record = catalog.make_record(SERVICE, path, external, None, {})
        if not (record.get('completed') is True and record.get('success') is True
                and record.get('kind') in ('daily', 'release') and record.get('proofs')
                and policy_time(record.get('created_at')) > failed_at):
            continue
        identities = {artifact: catalog.signature(Path(artifact))
                      for artifact in [*(str(path / name) for name in record['files']),
                                       *(item['path'] for item in record['proofs'])]}
        directory_identity = stamp(path.lstat())
        retention.verify_record(path, record)
        # verify_record binds files and EOF/CRC; daily archives also bind every
        # member's content and metadata to the manifest used during restore.
        if record['kind'] == 'daily':
            proof = catalog.load(path / 'recovery-point.json')
            restore = proof.get('database_restore', {})
            scope = proof.get('scope', {})
            require(isinstance(restore, dict) and isinstance(scope, dict)
                    and all(restore.get(key) is True for key in
                            ('postgres_restored', 'postgres_rows_match', 'redis_restored'))
                    and restore.get('network') == 'none' and restore.get('production_mounts') is False
                    and all(type(restore.get(key)) is int and restore[key] >= 0
                            for key in ('postgres_tables', 'redis_loaded_keys'))
                    and proof.get('production_containers_unchanged') is True
                    and all(scope.get(key) is True for key in
                            ('media_included', 'authentication_state_included')),
                    'replacement daily recovery lacks isolated restore or full-scope evidence')
            manifest = catalog.load(path / 'manifest.json')
            require(manifest.get('format') == 'ogg-daily-manifest-v1'
                    and manifest.get('service') == SERVICE, 'unknown daily manifest')
            verify_archive(path / 'backup.tar.gz', manifest['members'])
        else:
            require(any(item.get('metadata_file') for item in record['archives']),
                    'replacement recovery point lacks full archive metadata')
        require(all(catalog.signature(Path(artifact)) == identity
                    for artifact, identity in identities.items())
                and stamp(path.lstat()) == directory_identity,
                'replacement recovery evidence changed during full verification')
        return path.name
    raise Refused('unresolved daily backup has no newer verified recovery point')


def ensure_no_unresolved(backup_root):
    unresolved = []
    for path in backup_root.glob('daily-*'):
        require(path.is_dir() and path.resolve() == path, 'daily backup path is linked or not a directory')
        if ((path / 'complete').is_file() and not (path / 'complete').is_symlink()
                and not (path / 'failed.json').exists() and not (path / 'failed.json').is_symlink()):
            continue
        require({child.name for child in path.iterdir()} == {'failed.json'},
                'unresolved daily backup contains artifacts; manual review required')
        try:
            marker, identity = private_json(path / 'failed.json')
        except (Refused, ValueError, OSError) as error:
            raise Refused('unresolved daily backup has unsafe or invalid failure evidence') from error
        require(set(marker) == {'format', 'at', 'service', 'completed', 'automatic_prune_allowed'}
                and marker.get('format') == 'ogg-daily-failed-v1' and marker.get('service') == SERVICE
                and marker.get('completed') is False and marker.get('automatic_prune_allowed') is False,
                'unresolved daily backup has an unknown failure marker')
        failed_at = policy_time(marker.get('at'))
        try:
            started = dt.datetime.strptime(path.name, 'daily-%Y%m%dT%H%M%SZ').replace(tzinfo=dt.timezone.utc)
        except ValueError as error:
            raise Refused('unresolved daily backup has an ambiguous timestamp') from error
        require(started <= failed_at <= dt.datetime.now(dt.timezone.utc),
                'unresolved daily backup has an invalid failure time')
        unresolved.append((path, marker, identity, stamp(path.lstat()), failed_at))
    if not unresolved:
        return
    verified_newer_recovery(backup_root, max(item[-1] for item in unresolved))
    for path, marker, identity, directory_identity, _ in unresolved:
        current, current_identity = private_json(path / 'failed.json')
        require(current == marker and current_identity == identity and stamp(path.lstat()) == directory_identity
                and {child.name for child in path.iterdir()} == {'failed.json'},
                'unresolved daily backup changed during recovery verification')


def work_identity(work):
    info = work.lstat()
    require(work.resolve() == work and stat.S_ISDIR(info.st_mode), 'backup work path changed or linked')
    return info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid


def discard_retry_scratch(work, identity):
    """Delete only the current invocation's scratch; any other member prevents removal."""
    require(work_identity(work) == identity, 'backup work directory identity changed')
    require({path.name for path in work.iterdir()} <= {'.staging', '.restore'},
            'transient backup failure has archive or unknown residue')
    for name in ('.staging', '.restore'):
        path = work / name
        if path.exists() or path.is_symlink():
            require(path.is_dir() and not path.is_symlink(), 'transient backup scratch is linked or invalid')
            shutil.rmtree(path)
    require(work_identity(work) == identity, 'backup work directory identity changed')
    work.rmdir()


def create(config):
    root = ROOT; backup_root = root / 'backups'
    require(backup_root.is_dir() and backup_root.resolve() == backup_root, 'backup root missing or linked')
    deferred = retry_status()
    if deferred:
        return deferred
    ensure_no_unresolved(backup_root)
    require(shutil.disk_usage(backup_root).free >= 12 * GIB, 'free disk below 12 GiB daily-backup reserve')
    os.umask(0o077); os.nice(15)
    helper = load_helper()
    production_before = container_identity()
    timestamp = dt.datetime.now(dt.timezone.utc)
    name = 'daily-' + timestamp.strftime('%Y%m%dT%H%M%SZ')
    work = backup_root / name; work.mkdir(mode=0o700)
    identity = work_identity(work)
    completed = False
    discarded = False
    capture_complete = False
    try:
        stage, receipt, scope = capture_cfm(helper, work)
        capture_complete = True
        manifest = archive_metadata(stage)
        needed = sum(row.get('size', 0) for row in manifest.values())
        require(shutil.disk_usage(backup_root).free >= needed + 2 * GIB,
                'free disk below archive plus 2 GiB reserve')
        pack(stage, work / 'backup.tar.gz', manifest)
        db_names = ['database/postgres.dump', 'database/postgres-globals.sql', 'database/redis.rdb']
        restored = verify_archive(work / 'backup.tar.gz', manifest, db_names)
        verification = helper.rehearse(restored, receipt)
        require(container_identity() == production_before, 'production container identity or health changed')
        atomic_json(work / 'manifest.json', {'format': 'ogg-daily-manifest-v1', 'service': SERVICE,
            'created_at': timestamp.isoformat(), 'scope': scope, 'members': manifest})
        checksums = {item: sha_file(work / item) for item in ('backup.tar.gz', 'manifest.json')}
        atomic_json(work / 'checksums.json', checksums)
        proof = {'format': 'ogg-daily-proof-v1', 'service': SERVICE, 'kind': 'daily',
            'created_at': timestamp.isoformat(), 'verified_at': now(), 'completed': True, 'success': True,
            'source_commit': receipt.get('source_commit'), 'archive_verified': True,
            'metadata_verified': True, 'database_restore': verification, 'scope': scope,
            'dependencies': config['helpers'], 'checksums': checksums, 'production_containers_unchanged': True}
        atomic_json(work / 'recovery-point.json', proof)
        # Staging is owned by this invocation and never referenced by production.
        shutil.rmtree(stage)
        if (work / '.restore').exists():
            shutil.rmtree(work / '.restore')
        complete_value = {'format': 'ogg-daily-complete-v1', 'verified_at': now(),
            'proof_sha256': sha_file(work / 'recovery-point.json')}
        complete_sha = hashlib.sha256(json_bytes(complete_value)).hexdigest()
        required = ['payload/service/.env', 'payload/service/compose.yaml', 'payload/service/current.json',
                    'payload/database/postgres.dump']
        required += ['payload/database/redis.rdb', 'payload/database/postgres-globals.sql',
                     'payload/service/data/files', 'payload/service/data/log_input_images']
        point = {'name': name, 'kind': 'daily', 'created_at': timestamp.isoformat(),
            'completed': True, 'success': True, 'protected_reasons': [],
            'proofs': [{'path': str(work / 'recovery-point.json'), 'sha256': sha_file(work / 'recovery-point.json')}],
            'files': {item: sha_file(work / item) for item in
                ('backup.tar.gz', 'manifest.json', 'checksums.json', 'recovery-point.json')},
            'archives': [{'path': 'backup.tar.gz', 'required_roots': ['payload'], 'required_members': required}]}
        point['files']['complete'] = complete_sha
        atomic_json(work / 'catalog-entry.json', point)
        result = {'event': 'daily_backup_complete', 'service': SERVICE, 'name': name,
                  'archive_bytes': (work / 'backup.tar.gz').stat().st_size,
                  'catalog_entry': str(work / 'catalog-entry.json')}
        # This is the last publication marker. A crash before it is incomplete.
        atomic_json(work / 'complete', complete_value)
        completed = True
        return result
    except SnapshotChanged:
        require(not capture_complete, 'source-change error outside capture requires review')
        require(container_identity() == production_before, 'production container identity or health changed')
        require({path.name for path in work.iterdir()} <= {'.staging', '.restore'},
                'transient backup failure has archive or unknown residue')
        record_retry_failure(work)
        discard_retry_scratch(work, identity)
        discarded = True
        raise
    finally:
        if not completed and not discarded:
            require(work_identity(work) == identity, 'backup work directory identity changed')
            # Only invocation-created scratch directories are discarded. An archive, if
            # already made, remains an explicitly incomplete recovery artifact for review.
            try:
                for name in ('.staging', '.restore'):
                    if (work / name).is_dir() and not (work / name).is_symlink():
                        shutil.rmtree(work / name)
            finally:
                # Even a failed scratch cleanup is durable, non-retryable evidence.
                require(work_identity(work) == identity, 'backup work directory identity changed')
                atomic_json(work / 'failed.json', {'format': 'ogg-daily-failed-v2', 'at': now(),
                    'service': SERVICE, 'completed': False, 'automatic_prune_allowed': False,
                    'reason': 'non_retryable_or_residue',
                    'phase': 'after_capture' if capture_complete else 'capture'})


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dependency-config', type=Path, required=True)
    parser.add_argument('--execute', action='store_true', help='create one recovery point after caller checks today’s catalog')
    args = parser.parse_args(argv)
    require(os.geteuid() == 0, 'run as root on the target instance')
    config = checked_config(args.dependency_config)
    if not args.execute:
        print(json.dumps({'event': 'daily_backup_plan', 'service': SERVICE, 'will_create': True,
                          'source': str(ROOT), 'locks': config['locks']}))
        return 0
    try:
        with locks(config['locks']):
            # Re-check hashes after locking so a deploy cannot replace imported helpers.
            config = checked_config(args.dependency_config)
            result = create(config)
        print(json.dumps(result)); return 75 if result['event'] == 'daily_backup_deferred' else 0
    except BlockingIOError:
        print(json.dumps({'event': 'daily_backup_busy', 'service': SERVICE})); return 75


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception as error:
        print(json.dumps({'event': 'daily_backup_failed', 'error_type': type(error).__name__,
                          'reason': str(error) if isinstance(error, Refused) else 'project helper failed; private output withheld'}))
        sys.exit(1)
