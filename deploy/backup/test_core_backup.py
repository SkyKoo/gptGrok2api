"""Offline guards for persisted-state selection and cloud archive validation."""
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
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

        with patch.object(b, 'signature', changed), self.assertRaisesRegex(b.BackupError, 'changed during read'):
            b.capture(root)

    def test_changed_collection_never_becomes_uploadable(self):
        root = self.fixture()
        contents, manifest, stamps = b.capture(root)
        changed = dict(stamps, changed=('changed',))
        captures = [(contents, manifest, stamps), (contents, manifest, changed)] * 3
        with patch.object(b, 'capture', side_effect=captures), patch.object(b, 'pg_guard', return_value={}), \
             patch.object(b, 'pg_command', return_value=b'PGDMP-fixture'), \
             patch.object(b, 'redis_snapshot', return_value=b'REDIS-fixture'), patch.object(b.time, 'sleep'), \
             patch.object(b, 'emit'), self.assertRaisesRegex(b.BackupError, 'no upload'):
            b.snapshot()

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
