"""Capture other services against the explicitly selected deployment root."""
import argparse
import datetime
import importlib.util
import json
from pathlib import Path
import subprocess

import common


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--operation-dir', type=Path, default=Path(__file__).resolve().parent)
    args = parser.parse_args()
    common.host_required()
    root, op = args.root.resolve(), args.operation_dir.resolve()
    common.helper_hashes(op)
    spec = importlib.util.spec_from_file_location('services', op / 'verify-other-services.py')
    services = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(services)
    current = common.load(root / 'current.json')
    common.require(current.get('commit') == common.BASE_COMMIT and current.get('status') == 'healthy',
                   'Other-service baseline no longer matches the reviewed base')
    excluded = {current['container'], common.WORKER}
    ids = subprocess.check_output(['docker', 'ps', '-q'], timeout=30).decode().splitlines()
    common.require(bool(ids), 'No running containers found')
    rows = json.loads(subprocess.check_output(['docker', 'inspect', *ids], timeout=60))
    containers = sorted((services.container_summary(item) for item in rows
                         if item['Name'].lstrip('/') not in excluded), key=lambda item: item['name'])
    common.require(bool(containers), 'No other running services found; inspect the baseline')
    value = {'captured_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
             'production_commit': current['commit'], 'count': len(containers),
             'excluded_containers': sorted(excluded), 'containers': containers}
    with (op / 'other-services-before.json').open('x') as stream:
        json.dump(value, stream, indent=2)
    print(json.dumps(value))


if __name__ == '__main__':
    common.safe_main(main)
