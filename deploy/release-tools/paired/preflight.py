#!/usr/bin/env python3
"""Read production state without pausing, deploying or sending business requests."""
import argparse
import json
from pathlib import Path
import common


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--operation-dir', type=Path, default=Path(__file__).resolve().parent)
    parser.add_argument('--release', type=Path, required=True)
    parser.add_argument('--worker-manifest', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    common.host_required()
    common.require_pipeline_budget(args.operation_dir)
    with common.timing_phase(args.operation_dir, 'preflight'):
        report = common.preflight(args.root, args.release, args.worker_manifest, args.operation_dir)
    common.require_pipeline_budget(args.operation_dir)
    common.rolling(args.root).atomic_json(args.output, report)
    print(json.dumps({'verified': True, 'commit': report['candidate']['commit'],
                      'active_commit': report['current']['commit'],
                      'disk_free_bytes': report['disk_free_bytes'],
                      'active_requests': report['control'].get('active_requests'),
                      'jobs': report['control'].get('jobs')}))


if __name__ == '__main__':
    common.safe_main(main)
