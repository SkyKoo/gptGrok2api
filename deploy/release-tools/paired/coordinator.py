#!/usr/bin/env python3
"""Serialize apply and final verification with both installed backup coordinators.

The application release owns maintenance.lock internally. This wrapper holds only
its outer backup locks, so it cannot recursively deadlock the application lock.
A successful apply does not admit retention until every final verification passes.
"""
import argparse
from contextlib import contextmanager, ExitStack
from concurrent.futures import ThreadPoolExecutor
import fcntl
import json
import os
from pathlib import Path
import stat
import subprocess
import time
import uuid

import common

@contextmanager
def exclusive(path):
    fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW, 0o600)
    try:
        common.require(stat.S_ISREG(os.fstat(fd).st_mode), 'Coordinator lock is not a regular file')
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        os.close(fd)


def run(op, name, arguments, logfile, timeout=3600, *, continuation=False):
    # Full diagnostics stay in a private operation file; subprocess errors can
    # contain parsed configuration, so only the helper name leaves this wrapper.
    phase = Path(logfile).stem
    started = time.monotonic()
    bounded = (getattr(common, 'bounded_finalization_timeout', None)
               if continuation else getattr(common, 'bounded_timeout', None))
    if bounded:
        timeout = bounded(op, timeout)
        # release.py still clears its internal deadline before restoring a held
        # ingress or writer. The coordinator does not create a second window.
    timing = getattr(common, 'timing_event', None)
    if timing:
        timing(op, phase, 'started', timeout_seconds=timeout)
    try:
        with (op / logfile).open('ab') as log:
            result = subprocess.run(['python3', str(op / name), *map(str, arguments)],
                                    stdout=log, stderr=subprocess.STDOUT, timeout=timeout)
        common.require(result.returncode == 0, 'Release helper stopped: ' + name)
    except BaseException as error:
        if timing:
            timing(op, phase, 'failed', elapsed_seconds=time.monotonic() - started,
                   error_type=type(error).__name__)
        raise
    else:
        if timing:
            timing(op, phase, 'finished', elapsed_seconds=time.monotonic() - started)


def evidence_path(op, name):
    common.require(isinstance(name, str) and Path(name).name == name and name not in ('', '.', '..'),
                   'Final evidence filename is invalid')
    path = op / name
    common.require(path.is_file() and not path.is_symlink(), 'Final evidence file is missing or linked')
    return path


def next_evidence_path(op, name):
    """Keep earlier failures and successful checkpoints; never overwrite them."""
    target = op / name
    if not target.exists():
        return target
    return op / (target.stem + '-' + uuid.uuid4().hex + target.suffix)


def admit_retention(root, op, baseline, verification_path=None, other_services_path=None,
                    *, budget_check=None):
    r = common.rolling(root)
    host = r.Host()
    if budget_check:
        host.overall_deadline = time.monotonic() + budget_check(op)
    state, current = common.load(op / 'release-state.json'), common.load(root / 'current.json')
    backup = Path(state['backup'])
    receipt = common.load(backup / 'release.json')
    verification_path = evidence_path(op, (verification_path or op / 'verification.json').name)
    other_services_path = evidence_path(op, (other_services_path or op / 'other-services-after.json').name)
    verification = common.load(verification_path)
    others = common.load(other_services_path)
    common.require(state.get('status') == 'success' and state.get('phase') == 'success'
                   and state.get('final_backup_verified') is True
                   and state.get('commit') == common.COMMIT
                   and state.get('image_id') == baseline['candidate']['image_id']
                   and state.get('worker_image_id') == baseline['worker']['image_id']
                   and current.get('status') == 'healthy' and current.get('commit') == common.COMMIT
                   and current.get('image_id') == state['image_id']
                   and current.get('container') == state.get('candidate')
                   and current.get('frontdoor') == baseline['current']['frontdoor']
                   and current.get('slot') == state.get('candidate_slot')
                   and current.get('upstream') == r.SLOTS[state['candidate_slot']][1]
                   and current.get('backup') == str(backup)
                   and backup.parent == root / 'backups' and backup.resolve() == backup,
                   'Final verification has no matching healthy release')
    common.require(receipt.get('status') == 'success' and receipt.get('phase') == 'success'
                   and receipt.get('commit') == common.COMMIT and receipt.get('backup') == str(backup)
                   and receipt.get('retention_requires_final_verification') is True,
                   'Backup final-verification gate is missing')
    common.require(verification.get('verified') is True and verification.get('commit') == common.COMMIT
                   and verification.get('image_id') == current.get('image_id')
                   and verification.get('backup') == str(backup)
                   and others.get('verified') is True and others.get('current_commit') == common.COMMIT
                   and others.get('baseline_sha256') == common.file_sha(op / 'other-services-before.json'),
                   'Final public or other-service verification did not pass')
    common.require(common.helper_hashes(op) == baseline['helpers'], 'Frozen helpers changed')
    common.require(common.backup_policy_identity(root) == baseline['backup_policy'], 'Backup policy changed')
    control = host.control(current['container'])
    route = host.gateway(current['frontdoor'])['route']
    common.require(control.get('protocol') == common.PROTOCOL and control.get('phase') == 'active'
                   and control.get('writer_lease') is True and control.get('paused') is False
                   and route.get('held') is False and route.get('upstream') == current['upstream'],
                   'Final writer or ingress changed')
    for name in (current['container'], current['frontdoor'], common.WORKER, *common.DEPENDENCIES.values()):
        info = host.inspect(name)
        common.require(common.healthy(info), 'A final dependency is unhealthy')
        if name == current['container']:
            expected = state.get('candidate_summary')
            common.require(isinstance(expected, dict) and expected.get('id') == state.get('candidate_container_id')
                           and expected.get('image_id') == state['image_id']
                           and expected.get('restarts') == 0, 'Pinned app identity is missing')
        elif name == common.WORKER:
            expected = state.get('worker_summary')
            common.require(isinstance(expected, dict) and expected.get('id') == state.get('worker_container_id')
                           and expected.get('image_id') == state['worker_image_id']
                           and expected.get('restarts') == 0, 'Pinned Worker identity is missing')
        else:
            expected = baseline['containers'][name]
        # Health is an observation, not a container identity. A brief handoff
        # probe failure must recover to healthy, without pinning that failure
        # forever in the saved runtime summary. Every other field stays exact.
        expected = dict(expected, health='healthy')
        common.require(common.container_summary(info) == expected,
                       'Final container identity or configuration changed')
    retired_name = baseline['current']['container']
    retired = host.inspect(retired_name)
    common.require(retired['Id'] == baseline['containers'][retired_name]['id']
                   and retired['State']['Running'] is False, 'Retired writer changed or resumed')
    host.run(['docker', 'exec', current['container'], 'wget', '-q', '-T', '3', '-O', '/dev/null',
              'http://127.0.0.1:8080/readyz'], timeout=8)
    ready = {'status': 'verified', 'commit': common.COMMIT, 'backup': str(backup),
             'verified_at': r.now(), 'writer_verified': True, 'route_verified': True,
             'readyz_verified': True, 'dependencies_healthy': True,
             'verification_file': verification_path.name,
             'other_services_file': other_services_path.name,
             'verification_sha256': common.file_sha(verification_path),
             'other_services_sha256': common.file_sha(other_services_path)}
    target = backup / 'retention-ready.json'
    if target.exists():
        common.require(not target.is_symlink(), 'Final admission is linked')
        previous = common.load(target)
        common.require(set(previous) == set(ready)
                       and all(previous[key] == value for key, value in ready.items() if key != 'verified_at'),
                       'Existing final admission has a different identity or evidence')
        return previous
    r.atomic_json(target, ready)
    return ready


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('apply-and-verify', 'finalize'))
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--operation-dir', type=Path, default=Path(__file__).resolve().parent)
    parser.add_argument('--drain-timeout', type=int, default=600,
                        help='shared natural-drain deadline in seconds')
    parser.add_argument('--apply-grace', type=int, default=600,
                        help='post-drain apply/backup grace in seconds')
    args = parser.parse_args()
    common.host_required()
    budget_check = getattr(common, 'require_pipeline_budget', None)
    continuation = False
    state_path = args.operation_dir / 'release-state.json'
    if args.action == 'finalize' and state_path.is_file() and not state_path.is_symlink():
        state = common.load(state_path)
        continuation = state.get('status') in ('archive_pending', 'success')
        if continuation:
            ensure = getattr(common, 'ensure_finalization_budget', None)
            final_budget = getattr(common, 'require_finalization_budget', None)
            if ensure and final_budget:
                ensure(args.operation_dir)
                budget_check = final_budget
    if budget_check:
        budget_check(args.operation_dir)
    common.require(args.drain_timeout > 0 and args.apply_grace > 0
                   and args.drain_timeout + args.apply_grace <= 1200,
                   'Release deadlines must fit the 1200-second handoff budget')
    root, op = args.root.resolve(), args.operation_dir.resolve()
    timing = getattr(common, 'timing_event', None)
    summary = getattr(common, 'write_timing_summary', None)
    if timing:
        timing(op, 'coordinator', 'started', entrypoint=args.action)
    outcome = 'failed'
    try:
        baseline = common.load(op / 'baseline.json')
        common.require(common.helper_hashes(op) == baseline['helpers'], 'Frozen helpers changed')
        # This matches installed project coordinators: global -> project policy ->
        # release.py acquires the existing application maintenance lock itself.
        with ExitStack() as stack:
            stack.enter_context(exclusive(Path('/run/lock/ogg-backup-maintenance.lock')))
            stack.enter_context(exclusive(root / 'backup-policy/maintenance.lock'))
            common.require(common.backup_policy_identity(root) == baseline['backup_policy'], 'Backup policy changed')
            if args.action == 'apply-and-verify':
                common.require(not (op / 'release-state.json').exists(), 'Prior apply exists; inspect status before finalizing')
                if not (op / 'other-services-before.json').exists():
                    run(op, 'capture-services.py', ['--root', root, '--operation-dir', op], 'capture-services.log', 120)
                run(op, 'release.py', ['--root', root, '--operation-dir', op, 'apply',
                    '--release', op / 'release.json', '--worker-manifest', op / 'worker-manifest.json',
                    '--baseline', op / 'baseline.json', '--drain-timeout', args.drain_timeout],
                    'apply.log', args.drain_timeout + args.apply_grace)
            state = common.load(op / 'release-state.json')
            if args.action == 'finalize' and state.get('status') == 'archive_pending':
                run(op, 'release.py', ['--root', root, '--operation-dir', op, 'finish',
                    '--baseline', op / 'baseline.json', '--worker-manifest', op / 'worker-manifest.json'],
                    'finish.log', args.apply_grace, continuation=True)
                state = common.load(op / 'release-state.json')
            common.require(state.get('status') == 'success',
                           'Apply has not succeeded; only read-only status is permitted')
            backup = Path(state['backup'])
            common.require(backup.parent == root / 'backups' and backup.resolve() == backup
                           and not backup.is_symlink(), 'Finalization backup path changed')
            existing = backup / 'retention-ready.json'
            if existing.exists():
                prior = common.load(existing)
                ready = admit_retention(root, op, baseline,
                    evidence_path(op, prior.get('verification_file')),
                    evidence_path(op, prior.get('other_services_file')),
                    budget_check=budget_check)
                if budget_check:
                    budget_check(op)
                print(json.dumps({'verified': True, 'commit': ready['commit'], 'retention_admitted': True,
                                  'existing_admission_revalidated': True}))
                outcome = 'success'
                return
            verification_path = next_evidence_path(op, 'verification.json')
            other_services_path = next_evidence_path(op, 'other-services-after.json')
            # These probes are read-only and have disjoint evidence files. Running
            # them together removes a full helper round-trip from the tail without
            # changing either verification contract.
            with ThreadPoolExecutor(max_workers=2) as pool:
                futures = [pool.submit(run, op, 'verify.py',
                    ['--root', root, '--operation-dir', op, '--baseline', op / 'baseline.json',
                     '--worker-manifest', op / 'worker-manifest.json', '--output', verification_path],
                    'verify.log', args.apply_grace),
                    pool.submit(run, op, 'verify-other-services.py',
                    ['--root', root, '--before', op / 'other-services-before.json',
                     '--output', other_services_path], 'verify-other-services.log', 120)]
                for future in futures:
                    future.result()
            ready = admit_retention(root, op, baseline, verification_path, other_services_path,
                                    budget_check=budget_check)
            print(json.dumps({'verified': True, 'commit': ready['commit'], 'retention_admitted': True}))
            outcome = 'success'
    finally:
        if summary:
            summary(op, outcome)
    if budget_check:
        budget_check(args.operation_dir)


if __name__ == '__main__':
    common.safe_main(main)
