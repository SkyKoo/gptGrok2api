#!/usr/bin/env python3
"""Read-only comparison of non-CFM services and the reviewed disk reserve."""
from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys


FIELDS = (
    'name', 'id', 'image_id', 'running', 'health', 'restarts', 'oom',
    'started_at', 'networks', 'environment_sha256', 'mounts_sha256',
    'ports_sha256',
)


def digest(value):
    # Match other-services-before.json exactly; never return raw environment.
    raw = json.dumps(value, sort_keys=True, separators=(',', ':')).encode()
    return hashlib.sha256(raw).hexdigest()


def container_summary(info):
    return {
        'name': info['Name'].lstrip('/'),
        'id': info['Id'],
        'image_id': info['Image'],
        'running': info['State']['Running'],
        'health': info['State'].get('Health', {}).get('Status'),
        'restarts': info['RestartCount'],
        'oom': info['State'].get('OOMKilled'),
        'started_at': info['State']['StartedAt'],
        'networks': sorted(info['NetworkSettings']['Networks']),
        'environment_sha256': digest(sorted(info['Config'].get('Env', []))),
        'mounts_sha256': digest(sorted(info['Mounts'], key=lambda item: item['Destination'])),
        'ports_sha256': digest(info['HostConfig'].get('PortBindings')),
    }


def command(args):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
    if result.returncode:
        raise RuntimeError('Read-only Docker observation failed')
    return result.stdout


def rows_by_name(rows):
    result = {}
    for row in rows:
        if set(row) != set(FIELDS) or not isinstance(row.get('name'), str) or not row['name']:
            raise ValueError('Invalid baseline summary')
        if row['name'] in result:
            raise ValueError('Duplicate container name')
        result[row['name']] = row
    return result


def compare(before, after):
    previous, current = rows_by_name(before), rows_by_name(after)
    changes = []
    for name in sorted(set(previous) | set(current)):
        if name not in previous:
            changes.append({'name': name, 'change': 'added_running_container'})
        elif name not in current:
            changes.append({'name': name, 'change': 'missing_running_container'})
        else:
            fields = [key for key in FIELDS if previous[name][key] != current[name][key]]
            if fields:
                changes.append({
                    'name': name, 'change': 'summary_changed',
                    'fields': {key: {'before': previous[name][key], 'after': current[name][key]}
                               for key in fields},
                })
    return changes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--before', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    if args.output.exists():
        raise ValueError('Output exists; preserve prior evidence')
    baseline_raw = args.before.read_bytes()
    baseline = json.loads(baseline_raw)
    before = baseline['containers']
    rows_by_name(before)
    if not before or baseline.get('count') != len(before):
        raise ValueError('Expected the complete reviewed service baseline')
    current = json.loads((args.root / 'current.json').read_text())
    excluded = set(baseline['excluded_containers']) | {current['container'], 'cfm-browser-worker'}
    if any(row['name'] in excluded for row in before):
        raise ValueError('Exclusions would hide a baseline service')
    ids = command(['docker', 'ps', '-q']).decode().splitlines()
    if not ids:
        raise RuntimeError('No running containers observed')
    inspected = json.loads(command(['docker', 'inspect', *ids]))
    after = sorted((container_summary(info) for info in inspected
                    if info['Name'].lstrip('/') not in excluded), key=lambda row: row['name'])
    changes = compare(before, after)
    free = shutil.disk_usage(args.root).free
    reserve = 5 * 1024 ** 3
    report = {
        'verified_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
        'read_only': True,
        'baseline_sha256': hashlib.sha256(baseline_raw).hexdigest(),
        'baseline_commit': baseline['production_commit'],
        'current_commit': current['commit'],
        'excluded_containers': sorted(excluded),
        'before_count': len(before), 'after_count': len(after),
        'services_unchanged': not changes, 'differences': changes,
        'disk_free_bytes': free, 'recommended_disk_reserve_bytes': reserve,
        'disk_reserve_met': free >= reserve,
        'containers': after,
    }
    report['verified'] = report['services_unchanged'] and report['disk_reserve_met']
    with args.output.open('x') as stream:
        json.dump(report, stream, indent=2)
        stream.write('\n')
    print(json.dumps({key: report[key] for key in (
        'verified', 'services_unchanged', 'before_count', 'after_count', 'differences',
        'disk_free_bytes', 'recommended_disk_reserve_bytes', 'disk_reserve_met',
    )}))
    return 0 if report['verified'] else 1


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception as error:
        # Docker stderr and parsed private state may contain credentials.
        print(json.dumps({'verified': False, 'error_type': type(error).__name__,
                          'message': 'Read-only verification failed; no service was changed'}))
        sys.exit(1)
