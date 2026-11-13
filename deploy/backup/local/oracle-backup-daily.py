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

sys.dont_write_bytecode = True

GIB = 1024 ** 3
CHUNK = 1024 ** 2
MAX_TOTAL = 8 * GIB
MAX_FILE = 2 * GIB
SERVICE = 'gptgrok2api'
ROOT = Path('/opt/gptgrok2api')
HELPERS = ['/opt/gptgrok2api/tools/backup-core.py']


class Refused(RuntimeError):
    """An expected safety refusal; messages never contain business contents."""


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
    before = path.lstat()
    require(stat.S_ISDIR(before.st_mode) or stat.S_ISREG(before.st_mode),
            'linked or special source member')
    result = {'type': 'directory' if stat.S_ISDIR(before.st_mode) else 'file',
              'mode': stat.S_IMODE(before.st_mode), 'uid': before.st_uid,
              'gid': before.st_gid, 'mtime_ns': before.st_mtime_ns}
    if result['type'] == 'file':
        require(before.st_size <= MAX_FILE, 'source file exceeds capacity bound')
        result.update(size=before.st_size, sha256=sha_file(path))
    require(stamp(path.lstat()) == stamp(before), 'source changed during inventory')
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
                    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
                    with os.fdopen(fd, 'rb') as source, target.open('xb') as output:
                        require(stamp(os.fstat(source.fileno())) == identity, 'source changed before copy')
                        digest = hashlib.sha256(); copied = 0
                        for block in iter(lambda: source.read(CHUNK), b''):
                            copied += len(block); digest.update(block); output.write(block)
                            require(copied <= row['size'], 'source grew during copy')
                        require(digest.hexdigest() == row['sha256'] and copied == row['size'],
                                'copied content differs from inventory')
                        require(stamp(os.fstat(source.fileno())) == identity, 'source changed during copy')
                        output.flush(); os.fsync(output.fileno())
                    require(stamp(path.lstat()) == identity, 'source changed after copy')
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
            info = path.lstat()
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
        require(actual == row, 'business directory changed during inspection')
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
        shallow = [browser_worker, *browser_worker.iterdir()]
        for provider in browser_worker.iterdir():
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
            row = entry(path)[0]
            require(row['type'] == 'directory', 'invalid authentication directory')
            directories[path.relative_to(root).as_posix()] = row
    for tree in trees:
        for path in [tree, *sorted(tree.rglob('*'))]:
            info = path.lstat()
            require(stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode), 'linked or special business member')
            if stat.S_ISDIR(info.st_mode):
                directories[path.relative_to(root).as_posix()] = entry(path)[0]
    data = root / 'data'
    if data.exists():
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


def capture_cfm(core, work):
    root = ROOT
    stage = work / '.staging'; stage.mkdir(mode=0o700)
    media = {'data/files': root / 'data/files', 'data/log_input_images': root / 'data/log_input_images'}
    # Bulk copying happens before the core consistency window. The application
    # may legitimately update core JSON while these media bytes are being read.
    _, copied_media_identities = inventory(media, stage / 'service')
    require(stat_inventory(media) == copied_media_identities, 'media changed while preparing core snapshot')
    directories, directory_identities = directory_window(core, root)
    # The project's helper checks persisted core files and SQL before/after its
    # own collection and rejects a mixed snapshot. Do not weaken that check.
    contents, receipt = core.snapshot()
    require(directory_window(core, root) == (directories, directory_identities),
            'business directories changed during core snapshot')
    require(stat_inventory(media) == copied_media_identities, 'media changed around core snapshot')
    write_core(stage, contents, receipt, directories)
    preserve_root(root, stage / 'service')
    return stage, receipt, {'media_included': True, 'authentication_state_included': True,
        'excluded': ['logs', 'data/logs.jsonl', 'data/video_logs', 'browser_cache_and_history', 'live_lock_and_gateway_files'],
        'consistency': 'media SHA-verified before core collection; exact media identities and business directories stable across project core/SQL/Redis snapshot; no application pause'}



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


def ensure_no_unresolved(backup_root):
    for path in backup_root.glob('daily-*'):
        require(path.is_dir() and not path.is_symlink(), 'daily backup path is linked or not a directory')
        require((path / 'complete').is_file() and not (path / 'complete').is_symlink()
                and not (path / 'failed.json').exists(),
                'unresolved daily backup exists; reconcile it before creating another')


def create(config):
    root = ROOT; backup_root = root / 'backups'
    require(backup_root.is_dir() and backup_root.resolve() == backup_root, 'backup root missing or linked')
    ensure_no_unresolved(backup_root)
    require(shutil.disk_usage(backup_root).free >= 12 * GIB, 'free disk below 12 GiB daily-backup reserve')
    os.umask(0o077); os.nice(15)
    helper = load_helper()
    production_before = container_identity()
    timestamp = dt.datetime.now(dt.timezone.utc)
    name = 'daily-' + timestamp.strftime('%Y%m%dT%H%M%SZ')
    work = backup_root / name; work.mkdir(mode=0o700)
    completed = False
    try:
        stage, receipt, scope = capture_cfm(helper, work)
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
    finally:
        if not completed:
            # Only invocation-created scratch directories are discarded. An archive, if
            # already made, remains an explicitly incomplete recovery artifact for review.
            for name in ('.staging', '.restore'):
                if (work / name).is_dir() and not (work / name).is_symlink():
                    shutil.rmtree(work / name)
            atomic_json(work / 'failed.json', {'format': 'ogg-daily-failed-v1', 'at': now(),
                'service': SERVICE, 'completed': False, 'automatic_prune_allowed': False})


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
        print(json.dumps(result)); return 0
    except BlockingIOError:
        print(json.dumps({'event': 'daily_backup_busy', 'service': SERVICE})); return 75


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception as error:
        print(json.dumps({'event': 'daily_backup_failed', 'error_type': type(error).__name__,
                          'reason': str(error) if isinstance(error, Refused) else 'project helper failed; private output withheld'}))
        sys.exit(1)
