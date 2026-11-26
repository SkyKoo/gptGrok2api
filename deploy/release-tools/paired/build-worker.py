#!/usr/bin/env python3
"""Build or verify the fixed Adobe worker image; no server mutation or browser launch."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import uuid
from datetime import datetime, timezone

os.umask(0o077)
HERE = Path(__file__).resolve().parent
RELEASE_PLAN = json.loads((HERE / 'release-plan.json').read_text())
PLAN = dict(RELEASE_PLAN['worker'], commit=RELEASE_PLAN['candidate_commit'])


def run(args, **kwargs):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, **kwargs)
    if result.returncode:
        raise RuntimeError('command failed: ' + ' '.join(args[:3]))
    return result.stdout


def inspect(image):
    return json.loads(run(['docker', 'image', 'inspect', image]))[0]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', required=True)
    parser.add_argument('--operation-dir', required=True)
    parser.add_argument('--image', required=True)
    parser.add_argument('--skip-build', action='store_true', required=True, help='only verifies an already built fixed-revision image')
    args = parser.parse_args()
    source, op = Path(args.source).resolve(), Path(args.operation_dir).resolve()
    op.mkdir(parents=True, exist_ok=True, mode=0o700)
    commit = PLAN['commit']
    if not isinstance(commit, str) or len(commit) != 40:
        raise RuntimeError('pin the reviewed feature commit first')
    actual = run(['git', '-c', 'safe.directory=' + str(source), '-C', str(source), 'rev-parse', 'HEAD']).decode().strip()
    if actual != commit:
        raise RuntimeError('source HEAD differs from reviewed worker commit')
    names = run(['git', '-c', 'safe.directory=' + str(source), '-C', str(source), 'ls-tree', '-r', '--name-only', commit, 'grok-browser']).decode().splitlines()
    files = {name: run(['git', '-c', 'safe.directory=' + str(source), '-C', str(source), 'show', commit + ':' + name]) for name in names}
    package = json.loads(files['grok-browser/adobe/package-lock.json'])
    assert package['packages']['node_modules/cloakbrowser']['version'] == PLAN['node_cloakbrowser']
    assert package['packages']['node_modules/playwright']['version'] == PLAN['node_playwright']
    root_files = {'server.py', 'adobe_flow.py', 'xai_flow.py', 'oauth_device.py', 'browser_sessions.py', 'signup_errors.py', 'network_diagnostics.py', 'requirements.txt', 'run-docker.sh'}
    production = {}
    for name, raw in files.items():
        rel = name.removeprefix('grok-browser/')
        if rel in root_files or rel.startswith(('adobe/', 'common/', 'turnstile/')):
            production[rel] = hashlib.sha256(raw).hexdigest()
    manifest = dict(PLAN, image=args.image, production_files=production,
                    build_dockerfile_sha256=hashlib.sha256(files['grok-browser/Dockerfile']).hexdigest())
    candidate = inspect(args.image)
    if candidate['Architecture'] != 'arm64' or candidate['Config'].get('Labels', {}).get('org.opencontainers.image.revision') != commit:
        raise RuntimeError('candidate architecture or revision mismatch')
    manifest['image_id'] = candidate['Id']
    probe = str(HERE / 'worker-image-probe.py')
    command = ['docker', 'run', '--rm', '-i', '--network', 'none', '--cpus=1', '--memory=1g',
               '--workdir', '/app', '--entrypoint', 'python', '-e', 'PYTHONDONTWRITEBYTECODE=1',
               '-e', 'CLOAKBROWSER_CACHE_DIR=/root/.cloakbrowser',
               '--mount', 'type=bind,source=' + probe + ',target=/worker-image-probe.py,readonly',
               args.image, '/worker-image-probe.py']
    attempt = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8]
    stdout_path = op / ('worker-image-probe-' + attempt + '.stdout')
    stderr_path = op / ('worker-image-probe-' + attempt + '.stderr')
    with stdout_path.open('xb') as out, stderr_path.open('xb') as err:
        result = subprocess.run(command, input=json.dumps(manifest).encode(), stdout=out, stderr=err, timeout=60)
    if result.returncode:
        raise RuntimeError('worker import/source probe failed; no browser was started; attempt=' + attempt)
    manifest['runtime_probe'] = json.loads(stdout_path.read_text())
    manifest['runtime_probe_attempt'] = attempt
    (op / 'worker-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    print(json.dumps({'manifest': str(op / 'worker-manifest.json'), 'commit': commit, 'image_id': candidate['Id'], 'source_files_verified': len(production), 'browser_started': False}))


if __name__ == '__main__':
    main()
