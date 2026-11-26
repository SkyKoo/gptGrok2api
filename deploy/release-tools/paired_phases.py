"""Small phase boundaries shared by paired app/Worker release tools.

The caller owns the existing durable release journal and its maintenance lock.
Archive callbacks operate only on the sealed private backup. They must verify
existing output on retry; no callback here may apply or roll back a release.
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import math
from pathlib import Path
import re
import time
from typing import Callable, Mapping


class PhaseError(RuntimeError):
    """A phase lacks the exact identity or completion proof it requires."""


class ReleaseInputError(RuntimeError):
    """Public diagnostic containing only fixed package names or SHA-256 values."""


def checked_file(path):
    if path.is_symlink() or not path.is_file():
        raise ReleaseInputError(f'Missing or non-regular release input: {path.name}')
    return path.read_bytes()


def verify_rolling_helper(root, expected):
    if not isinstance(expected, str) or not re.fullmatch('[0-9a-f]{64}', expected):
        raise ReleaseInputError('Plan must pin a full rolling helper SHA-256')
    path = Path(root) / 'tools/rolling-deploy.py'
    actual = hashlib.sha256(checked_file(path)).hexdigest()
    if actual != expected:
        raise ReleaseInputError(f'Rolling helper SHA-256 differs: installed {actual}, '
                                f'reviewed {expected}')
    return path


def verified_bundle_hashes(directory, plan, names):
    directory = Path(directory)
    manifest_name = 'bundle-manifest.json'
    raw = checked_file(directory / manifest_name)
    manifest = json.loads(raw)
    if (manifest.get('format') != 1 or manifest.get('operation') != plan['operation']
            or manifest.get('candidate_commit') != plan['candidate_commit']
            or set(manifest.get('files', {})) != set(names)):
        raise ReleaseInputError('Release bundle manifest differs from the reviewed plan or file list')
    hashes = {manifest_name: hashlib.sha256(raw).hexdigest()}
    for name in sorted(names):
        hashes[name] = hashlib.sha256(checked_file(directory / name)).hexdigest()
        if hashes[name] != manifest['files'][name]:
            raise ReleaseInputError(f'Release bundle SHA-256 differs: {name}')
    return hashes


@dataclass(frozen=True)
class ReleaseContext:
    commit: str
    image_id: str
    worker_image_id: str
    backup: str

    def verify(self, state: Mapping):
        for name in ('commit', 'image_id', 'worker_image_id', 'backup'):
            expected = getattr(self, name)
            if not isinstance(expected, str) or not expected or state.get(name) != expected:
                raise PhaseError('Release finalization identity differs: ' + name)


def seal_raw_snapshot(seal: Callable, record: Callable):
    """Record the raw backup only after its exact checks and fsync have returned."""
    seal()
    record('frozen_backup_sealed', raw_backup_sealed=True)


def finish_after_release(context: ReleaseContext, state: Mapping, *, record: Callable,
                         verify_live: Callable, finish_archive: Callable):
    """Finish or resume a sealed backup after confirmed traffic/writer handoff.

The callbacks never receive an apply or rollback capability. A lost reply from
release/resume is not enough to enter this function: the journal must already
contain all confirmed milestones, and the live identity is checked twice.
"""
    context.verify(state)
    if state.get('status') not in ('archive_pending', 'success'):
        raise PhaseError('Only a confirmed released application may finalize its backup')
    for name in ('raw_backup_sealed', 'traffic_switched', 'writer_resumed', 'old_writer_stopped'):
        if state.get(name) is not True:
            raise PhaseError('Release finalization milestone is unconfirmed: ' + name)
    if state.get('status') == 'success' and state.get('final_backup_verified') is not True:
        raise PhaseError('Successful release has no final backup proof')
    # Revalidate live state before touching an earlier checkpoint or its output.
    verify_live()
    record('archive_pending', status='archive_pending', final_backup_verified=False,
           backup_finalization_error=None)
    try:
        finish_archive()
        verify_live()
    except BaseException as error:
        # Do not store exception text: underlying tools may include credentials.
        record('archive_pending', status='archive_pending', final_backup_verified=False,
               backup_finalization_error=type(error).__name__)
        raise
    record('success', status='success', final_backup_verified=True,
           state_unchanged_verified=True, backup_finalization_error=None)


def wait_stable_health(inspect: Callable, expected: Mapping, summarize: Callable, *,
                       timeout: float = 45, interval: float = 0.5,
                       monotonic: Callable = time.monotonic, sleep: Callable = time.sleep):
    """Allow only health to converge; reject identity/config drift on every poll.

``expected`` is the normal exact container summary with a healthy baseline.
Process exit, OOM, replacement, restart or config changes fail immediately.
Callers must use a bounded inspection operation as well as this polling bound.
"""
    if (not math.isfinite(timeout) or not math.isfinite(interval)
            or timeout <= 0 or interval <= 0):
        raise PhaseError('Health wait budgets must be positive and finite')
    required = {'id', 'image_id', 'restarts', 'running', 'health', 'oom'}
    if (not required <= set(expected) or expected['health'] != 'healthy'
            or expected['running'] is not True or expected['oom'] is not False):
        raise PhaseError('Health wait requires a healthy pinned container baseline')
    fixed = {key: value for key, value in expected.items() if key != 'health'}
    deadline = monotonic() + timeout
    while True:
        info = inspect()
        actual = summarize(info)
        if set(actual) != set(expected) or {key: value for key, value in actual.items()
                                           if key != 'health'} != fixed:
            raise PhaseError('Container identity or configuration changed during health wait')
        if actual['health'] == 'healthy':
            if monotonic() > deadline:
                raise PhaseError('Container health observation exceeded its deadline')
            return info
        if actual['health'] not in ('starting', 'unhealthy'):
            raise PhaseError('Container has no supported health observation')
        remaining = deadline - monotonic()
        if remaining <= 0:
            raise PhaseError('Container health did not recover before its deadline')
        sleep(min(interval, remaining))
