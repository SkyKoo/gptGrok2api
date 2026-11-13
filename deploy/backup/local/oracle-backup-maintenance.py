#!/usr/bin/env python3
"""Coordinate local recovery points and bounded retention on oracle-a1.

Default: generate a read-only catalog and plan. --execute creates a missing daily
point after 05:20 Asia/Shanghai and executes a fresh, validated retention plan.
Unchanged timer runs only inspect small completion/status metadata. No cloud job,
application version, production data or image is changed by this coordinator.
"""
import argparse
import contextlib
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
from zoneinfo import ZoneInfo

CODE = Path(__file__).resolve().parent
BASE = Path('/opt/gptgrok2api/backup-policy')
SERVICES = ('gptgrok2api',)
TZ = ZoneInfo('Asia/Shanghai')


def atomic_json(path, value):
    temporary = path.with_name(path.name + '.new')
    with temporary.open('x') as stream:
        os.chmod(temporary, 0o600)
        json.dump(value, stream, ensure_ascii=False, sort_keys=True, indent=2)
        stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
    os.replace(temporary, path)


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def completion_signal():
    """No reads of credentials, media, database contents or large archives."""
    values = []
    for service in SERVICES:
        root = Path('/opt') / service
        paths = [root / 'current.json', root / 'rolling-status.json']
        for directory in sorted((root / 'backups').iterdir()):
            paths.append(directory)
            if directory.is_dir() and not directory.is_symlink():
                paths.extend(directory / name for name in (
                    'complete', 'release.json', 'recovery-point.json', 'checksums.json'))
        paths.extend((root / 'operations').glob('*/release-state.json'))
        for path in paths:
            if path.exists():
                item = path.lstat()
                values.append([str(path), item.st_ino, item.st_mode, item.st_size,
                               item.st_mtime_ns, item.st_ctime_ns])
    return hashlib.sha256(json.dumps(values, sort_keys=True).encode()).hexdigest()


def covered_today(catalog, service, today):
    for record in catalog['services'][service]['backups']:
        if record.get('completed') is True and record.get('success') is True:
            if record.get('kind') not in ('daily', 'release') or not record.get('proofs'):
                continue
            date = dt.datetime.fromisoformat(record['created_at'].replace('Z', '+00:00'))
            if date.tzinfo is None:
                raise ValueError('Recovery timestamp lacks timezone')
            if date.astimezone(TZ).date() == today:
                return True
    return False


def command(script, arguments, logfile, timeout=7200):
    with logfile.open('x') as output:
        result = subprocess.run([sys.executable, '-B', str(CODE / script), *map(str, arguments)],
                                stdout=output, stderr=subprocess.STDOUT, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f'{script} failed ({result.returncode}); evidence: {logfile.name}')


def make_catalog(destination, logfile):
    arguments = ['--service', 'gptgrok2api', '--output', destination]
    if (BASE / 'catalog.json').exists():
        arguments += ['--previous', BASE / 'catalog.json']
    if (BASE / 'attestations.json').exists():
        arguments += ['--attestations', BASE / 'attestations.json']
    command('backup_catalog.py', arguments, logfile)
    return json.loads(destination.read_text())


def run(execute=False, force=False):
    now = dt.datetime.now(TZ)
    state_path = BASE / 'state.json'
    state = json.loads(state_path.read_text()) if state_path.exists() else {}
    signal = completion_signal()
    due = now.time().replace(tzinfo=None) >= dt.time(5, 20)
    same_day = state.get('date') == now.date().isoformat()
    if execute and not force and same_day and state.get('signal') == signal:
        if not due or all(state.get('covered', {}).get(s) for s in SERVICES):
            return {'event': 'unchanged', 'checked_at': now.isoformat()}
    run_dir = BASE / 'runs' / now.strftime('%Y%m%dT%H%M%S%f')
    run_dir.mkdir(parents=True, mode=0o700)
    catalog_path = run_dir / 'catalog-before.json'
    catalog = make_catalog(catalog_path, run_dir / 'catalog-before.log')
    pending = {s: cfg['blocked_reason'] for s, cfg in catalog['services'].items() if cfg.get('blocked_reason')}
    if pending:
        return {'event': 'publication_pending', 'services': pending, 'run': str(run_dir)}
    if execute and due:
        for service in SERVICES:
            if not covered_today(catalog, service, now.date()):
                command('oracle-backup-daily.py', ['--dependency-config', CODE / 'dependencies.json', '--execute'],
                        run_dir / (service + '-daily.log'))
        catalog_path = run_dir / 'catalog-ready.json'
        catalog = make_catalog(catalog_path, run_dir / 'catalog-ready.log')
        if any(cfg.get('blocked_reason') for cfg in catalog['services'].values()):
            return {'event': 'publication_pending', 'run': str(run_dir)}
        if not all(covered_today(catalog, service, now.date()) for service in SERVICES):
            raise RuntimeError('Daily coverage remains incomplete; retention refused')
    plan_path = run_dir / 'plan.json'
    command('backup_retention.py', ['plan', '--catalog', catalog_path, '--output', plan_path],
            run_dir / 'plan.log')
    plan = json.loads(plan_path.read_text())
    selected = {service: [row['name'] for row in value['rows'] if row['action'] == 'delete']
                for service, value in plan['services'].items()}
    if not execute:
        return {'event': 'plan_only', 'run': str(run_dir), 'selected': selected}
    command('backup_retention.py', ['execute', '--catalog', catalog_path, '--plan', plan_path,
            '--expect-plan-sha256', sha(plan_path), '--audit', run_dir / 'execution.jsonl'],
            run_dir / 'execute.log')
    final = make_catalog(run_dir / 'catalog-after.json', run_dir / 'catalog-after.log')
    atomic_json(BASE / 'catalog.json', final)
    result = {'event': 'maintenance_complete', 'date': now.date().isoformat(),
              'finished_at': dt.datetime.now(TZ).isoformat(), 'signal': completion_signal(),
              'covered': {s: covered_today(final, s, now.date()) for s in SERVICES},
              'run': str(run_dir), 'removed': selected}
    atomic_json(state_path, result)
    return result


def verify_package():
    manifest = json.loads((CODE / 'manifest.json').read_text())
    expected = {'backup_catalog.py', 'backup_retention.py', 'oracle-backup-daily.py', 'oracle-backup-maintenance.py'}
    if manifest.get('format') != 'ogg-backup-local-package-v1' or manifest.get('service') != SERVICES[0] or set(manifest.get('files', {})) != expected:
        raise RuntimeError('Invalid project backup package manifest')
    for name, expected_sha in manifest['files'].items():
        path = CODE / name
        if path.is_symlink() or sha(path) != expected_sha:
            raise RuntimeError('Project backup package checksum mismatch')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--execute', action='store_true')
    parser.add_argument('--force-check', action='store_true', help='recheck even if no completion metadata changed')
    args = parser.parse_args()
    if os.geteuid() != 0 or BASE.is_symlink() or not BASE.is_dir():
        raise RuntimeError('Run from the installed root-owned policy directory')
    verify_package()
    os.umask(0o077)
    with contextlib.ExitStack() as locks:
        for path in (Path('/run/lock/ogg-backup-maintenance.lock'), BASE / 'maintenance.lock'):
            lock = locks.enter_context(path.open('a'))
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                print(json.dumps({'event': 'already_running'})); return
        print(json.dumps(run(args.execute, args.force_check), ensure_ascii=False))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'event': 'maintenance_failed', 'error': str(error)}), file=sys.stderr)
        raise SystemExit(1)
