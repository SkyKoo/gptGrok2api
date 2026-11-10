#!/usr/bin/env python3
"""Project-owned CFM snapshot and 1Panel upload entry point.

Deploy this exact file to /opt/gptgrok2api/tools/backup-core.py. Keep secrets and
site-specific 1Panel job IDs in private server state, never in this repository.
"""
import argparse
import datetime as dt
import fcntl
import gzip
import hashlib
import http.client
import io
import json
import os
from pathlib import Path
import posixpath
import shutil
import socket
import sqlite3
import stat
import subprocess
import tarfile
import time
import urllib.parse
import urllib.request

ROOT = Path('/opt/1panel/backup/ogg-cfm-core')
SOURCE = Path('/opt/gptgrok2api')
PG = 'gptgrok2api-postgres-1'
REDIS = 'gptgrok2api-redis-1'
RUN_NAME = 'O-CFM-Core-GoogleDrive'
UPLOAD_NAME = 'O-CFM-Core-Upload-GoogleDrive'
MAX_BYTES = 64 * 1024 * 1024
MAX_FILES = 5000
TOP = ('.env', 'compose.yaml', 'current.json', 'access.json', 'slot-blue.json',
       'slot-green.json', 'secrets')
DATA_DIRS = {'image_tasks', 'video_tasks', 'grok_media_reviews', 'adobe_jobs'}
EXCLUDED = {'files', 'log_input_images', 'logs.jsonl', 'video_logs', 'backups',
            '.cfm-writer.lock', '.cfm-gateway.json', '.cfm-legacy-instance.json'}


class BackupError(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise BackupError(message)


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def atomic_json(path, value):
    path = Path(path)
    temporary = path.with_suffix(path.suffix + '.tmp')
    with temporary.open('w') as handle:
        os.fchmod(handle.fileno(), 0o600)
        json.dump(value, handle, indent=2)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def emit(event, **fields):
    print(json.dumps({'at': now(), 'event': event, **fields}), flush=True)


def run(args, *, data=None, timeout=45):
    try:
        proc = subprocess.run(args, input=data, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise BackupError('bounded command timed out') from None
    require(proc.returncode == 0, 'command failed; private output withheld')
    require(len(proc.stdout) <= MAX_BYTES, 'command output exceeds backup limit')
    return proc.stdout


def database():
    db = sqlite3.connect('file:/opt/1panel/db/agent.db?mode=ro', uri=True)
    db.row_factory = sqlite3.Row
    return db


class AgentConnection(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(30)
        self.sock.connect('/etc/1panel/agent.sock')


def api(path, body):
    connection = AgentConnection('localhost')
    try:
        connection.request('POST', '/api/v2' + path, json.dumps(body),
                           {'Content-Type': 'application/json'})
        response = connection.getresponse()
        result = json.loads(response.read())
        require(response.status == 200 and result.get('code') == 200,
                '1Panel request rejected: ' + path)
        return result.get('data')
    finally:
        connection.close()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def google(url, token=None, body=None):
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    request = urllib.request.Request(url, headers=headers,
                                    data=json.dumps(body).encode() if body else None)
    with urllib.request.build_opener(NoRedirect).open(request, timeout=45) as response:
        raw = response.read(MAX_BYTES + 1)
    require(len(raw) <= MAX_BYTES, 'cloud response exceeds size limit')
    return raw


def cloud_session():
    with database() as db:
        row = db.execute('SELECT name,type,vars,backup_path FROM backup_accounts WHERE id=2').fetchone()
    require(row and row['name'] == 'google-drive-o-backup' and row['type'] == 'GoogleDrive',
            'expected Google Drive account not found')
    values = json.loads(row['vars'])
    body = {key: values[key] for key in ('client_id', 'client_secret', 'refresh_token', 'redirect_uri') if key in values}
    body['grant_type'] = 'refresh_token'
    token = json.loads(google('https://www.googleapis.com/oauth2/v4/token', body=body))['access_token']
    quota = json.loads(google('https://www.googleapis.com/drive/v3/about?fields=storageQuota', token))['storageQuota']
    require(int(quota['limit']) - int(quota['usage']) >= 1024 ** 3, 'Google Drive headroom below 1 GiB')
    return token, row['backup_path'], quota


def cloud_file(token, full_path):
    parent = 'root'
    for part in full_path.strip('/').split('/'):
        escaped = part.replace('\\', '\\\\').replace("'", "\\'")
        query = "'" + parent + "' in parents and trashed=false and name='" + escaped + "'"
        url = 'https://www.googleapis.com/drive/v3/files?' + urllib.parse.urlencode({
            'q': query, 'fields': 'files(id,name,size,md5Checksum),nextPageToken', 'pageSize': 100})
        value = json.loads(google(url, token))
        require(not value.get('nextPageToken') and len(value['files']) == 1, 'cloud path is ambiguous')
        item = value['files'][0]
        parent = item['id']
    return item


def signature(path):
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode), 'non-regular source file')
    require(info.st_size <= 16 * 1024 ** 2, 'source file exceeds size bound')
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns,
            info.st_mode, info.st_uid, info.st_gid)


def browser_profile_paths(root):
    """Include authentication storage; exclude caches, history and live locks."""
    paths = []
    if not root.exists():
        return paths
    require(not root.is_symlink(), 'linked browser worker storage')
    for provider in sorted(root.iterdir()):
        require(provider.name == 'adobe' and provider.is_dir() and not provider.is_symlink(),
                'unknown browser worker state needs scope review')
        for profile in sorted(provider.iterdir()):
            require(profile.is_dir() and not profile.is_symlink(), 'invalid browser profile')
            require(not (profile / '.lock').exists(), 'browser profile is active; retry backup after it closes')
            state = profile / 'state.json'
            if state.exists():
                require(not state.is_symlink(), 'linked browser state')
                paths.append(state)
            browser = profile / 'browser'
            if not browser.exists():
                continue
            require(not browser.is_symlink(), 'linked browser profile')
            # Local State contains OS-cookie encryption metadata. Local/Session
            # Storage and IndexedDB are session data even when filenames end .log.
            for relative in ('Local State', 'Default/Preferences', 'Default/Secure Preferences',
                             'Default/Cookies', 'Default/Cookies-journal',
                             'Default/Network/Cookies', 'Default/Network/Cookies-journal',
                             'Default/Web Data', 'Default/Web Data-journal',
                             'Default/Local Storage', 'Default/Session Storage', 'Default/IndexedDB'):
                target = browser / relative
                require(not target.is_symlink(), 'linked browser authentication state')
                if not target.exists():
                    continue
                children = sorted(target.rglob('*')) if target.is_dir() else [target]
                for child in children:
                    if child.name in ('LOCK', 'lockfile') or child.name.startswith('Singleton'):
                        continue
                    require(not child.is_symlink(), 'linked browser authentication state')
                    if not child.is_dir():
                        paths.append(child)
    return paths


def source_paths(root):
    require(not (root / 'data/cfm-maintenance').exists(), 'CFM is under maintenance')
    paths = []
    for name in TOP:
        path = root / name
        require(path.exists() and not path.is_symlink(), 'required config missing or linked')
        if path.is_dir():
            for child in sorted(path.rglob('*')):
                require(not child.is_symlink(), 'symlink in config tree')
                if not child.is_dir():
                    paths.append(child)
        else:
            paths.append(path)
    for path in sorted((root / 'data').iterdir()):
        if path.name in EXCLUDED:
            continue
        if path.name == 'browser-worker':
            paths.extend(browser_profile_paths(path))
            continue
        require(not path.is_symlink(), 'symlink in business tree')
        if path.is_dir():
            require(path.name in DATA_DIRS, 'new business directory needs scope review')
            for child in sorted(path.rglob('*')):
                require(not child.is_symlink(), 'symlink in business tree')
                if not child.is_dir():
                    require(child.name.endswith(('.json', '.json.enc')), 'unexpected task file format')
                    paths.append(child)
        else:
            require(path.name.endswith(('.json', '.json.enc')), 'new business file needs scope review')
            paths.append(path)
    require(len(paths) <= MAX_FILES, 'source file count exceeds limit')
    return sorted(paths)


def capture(root):
    contents, manifest, stamps = {}, {}, {}
    total = 0
    for path in source_paths(root):
        before = signature(path)
        raw = path.read_bytes()
        require(signature(path) == before, 'source changed during read')
        relative = str(path.relative_to(root))
        if path.suffix == '.json':
            json.loads(raw)
        total += len(raw)
        require(total < MAX_BYTES, 'source total exceeds limit')
        contents[relative] = raw
        stamps[relative] = before
        manifest[relative] = {'bytes': len(raw), 'sha256': digest(raw),
                              'mode': stat.S_IMODE(before[5]), 'uid': before[6], 'gid': before[7]}
    return contents, manifest, stamps


def pg_command(command, data=None, container=PG):
    return run(['docker', 'exec', '-i', container, 'sh', '-c', command], data=data)


def pg_guard(container=PG):
    names = pg_command('exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"',
        b"SELECT json_agg(tablename ORDER BY tablename) FROM pg_tables WHERE schemaname='public';", container)
    tables = json.loads(names)
    require(isinstance(tables, list) and 0 < len(tables) <= 100, 'unexpected PostgreSQL table inventory')
    queries = []
    for name in tables:
        identifier = '"' + name.replace('"', '""') + '"'
        literal = "'" + name.replace("'", "''") + "'"
        queries.append("SELECT " + literal + ",count(*),md5(coalesce(string_agg(to_jsonb(t)::text,E'\\n' ORDER BY to_jsonb(t)::text),'')) FROM public." + identifier + ' t')
    rows = pg_command('exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"',
                      ('BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;\n' + ';\n'.join(queries) + ';\nCOMMIT;').encode(), container)
    return {'tables': tables, 'rows_sha256': digest(rows)}


def redis_info(section):
    raw = run(['docker', 'exec', REDIS, 'redis-cli', '--raw', 'INFO', section]).decode()
    return dict(line.split(':', 1) for line in raw.splitlines() if ':' in line and not line.startswith('#'))


def redis_snapshot():
    require(int(redis_info('memory')['used_memory']) < 16 * 1024 ** 2, 'Redis exceeds reviewed memory bound')
    previous = redis_info('persistence')
    require(previous['rdb_bgsave_in_progress'] == '0' and previous['aof_rewrite_in_progress'] == '0',
            'Redis persistence is already busy')
    response = run(['docker', 'exec', REDIS, 'redis-cli', '--raw', 'BGSAVE'])
    require(b'Background saving started' in response, 'Redis did not accept BGSAVE')
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        info = redis_info('persistence')
        if info['rdb_bgsave_in_progress'] == '0':
            require(info['rdb_last_bgsave_status'] == 'ok' and
                    int(info['rdb_saves']) > int(previous['rdb_saves']), 'Redis snapshot failed')
            break
        time.sleep(.25)
    else:
        raise BackupError('Redis BGSAVE exceeded 20 seconds')
    path = SOURCE / 'redis-data/dump.rdb'
    before = signature(path)
    raw = path.read_bytes()
    run(['docker', 'exec', REDIS, 'redis-check-rdb', '/data/dump.rdb'])
    require(signature(path) == before and raw.startswith(b'REDIS'), 'Redis snapshot changed or invalid')
    return raw


def snapshot():
    # The full before/after checks bracket the Redis fork and the SQL snapshot.
    # They reject a mixed persisted-state copy without pausing production work.
    for attempt in range(1, 4):
        started = now()
        contents, manifest, stamps = capture(SOURCE)
        before_db = pg_guard()
        pg_dump = pg_command('exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc')
        globals_dump = pg_command('exec pg_dumpall -U "$POSTGRES_USER" --globals-only')
        rdb = redis_snapshot()
        after_db = pg_guard()
        _, after_manifest, after_stamps = capture(SOURCE)
        if stamps == after_stamps and manifest == after_manifest and before_db == after_db:
            require(pg_dump.startswith(b'PGDMP'), 'invalid PostgreSQL dump header')
            pg_command('exec pg_restore --file=/dev/null', pg_dump)
            contents.update({'database/postgres.dump': pg_dump,
                             'database/postgres-globals.sql': globals_dump,
                             'database/redis.rdb': rdb})
            for name in ('database/postgres.dump', 'database/postgres-globals.sql', 'database/redis.rdb'):
                manifest[name] = {'bytes': len(contents[name]), 'sha256': digest(contents[name]),
                                  'mode': 0o600, 'uid': 0, 'gid': 0}
            require(sum(len(raw) for raw in contents.values()) < MAX_BYTES, 'snapshot exceeds total limit')
            receipt = {'schema': 1, 'started_at': started, 'finished_at': now(), 'attempt': attempt,
                       'source_commit': json.loads(contents['current.json'])['commit'],
                       'files': manifest, 'postgres': before_db,
                       'consistency': 'persisted files and SQL rows stable across collection; Redis point-in-time RDB',
                       'excluded': sorted(EXCLUDED), 'media_included': False,
                       'restore_policy': 'Restore offline. Inspect pending external tasks before enabling workers.'}
            contents['manifest.json'] = json.dumps(receipt, indent=2).encode()
            return contents, receipt
        emit('collection_changed', attempt=attempt)
        time.sleep(2)
    raise BackupError('data changed during all three collection attempts; no upload')


def write_stage(contents, manifest):
    stage = ROOT / 'cfm-core'
    require(not stage.exists(), 'previous snapshot exists; reconcile before retrying')
    stage.mkdir(mode=0o700)
    for relative, raw in contents.items():
        path = stage / relative
        require(not Path(relative).is_absolute() and '..' not in Path(relative).parts, 'unsafe stage path')
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        with path.open('xb') as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
        item = manifest['files'].get(relative, {'mode': 0o600, 'uid': 0, 'gid': 0})
        os.chmod(path, item['mode'])
        os.chown(path, item['uid'], item['gid'])
    return stage


def verify_tar(raw, contents, receipt):
    require(len(raw) < MAX_BYTES, 'tar exceeds bound')
    recovered = {}
    with tarfile.open(fileobj=io.BytesIO(raw), mode='r:') as archive:
        for member in archive:
            path = Path(member.name)
            require(not path.is_absolute() and '..' not in path.parts, 'unsafe archive member')
            if member.isdir():
                continue
            require(member.isfile() and path.parts[0] == 'cfm-core', 'unexpected archive member type/root')
            name = str(Path(*path.parts[1:]))
            require(name in contents and name not in recovered and member.size <= MAX_BYTES, 'unexpected or duplicate member')
            content = archive.extractfile(member).read()
            require(content == contents[name], 'cloud content does not match staged snapshot')
            item = receipt['files'].get(name, {'mode': 0o600, 'uid': 0, 'gid': 0})
            require((member.mode, member.uid, member.gid) == (item['mode'], item['uid'], item['gid']),
                    'cloud file permissions/ownership differ')
            recovered[name] = content
    require(set(recovered) == set(contents), 'cloud archive is missing files')
    return recovered


def verify_cloud(backup, secret, contents, receipt):
    token, prefix, quota = cloud_session()
    path = posixpath.join(prefix.strip('/'), backup['file_dir'], backup['file_name'])
    meta = cloud_file(token, path)
    require(int(meta['size']) <= MAX_BYTES, 'cloud backup exceeds bound')
    encrypted = google('https://www.googleapis.com/drive/v3/files/' + meta['id'] + '?alt=media', token)
    require(encrypted.startswith(b'Salted__') and len(encrypted) == int(meta['size']) and
            hashlib.md5(encrypted).hexdigest() == meta['md5Checksum'], 'encrypted cloud checksum mismatch')
    env = dict(os.environ, OGG_CFM_SECRET=secret)
    decrypted = subprocess.run(['openssl', 'enc', '-d', '-aes-256-cbc', '-salt', '-pass', 'env:OGG_CFM_SECRET'],
                               input=encrypted, capture_output=True, env=env, timeout=30)
    require(decrypted.returncode == 0, 'cloud decryption failed')
    with gzip.GzipFile(fileobj=io.BytesIO(decrypted.stdout)) as archive:
        plain = archive.read(MAX_BYTES + 1)
        require(len(plain) <= MAX_BYTES and not archive.read(1), 'expanded archive exceeds bound')
    recovered = verify_tar(plain, contents, receipt)
    pg_command('exec pg_restore --file=/dev/null', recovered['database/postgres.dump'])
    return {'cloud_bytes': len(encrypted), 'fourteen_copies_estimate_bytes': len(encrypted) * 14,
            'cloud_path': path, 'sha256': digest(encrypted), 'cloud_verified': True,
            'file_count': len(receipt['files']), 'quota_after': quota}


def container_identity():
    ids = run(['docker', 'ps', '-aq']).decode().split()
    rows = json.loads(run(['docker', 'inspect', *ids]))
    return {x['Name']: {'id': x['Id'], 'image': x['Image'], 'started': x['State']['StartedAt'],
                       'status': x['State']['Status'], 'restarts': x['RestartCount'],
                       'health': x['State'].get('Health', {}).get('Status')}
            for x in rows}


def rehearse(contents, receipt):
    created = []
    suffix = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%d%H%M%S')
    try:
        pg_image = run(['docker', 'inspect', PG, '--format', '{{.Image}}']).decode().strip()
        redis_image = run(['docker', 'inspect', REDIS, '--format', '{{.Image}}']).decode().strip()
        pg_id = run(['docker', 'create', '--name', 'ogg-cfm-restore-pg-' + suffix,
            '--network', 'none', '--read-only', '--memory', '384m', '--cpus', '0.5', '--pids-limit', '64',
            '--tmpfs', '/var/lib/postgresql/data:rw,nosuid,size=128m',
            '--tmpfs', '/var/run/postgresql:rw,nosuid,size=8m', '--tmpfs', '/tmp:rw,nosuid,size=16m',
            '-e', 'POSTGRES_USER=postgres', '-e', 'POSTGRES_DB=restorecheck',
            '-e', 'POSTGRES_HOST_AUTH_METHOD=trust', pg_image,
            'postgres', '-c', 'shared_buffers=16MB', '-c', 'max_connections=10']).decode().strip()
        created.append(pg_id)
        run(['docker', 'start', pg_id])
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            check = subprocess.run(['docker', 'exec', pg_id, 'pg_isready', '-U', 'postgres', '-d', 'restorecheck'],
                                   capture_output=True, timeout=5)
            if check.returncode == 0:
                # pg_isready also sees the entrypoint's temporary init server.
                ready = run(['docker', 'exec', pg_id, 'sh', '-c', 'tr "\\0" " " </proc/1/cmdline'])
                if ready.startswith(b'postgres '):
                    break
            time.sleep(1)
        else:
            raise BackupError('isolated PostgreSQL did not become ready')
        run(['docker', 'exec', '-i', pg_id, 'pg_restore', '-U', 'postgres', '-d', 'restorecheck',
             '--exit-on-error', '--no-owner', '--no-privileges'], data=contents['database/postgres.dump'], timeout=60)
        require(pg_guard(pg_id) == receipt['postgres'], 'isolated PostgreSQL rows differ from source snapshot')
        redis_id = run(['docker', 'create', '--name', 'ogg-cfm-restore-redis-' + suffix,
            '--network', 'none', '--read-only', '--memory', '64m', '--cpus', '0.25', '--pids-limit', '32',
            '--tmpfs', '/data:rw,nosuid,size=32m', '--entrypoint', 'sh', redis_image, '-c',
            'while [ ! -f /data/ready ]; do sleep 0.1; done; exec redis-server --port 0 --unixsocket /data/redis.sock --save "" --appendonly no --dir /data']).decode().strip()
        created.append(redis_id)
        run(['docker', 'start', redis_id])
        run(['docker', 'exec', '-i', redis_id, 'sh', '-c',
             'umask 077; cat > /data/dump.rdb; redis-check-rdb /data/dump.rdb >/data/check.log && touch /data/ready'],
            data=contents['database/redis.rdb'])
        checksum = run(['docker', 'exec', redis_id, 'sha256sum', '/data/dump.rdb']).decode().split()[0]
        require(checksum == digest(contents['database/redis.rdb']), 'isolated Redis copy digest mismatch')
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            check = subprocess.run(['docker', 'exec', redis_id, 'redis-cli', '-s', '/data/redis.sock', 'PING'],
                                   capture_output=True, timeout=5)
            if check.returncode == 0 and check.stdout.strip() == b'PONG':
                break
            time.sleep(.5)
        else:
            raise BackupError('isolated Redis did not load RDB')
        info = run(['docker', 'exec', redis_id, 'redis-cli', '-s', '/data/redis.sock', 'INFO', 'keyspace']).decode()
        counts = [int(line.split('keys=', 1)[1].split(',', 1)[0]) for line in info.splitlines() if line.startswith('db')]
        return {'postgres_restored': True, 'postgres_tables': len(receipt['postgres']['tables']),
                'postgres_rows_match': True, 'redis_restored': True, 'redis_loaded_keys': sum(counts),
                'network': 'none', 'production_mounts': False}
    finally:
        for container in reversed(created):
            run(['docker', 'rm', '-f', container])


def wait_backup(job_id, previous_id):
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        with database() as db:
            record = db.execute('SELECT id,status FROM job_records WHERE cronjob_id=? AND id>? ORDER BY id DESC LIMIT 1',
                                (job_id, previous_id)).fetchone()
        if record and record['status'] == 'Success':
            with database() as db:
                backup = db.execute("SELECT * FROM backup_records WHERE cronjob_id=? AND status='Success' ORDER BY id DESC LIMIT 1", (job_id,)).fetchone()
            require(backup is not None, 'successful upload has no backup record')
            return dict(record), dict(backup)
        if record and record['status'] in ('Failed', 'Error', 'Cancel', 'Canceled'):
            raise BackupError('native upload task failed; retain staged snapshot')
        time.sleep(2)
    raise BackupError('native upload result unknown; retain state and do not retry')


def main():
    os.umask(0o077)
    require(os.geteuid() == 0, 'must run as root via 1Panel')
    config = json.loads((ROOT / 'settings.json').read_text())
    with (ROOT / 'backup.lock').open('a') as own_lock, (SOURCE / 'maintenance.lock').open('r+') as service_lock:
        fcntl.flock(own_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.flock(service_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        require(not (ROOT / 'pending.json').exists() and not (ROOT / 'cfm-core').exists(),
                'unfinished backup exists; reconcile before starting another')
        require(shutil.disk_usage(ROOT).free > 2 * 1024 ** 3, 'local disk headroom below 2 GiB')
        with database() as db:
            upload = dict(db.execute('SELECT * FROM cronjobs WHERE id=?', (config['upload_job_id'],)).fetchone())
            old_id = db.execute('SELECT coalesce(max(id),0) FROM job_records WHERE cronjob_id=?', (upload['id'],)).fetchone()[0]
        require(upload['name'] == UPLOAD_NAME and upload['status'] == 'Disable' and not upload['is_executing']
                and upload['source_dir'] == str(ROOT / 'cfm-core') and upload['source_account_ids'] == '2'
                and upload['download_account_id'] == 2 and upload['retain_copies'] == 14
                and len(upload['secret']) == 64, 'native upload configuration changed')
        cloud_session()
        containers = container_identity()
        contents, receipt = snapshot()
        write_stage(contents, receipt)
        pending = {'phase': 'staged', 'at': now(), 'upload_job_id': upload['id'], 'previous_record_id': old_id,
                   'source_commit': receipt['source_commit']}
        atomic_json(ROOT / 'pending.json', pending)
        emit('snapshot_ready', files=len(receipt['files']), bytes=sum(len(x) for x in contents.values()), attempt=receipt['attempt'])
        # A failed/unknown POST is never repeated automatically.
        pending['phase'] = 'upload_requested'
        atomic_json(ROOT / 'pending.json', pending)
        api('/cronjobs/handle', {'id': upload['id']})
        record, backup = wait_backup(upload['id'], old_id)
        result = verify_cloud(backup, upload['secret'], contents, receipt)
        if config.get('initial_rehearsal_required'):
            result['restore_rehearsal'] = rehearse(contents, receipt)
        require(container_identity() == containers, 'container identity/state changed during backup')
        result.update(status='success', completed_at=now(), source_commit=receipt['source_commit'],
                      job_record_id=record['id'], backup_record_id=backup['id'],
                      snapshot_attempt=receipt['attempt'], containers_unchanged=len(containers),
                      postgres_guard=receipt['postgres'], schedule_shanghai='03:50 daily', retain_copies=14,
                      media_included=False, snapshot_manifest_sha256=digest(contents['manifest.json']))
        atomic_json(ROOT / 'last-success.json', result)
        atomic_json(ROOT / 'last-manifest.json', receipt)
        config['initial_rehearsal_required'] = False
        atomic_json(ROOT / 'settings.json', config)
        # Only this verified staging directory is removed. Cloud rotation is owned by 1Panel.
        shutil.rmtree(ROOT / 'cfm-core')
        (ROOT / 'pending.json').unlink()
        emit('completed', **result)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()
    try:
        main()
    except Exception as exc:
        emit('failed', error_type=type(exc).__name__,
             reason=str(exc) if isinstance(exc, BackupError) else 'private exception detail withheld')
        raise SystemExit(1)
