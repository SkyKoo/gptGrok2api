#!/usr/bin/env python3
"""Build or verify pinned ARM64 app and Worker images without touching services.

Prebuilt images may be reused only when their immutable ID, architecture and
OCI revision match the fixed candidate. Worker reuse still runs the source and
runtime import probe; no browser is launched.
"""
import argparse
import json
from pathlib import Path
import shutil
import subprocess
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

import common

BUILD_HEADROOM_BYTES = 12 * 1024 ** 3


def build_image(command, log_path, timeout):
    with log_path.open('xb') as output:
        result = subprocess.run(command, stdout=output, stderr=subprocess.STDOUT, timeout=timeout)
    common.require(result.returncode == 0, 'Build failed; inspect the private log')


def verify_image(host, tag, commit, *, app, image=None):
    image = image if image is not None else host.image(tag)
    common.check_image(host, tag, image['Id'], commit, app=app)
    return image


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--operation-dir', type=Path, default=Path(__file__).resolve().parent)
    parser.add_argument('--app-image', default=None,
                        help='candidate app tag; defaults to gptgrok2api:sha-COMMIT')
    parser.add_argument('--worker-image', default=None,
                        help='candidate Worker tag; defaults to cfm-browser-worker:sha-COMMIT')
    parser.add_argument('--reuse-app-image', action='store_true',
                        help='reuse an existing immutable app image after strict verification')
    parser.add_argument('--reuse-worker-image', action='store_true',
                        help='reuse an existing immutable Worker image after strict verification')
    parser.add_argument('--force-build', action='store_true',
                        help='rebuild even when an exact immutable candidate image already exists')
    parser.add_argument('--build-timeout', type=int, default=900,
                        help='maximum seconds for each image build')
    args = parser.parse_args()
    common.host_required()
    common.require(0 < args.build_timeout <= 900,
                   'Each image build must fit the 900-second budget')
    source, op = args.source.resolve(), args.operation_dir.resolve()
    common.helper_hashes(op)
    common.require_pipeline_budget(op)
    common.timing_event(op, 'build-command', 'started', entrypoint='paired-build')
    r = common.rolling(args.root)
    host = r.Host()
    host.overall_deadline = time.monotonic() + common.require_pipeline_budget(op)
    current = common.load(args.root / 'current.json')
    common.require(current['commit'] == common.BASE_COMMIT and current['status'] == 'healthy',
                   'The production base changed')
    common.require(shutil.disk_usage(args.root).free >= BUILD_HEADROOM_BYTES,
                   'Less than the reviewed 12 GiB build headroom remains')
    common.require(not (op / 'release.json').exists() and not (op / 'worker-manifest.json').exists(),
                   'Build evidence exists; inspect it instead of overwriting or rebuilding')
    identity = common.verify_source(host, {'source': str(source)})
    git = ['git', '-c', 'safe.directory=' + str(source), '-C', str(source)]
    host.run(git + ['merge-base', '--is-ancestor', common.BASE_COMMIT, common.COMMIT])
    app_tag = args.app_image or ('gptgrok2api:sha-' + common.COMMIT[:12])
    worker_tag = args.worker_image or ('cfm-browser-worker:sha-' + common.COMMIT[:12])
    existing_images = {}

    def existing(tag, *, app):
        try:
            image = host.image(tag)
        except r.ReleaseError:
            return None
        # An existing candidate tag is immutable. A present but mismatched tag
        # must be reviewed or explicitly replaced with --force-build.
        image = verify_image(host, tag, common.COMMIT, app=app, image=image)
        existing_images[(tag, app)] = image
        return image

    reuse_app = bool(args.reuse_app_image or (not args.force_build and existing(app_tag, app=True)))
    reuse_worker = bool(args.reuse_worker_image or (not args.force_build and existing(worker_tag, app=False)))
    built = {}
    commands = {}
    if not reuse_app:
        commands['app'] = (['docker', 'build', '--platform', 'linux/arm64',
                            '--label', 'org.opencontainers.image.revision=' + common.COMMIT,
                            '--label', 'org.opencontainers.image.source=https://github.com/SkyKoo/gptGrok2api',
                            '--target', 'app', '-t', app_tag, str(source)], op / 'build-app.log')
    if not reuse_worker:
        commands['worker'] = (['docker', 'build', '--platform', 'linux/arm64',
                               '--label', 'org.opencontainers.image.revision=' + common.COMMIT,
                               '--label', 'org.opencontainers.image.source=https://github.com/SkyKoo/gptGrok2api',
                               '-f', str(source / 'grok-browser/Dockerfile'), '-t', worker_tag, str(source)],
                              op / 'build-worker.log')
    if commands:
        build_timeout = common.bounded_timeout(op, args.build_timeout)
        with common.timing_phase(op, 'image-build', images=sorted(commands),
                                 timeout_seconds=build_timeout):
            failures = []
            with ThreadPoolExecutor(max_workers=len(commands)) as pool:
                futures = {pool.submit(build_image, command, log, build_timeout): name
                           for name, (command, log) in commands.items()}
                for future in as_completed(futures):
                    name = futures[future]
                    try:
                        future.result()
                    except BaseException as error:
                        failures.append((name, error))
            common.require(not failures, 'One or more image builds failed; inspect private logs')
    else:
        common.timing_event(op, 'image-build', 'skipped', reason='immutable-images-reused')
    with common.timing_phase(op, 'image-identity'):
        built['app'] = existing_images.get((app_tag, True)) if reuse_app else None
        built['worker'] = existing_images.get((worker_tag, False)) if reuse_worker else None
        if built['app'] is None:
            built['app'] = verify_image(host, app_tag, common.COMMIT, app=True)
        if built['worker'] is None:
            built['worker'] = verify_image(host, worker_tag, common.COMMIT, app=False)
    common.require(common.verify_source(host, {'source': str(source)}) == identity,
                   'Source changed during build')
    # These two preparations read disjoint evidence files and only inspect the
    # already verified images. Running them together removes a serial helper
    # round-trip while leaving the candidate identities and source tree checks
    # unchanged.
    def prepare_release():
        with common.timing_phase(op, 'release-prepare'):
            prepare_timeout = common.bounded_timeout(op, 120)
            host.run(['python3', str(args.root / 'tools/rolling-deploy.py'), 'prepare',
                      '--source', str(source), '--image', app_tag,
                      '--output', str(op / 'release.json')], timeout=prepare_timeout)

    def prepare_worker():
        with common.timing_phase(op, 'worker-manifest'):
            worker_manifest_timeout = common.bounded_timeout(op, 90)
            host.run(['python3', str(op / 'build-worker.py'), '--source', str(source),
                      '--operation-dir', str(op), '--image', worker_tag,
                      '--skip-build'], timeout=worker_manifest_timeout)

    with ThreadPoolExecutor(max_workers=2) as pool:
        futures = [pool.submit(prepare_release), pool.submit(prepare_worker)]
        failures = []
        for future in futures:
            try:
                future.result()
            except BaseException as error:
                failures.append(error)
        common.require(not failures, 'Release preparation stopped; inspect private evidence')
    release = common.load(op / 'release.json')
    common.require(release['commit'] == common.COMMIT and release['base_commit'] == common.BASE_COMMIT,
                   'Prepared release identity differs')
    worker = common.load(op / 'worker-manifest.json')
    common.require(worker['image'] == worker_tag and worker['image_id'] == built['worker']['Id'],
                   'Worker manifest does not match the verified image')
    r.atomic_json(op / 'build-proof.json', {'commit': common.COMMIT, 'source': identity,
                  'app_image': app_tag, 'app_image_id': built['app']['Id'],
                  'app_image_reused': reuse_app,
                  'worker_image': worker_tag, 'worker_image_id': built['worker']['Id'],
                  'worker_image_reused': reuse_worker,
                  'browser_started': False})
    common.timing_event(op, 'build-command', 'finished', entrypoint='paired-build')
    common.require_pipeline_budget(op)
    print(json.dumps({'built': True, 'commit': common.COMMIT,
                      'app_image': app_tag, 'worker_image': worker_tag,
                      'app_image_reused': reuse_app,
                      'worker_image_reused': reuse_worker,
                      'browser_started': False}))


if __name__ == '__main__':
    common.safe_main(main)
