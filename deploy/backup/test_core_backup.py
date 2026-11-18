"""Offline guards for persisted-state selection and cloud archive validation."""
import importlib.util
from contextlib import ExitStack
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import shutil
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('cfm_backup', Path(__file__).with_name('cfm_core_backup.py'))
b = importlib.util.module_from_spec(spec)
spec.loader.exec_module(b)


class BackupTests(unittest.TestCase):
    def fixture(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        for name in b.TOP:
            path = root / name
            if name == 'secrets':
                path.mkdir()
                (path / 'test-key').write_text('fixture-only')
            else:
                path.write_text(json.dumps({'commit': 'fixture'}) if path.suffix == '.json' else 'fixture')
        (root / 'data').mkdir()
        (root / 'data/accounts.json').write_text('{"accounts":[]}')
        (root / 'data/image_tasks').mkdir()
        (root / 'data/image_tasks/a.json').write_text('{"status":"success"}')
        (root / 'data/files').mkdir()
        (root / 'data/files/image.png').write_bytes(b'not-a-backup-target')
        (root / 'data/logs.jsonl').write_text('not-a-backup-target')
        return root

    def test_media_and_logs_excluded_but_new_top_level_json_included(self):
        root = self.fixture()
        for name in ('tools', 'backups', 'backup-state', 'releases'):
            (root / name).mkdir()
            (root / name / 'snapshot.tar.gz').write_bytes(b'never include backup outputs')
        (root / 'data/new-feature.json').write_text('{}')
        contents, _, _ = b.capture(root)
        self.assertIn('data/new-feature.json', contents)
        self.assertIn('data/image_tasks/a.json', contents)
        self.assertNotIn('data/logs.jsonl', contents)
        self.assertFalse(any(name.startswith('data/files/') for name in contents))
        self.assertFalse(any(name.startswith(('tools/', 'backups/', 'backup-state/', 'releases/')) for name in contents))

    def test_adobe_encrypted_state_and_browser_authentication_are_backed_up(self):
        root = self.fixture()
        (root / 'data/adobe_accounts.json.enc').write_bytes(b'encrypted-account-fixture')
        (root / 'data/adobe_relogin_tasks.json.enc').write_bytes(b'encrypted-relogin-fixture')
        jobs = root / 'data/adobe_jobs'
        jobs.mkdir()
        (jobs / 'task.json.enc').write_bytes(b'encrypted-task-fixture')
        profile = root / 'data/browser-worker/adobe/profile-a'
        (profile / 'browser/Default/Local Storage/leveldb').mkdir(parents=True)
        (profile / 'state.json').write_text('{"result":{}}')
        (profile / 'browser/Local State').write_text('{}')
        (profile / 'browser/Default/Local Storage/leveldb/000001.log').write_bytes(b'session-data')
        (profile / 'browser/Default/Local Storage/leveldb/LOCK').touch()
        (profile / 'browser/Default/Cache').mkdir()
        (profile / 'browser/Default/Cache/cache.bin').write_bytes(b'not backed up')
        contents, _, _ = b.capture(root)
        self.assertIn('data/adobe_accounts.json.enc', contents)
        self.assertIn('data/adobe_relogin_tasks.json.enc', contents)
        self.assertIn('data/adobe_jobs/task.json.enc', contents)
        self.assertTrue(any(name.endswith('000001.log') for name in contents))
        self.assertFalse(any('/Cache/' in name or name.endswith('/LOCK') for name in contents))
        (profile / '.lock').touch(mode=0o600)
        with self.assertRaisesRegex(b.SnapshotChanged, 'active'):
            b.capture(root)

    def test_unknown_profile_locks_are_fatal_and_never_followed(self):
        for kind in ('symlink', 'directory', 'fifo', 'permissions', 'oversized'):
            with self.subTest(kind=kind):
                root = self.fixture()
                profile = root / 'data/browser-worker/adobe/profile-a'
                profile.mkdir(parents=True)
                lock = profile / '.lock'
                if kind == 'symlink':
                    lock.symlink_to(profile / 'absent-target')
                elif kind == 'directory':
                    lock.mkdir()
                elif kind == 'fifo':
                    os.mkfifo(lock, 0o600)
                else:
                    lock.touch(mode=0o600)
                    if kind == 'permissions':
                        lock.chmod(0o644)
                    else:
                        lock.write_bytes(b'x' * 4097)
                with self.assertRaisesRegex(b.BackupError, 'unreviewed browser profile lock') as error:
                    b.capture(root)
                self.assertNotIsInstance(error.exception, b.SnapshotChanged)

    def test_cloakbrowser_runtime_is_validated_but_excluded(self):
        root = self.fixture()
        runtime = root / 'data/browser-worker/cloakbrowser/chromium-146.0.1'
        runtime.mkdir(parents=True)
        (runtime / 'chrome').write_text('chrome')
        (runtime / 'chromedriver').write_text('driver')
        (runtime.parent / '.welcome_shown').write_text('ok')
        cache = root / 'browser-cache/cloakbrowser/chromium-146.0.1'
        cache.mkdir(parents=True)
        (cache / 'chrome').write_text('chrome')
        contents, _, _ = b.capture(root)
        self.assertFalse(any(name.startswith('data/browser-worker/cloakbrowser/')
                             for name in contents))
        self.assertFalse(any(name.startswith('browser-cache/') for name in contents))
        (runtime.parent / 'unexpected-state').write_text('must reject')
        with self.assertRaisesRegex(b.BackupError, 'runtime (entry|cache member)'):
            b.capture(root)
        (runtime.parent / 'unexpected-state').unlink()
        cache = root / 'browser-cache/cloakbrowser'
        shutil.rmtree(cache.parent)
        cache.parent.mkdir(parents=True)
        cache.symlink_to(runtime.parent)
        with self.assertRaisesRegex(b.BackupError, 'linked browser runtime cache'):
            b.capture(root)

    def test_unknown_directory_rejected(self):
        root = self.fixture()
        (root / 'data/new-store').mkdir()
        with self.assertRaisesRegex(b.BackupError, 'scope review'):
            b.capture(root)

    def test_symlink_and_maintenance_rejected(self):
        root = self.fixture()
        link = root / 'data/linked.json'
        link.symlink_to(root / 'data/accounts.json')
        with self.assertRaisesRegex(b.BackupError, 'symlink'):
            b.capture(root)
        link.unlink()
        (root / 'data/cfm-maintenance').touch()
        with self.assertRaisesRegex(b.BackupError, 'maintenance'):
            b.capture(root)

    def test_change_while_reading_rejected(self):
        root = self.fixture()
        real_signature = b.signature
        calls = 0

        def changed(path):
            nonlocal calls
            calls += 1
            value = real_signature(path)
            return value if calls % 2 else value[:-1] + (value[-1] + 1,)

        with patch.object(b, 'signature', changed), self.assertRaisesRegex(b.SnapshotChanged, 'changed during read'):
            b.capture(root)

    def test_atomic_replacement_during_open_is_retried_as_a_complete_snapshot(self):
        root = self.fixture()
        target = root / 'data/accounts.json'
        replacement = root / 'replacement.tmp'
        updated = b'{"accounts":[{"id":"new"}]}'
        replacement.write_bytes(updated)
        real_open = os.open
        replacements = 0

        def replace_after_open(path, flags, *args, **kwargs):
            nonlocal replacements
            descriptor = real_open(path, flags, *args, **kwargs)
            if path == target and replacements == 0:
                os.replace(replacement, target)
                replacements += 1
            return descriptor

        with patch.object(b, 'SOURCE', root), patch.object(b.os, 'open', replace_after_open), \
             patch.object(b, 'snapshot_once', wraps=b.snapshot_once) as collect, \
             patch.object(b, 'pg_guard', return_value={}) as guard, \
             patch.object(b, 'pg_command', return_value=b'PGDMP-fixture'), \
             patch.object(b, 'redis_snapshot', return_value=b'REDIS-fixture'), \
             patch.object(b.time, 'sleep') as delay, patch.object(b, 'emit'):
            contents, receipt = b.snapshot()
        self.assertEqual(replacements, 1)
        self.assertEqual(collect.call_count, 2)
        self.assertEqual(guard.call_count, 2)
        delay.assert_called_once_with(2)
        self.assertEqual(contents['data/accounts.json'], updated)
        self.assertEqual(receipt['attempt'], 2)
        self.assertEqual(json.loads(contents['manifest.json']), receipt)

    def test_selected_file_deleted_before_read_is_transient(self):
        root = self.fixture()
        real_paths = b.source_paths

        def remove_after_selection(source):
            paths = real_paths(source)
            (source / 'data/accounts.json').unlink()
            return paths

        with patch.object(b, 'source_paths', remove_after_selection), \
             self.assertRaisesRegex(b.SnapshotChanged, 'disappeared after selection'):
            b.capture(root)

    def test_selected_directory_members_disappearing_are_transient(self):
        for kind in ('task', 'browser_provider'):
            with self.subTest(kind=kind):
                root = self.fixture()
                if kind == 'task':
                    scanned = root / 'data/image_tasks'
                    victim = scanned / 'a.json'
                else:
                    scanned = root / 'data/browser-worker'
                    victim = scanned / 'adobe'
                    victim.mkdir(parents=True)
                inode = scanned.stat().st_ino
                original = os.listdir

                def disappear_after_listing(descriptor):
                    names = original(descriptor)
                    if os.fstat(descriptor).st_ino == inode:
                        victim.rmdir() if kind == 'browser_provider' else victim.unlink()
                    return names

                with patch.object(b.os, 'listdir', disappear_after_listing), \
                     self.assertRaisesRegex(b.SnapshotChanged, 'disappeared during enumeration'):
                    b.capture(root)

    def test_selected_directory_replaced_by_link_is_never_traversed(self):
        root = self.fixture()
        nested = root / 'secrets/nested'
        nested.mkdir()
        outside = root / 'outside'
        outside.mkdir()
        (outside / 'secret').write_text('must not traverse')
        paths = b.selected_tree(root / 'secrets')
        self.assertEqual(next(paths)[0], nested)
        nested.rmdir()
        nested.symlink_to(outside, target_is_directory=True)
        with self.assertRaisesRegex(b.BackupError, 'linked or invalid source directory'):
            next(paths)

    def test_snapshot_once_has_no_retry_and_records_one_attempt(self):
        root = self.fixture()
        with patch.object(b, 'SOURCE', root), patch.object(b, 'pg_guard', return_value={}), \
             patch.object(b, 'pg_command', return_value=b'PGDMP-fixture'), \
             patch.object(b, 'redis_snapshot', return_value=b'REDIS-fixture'), \
             patch.object(b.time, 'sleep') as delay:
            contents, receipt = b.snapshot_once()
        delay.assert_not_called()
        self.assertEqual(receipt['attempt'], 1)
        self.assertEqual(json.loads(contents['manifest.json']), receipt)
        self.assertIn('database/postgres.dump', contents)

    def test_changed_collection_never_becomes_uploadable(self):
        root = self.fixture()
        contents, manifest, stamps = b.capture(root)
        changed = dict(stamps, changed=('changed',))
        captures = [(contents, manifest, stamps), (contents, manifest, changed)] * 3
        with patch.object(b, 'capture', side_effect=captures) as capture, \
             patch.object(b, 'pg_guard', return_value={}), \
             patch.object(b, 'pg_command', return_value=b'PGDMP-fixture'), \
             patch.object(b, 'redis_snapshot', return_value=b'REDIS-fixture'), \
             patch.object(b.time, 'sleep') as delay, \
             patch.object(b, 'emit'), self.assertRaisesRegex(b.BackupError, 'no upload'):
            b.snapshot()
        self.assertEqual(capture.call_count, 6)
        self.assertEqual(delay.call_count, 2)

    def test_sql_changes_still_exhaust_the_retry_budget(self):
        root = self.fixture()
        guards = [{'rows_sha256': 'before'}, {'rows_sha256': 'after'}] * 3
        with patch.object(b, 'SOURCE', root), patch.object(b, 'pg_guard', side_effect=guards) as guard, \
             patch.object(b, 'pg_command', return_value=b'PGDMP-fixture'), \
             patch.object(b, 'redis_snapshot', return_value=b'REDIS-fixture'), \
             patch.object(b.time, 'sleep') as delay, patch.object(b, 'emit'), \
             self.assertRaisesRegex(b.BackupError, 'no upload'):
            b.snapshot()
        self.assertEqual(guard.call_count, 6)
        self.assertEqual(delay.call_count, 2)

    def test_non_transient_source_failures_are_not_retried(self):
        for failure in ('unknown_directory', 'link', 'invalid_json', 'capacity', 'permission',
                        'missing_data_root', 'missing_required_config'):
            with self.subTest(failure=failure), ExitStack() as contexts:
                root = self.fixture()
                expected = b.BackupError
                if failure == 'unknown_directory':
                    (root / 'data/new-store').mkdir()
                elif failure == 'link':
                    (root / 'data/linked.json').symlink_to(root / 'data/accounts.json')
                elif failure == 'invalid_json':
                    (root / 'data/accounts.json').write_text('{')
                    expected = json.JSONDecodeError
                elif failure == 'capacity':
                    contexts.enter_context(patch.object(b, 'MAX_CORE_BYTES', 1))
                elif failure == 'permission':
                    contexts.enter_context(patch.object(b.os, 'open', side_effect=PermissionError('denied')))
                    expected = PermissionError
                elif failure == 'missing_data_root':
                    shutil.rmtree(root / 'data')
                    expected = FileNotFoundError
                elif failure == 'missing_required_config':
                    (root / '.env').unlink()
                contexts.enter_context(patch.object(b, 'SOURCE', root))
                collect = contexts.enter_context(patch.object(b, 'snapshot_once', wraps=b.snapshot_once))
                delay = contexts.enter_context(patch.object(b.time, 'sleep'))
                with self.assertRaises(expected):
                    b.snapshot()
                self.assertEqual(collect.call_count, 1)
                delay.assert_not_called()

    def test_symlink_replacement_before_open_is_not_followed_or_retried(self):
        root = self.fixture()
        target = root / 'data/accounts.json'
        original = b.signature

        def replace_with_link(path):
            value = original(path)
            if path == target:
                path.unlink()
                path.symlink_to(root / 'current.json')
            return value

        with patch.object(b, 'SOURCE', root), patch.object(b, 'signature', replace_with_link), \
             patch.object(b, 'snapshot_once', wraps=b.snapshot_once) as collect, \
             patch.object(b.time, 'sleep') as delay, self.assertRaises(OSError):
            b.snapshot()
        self.assertEqual(collect.call_count, 1)
        delay.assert_not_called()

    def test_redis_validation_distinguishes_changes_from_invalid_content(self):
        for failure in ('changed', 'invalid'):
            with self.subTest(failure=failure):
                root = self.fixture()
                path = root / 'redis-data/dump.rdb'
                path.parent.mkdir()
                path.write_bytes(b'REDIS-fixture' if failure == 'changed' else b'bad-header')
                infos = [
                    {'used_memory': '1'},
                    {'rdb_bgsave_in_progress': '0', 'aof_rewrite_in_progress': '0', 'rdb_saves': '1'},
                    {'rdb_bgsave_in_progress': '0', 'rdb_last_bgsave_status': 'ok', 'rdb_saves': '2'},
                ]

                def redis_command(args):
                    if 'BGSAVE' in args:
                        return b'Background saving started'
                    if failure == 'changed':
                        replacement = path.with_suffix('.tmp')
                        replacement.write_bytes(b'REDIS-new-snapshot')
                        os.replace(replacement, path)
                    return b''

                with patch.object(b, 'SOURCE', root), patch.object(b, 'redis_info', side_effect=infos), \
                     patch.object(b, 'run', side_effect=redis_command), self.assertRaises(b.BackupError) as error:
                    b.redis_snapshot()
                self.assertEqual(isinstance(error.exception, b.SnapshotChanged), failure == 'changed')

    def archive(self, member_names, content=b'fixture', mode=0o600):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode='w') as tar:
            for name in member_names:
                item = tarfile.TarInfo(name)
                item.mode = mode
                item.size = len(content)
                tar.addfile(item, io.BytesIO(content))
        return output.getvalue()

    def test_cloud_archive_must_match_content_mode_and_members(self):
        contents = {'data/accounts.json': b'fixture'}
        receipt = {'files': {'data/accounts.json': {'mode': 0o600, 'uid': 0, 'gid': 0}}}
        good = self.archive(['cfm-core/data/accounts.json'])
        self.assertEqual(b.verify_tar(good, contents, receipt), contents)
        for raw in (self.archive(['cfm-core/data/accounts.json'], b'corrupted'),
                    self.archive(['cfm-core/data/accounts.json'], mode=0o644),
                    self.archive(['cfm-core/data/accounts.json'] * 2),
                    self.archive(['cfm-core/../../escape']),
                    self.archive(['cfm-core/unknown.json'])):
            with self.subTest(archive_length=len(raw)), self.assertRaises(b.BackupError):
                b.verify_tar(raw, contents, receipt)

    def test_missing_archive_member_rejected(self):
        with self.assertRaisesRegex(b.BackupError, 'missing'):
            b.verify_tar(self.archive([]), {'a': b'fixture'}, {'files': {}})

    def test_unknown_upload_outcome_cannot_reuse_old_success(self):
        class Rows:
            def __enter__(self):
                return self
            def __exit__(self, *args):
                return False
            def execute(self, sql, args):
                self.assert_query = (sql, args)
                return self
            def fetchone(self):
                return None
        rows = Rows()
        with patch.object(b, 'database', return_value=rows), \
             patch.object(b.time, 'monotonic', side_effect=[0, 1, 301]), patch.object(b.time, 'sleep'), \
             self.assertRaisesRegex(b.BackupError, 'do not retry'):
            b.wait_backup(5, 12)
        self.assertIn('id>?', rows.assert_query[0])
        self.assertEqual(rows.assert_query[1], (5, 12))


if __name__ == '__main__':
    unittest.main()
