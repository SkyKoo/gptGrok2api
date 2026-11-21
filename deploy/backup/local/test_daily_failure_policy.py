"""Future daily retry policy; fixtures never read production data or run Docker."""
import contextlib
import datetime as dt
from decimal import Decimal
import importlib.util
import json
import os
from pathlib import Path
import tarfile
import tempfile
import types
import unittest
from unittest import mock


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
    return module


daily = load('daily_failure_policy', 'oracle-backup-daily.py')
maintenance = load('daily_failure_maintenance', 'oracle-backup-maintenance.py')


class FailurePolicyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.root = self.base / 'gptgrok2api'
        self.backups = self.root / 'backups'; self.backups.mkdir(parents=True)
        patch = mock.patch.object(daily, 'ROOT', self.root)
        patch.start(); self.addCleanup(patch.stop)
        self.at = dt.datetime(2026, 11, 18, 1, tzinfo=dt.timezone.utc)

    def legacy(self):
        path = self.backups / 'daily-20261113T000000Z'; path.mkdir(mode=0o700)
        daily.atomic_json(path / 'failed.json', {'format': 'ogg-daily-failed-v1',
            'at': '2026-11-13T00:02:00+00:00', 'service': daily.SERVICE,
            'completed': False, 'automatic_prune_allowed': False})
        return path

    def release(self, name='rolling-20261114T010000Z-example'):
        path = self.backups / name; path.mkdir()
        source = self.base / name; source.mkdir()
        for name, value in {'.env': b'fixture', 'compose.yaml': b'fixture',
                            'current.json': b'{}'}.items():
            (source / name).write_bytes(value)
        manifest = daily.archive_metadata(source)
        def metadata(member):
            relative = Path(*Path(member.name).parts[1:]).as_posix()
            member.pax_headers['mtime'] = format(Decimal(manifest[relative]['mtime_ns']) / 10**9, '.9f')
            return member
        with tarfile.open(path / 'snapshot.tar.gz', 'x:gz') as archive:
            archive.add(source, arcname='snapshot', filter=metadata)
        for name in ('postgres.dump', 'postgres-globals.sql', 'redis.rdb'):
            (path / name).write_bytes(b'fixture')
        daily.atomic_json(path / 'snapshot-metadata.json', manifest)
        daily.atomic_json(path / 'complete', {'protocol': 'cfm-single-writer-v1', 'metadata_verified': True})
        daily.atomic_json(path / 'release.json', {'backup': str(path), 'status': 'success',
                          'phase': 'success', 'commit': 'a' * 40, 'final_backup_verified': True})
        self.release_checksums(path)
        return path

    def release_checksums(self, path):
        daily.atomic_json(path / 'checksums.json', {name: daily.sha_file(path / name)
            for name in ('snapshot.tar.gz', 'postgres.dump', 'postgres-globals.sql',
                         'redis.rdb', 'snapshot-metadata.json')})

    def daily_successor(self):
        def capture(helper, work):
            stage = work / '.staging'; stage.mkdir()
            for relative, content in {'service/.env': b'fixture', 'service/compose.yaml': b'fixture',
                'service/current.json': b'{"commit":"abc"}', 'database/postgres.dump': b'PGDMPx',
                'database/postgres-globals.sql': b'sql', 'database/redis.rdb': b'REDISx'}.items():
                path = stage / relative; path.parent.mkdir(parents=True, exist_ok=True); path.write_bytes(content)
            (stage / 'service/data/files').mkdir(parents=True)
            (stage / 'service/data/log_input_images').mkdir()
            return stage, {'source_commit': 'abc'}, {'media_included': True, 'authentication_state_included': True}
        helper = types.SimpleNamespace(rehearse=lambda *args: {'postgres_restored': True,
            'postgres_tables': 7, 'postgres_rows_match': True, 'redis_restored': True,
            'redis_loaded_keys': 2, 'network': 'none', 'production_mounts': False})
        with self.capture(capture), mock.patch.object(daily, 'load_helper', return_value=helper):
            result = daily.create({'helpers': {}})
        return self.backups / result['name']

    def update_daily_proof(self, path, proof):
        daily.atomic_json(path / 'recovery-point.json', proof)
        complete = json.loads((path / 'complete').read_text())
        complete['proof_sha256'] = daily.sha_file(path / 'recovery-point.json')
        daily.atomic_json(path / 'complete', complete)
        entry = json.loads((path / 'catalog-entry.json').read_text())
        entry['proofs'][0]['sha256'] = complete['proof_sha256']
        entry['files'] = {name: daily.sha_file(path / name) for name in entry['files']}
        daily.atomic_json(path / 'catalog-entry.json', entry)

    @contextlib.contextmanager
    def policy_validation(self):
        catalog, retention = daily.policy_modules()
        with mock.patch.object(daily, 'policy_modules', return_value=(catalog, retention)), \
             mock.patch.object(retention, 'ROOTS', {daily.SERVICE: self.backups}), \
             mock.patch.object(catalog, 'verify_postgres_dump') as postgres:
            yield catalog, retention, postgres

    @contextlib.contextmanager
    def capture(self, action, identities=None):
        with mock.patch.object(daily, 'load_helper', return_value=object()), \
             mock.patch.object(daily, 'capture_cfm', side_effect=action) as capture, \
             mock.patch.object(daily.shutil, 'disk_usage', return_value=types.SimpleNamespace(free=32*daily.GIB)), \
             mock.patch.object(daily.os, 'nice'), mock.patch.object(daily.os, 'umask'), \
             mock.patch.object(daily, 'container_identity', side_effect=identities or [{'stable': True}] * 2):
            yield capture

    def test_legacy_marker_without_newer_success_remains_unchanged_and_blocked(self):
        old = self.legacy()
        contents = (old / 'failed.json').read_bytes()
        before = daily.stamp((old / 'failed.json').stat())
        with self.policy_validation(), self.assertRaisesRegex(daily.Refused, 'no newer verified'):
            daily.ensure_no_unresolved(self.backups)
        self.assertEqual((old / 'failed.json').read_bytes(), contents)
        self.assertEqual(daily.stamp((old / 'failed.json').stat()), before)

    def test_newer_release_is_fully_reverified_and_old_marker_stays_protected(self):
        old = self.legacy(); self.release()
        before = (old / 'failed.json').read_bytes(), daily.stamp((old / 'failed.json').stat())
        with self.policy_validation() as (catalog, retention, postgres), \
             mock.patch.object(retention, 'verify_archive', wraps=retention.verify_archive) as archive:
            daily.ensure_no_unresolved(self.backups)
            self.assertEqual(postgres.call_count, 1)
            self.assertEqual(archive.call_count, 1)
            record = catalog.make_record(daily.SERVICE, old, {}, None, {})
            self.assertFalse(record['completed'])
            self.assertTrue(record['protected_reasons'])
        self.assertEqual(((old / 'failed.json').read_bytes(), daily.stamp((old / 'failed.json').stat())), before)

    def test_successful_daily_supersedes_without_modifying_old_marker(self):
        successor = self.daily_successor(); old = self.legacy()
        before = (old / 'failed.json').read_bytes(), daily.stamp((old / 'failed.json').stat())
        with self.policy_validation(), mock.patch.object(daily, 'verify_archive', wraps=daily.verify_archive) as archive:
            daily.ensure_no_unresolved(self.backups)
            self.assertEqual(archive.call_args.args[0], successor / 'backup.tar.gz')
        self.assertEqual(((old / 'failed.json').read_bytes(), daily.stamp((old / 'failed.json').stat())), before)

    def test_daily_success_requires_restore_isolation_scope_and_unchanged_production(self):
        successor = self.daily_successor(); self.legacy()
        original = json.loads((successor / 'recovery-point.json').read_text())
        mutations = [('database_restore', 'postgres_rows_match', False),
                     ('database_restore', 'redis_restored', False),
                     ('database_restore', 'network', 'production'),
                     ('database_restore', 'production_mounts', True),
                     ('scope', 'authentication_state_included', False),
                     (None, 'production_containers_unchanged', False)]
        for section, key, value in mutations:
            with self.subTest(field=key):
                proof = json.loads(json.dumps(original))
                (proof[section] if section else proof)[key] = value
                self.update_daily_proof(successor, proof)
                with self.policy_validation(), self.assertRaisesRegex(daily.Refused, 'isolated restore or full-scope'):
                    daily.ensure_no_unresolved(self.backups)

    def test_older_success_cannot_supersede_failure(self):
        self.legacy(); self.release('rolling-20261112T010000Z-example')
        with self.policy_validation(), self.assertRaisesRegex(daily.Refused, 'no newer verified'):
            daily.ensure_no_unresolved(self.backups)

    def test_archive_residue_and_hidden_or_linked_members_stay_blocked(self):
        old = self.legacy(); self.release()
        for name in ('backup.tar.gz', '.unknown'):
            member = old / name; member.write_bytes(b'preserve')
            with self.policy_validation(), self.assertRaisesRegex(daily.Refused, 'contains artifacts'):
                daily.ensure_no_unresolved(self.backups)
            self.assertEqual(member.read_bytes(), b'preserve'); member.unlink()
        (old / 'outside').symlink_to('/absent')
        with self.policy_validation(), self.assertRaisesRegex(daily.Refused, 'contains artifacts'):
            daily.ensure_no_unresolved(self.backups)

    def test_linked_or_unknown_marker_cannot_be_superseded(self):
        old = self.legacy(); self.release()
        marker = old / 'failed.json'
        value = json.loads(marker.read_text()); value['format'] = 'ogg-daily-failed-v2'
        daily.atomic_json(marker, value)
        with self.policy_validation(), self.assertRaisesRegex(daily.Refused, 'unknown failure marker'):
            daily.ensure_no_unresolved(self.backups)
        original = self.base / 'marker'; marker.rename(original); marker.symlink_to(original)
        with self.policy_validation(), self.assertRaisesRegex(daily.Refused, 'unsafe or invalid'):
            daily.ensure_no_unresolved(self.backups)

    def test_newer_release_hash_change_is_not_trusted(self):
        self.legacy(); release = self.release()
        (release / 'postgres.dump').write_bytes(b'tampered')
        with self.policy_validation(), self.assertRaisesRegex(daily.Refused, 'no newer verified'):
            daily.ensure_no_unresolved(self.backups)

    def test_newer_release_truncated_archive_fails_even_with_matching_hash(self):
        self.legacy(); release = self.release()
        archive = release / 'snapshot.tar.gz'; archive.write_bytes(archive.read_bytes()[:-6])
        self.release_checksums(release)
        with self.policy_validation(), self.assertRaises((EOFError, OSError)):
            daily.ensure_no_unresolved(self.backups)

    def test_marker_changed_during_newer_validation_is_rejected(self):
        old = self.legacy(); self.release()
        with self.policy_validation() as (_, retention, _):
            original = retention.verify_record
            def change_marker(*args):
                result = original(*args)
                os.utime(old / 'failed.json', ns=(1, 1))
                return result
            with mock.patch.object(retention, 'verify_record', side_effect=change_marker), \
                 self.assertRaisesRegex(daily.Refused, 'changed during recovery'):
                daily.ensure_no_unresolved(self.backups)

    def test_transient_exhaustion_records_private_bounded_evidence_without_failed_backup(self):
        old = self.backups / 'old'; old.mkdir(); (old / 'proof').write_bytes(b'preserve')
        def changed(helper, work):
            stage = work / '.staging'; stage.mkdir(); (stage / 'scratch').write_bytes(b'temporary')
            raise daily.SnapshotChanged('concurrent capture')
        with self.capture(changed), self.assertRaises(daily.SnapshotChanged):
            daily.create({'helpers': {}})
        self.assertEqual(list(self.backups.iterdir()), [old])
        self.assertEqual((old / 'proof').read_bytes(), b'preserve')
        policy = self.root / 'backup-policy/daily-retry.json'
        self.assertEqual(policy.stat().st_mode & 0o777, 0o600)
        self.assertEqual(len(daily.retry_state()['failures']), 1)
        self.assertEqual(daily.retry_status()['reason'], 'cooldown')
        with mock.patch.object(daily, 'capture_cfm') as capture:
            self.assertEqual(daily.create({'helpers': {}})['event'], 'daily_backup_deferred')
            capture.assert_not_called()

    def test_marker_only_v2_requires_a_newer_verified_full_backup(self):
        def unknown(helper, work):
            (work / '.staging').mkdir()
            raise daily.Refused('unknown scope')
        with self.capture(unknown), self.assertRaises(daily.Refused):
            daily.create({'helpers': {}})
        created = next(self.backups.glob('daily-*'))
        self.assertEqual(json.loads((created / 'failed.json').read_text())['format'], 'ogg-daily-failed-v2')
        self.assertFalse((self.root / 'backup-policy/daily-retry.json').exists())
        with self.assertRaisesRegex(daily.Refused, 'no newer verified recovery point'):
            daily.ensure_no_unresolved(self.backups)

    def test_transient_with_archive_or_container_change_remains_failed(self):
        def residue(helper, work):
            (work / 'backup.tar.gz').write_bytes(b'unverified')
            raise daily.SnapshotChanged('changed')
        with self.capture(residue), self.assertRaisesRegex(daily.Refused, 'residue'):
            daily.create({'helpers': {}})
        created = next(self.backups.glob('daily-*'))
        self.assertEqual((created / 'backup.tar.gz').read_bytes(), b'unverified')
        self.assertTrue((created / 'failed.json').exists())
        self.assertFalse((self.root / 'backup-policy/daily-retry.json').exists())

    def test_changed_production_identity_disallows_transient_cleanup(self):
        def changed(helper, work):
            raise daily.SnapshotChanged('changed')
        with self.capture(changed, [{'stable': True}, {'stable': False}]), \
             self.assertRaisesRegex(daily.Refused, 'production container identity'):
            daily.create({'helpers': {}})
        created = next(self.backups.glob('daily-*'))
        self.assertTrue((created / 'failed.json').exists())
        self.assertFalse((self.root / 'backup-policy/daily-retry.json').exists())

    def test_journal_failure_keeps_failed_backup_for_review(self):
        def changed(helper, work):
            raise daily.SnapshotChanged('changed')
        with self.capture(changed), mock.patch.object(daily, 'record_retry_failure', side_effect=OSError('full')):
            with self.assertRaises(OSError):
                daily.create({'helpers': {}})
        self.assertTrue(next(self.backups.glob('daily-*/failed.json')).is_file())

    def test_cleanup_failure_keeps_v2_marker_even_when_cleanup_repeatedly_fails(self):
        def changed(helper, work):
            (work / '.staging').mkdir()
            raise daily.SnapshotChanged('changed')
        with self.capture(changed), mock.patch.object(daily.shutil, 'rmtree', side_effect=OSError('busy')):
            with self.assertRaises(OSError):
                daily.create({'helpers': {}})
        created = next(self.backups.glob('daily-*'))
        self.assertTrue((created / '.staging').is_dir())
        self.assertEqual(json.loads((created / 'failed.json').read_text())['format'], 'ogg-daily-failed-v2')
        self.assertEqual(len(daily.retry_state()['failures']), 1)
        with self.assertRaisesRegex(daily.Refused, 'contains artifacts'):
            daily.ensure_no_unresolved(self.backups)

    def test_cooldown_daily_budget_and_next_day_are_bounded(self):
        for index in range(6):
            at = self.at + dt.timedelta(hours=index)
            daily.record_retry_failure(self.backups / f'daily-fixture-{index}', at)
            self.assertEqual(daily.retry_status(at + dt.timedelta(minutes=59))['failed_attempts_today'], index + 1)
        self.assertEqual(daily.retry_status(self.at + dt.timedelta(hours=6))['reason'], 'daily_failure_limit')
        tomorrow = self.at + dt.timedelta(days=1)
        self.assertIsNone(daily.retry_status(tomorrow))
        daily.record_retry_failure(self.backups / 'daily-next-day', tomorrow)
        self.assertEqual(len(daily.retry_state()['failures']), 6)
        self.assertEqual(daily.retry_status(tomorrow)['failed_attempts_today'], 1)

    def test_retry_policy_rejects_links_unknown_reason_and_clock_reversal(self):
        daily.record_retry_failure(self.backups / 'daily-fixture', self.at)
        with self.assertRaisesRegex(daily.Refused, 'clock moved backwards'):
            daily.retry_status(self.at - dt.timedelta(seconds=1))
        path = self.root / 'backup-policy/daily-retry.json'
        state = json.loads(path.read_text()); state['failures'][0]['reason'] = 'ignore_everything'
        daily.atomic_json(path, state)
        with self.assertRaisesRegex(daily.Refused, 'unknown daily retry'):
            daily.retry_status(self.at)
        old = self.base / 'state'; path.rename(old); path.symlink_to(old)
        with self.assertRaisesRegex(daily.Refused, 'linked'):
            daily.retry_status(self.at)


class MaintenanceFailureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        class Clock(dt.datetime):
            @classmethod
            def now(cls, tz=None):
                value = cls(2026, 11, 18, 6, tzinfo=maintenance.TZ)
                return value.astimezone(tz) if tz else value.replace(tzinfo=None)
        for patch in (mock.patch.object(maintenance, 'BASE', self.base),
                      mock.patch.object(maintenance.dt, 'datetime', Clock),
                      mock.patch.object(maintenance, 'completion_signal', return_value='signal')):
            patch.start(); self.addCleanup(patch.stop)
        self.catalog = {'services': {daily.SERVICE: {'backups': [], 'blocked_reason': None}}}

    def test_cooldown_returns_before_daily_and_retention(self):
        with mock.patch.object(maintenance, 'make_catalog', return_value=self.catalog), \
             mock.patch.object(maintenance, 'daily_retry_status', return_value={'event': 'daily_backup_deferred'}), \
             mock.patch.object(maintenance, 'command') as command:
            self.assertEqual(maintenance.run(execute=True)['event'], 'daily_backup_deferred')
        command.assert_not_called()

    def test_failed_daily_never_reaches_retention(self):
        with mock.patch.object(maintenance, 'make_catalog', return_value=self.catalog), \
             mock.patch.object(maintenance, 'daily_retry_status', return_value=None), \
             mock.patch.object(maintenance, 'command', side_effect=RuntimeError('daily failed')) as command:
            with self.assertRaisesRegex(RuntimeError, 'daily failed'):
                maintenance.run(execute=True)
        self.assertEqual(command.call_count, 1)
        self.assertEqual(command.call_args.args[0], 'oracle-backup-daily.py')


if __name__ == '__main__':
    unittest.main()
