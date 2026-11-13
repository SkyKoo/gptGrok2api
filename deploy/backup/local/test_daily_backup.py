"""Daily recovery-point boundary tests; no Docker or production filesystem."""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('daily', Path(__file__).resolve().parent / 'oracle-backup-daily.py')
daily = importlib.util.module_from_spec(spec)
spec.loader.exec_module(daily)


class DailyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()

    def tearDown(self):
        self.temp.cleanup()

    def fixture(self):
        source = self.root / 'source'; source.mkdir()
        (source / 'empty').mkdir(mode=0o750)
        (source / 'private').mkdir(mode=0o710)
        (source / 'private/record.bin').write_bytes(b'backup contents\x00' * 500)
        (source / 'private/record.bin').chmod(0o640)
        for path in source.rglob('*'):
            os.utime(path, ns=(1700000000123456789, 1700000000123456789))
        return source

    def test_copy_and_archive_preserve_empty_directories_and_nanosecond_metadata(self):
        source = self.fixture()
        stage = self.root / 'stage'; stage.mkdir(mode=0o700)
        initial = daily.inventory({'service': source})
        self.assertEqual(daily.inventory({'service': source}, stage), initial)
        for relative, row in initial[0].items():
            self.assertEqual(daily.entry(stage / relative)[0], row)
        manifest = daily.archive_metadata(stage)
        archive = self.root / 'backup.tar.gz'
        daily.pack(stage, archive, manifest)
        recovered = daily.verify_archive(archive, manifest, ['service/private/record.bin'])
        self.assertEqual(recovered['service/private/record.bin'], (source / 'private/record.bin').read_bytes())
        self.assertEqual(manifest['service/empty']['type'], 'directory')

    def test_archive_requires_gzip_trailer_not_only_tar_eof(self):
        source = self.fixture(); manifest = daily.archive_metadata(source)
        archive = self.root / 'backup.tar.gz'; daily.pack(source, archive, manifest)
        archive.write_bytes(archive.read_bytes()[:-6])
        with self.assertRaises((EOFError, OSError)):
            daily.verify_archive(archive, manifest)

    def test_archive_content_tampering_rejected(self):
        source = self.fixture(); manifest = daily.archive_metadata(source)
        archive = self.root / 'backup.tar.gz'; daily.pack(source, archive, manifest)
        manifest['private/record.bin']['sha256'] = '0' * 64
        with self.assertRaisesRegex(daily.Refused, 'differs'):
            daily.verify_archive(archive, manifest)

    def test_symlink_and_parent_escape_rejected(self):
        source = self.fixture()
        (source / 'linked').symlink_to(source / 'private/record.bin')
        with self.assertRaisesRegex(daily.Refused, 'linked'):
            daily.inventory({'service': source})
        with self.assertRaises(daily.Refused):
            daily.safe_path(source, '../outside')

    def test_failure_has_no_complete_and_discards_only_invocation_scratch(self):
        root = self.root / 'cfm'; (root / 'backups').mkdir(parents=True)
        untouched = root / 'backups/old'; untouched.mkdir(); (untouched / 'data').write_text('preserve')
        def failed_capture(helper, work):
            (work / '.staging').mkdir(); (work / '.staging/private').write_text('temporary')
            raise daily.Refused('source changed during capture')
        with mock.patch.object(daily, 'ROOT', root), \
             mock.patch.object(daily, 'load_helper', return_value=object()), \
             mock.patch.object(daily, 'capture_cfm', side_effect=failed_capture), \
             mock.patch.object(daily.shutil, 'disk_usage', return_value=types.SimpleNamespace(free=32*daily.GIB)), \
             mock.patch.object(daily.os, 'nice'), mock.patch.object(daily.os, 'umask'), \
             mock.patch.object(daily, 'container_identity', return_value={'stable': True}):
            with self.assertRaises(daily.Refused):
                daily.create({'helpers': {}})
        created = next((root / 'backups').glob('daily-*'))
        self.assertFalse((created / 'complete').exists())
        self.assertFalse((created / '.staging').exists())
        self.assertFalse(json.loads((created / 'failed.json').read_text())['automatic_prune_allowed'])
        self.assertEqual((untouched / 'data').read_text(), 'preserve')

    def test_complete_is_written_only_after_restore_and_archive_verified(self):
        root = self.root / 'cfm'; (root / 'backups').mkdir(parents=True)
        def capture(helper, work):
            stage = work / '.staging'; stage.mkdir()
            for relative, content in {'service/.env': b'secret', 'service/compose.yaml': b'compose',
                'service/current.json': b'{"commit":"abc"}', 'database/postgres.dump': b'PGDMPx',
                'database/postgres-globals.sql': b'sql', 'database/redis.rdb': b'REDISx'}.items():
                p = stage / relative; p.parent.mkdir(parents=True, exist_ok=True); p.write_bytes(content)
            (stage / 'service/data/files').mkdir(parents=True)
            (stage / 'service/data/log_input_images').mkdir()
            return stage, {'source_commit': 'abc'}, {'media_included': True}
        def restore(contents, receipt):
            self.assertEqual(contents['database/postgres.dump'], b'PGDMPx')
            self.assertFalse(any((root / 'backups').glob('daily-*/complete')))
            return {'postgres_restored': True, 'redis_restored': True, 'network': 'none'}
        helper = types.SimpleNamespace(rehearse=restore)
        with mock.patch.object(daily, 'ROOT', root), \
             mock.patch.object(daily, 'load_helper', return_value=helper), \
             mock.patch.object(daily, 'capture_cfm', side_effect=capture), \
             mock.patch.object(daily.shutil, 'disk_usage', return_value=types.SimpleNamespace(free=32*daily.GIB)), \
             mock.patch.object(daily.os, 'nice'), mock.patch.object(daily.os, 'umask'), \
             mock.patch.object(daily, 'container_identity', return_value={'stable': True}):
            result = daily.create({'helpers': {}})
        work = root / 'backups' / result['name']
        self.assertTrue((work / 'complete').exists())
        point = json.loads((work / 'catalog-entry.json').read_text())
        for name, sha in point['files'].items():
            self.assertEqual(daily.sha_file(work / name), sha)
        self.assertFalse((work / '.staging').exists())
        self.assertEqual(point['kind'], 'daily')

    def core_fixture(self):
        root = self.root / 'cfm'; (root / 'data/files').mkdir(parents=True)
        (root / 'data/log_input_images').mkdir()
        (root / 'data/image_tasks').mkdir()
        account = root / 'data/accounts.json'; account.write_bytes(b'{"version":1}')
        media = root / 'data/files/image.bin'; media.write_bytes(b'original media')
        def snapshot():
            raw = account.read_bytes(); info = account.stat()
            contents = {'data/accounts.json': raw, 'database/postgres.dump': b'PGDMPx'}
            files = {name: {'bytes': len(content), 'sha256': hashlib.sha256(content).hexdigest(),
                           'mode': 0o600, 'uid': info.st_uid, 'gid': info.st_gid}
                     for name, content in contents.items()}
            return contents, {'files': files}
        helper = types.SimpleNamespace(source_paths=lambda source: [account], snapshot=snapshot,
                                       TOP=(), DATA_DIRS={'image_tasks'})
        work = self.root / 'work'; work.mkdir()
        return root, account, media, helper, work

    def test_core_change_during_bulk_media_copy_is_allowed(self):
        root, account, media, helper, work = self.core_fixture()
        original = daily.inventory
        def copy_then_business_write(roots, destination=None):
            result = original(roots, destination)
            if destination:
                account.write_bytes(b'{"version":2}')
            return result
        with mock.patch.object(daily, 'ROOT', root), mock.patch.object(daily, 'inventory', side_effect=copy_then_business_write):
            stage, receipt, scope = daily.capture_cfm(helper, work)
        self.assertEqual((stage / 'service/data/accounts.json').read_bytes(), b'{"version":2}')
        self.assertEqual((stage / 'service/data/files/image.bin').read_bytes(), b'original media')
        self.assertTrue(scope['media_included'])

    def test_media_change_during_core_snapshot_rejected_even_with_mtime_restored(self):
        root, account, media, helper, work = self.core_fixture()
        snapshot = helper.snapshot
        before = media.stat()
        def changed_media():
            media.write_bytes(b'modified media')
            os.utime(media, ns=(before.st_atime_ns, before.st_mtime_ns))
            return snapshot()
        helper.snapshot = changed_media
        with mock.patch.object(daily, 'ROOT', root):
            with self.assertRaisesRegex(daily.Refused, 'media changed around'):
                daily.capture_cfm(helper, work)

    def test_empty_directory_permission_change_inside_core_window_rejected(self):
        root, account, media, helper, work = self.core_fixture()
        snapshot = helper.snapshot
        def changed_directory():
            (root / 'data/image_tasks').chmod(0o711)
            return snapshot()
        helper.snapshot = changed_directory
        with mock.patch.object(daily, 'ROOT', root):
            with self.assertRaisesRegex(daily.Refused, 'business directories changed'):
                daily.capture_cfm(helper, work)

    def test_media_addition_inside_core_window_rejected(self):
        root, account, media, helper, work = self.core_fixture()
        snapshot = helper.snapshot
        def added_media():
            (root / 'data/files/new.bin').write_bytes(b'new media')
            return snapshot()
        helper.snapshot = added_media
        with mock.patch.object(daily, 'ROOT', root):
            with self.assertRaisesRegex(daily.Refused, 'media changed around'):
                daily.capture_cfm(helper, work)

    def test_original_core_helper_rejects_changes_inside_its_snapshot(self):
        root, account, media, fixture, work = self.core_fixture()
        core_spec = importlib.util.spec_from_file_location('tested_project_core',
            Path(__file__).resolve().parent.parent / 'cfm_core_backup.py')
        core = importlib.util.module_from_spec(core_spec); core_spec.loader.exec_module(core)
        captured = []
        def changing_capture(source):
            raw = str(len(captured)).encode(); captured.append(raw)
            return ({'data/accounts.json': raw}, {'data/accounts.json': {'sha256': hashlib.sha256(raw).hexdigest()}},
                    {'data/accounts.json': (len(captured),)})
        with mock.patch.object(daily, 'ROOT', root), mock.patch.object(core, 'SOURCE', root), \
             mock.patch.object(core, 'TOP', ()), mock.patch.object(core, 'DATA_DIRS', {'image_tasks'}), \
             mock.patch.object(core, 'source_paths', return_value=[account]), \
             mock.patch.object(core, 'capture', side_effect=changing_capture), \
             mock.patch.object(core, 'pg_guard', return_value={}), \
             mock.patch.object(core, 'pg_command', return_value=b'PGDMPx'), \
             mock.patch.object(core, 'redis_snapshot', return_value=b'REDISx'), \
             mock.patch.object(core.time, 'sleep'), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(core.BackupError):
                daily.capture_cfm(core, work)
        self.assertEqual(len(captured), 6)

    def test_empty_business_and_auth_directories_preserved(self):
        root = self.root / 'cfm'; root.mkdir()
        for name in ('secrets', 'data/adobe_jobs/empty', 'data/image_tasks',
                     'data/browser-worker/adobe/profile/browser/Default/IndexedDB/empty'):
            (root / name).mkdir(parents=True, exist_ok=True)
        (root / 'data/adobe_jobs/empty').chmod(0o750)
        helper = types.SimpleNamespace(TOP=('secrets',), DATA_DIRS={'adobe_jobs', 'image_tasks'},
                                       browser_profile_paths=lambda root: [])
        directories = daily.business_directories(helper, root, [])
        stage = self.root / 'stage'; stage.mkdir()
        daily.write_core(stage, {}, {'files': {}}, directories)
        self.assertEqual(daily.entry(stage / 'service/data/adobe_jobs/empty')[0],
                         daily.entry(root / 'data/adobe_jobs/empty')[0])
        self.assertTrue((stage / 'service/secrets').is_dir())
        self.assertTrue((stage / 'service/data/image_tasks').is_dir())
        self.assertTrue((stage / 'service/data/browser-worker/adobe/profile/browser/Default/IndexedDB/empty').is_dir())

    def test_unresolved_daily_prevents_additional_packages(self):
        backups = self.root / 'backups'; backups.mkdir()
        failed = backups / 'daily-20261113T000000Z'; failed.mkdir()
        (failed / 'failed.json').write_text('{}')
        with self.assertRaisesRegex(daily.Refused, 'unresolved'):
            daily.ensure_no_unresolved(backups)
        (failed / 'complete').write_text('{}')
        with self.assertRaisesRegex(daily.Refused, 'unresolved'):
            daily.ensure_no_unresolved(backups)
        (failed / 'failed.json').unlink()
        daily.ensure_no_unresolved(backups)

    def test_catalog_failure_never_leaves_complete(self):
        root = self.root / 'cfm'; (root / 'backups').mkdir(parents=True)
        def capture(helper, work):
            stage = work / '.staging'; stage.mkdir()
            (stage / 'database').mkdir()
            for name, raw in [('postgres.dump', b'PGDMPx'), ('postgres-globals.sql', b'sql'), ('redis.rdb', b'REDISx')]:
                (stage / 'database' / name).write_bytes(raw)
            return stage, {}, {}
        original_json = daily.atomic_json
        def broken_json(path, value):
            if path.name == 'catalog-entry.json':
                raise OSError('simulated disk failure')
            original_json(path, value)
        with mock.patch.object(daily, 'ROOT', root), \
             mock.patch.object(daily, 'load_helper', return_value=types.SimpleNamespace(rehearse=lambda *args: {})), \
             mock.patch.object(daily, 'capture_cfm', side_effect=capture), \
             mock.patch.object(daily.shutil, 'disk_usage', return_value=types.SimpleNamespace(free=32*daily.GIB)), \
             mock.patch.object(daily.os, 'nice'), mock.patch.object(daily.os, 'umask'), \
             mock.patch.object(daily, 'container_identity', return_value={'stable': True}), \
             mock.patch.object(daily, 'atomic_json', side_effect=broken_json):
            with self.assertRaises(OSError):
                daily.create({'helpers': {}})
        work = next((root / 'backups').glob('daily-*'))
        self.assertFalse((work / 'complete').exists())
        self.assertTrue((work / 'failed.json').exists())

    def test_busy_lock_prevents_create(self):
        config = {'locks': ['/opt/service/maintenance.lock']}
        @contextlib.contextmanager
        def busy(paths):
            raise BlockingIOError()
            yield
        with mock.patch.object(daily.os, 'geteuid', return_value=0), \
             mock.patch.object(daily, 'checked_config', return_value=config), \
             mock.patch.object(daily, 'locks', busy), mock.patch.object(daily, 'create') as create, \
             contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(daily.main(['--dependency-config', '/private/config', '--execute']), 75)
            create.assert_not_called()


    def test_entry_does_not_accept_cross_project_service_selection(self):
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as result:
                daily.main(['--dependency-config', '/unused/config', '--service', 'other-project'])
        self.assertEqual(result.exception.code, 2)


if __name__ == '__main__':
    unittest.main()
