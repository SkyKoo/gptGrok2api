#!/usr/bin/env python3
"""Create or continuously execute a frozen app + Worker release bundle.

``init`` only creates a private operation directory. ``run`` executes all
remaining phases in one process and keeps the init timestamp as the shared
release deadline.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone

if __name__ == '__main__':
    sys.dont_write_bytecode = True  # Keep the installed tool directory immutable.
from paired_phases import checked_file, verified_bundle_hashes, verify_rolling_helper

HERE = Path(__file__).resolve().parent
TEMPLATES = ('common.py', 'release.py', 'preflight.py', 'verify.py',
             'state-guard.py', 'coordinator.py', 'worker-release.py',
             'build.py', 'build-worker.py', 'worker-image-probe.py',
             'snapshot.py', 'capture-services.py', 'verify-other-services.py')
MODULES = ('snapshot_integrity.py', 'paired_phases.py')


def require(value, message):
    if not value:
        raise ValueError(message)


PIPELINE_BUDGET_SECONDS = 1200


def pipeline_remaining(operation):
    """Read the init timestamp shared by every release subprocess."""
    operation = Path(operation)
    timing = operation / 'pipeline-events.jsonl'
    require(operation.is_dir() and not operation.is_symlink()
            and timing.is_file() and not timing.is_symlink(),
            'Release operation timing evidence is missing')
    try:
        record = json.loads(timing.read_text().splitlines()[0])
    except (OSError, IndexError, ValueError, json.JSONDecodeError) as error:
        raise ValueError('Release operation timing evidence is invalid') from error
    require(record.get('phase') == 'operation-created'
            and record.get('status') == 'started'
            and record.get('budget_seconds') == PIPELINE_BUDGET_SECONDS,
            'Release operation start record is invalid')
    started = record.get('epoch')
    require(type(started) in (int, float) and started > 0,
            'Release operation start time is invalid')
    remaining = PIPELINE_BUDGET_SECONDS - (time.time() - float(started))
    require(remaining > 0,
            'The 20-minute release budget is exhausted; preserve the operation evidence')
    return remaining


def bounded_subprocess_timeout(operation, requested):
    require(type(requested) in (int, float) and requested > 0,
            'Release subprocess timeout must be positive')
    return max(0.001, min(float(requested), pipeline_remaining(operation)))


def validate_plan(plan, *, require_backup_lock=True):
    for key in ('base_commit', 'candidate_commit'):
        require(isinstance(plan.get(key), str) and re.fullmatch('[0-9a-f]{40}', plan[key]),
                'Both commits must be full reviewed Git identities')
    require(plan.get('branch') == 'main', 'Only reviewed main releases are supported')
    require(plan.get('schema_migration') is False and plan.get('worker_change') == 'image_only',
            'Only no-migration, image-only Worker releases are supported')
    require(isinstance(plan.get('operation'), str) and
            re.fullmatch('cfm-[a-z0-9][a-z0-9-]{2,100}', plan['operation']), 'Invalid operation name')
    for key in ('rolling_sha256', 'backup_policy_package'):
        require(isinstance(plan.get(key), str) and re.fullmatch('[0-9a-f]{64}', plan[key]),
                'Pin the installed tool and backup package digests')
    for version in ('previous', 'candidate'):
        value = plan.get('compatible_contract', {}).get(version, {})
        require(set(value) == {'state', 'browser'} and all(
            isinstance(v, str) and re.fullmatch('[0-9a-f]{64}', v) for v in value.values()),
            'Pin both complete compatibility contracts')
    if require_backup_lock or 'backup_maintenance_lock' in plan:
        lock = plan.get('backup_maintenance_lock')
        require(isinstance(lock, str) and Path(lock).is_absolute()
                and Path(lock) != Path('/') and '..' not in Path(lock).parts
                and not any(character in lock for character in '\0\n\r'),
                'Pin the absolute host backup maintenance lock path')
    inputs = plan.get('state_contract_inputs')
    require(isinstance(inputs, list) and inputs and len(inputs) == len(set(inputs)),
            'Review the state contract input list')
    for name in inputs:
        require(isinstance(name, str) and name and not PurePosixPath(name).is_absolute()
                and '..' not in PurePosixPath(name).parts and '\\' not in name,
                'Contract inputs must remain within the source tree')
    worker = plan.get('worker', {})
    require(worker.get('container') == 'cfm-browser-worker' and worker.get('service') == 'grok-browser'
            and worker.get('architecture') == 'arm64' and type(worker.get('capacity')) is int
            and worker['capacity'] == 1 and worker.get('commit') == plan['candidate_commit'],
            'Worker topology or candidate identity is unsupported')
    for key in ('python_cloakbrowser', 'python_playwright', 'node_cloakbrowser', 'node_playwright'):
        require(isinstance(worker.get(key), str) and re.fullmatch(r'\d+\.\d+\.\d+', worker[key]),
                'Pin reviewed browser dependency versions')
    require(type(worker.get('node_minimum_major')) is int and worker['node_minimum_major'] >= 22,
            'Pin the reviewed Node runtime floor')
    previous = plan.get('previous_worker', {})
    require(previous.get('container') == 'cfm-browser-worker' and
            re.fullmatch('[0-9a-f]{40}', previous.get('commit', '')) and
            re.fullmatch('sha256:[0-9a-f]{64}', previous.get('image_id', '')) and
            isinstance(previous.get('image'), str) and previous['image'], 'Pin the previous Worker image')
    require(plan['base_commit'] != plan['candidate_commit']
            or previous['commit'] != plan['candidate_commit'],
            'Candidate must differ from base or previous Worker')


def initialize(plan_path, output, root=None):
    pipeline_started = time.time()
    pipeline_started_at = datetime.now(timezone.utc).isoformat()
    plan = json.load(sys.stdin) if str(plan_path) == '-' else json.loads(Path(plan_path).read_text())
    validate_plan(plan)
    if root is not None:
        verify_rolling_helper(root, plan['rolling_sha256'])
    output = Path(output).absolute()
    require(output.name == plan['operation'], 'Output basename must match the reviewed operation')
    require(not output.exists() and not output.is_symlink(), 'Operation already exists; never overwrite it')
    require(output.parent.is_dir() and not output.parent.is_symlink(), 'Output parent must be a real directory')
    files = {name: (HERE / 'paired' / name).read_bytes() for name in TEMPLATES}
    files.update({name: (HERE / name).read_bytes() for name in MODULES})
    files['release-plan.json'] = (json.dumps(plan, indent=2, ensure_ascii=False) + '\n').encode()
    for name, data in files.items():
        if name.endswith('.py'):
            compile(data, name, 'exec')
    manifest = {'format': 1, 'operation': plan['operation'], 'candidate_commit': plan['candidate_commit'],
                'files': {name: hashlib.sha256(data).hexdigest() for name, data in files.items()}}
    os.umask(0o077)
    # A failed write remains confined to a temporary private directory. Nothing
    # resembles a complete executable bundle until the atomic rename succeeds.
    with tempfile.TemporaryDirectory(prefix='.paired-init-', dir=output.parent) as temp:
        staging = Path(temp) / 'bundle'
        staging.mkdir(mode=0o700)
        for name, data in files.items():
            (staging / name).write_bytes(data)
        (staging / 'bundle-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        started = {'at': pipeline_started_at, 'epoch': pipeline_started,
                   'phase': 'operation-created', 'status': 'started',
                   'init_elapsed_seconds': round(time.time() - pipeline_started, 3),
                   'budget_seconds': 1200}
        timing = staging / 'pipeline-events.jsonl'
        timing.write_text(json.dumps(started, separators=(',', ':')) + '\n')
        os.chmod(timing, 0o600)
        verified_bundle_hashes(staging, plan, files)
        os.rename(staging, output)
    return manifest


def run_release(operation, root, source, *, build_timeout=900, drain_timeout=600,
                apply_grace=600, force_build=False):
    """Run the paired release phases continuously from one persisted deadline.

    The operation directory must already have been created by ``init``. Each
    helper keeps its own private evidence and the coordinator remains the only
    component allowed to perform the handoff. A failed coordinator may be
    resumed only when its journal proves that traffic is already healthy and
    the remaining shared budget still permits archive finalization.
    """
    operation = Path(operation).absolute()
    root = Path(root).resolve()
    source = Path(source).resolve()
    require(operation.is_dir() and not operation.is_symlink(),
            'Operation directory must be an existing real directory')
    require((operation / 'pipeline-events.jsonl').is_file()
            and (operation / 'bundle-manifest.json').is_file(),
            'Operation bundle is incomplete; run init first')
    plan = json.loads(checked_file(operation / 'release-plan.json'))
    # An existing batch executes its own immutable coordinator and lock policy.
    validate_plan(plan, require_backup_lock=False)
    require(operation.name == plan['operation'], 'Operation directory differs from the reviewed plan')
    verified_bundle_hashes(operation, plan, (*TEMPLATES, *MODULES, 'release-plan.json'))
    verify_rolling_helper(root, plan['rolling_sha256'])
    require(source.is_dir() and not source.is_symlink(),
            'Source tree must be an existing real directory')
    require(0 < build_timeout <= 900 and 0 < drain_timeout <= 1200
            and 0 < apply_grace <= 1200
            and drain_timeout + apply_grace <= 1200,
            'Release phase budgets exceed the 1200-second handoff limit')
    python = sys.executable
    common = ['--root', str(root), '--operation-dir', str(operation)]

    def invoke(name, args, *, allow_failure=False):
        command = [python, '-B', str(operation / name), *args]
        requested = {'build.py': build_timeout,
                     'worker-release.py': 300,
                     'preflight.py': 300}.get(name, drain_timeout + apply_grace)
        timeout = bounded_subprocess_timeout(operation, requested)
        # A timeout leaves the operation directory untouched for diagnosis; no
        # phase is retried from this continuous entrypoint.
        result = subprocess.run(command, check=False, timeout=timeout)
        if not allow_failure:
            require(result.returncode == 0, 'Release phase stopped: ' + name)
        return result

    build_args = [*common, '--source', str(source), '--build-timeout', str(build_timeout)]
    if force_build:
        build_args.append('--force-build')
    invoke('build.py', build_args)
    invoke('worker-release.py', [*common, '--candidate-manifest',
                                 str(operation / 'worker-manifest.json'), '--action', 'prepare'])
    invoke('preflight.py', [*common, '--release', str(operation / 'release.json'),
                            '--worker-manifest', str(operation / 'worker-manifest.json'),
                            '--output', str(operation / 'baseline.json')])

    coordinator = [*common, 'apply-and-verify', '--drain-timeout', str(drain_timeout),
                   '--apply-grace', str(apply_grace)]
    result = invoke('coordinator.py', coordinator, allow_failure=True)
    if result.returncode:
        state_path = operation / 'release-state.json'
        if state_path.is_file() and not state_path.is_symlink():
            try:
                state = json.loads(state_path.read_text())
            except (OSError, ValueError):
                state = {}
            if state.get('status') in ('archive_pending', 'success'):
                # This is a read-only archive/final-verification continuation.
                # It shares the original init timestamp and cannot restart
                # apply, traffic switching or rollback.
                invoke('coordinator.py', [*common, 'finalize'])
                result = None
        if result is not None:
            require(False, 'Release coordinator stopped; inspect its journal before retrying')
    print(json.dumps({'released': True, 'operation': str(operation),
                      'archive_resumed': result is None}, ensure_ascii=False))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('init', 'run'))
    parser.add_argument('--plan', type=Path, help='reviewed plan file, or - to read JSON to EOF from stdin')
    parser.add_argument('--output', type=Path)
    parser.add_argument('--root', type=Path, help='runtime root for the installed rolling helper check (required for run)')
    parser.add_argument('--operation-dir', type=Path, default=None)
    parser.add_argument('--source', type=Path, default=None)
    parser.add_argument('--build-timeout', type=int, default=900)
    parser.add_argument('--drain-timeout', type=int, default=600)
    parser.add_argument('--apply-grace', type=int, default=600)
    parser.add_argument('--force-build', action='store_true')
    args = parser.parse_args()
    # Installed layout: <root>/tools/paired-deploy/versions/<digest>.
    installed_root = None
    if len(HERE.parents) >= 4 and HERE.parts[-4:-1] == ('tools', 'paired-deploy', 'versions'):
        installed_root = HERE.parents[3]
    root = args.root or installed_root
    if args.action == 'init':
        require(args.plan is not None and args.output is not None,
                'init requires --plan and --output')
        value = initialize(args.plan, args.output, root)
        print(json.dumps({'created': str(args.output), 'files': len(value['files']),
                          'candidate_commit': value['candidate_commit'], 'deployment_started': False}))
        return
    require(args.source is not None, 'run requires --source')
    require(root is not None, 'run requires --root outside the installed tool directory')
    if args.operation_dir is not None:
        require(args.plan is None and args.output is None,
                'run with --operation-dir cannot also receive --plan or --output')
        operation = args.operation_dir
    else:
        require(args.plan is not None and args.output is not None,
                'run requires --plan and --output when --operation-dir is omitted')
        # initialize() performs the immutable, no-overwrite check before any
        # Docker or deployment helper is started. A second invocation against
        # the same output therefore stops without touching prior evidence.
        initialize(args.plan, args.output, root)
        operation = args.output
    run_release(operation, root, args.source, build_timeout=args.build_timeout,
                drain_timeout=args.drain_timeout, apply_grace=args.apply_grace,
                force_build=args.force_build)


if __name__ == '__main__':
    main()
