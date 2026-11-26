"""Real offline backup verification, sealed identities and corruption fallbacks."""
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


HERE = Path(__file__).resolve().parent


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


common = SimpleNamespace(require=require, PROTOCOL='test-protocol')
with patch.dict(sys.modules, {'common': common}):
    snapshot = load('paired_archive_snapshot_test', HERE / 'paired/snapshot.py')
legacy = load('paired_archive_legacy_test', HERE / 'rolling-deploy.py')


class BackupVerificationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.root, self.work = self.base / 'source', self.base / 'backup'
        self.root.mkdir(); self.work.mkdir()
        (self.root / 'data').mkdir(); (self.root / 'logs').mkdir()
        (self.root / 'data/empty').mkdir()
        (self.root / 'data/account.enc').write_bytes(b'encrypted fixture' * 1024)
        (self.root / 'data/runtime.json').write_bytes(b'runtime')
        (self.root / 'logs/fixture.log').write_bytes(b'log fixture')
        self.boot = patch.object(snapshot, 'boot_identity', return_value='same-test-boot')
        self.boot.start(); self.addCleanup(self.boot.stop)
        self.host = SimpleNamespace(checkpoint=lambda: None)
        self.value = snapshot.make_legacy(legacy, self.root, self.work, self.host)
        self.value.sync(frozen=True)
        for name in snapshot.DATABASE_FILES:
            (self.work / name).write_bytes(b'database fixture')
        self.value.seal()
        self.value.finish()

    def verify(self, **kwargs):
        return snapshot.verify_backup(self.work, runtime_files={'runtime.json'}, **kwargs)

    def test_direct_snapshot_keeps_only_streaming_archive(self):
        root = self.base / 'direct-source'
        work = self.base / 'direct-backup'
        root.mkdir(); work.mkdir()
        (root / 'data').mkdir(); (root / 'logs').mkdir()
        (root / 'data' / 'account.enc').write_bytes(b'fixture' * 1024)
        (root / 'data' / 'cfm-maintenance').write_bytes(b'owned by deploy')
        (root / 'data' / 'nested').mkdir()
        (root / 'data' / 'nested' / 'cfm-maintenance').write_bytes(b'business fixture')
        (root / 'logs' / 'fixture.log').write_bytes(b'log')
        older_runtime_files = legacy.RUNTIME_FILES - {'cfm-maintenance'}
        with patch.object(legacy, 'PROTOCOL', common.PROTOCOL), \
             patch.object(legacy, 'RUNTIME_FILES', older_runtime_files):
            value = snapshot.make(legacy, root, work, self.host)
            value.sync(frozen=True)
            self.assertIn('data/cfm-maintenance', value.frozen_manifest)
            self.assertEqual(set(value.business_files()), {'account.enc', 'nested/cfm-maintenance'})
            for name in snapshot.DATABASE_FILES:
                (work / name).write_bytes(b'database fixture')
            value.seal()
            value.finish()
            self.assertFalse((work / 'snapshot').exists())
            result = snapshot.verify_backup(work, runtime_files=older_runtime_files)
            self.assertEqual(result['files'], value.business_files())
        self.assertEqual(result['archive']['content_verified'], True)
        self.assertEqual(result['raw_snapshot']['method'], 'direct_archive')

    def test_frozen_sync_keeps_legacy_copy_for_identity_proof(self):
        root = self.base / 'single-copy-source'
        work = self.base / 'single-copy-backup'
        root.mkdir(); work.mkdir()
        (root / 'data').mkdir(); (root / 'logs').mkdir()
        (root / 'data' / 'account.enc').write_bytes(b'fixture')
        value = snapshot.make_legacy(legacy, root, work, self.host)
        value.sync(frozen=True)
        self.assertTrue((work / 'snapshot' / 'data' / 'account.enc').is_file())

    def test_frozen_copy_reuses_source_read_and_hashes_target_once(self):
        root = self.base / 'optimized-source'
        work = self.base / 'optimized-backup'
        root.mkdir(); work.mkdir()
        (root / 'data').mkdir(); (root / 'logs').mkdir()
        (root / 'data' / 'account.enc').write_bytes(b'fixture')
        value = snapshot.make_legacy(legacy, root, work, self.host)
        value.sync(frozen=True)
        stats = {}
        snapshot.integrity.preserve_layout(
            root, work / 'snapshot', checkpoint=self.host.checkpoint, stats=stats,
            source_content_verified=True)
        self.assertEqual(stats['phases']['preserve_source'].get('content_bytes_read', 0), 0)
        self.assertGreater(stats['phases']['preserve_target']['content_bytes_read'], 0)

    def test_file_proof_checkpoint_can_stop_large_evidence_hash(self):
        path = self.work / 'large-evidence.bin'
        path.write_bytes(b'x' * (3 * 1024 * 1024))
        calls = []

        def abort():
            calls.append(True)
            if len(calls) >= 2:
                raise RuntimeError('budget exhausted')

        with self.assertRaisesRegex(RuntimeError, 'budget exhausted'):
            snapshot.file_proof(path, checkpoint=abort)
        self.assertGreaterEqual(len(calls), 2)

    def test_sealed_archive_report_reuses_full_read_and_unchanged_raw_needs_no_content_read(self):
        stats = {}
        with patch.object(snapshot.integrity, 'verify_archive', wraps=snapshot.integrity.verify_archive) as archive, \
             patch.object(snapshot, 'file_proof', wraps=snapshot.file_proof) as proof:
            result = self.verify(stats=stats)
        archive.assert_not_called()
        self.assertNotIn('snapshot.tar.gz', [call.args[0].name for call in proof.call_args_list])
        self.assertEqual(result['raw_snapshot']['method'], 'sealed_identity')
        self.assertEqual(set(result['files']), {'account.enc'})
        self.assertEqual(stats.get('content_bytes_read', 0), 0)
        self.assertIn(snapshot.RAW_IDENTITY, result['checksums_verified'])

    def test_legacy_backup_without_archive_report_falls_back_to_full_read(self):
        checks = json.loads((self.work / 'checksums.json').read_text())
        del checks['archive-report.json']
        (self.work / 'checksums.json').write_text(json.dumps(checks))
        with patch.object(snapshot.integrity, 'verify_archive', wraps=snapshot.integrity.verify_archive) as archive:
            self.verify()
        archive.assert_called_once()

    def test_same_content_with_changed_ctime_falls_back_to_exact_capture(self):
        path = self.work / 'snapshot/data/account.enc'
        info = path.stat()
        os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns))
        stats = {}
        result = self.verify(stats=stats)
        self.assertEqual(result['raw_snapshot']['method'], 'full_content')
        self.assertGreater(stats['phases']['capture']['content_bytes_read'], 0)

    def test_same_size_mtime_content_change_is_detected(self):
        path = self.work / 'snapshot/data/account.enc'
        info = path.stat()
        path.write_bytes(b'x' * info.st_size)
        os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns))
        with self.assertRaisesRegex(RuntimeError, 'Raw snapshot differs'):
            self.verify()

    def test_same_boot_does_not_allow_changed_permissions_or_missing_empty_directory(self):
        path = self.work / 'snapshot/data/empty'
        path.rmdir()
        with self.assertRaisesRegex(RuntimeError, 'Raw snapshot differs'):
            self.verify()

    def test_restart_or_unavailable_boot_identity_forces_full_content_verification(self):
        for boot in ('another-boot', None):
            with self.subTest(boot=boot), patch.object(snapshot, 'boot_identity', return_value=boot):
                self.assertEqual(self.verify()['raw_snapshot']['method'], 'full_content')

    def test_legacy_backup_without_raw_receipt_uses_full_content(self):
        path = self.work / 'checksums.json'
        checks = json.loads(path.read_text())
        del checks[snapshot.RAW_IDENTITY]
        path.write_text(json.dumps(checks))
        (self.work / snapshot.RAW_IDENTITY).unlink()
        self.assertEqual(self.verify()['raw_snapshot']['method'], 'full_content')

    def test_changed_receipt_cannot_claim_unchanged_raw_identity(self):
        path = self.work / snapshot.RAW_IDENTITY
        data = json.loads(path.read_text())
        data['boot_id'] = 'changed'
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(RuntimeError, 'checksums changed'):
            self.verify()

    def test_wrong_root_or_manifest_binding_never_reuses_receipt(self):
        receipt = json.loads((self.work / snapshot.RAW_IDENTITY).read_text())
        manifest = self.value.frozen_layout
        digest = snapshot.file_proof(self.work / 'snapshot-metadata.json')['sha256']
        for key, value in [('root', '/different-root'), ('manifest_sha256', '0' * 64)]:
            with self.subTest(key=key):
                result = snapshot.verify_raw_snapshot(self.work, manifest, digest, dict(receipt, **{key: value}))
                self.assertEqual(result['method'], 'full_content')

    def test_archive_corruption_and_changed_database_still_fail(self):
        path = self.work / 'snapshot.tar.gz'
        original = path.read_bytes()
        path.write_bytes(original[:-5])
        with self.assertRaises((RuntimeError, OSError, EOFError)):
            self.verify()
        path.write_bytes(original)
        (self.work / 'postgres.dump').write_bytes(b'changed database')
        with self.assertRaisesRegex(RuntimeError, 'checksums changed'):
            self.verify()

    def test_checksum_manifest_cannot_omit_database_or_add_arbitrary_paths(self):
        path = self.work / 'checksums.json'
        original = json.loads(path.read_text())
        variants = [dict(original, **{'../outside': '0' * 64}),
                    {k: v for k, v in original.items() if k != 'postgres.dump'}]
        for checks in variants:
            path.write_text(json.dumps(checks))
            with self.subTest(keys=list(checks)), self.assertRaisesRegex(RuntimeError, 'checksum set differs'):
                self.verify()

    def test_raw_symlink_is_rejected_even_with_fallback(self):
        path = self.work / 'snapshot/data/account.enc'
        path.unlink(); path.symlink_to(self.root / 'data/account.enc')
        with self.assertRaises((RuntimeError, OSError)):
            self.verify()


if __name__ == '__main__':
    unittest.main()
