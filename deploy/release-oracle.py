#!/usr/bin/env python3
"""Start the reviewed O host release from its fixed CFM source checkout."""

import argparse
import json
from pathlib import Path
import re
import subprocess
import sys


RUNTIME = Path('/opt/gptgrok2api')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('plan', help='reviewed paired release plan, or - to read it from stdin')
    args = parser.parse_args()
    try:
        raw = sys.stdin.read() if args.plan == '-' else Path(args.plan).read_text(encoding='utf-8')
        plan = json.loads(raw)
    except (OSError, ValueError) as error:
        parser.error(f'cannot read release plan: {error}')

    operation = plan.get('operation') if isinstance(plan, dict) else None
    if not isinstance(operation, str) or not re.fullmatch(r'cfm-[a-z0-9][a-z0-9-]{2,100}', operation):
        parser.error('the plan must name a valid CFM release operation')

    source = Path(__file__).resolve().parents[1]
    git = ['git', '-C', str(source)]
    commit = subprocess.check_output([*git, 'rev-parse', 'HEAD'], text=True).strip()
    if plan.get('candidate_commit') != commit:
        parser.error('this checkout does not match the candidate commit in the plan')
    if subprocess.check_output([*git, 'status', '--porcelain'], text=True).strip():
        parser.error('the candidate checkout must be clean')

    current_tool = RUNTIME / 'tools/paired-deploy/current/paired-deploy.py'
    tool = subprocess.check_output(['sudo', '-n', 'readlink', '-e', str(current_tool)], text=True).strip()
    release = ['python3', '-B', tool]
    output = RUNTIME / 'operations' / operation
    subprocess.run(['sudo', '-n', *release, 'init', '--root', str(RUNTIME),
                    '--plan', '-', '--output', str(output)], input=raw, text=True, check=True)
    subprocess.run(['sudo', '-n', 'systemd-run', f'--unit={operation}',
                    f'--property=WorkingDirectory={RUNTIME}', *release, 'run',
                    '--root', str(RUNTIME), '--operation-dir', str(output),
                    '--source', str(source)], check=True)
    print(f'Release started. Follow it with sudo journalctl -fu {operation}.service')


if __name__ == '__main__':
    try:
        main()
    except subprocess.CalledProcessError as error:
        sys.exit(error.returncode)
