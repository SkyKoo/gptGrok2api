"""Offline content, snapshot and lifecycle regressions for the paired state guard."""
import copy
from decimal import Decimal
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import snapshot_integrity as integrity

spec = importlib.util.spec_from_file_location(
    'paired_state_guard_test', Path(__file__).with_name('paired') / 'state-guard.py')
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


def tables():
    result = {name: [] for name in guard.TABLES}
    result['mailboxes'] = [{'id': 1, 'email': 'private@example.invalid', 'metadata': {'value': 1}}]
    result['mailbox_schema_migrations'] = [{'version': 3}]
    return result


def database():
    return guard.database_state(tables(), 'CREATE TABLE example (id bigint);')


class TableDigestTests(unittest.TestCase):
    def test_reordered_rows_and_objects_have_same_compact_digest(self):
        before = tables()
        before['mailboxes'].append({'id': 2, 'metadata': {'b': 2, 'a': 1}})
        after = copy.deepcopy(before)
        after['mailboxes'].reverse()
        after['mailboxes'][0]['metadata'] = {'a': 1, 'b': 2}
        value = guard.database_state(before, 'schema')
        self.assertEqual(value, guard.database_state(after, 'schema'))
        self.assertEqual(value['tables']['mailboxes']['rows'], 2)
        self.assertEqual(set(value['tables']['mailboxes']), {'rows', 'sha256'})
        self.assertNotIn('private@example.invalid', json.dumps(value))
        self.assertLess(len(json.dumps(value)), 1300)

    def test_changed_deleted_added_rows_and_schema_are_detected(self):
        before = guard.database_state(tables(), 'schema')
        for change in ('edit', 'delete', 'insert', 'schema'):
            with self.subTest(change=change):
                altered = tables()
                schema = 'schema'
                if change == 'edit': altered['mailboxes'][0]['metadata']['value'] = 2
                if change == 'delete': altered['mailboxes'].clear()
                if change == 'insert': altered['mailboxes'].append({'id': 2, 'metadata': {'value': 1}})
                if change == 'schema': schema = 'new schema'
                self.assertNotEqual(before, guard.database_state(altered, schema))

    def test_duplicate_identity_is_rejected_instead_of_collapsing_rows(self):
        for table, row in [('mailboxes', {'id': 1}),
                           ('mailbox_schema_migrations', {'version': 1}),
                           ('mailbox_registration_blocks', {'mailbox_id': 1, 'platform': 'adobe'})]:
            with self.subTest(table=table), self.assertRaisesRegex(guard.VerificationError, 'duplicate'):
                guard.table_state(table, [row, row.copy()])

    def test_type_precision_and_array_order_remain_visible(self):
        values = [1, True, '1', Decimal('1.000000000000000001'),
                  Decimal('1.000000000000000002'), [1, 2], [2, 1], None]
        outputs = [guard.table_state('mailboxes', [{'id': 1, 'value': value}])['sha256'] for value in values]
        self.assertEqual(len(set(outputs)), len(values))
        with self.assertRaisesRegex(guard.VerificationError, 'type'):
            guard.canonical(1.1)

    def test_unexpected_table_set_is_rejected(self):
        value = tables(); value['hidden'] = []
        with self.assertRaisesRegex(guard.VerificationError, 'table set'):
            guard.database_state(value, 'schema')


class FileCaptureTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.data = self.root / 'data'; self.data.mkdir()
        (self.root / 'logs').mkdir()
        self.file = self.data / 'accounts.json.enc'; self.file.write_bytes(b'secret-data')

    def tearDown(self):
        self.temporary.cleanup()

    def capture(self, **kwargs):
        with patch.object(guard, 'inspect_database', return_value=database()) as inspector:
            result = guard.capture(self.root, 'postgres', **kwargs)
        inspector.assert_called_once_with('postgres')
        return result

    def test_one_file_content_pass_and_one_database_scan_per_boundary(self):
        first = self.capture()
        self.assertEqual(first['metrics']['content_files_read'], 1)
        self.assertEqual(first['metrics']['content_bytes_read'], len(b'secret-data'))
        self.assertEqual(first['metrics']['database_scans'], 1)
        self.assertEqual(guard.business_files(first), {'accounts.json.enc': hashlib.sha256(b'secret-data').hexdigest()})
        report = guard.verify(first, self.capture())
        self.assertEqual(report['status'], 'verified')
        self.assertEqual(report['tables']['mailboxes'], 1)

    def test_frozen_content_proof_is_reused_without_content_reads(self):
        frozen = integrity.capture_layout(self.root, selected=True)
        reused = self.capture(source_capture=frozen)
        self.assertTrue(reused['metrics']['source_manifest_reused'])
        self.assertEqual(reused['metrics']['content_files_read'], 0)
        self.assertEqual(reused['metrics']['content_bytes_read'], 0)
        guard.verify(reused, self.capture())

    def test_frozen_proof_without_browser_metrics_still_verifies_business_files(self):
        metrics = self.data / 'browser-worker/profiles/fixture/browser/BrowserMetrics'
        metrics.mkdir(parents=True)
        (metrics / 'sample.pma').write_bytes(b'runtime cache')
        business = self.data / 'BrowserMetrics'
        business.mkdir()
        (business / 'record').write_bytes(b'business data')
        ordinary_file = metrics.parent.parent / 'BrowserMetrics'
        ordinary_file.write_bytes(b'ordinary file')

        frozen = integrity.capture_layout(self.root, selected=True)
        omitted = metrics.relative_to(self.root).as_posix()
        for name in list(frozen.manifest):
            if name == omitted or name.startswith(omitted + '/'):
                del frozen.manifest[name]
                del frozen.identities[name]

        reused = self.capture(source_capture=frozen)
        guard.verify(reused, self.capture())
        self.assertIn('BrowserMetrics/record', reused['files'])
        self.assertIn(ordinary_file.relative_to(self.data).as_posix(), reused['files'])
        self.assertNotIn(metrics.relative_to(self.data).as_posix(), reused['files'])
        (business / 'record').write_bytes(b'changed data')
        with self.assertRaisesRegex(guard.VerificationError, 'data changed'):
            self.capture(source_capture=frozen)

    def test_owned_maintenance_marker_may_be_removed_after_frozen_capture(self):
        marker = self.data / 'cfm-maintenance'
        marker.write_text('deploy:fixture')
        frozen = integrity.capture_layout(self.root, selected=True)
        marker.unlink()
        reused = self.capture(source_capture=frozen)
        self.assertNotIn('cfm-maintenance', reused['files'])
        self.assertEqual(reused['metrics']['content_files_read'], 0)

    def test_business_root_permissions_remain_protected_when_marker_is_removed(self):
        self.data.chmod(0o700)
        marker = self.data / 'cfm-maintenance'
        marker.write_text('deploy:fixture')
        frozen = integrity.capture_layout(self.root, selected=True)
        marker.unlink()
        self.data.chmod(0o755)
        with self.assertRaisesRegex(guard.VerificationError, 'data root identity'):
            self.capture(source_capture=frozen)

    def test_same_size_mtime_restored_write_invalidates_frozen_proof(self):
        frozen = integrity.capture_layout(self.root, selected=True)
        before = self.file.stat()
        self.file.write_bytes(b'changed-xxx')
        os.utime(self.file, ns=(before.st_atime_ns, before.st_mtime_ns))
        with self.assertRaisesRegex(guard.VerificationError, 'data changed'):
            self.capture(source_capture=frozen)

    def test_mutation_during_database_read_is_rejected_without_second_content_pass(self):
        def changed(_):
            before = self.file.stat()
            self.file.write_bytes(b'changed-xxx')
            os.utime(self.file, ns=(before.st_atime_ns, before.st_mtime_ns))
            return database()
        with patch.object(guard, 'inspect_database', side_effect=changed), \
             self.assertRaisesRegex(guard.VerificationError, 'data changed'):
            guard.capture(self.root, 'postgres')

    def test_database_failure_leaves_no_successful_capture(self):
        with patch.object(guard, 'inspect_database', side_effect=guard.VerificationError('failed')), \
             self.assertRaisesRegex(guard.VerificationError, 'failed'):
            guard.capture(self.root, 'postgres')

    def test_runtime_exclusion_is_exact_and_does_not_hide_nested_files(self):
        (self.data / '.cfm-writer.lock').write_text('runtime')
        (self.data / 'cfm-maintenance').write_text('deploy:fixture')
        nested = self.data / 'nested'; nested.mkdir()
        (nested / '.cfm-writer.lock').write_text('business')
        result = self.capture()
        self.assertNotIn('.cfm-writer.lock', result['files'])
        self.assertNotIn('cfm-maintenance', result['files'])
        self.assertIn('nested/.cfm-writer.lock', result['files'])
        self.assertEqual(result['metrics']['content_files_read'], 2)

    def test_permissions_empty_directories_file_set_and_content_are_protected(self):
        before = self.capture()
        for change in ('mode', 'directory', 'add', 'edit'):
            with self.subTest(change=change):
                if change == 'mode': self.file.chmod(0o400)
                if change == 'directory': (self.data / 'empty').mkdir()
                if change == 'add': (self.data / 'new').write_text('new')
                if change == 'edit': self.file.write_text('replacement')
                with self.assertRaisesRegex(guard.VerificationError, 'business file'):
                    guard.verify(before, self.capture())
                self.file.chmod(0o600)
                self.file.write_bytes(b'secret-data')
                # Restore fixture permission as captured, independently of umask.
                self.file.chmod(before['files']['accounts.json.enc']['mode'])
                if (self.data / 'empty').exists(): (self.data / 'empty').rmdir()
                if (self.data / 'new').exists(): (self.data / 'new').unlink()

    def test_symlinks_special_files_and_linked_parent_are_refused(self):
        link = self.data / 'linked'; link.symlink_to(self.file)
        with self.assertRaises(guard.VerificationError): self.capture()
        link.unlink()
        os.mkfifo(self.data / 'fifo')
        with self.assertRaises(guard.VerificationError): self.capture()
        (self.data / 'fifo').unlink()
        root_link = self.root / 'root-link'; root_link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises((guard.VerificationError, OSError)):
            with patch.object(guard, 'inspect_database', return_value=database()):
                guard.capture(root_link, 'postgres')

    def test_unknown_baseline_format_is_rejected(self):
        now = self.capture()
        before = dict(now, format='cfm-adobe-state-unchanged-v1')
        with self.assertRaisesRegex(guard.VerificationError, 'format'):
            guard.verify(before, now)

    def test_private_evidence_is_not_overwritten_or_world_readable(self):
        self.root.chmod(0o700)
        path = self.root / 'snapshot.json'
        guard.private_write(path, self.capture())
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        with self.assertRaisesRegex(guard.VerificationError, 'overwrite'):
            guard.private_write(path, {})


class DatabaseSnapshotTests(unittest.TestCase):
    def run_inspection(self, *, payload=None, schema=b'CREATE TABLE public.example (id int);', failure=None):
        payload = payload or {'relations': list(guard.TABLES), 'tables': tables()}
        original_popen = subprocess.Popen
        processes, arguments = [], []
        output = b'00000003-0000001B-1\n' + json.dumps(payload).encode() + b'\n'
        def popen(args, **kwargs):
            arguments.append((args, kwargs))
            child = original_popen([sys.executable, '-c',
                'import os,sys; os.write(1,' + repr(output) + '); sys.stdin.buffer.read()'], **kwargs)
            processes.append(child)
            return child
        def dump(args, payload=None):
            self.assertIsNone(processes[0].poll())
            self.assertIn('--snapshot="$1"', args[-3])
            self.assertEqual(args[-1], '00000003-0000001B-1')
            if failure: raise failure
            return schema
        try:
            with patch.object(guard.subprocess, 'Popen', side_effect=popen), \
                 patch.object(guard, 'command', side_effect=dump) as command:
                result = guard.inspect_database('postgres')
            command.assert_called_once()
            self.assertEqual(arguments[0][1]['stderr'], subprocess.DEVNULL)
            return result
        finally:
            for process in processes:
                self.assertIsNotNone(process.poll(), 'snapshot exporter leaked')

    def test_schema_imports_same_live_snapshot_and_exporter_is_reaped(self):
        result = self.run_inspection()
        self.assertEqual(result, guard.database_state(tables(), 'CREATE TABLE public.example (id int);'))

    def test_pg_dump_failure_still_releases_exported_snapshot(self):
        with self.assertRaisesRegex(guard.VerificationError, 'safe error'):
            self.run_inspection(failure=guard.VerificationError('safe error'))

    def test_random_pg_dump_restriction_tokens_are_ignored_only_at_directives(self):
        first = self.run_inspection(schema=b'\\restrict abc\nCREATE TABLE t (id int);\n\\unrestrict abc\n')
        second = self.run_inspection(schema=b'\\restrict xyz\nCREATE TABLE t (id int);\n\\unrestrict xyz\n')
        self.assertEqual(first, second)
        third = self.run_inspection(schema=b'\\restrict xyz\nCREATE TABLE t (id text);\n\\unrestrict xyz\n')
        self.assertNotEqual(first, third)

    def test_unexpected_relations_abort_and_release_snapshot(self):
        value = {'relations': list(guard.TABLES) + ['unexpected'], 'tables': tables()}
        with self.assertRaisesRegex(guard.VerificationError, 'table set'):
            self.run_inspection(payload=value)

    def test_database_command_errors_do_not_expose_output_or_values(self):
        result = SimpleNamespace(returncode=1, stdout=b'secret-token', stderr=b'secret-password')
        with patch.object(guard.subprocess, 'run', return_value=result), \
             self.assertRaises(guard.VerificationError) as error:
            guard.command(['test'])
        self.assertNotIn('secret', str(error.exception))

    def test_snapshot_response_timeout_size_and_truncation_are_bounded(self):
        scripts = [('import time; time.sleep(1)', {'timeout': .02}, 'deadline'),
                   ('import os; os.write(1,b"x"*100)', {'limit': 10}, 'size'),
                   ('pass', {}, 'ended')]
        for script, kwargs, message in scripts:
            with self.subTest(message=message):
                process = subprocess.Popen([sys.executable, '-c', script], stdin=subprocess.PIPE,
                                           stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0)
                try:
                    with self.assertRaisesRegex(guard.VerificationError, message):
                        guard.database_lines(process, 2, **kwargs)
                finally:
                    guard.close_database_snapshot(process)
                self.assertIsNotNone(process.poll())

    def test_hung_exporter_is_killed_and_reaped(self):
        process = Mock()
        process.wait.side_effect = [subprocess.TimeoutExpired('psql', 5), 0]
        guard.close_database_snapshot(process)
        process.kill.assert_called_once_with()
        self.assertEqual(process.wait.call_count, 2)
        process.stdout.close.assert_called_once_with()


if __name__ == '__main__':
    unittest.main()
