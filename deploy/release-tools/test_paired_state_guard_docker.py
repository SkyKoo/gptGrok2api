"""Opt-in state guard integration against one disposable, networkless PostgreSQL.

CFM_STATE_GUARD_DOCKER=1 python3 -m unittest discover -s deploy/release-tools \
    -p test_paired_state_guard_docker.py -v

Uses an existing postgres:17-alpine image, 1 CPU/512 MiB and tmpfs storage.
Never pulls images, publishes ports, mounts host data or contacts production.
"""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import time
import unittest
from unittest.mock import patch
import uuid

spec = importlib.util.spec_from_file_location(
    'paired_guard_docker_test', Path(__file__).with_name('paired') / 'state-guard.py')
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


def run(args, *, data=None, timeout=30):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            timeout=timeout, check=False)
    if result.returncode:
        raise RuntimeError('Local Docker test command failed: ' + args[0])
    return result.stdout


@unittest.skipUnless(os.environ.get('CFM_STATE_GUARD_DOCKER') == '1',
                     'explicit local PostgreSQL integration only')
class PostgreSQLStateGuard(unittest.TestCase):
    def test_consistent_snapshot_rows_schema_and_exporter_cleanup(self):
        token = uuid.uuid4().hex
        name, label = 'cfm-state-guard-' + token[:12], 'io.cfm.state-guard-test=' + token
        image = json.loads(run(['docker', 'image', 'inspect', 'postgres:17-alpine']))[0]
        self.assertEqual(image['Os'], 'linux')
        created_id = None

        def sql(text):
            return run(['docker', 'exec', '-i', name, 'psql', '-XAtq', '-v', 'ON_ERROR_STOP=1',
                        '-U', 'cfm_guard_test', '-d', 'cfm_guard_test'], data=text.encode()).strip()

        def exporters():
            return int(sql("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() "
                           "AND pid<>pg_backend_pid() AND state='idle in transaction';"))

        def state(database):
            return {'format': guard.FORMAT, 'files': {}, 'database': database}

        try:
            created_id = run(['docker', 'run', '-d', '--pull=never', '--network=none',
                '--cpus=1', '--memory=512m', '--name', name, '--label', label,
                '--tmpfs', '/var/lib/postgresql/data:rw,size=256m',
                '-e', 'POSTGRES_HOST_AUTH_METHOD=trust', '-e', 'POSTGRES_USER=cfm_guard_test',
                '-e', 'POSTGRES_DB=cfm_guard_test', image['Id']], timeout=30).decode().strip()
            end = time.monotonic() + 40
            while True:
                try:
                    run(['docker', 'exec', name, 'pg_isready', '-h', '127.0.0.1',
                         '-U', 'cfm_guard_test', '-d', 'cfm_guard_test'], timeout=5)
                    break
                except RuntimeError:
                    if time.monotonic() > end:
                        raise
                    time.sleep(.2)
            actual = json.loads(run(['docker', 'inspect', created_id]))[0]
            self.assertEqual(actual['HostConfig']['NetworkMode'], 'none')
            self.assertEqual(actual['HostConfig']['NanoCpus'], 1000000000)
            self.assertEqual(actual['HostConfig']['Memory'], 512 * 1024 * 1024)
            self.assertFalse(actual['HostConfig'].get('PortBindings'))
            self.assertFalse(actual['HostConfig'].get('Binds'))
            self.assertTrue(all(mount['Type'] == 'tmpfs' for mount in actual['Mounts']))
            for table in guard.TABLES:
                if table == 'mailbox_schema_migrations':
                    fields, row = 'version bigint PRIMARY KEY', '1'
                elif table in guard.CHECK_TABLES:
                    fields, row = 'mailbox_id bigint, platform text, PRIMARY KEY(mailbox_id,platform)', "1,'adobe'"
                else:
                    fields, row = 'id bigint PRIMARY KEY, metadata jsonb, amount numeric', "1,'{\"value\":1}',1.000000000000000001"
                sql('CREATE TABLE public.' + table + ' (' + fields + '); INSERT INTO public.' + table + ' VALUES (' + row + ');')

            before = guard.inspect_database(name)
            self.assertEqual(before, guard.inspect_database(name))
            self.assertEqual(exporters(), 0)
            real_command = guard.command
            imported_snapshots = []

            def check_exported_snapshot(args, payload=None):
                self.assertEqual(exporters(), 1)
                snapshot_id = args[-1]
                # A concurrent write commits after the guard's row query. Both
                # readers importing its still-live snapshot must see the old row.
                sql("UPDATE mailboxes SET metadata='{\"value\":2}' WHERE id=1;")
                value = sql("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; "
                            "SET TRANSACTION SNAPSHOT '" + snapshot_id + "'; "
                            "SELECT metadata->>'value' FROM mailboxes WHERE id=1; ROLLBACK;")
                self.assertEqual(value, b'1')
                imported_snapshots.append(snapshot_id)
                return real_command(args, payload)

            with patch.object(guard, 'command', side_effect=check_exported_snapshot):
                same_snapshot = guard.inspect_database(name)
            self.assertEqual(same_snapshot, before)
            self.assertEqual(len(imported_snapshots), 1)
            self.assertEqual(exporters(), 0)
            after_row = guard.inspect_database(name)
            with self.assertRaisesRegex(guard.VerificationError, 'database rows or schema'):
                guard.verify(state(before), state(after_row))
            sql('ALTER TABLE mailboxes ADD COLUMN extra text;')
            after_schema = guard.inspect_database(name)
            self.assertNotEqual(after_row['schema_sha256'], after_schema['schema_sha256'])
            with self.assertRaisesRegex(guard.VerificationError, 'database rows or schema'):
                guard.verify(state(after_row), state(after_schema))

            with patch.object(guard, 'command', side_effect=guard.VerificationError('synthetic dump failure')):
                with self.assertRaisesRegex(guard.VerificationError, 'synthetic dump failure'):
                    guard.inspect_database(name)
            self.assertEqual(exporters(), 0)
            actual = json.loads(run(['docker', 'inspect', created_id]))[0]
            self.assertFalse(actual['State']['OOMKilled'])
            self.assertEqual(actual['RestartCount'], 0)
            print(json.dumps({'check': 'state_guard_postgres', 'status': 'passed',
                              'container': name, 'image_id': image['Id'],
                              'architecture': image['Architecture'], 'exporters_remaining': 0,
                              'snapshot_shared': True, 'row_change_rejected': True,
                              'schema_change_rejected': True, 'failure_cleanup_verified': True}), flush=True)
        finally:
            # Even a timed-out run may have created this exact name. Ownership is
            # checked before removal; unrelated containers are never touched.
            found = run(['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$',
                         '--filter', 'label=' + label]).decode().split()
            for identifier in found:
                info = json.loads(run(['docker', 'inspect', identifier]))[0]
                self.assertEqual(info['Name'], '/' + name)
                self.assertEqual(info['Config']['Labels'].get('io.cfm.state-guard-test'), token)
                if created_id is not None:
                    self.assertEqual(info['Id'], created_id)
                run(['docker', 'rm', '-f', info['Id']])
            remaining = run(['docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$',
                             '--filter', 'label=' + label]).strip()
            self.assertEqual(remaining, b'')
            print(json.dumps({'check': 'state_guard_postgres_cleanup', 'container': name,
                              'removed_owned_containers': len(found), 'remaining': 0}), flush=True)


if __name__ == '__main__':
    unittest.main()
