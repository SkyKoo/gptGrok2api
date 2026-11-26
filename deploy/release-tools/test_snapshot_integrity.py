"""本地故障注入：快照完整性、目录权限、归档 EOF 及减少内容读取。"""
import copy
from decimal import Decimal
import gzip
import io
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock

import snapshot_integrity as snapshot


class SnapshotIntegrityTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        # macOS 的系统临时路径可能经过 /var 软链接；测试明确使用真实父目录。
        self.base = Path(self.temp.name).resolve()
        self.source = self.base / 'source'
        self.target = self.base / 'snapshot'
        self.source.mkdir(mode=0o700)
        for name, mode in (('data', 0o750), ('data/nested', 0o750),
                           ('data/empty', 0o711), ('logs', 0o751)):
            path = self.source / name
            path.mkdir(mode=mode)
            path.chmod(mode)
        for name, mode, raw in (('data/nested/account.json.enc', 0o640, b'private-fixture'),
                                ('logs/runtime.log', 0o640, b'log-fixture'),
                                ('.env', 0o600, b'fixture-env'),
                                ('data/zero', 0o600, b'')):
            path = self.source / name
            path.write_bytes(raw)
            path.chmod(mode)
        for path in sorted(self.source.rglob('*'), reverse=True):
            os.utime(path, ns=(1000000000123456789, 1000000000123456789))
        self.target.mkdir(mode=0o700)
        # 仿旧复制流程：文件内容已复制，目录权限/空目录尚需修复。
        for path in self.source.rglob('*'):
            if path.is_file():
                target = self.target / path.relative_to(self.source)
                target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                target.write_bytes(path.read_bytes())
        self.path = self.base / 'snapshot.tar.gz'

    def tearDown(self):
        self.temp.cleanup()

    def preserve(self, **kwargs):
        return snapshot.preserve_layout(self.source, self.target, **kwargs)

    def write_fixture_archive(self, expected, *, alter=None, append=None):
        with tarfile.open(self.path, 'w:gz', format=tarfile.PAX_FORMAT) as archive:
            for name in sorted(expected):
                row = expected[name]
                member = tarfile.TarInfo('snapshot' if name == '.' else 'snapshot/' + name)
                member.type = tarfile.DIRTYPE if row['type'] == 'directory' else tarfile.REGTYPE
                member.uid, member.gid, member.mode = row['uid'], row['gid'], row['mode']
                member.pax_headers['mtime'] = format(Decimal(row['mtime_ns']) / 1000000000, '.9f')
                raw = None
                if row['type'] == 'file':
                    data = (self.target / name).read_bytes()
                    member.size = len(data)
                    raw = io.BytesIO(data)
                if alter:
                    replacement = alter(name, member)
                    if replacement is False:
                        continue
                archive.addfile(member, raw if member.isfile() else None)
            if append:
                append(archive)

    def test_round_trip_keeps_empty_directories_permissions_and_nanoseconds(self):
        stats = {}
        expected = self.preserve(stats=stats)
        source = snapshot.capture_layout(self.source, selected=True).manifest
        self.assertEqual({name: row for name, row in expected.items() if name != '.'}, source)
        result = snapshot.write_archive(self.target, self.path, expected, stats=stats)
        import hashlib
        self.assertEqual(result['sha256'], hashlib.sha256(self.path.read_bytes()).hexdigest())
        destination = self.base / 'restored'
        restored = snapshot.restore_archive(self.path, expected, destination, stats=stats)
        self.assertTrue(restored['disk_content_verified'])
        self.assertEqual(snapshot.capture_layout(destination).manifest, expected)
        self.assertEqual((destination / 'data/empty').stat().st_mode & 0o777, 0o711)
        self.assertEqual((destination / 'data/nested/account.json.enc').stat().st_mtime_ns, 1000000000123456789)
        size = sum(row.get('size', 0) for row in source.values())
        files = sum(row['type'] == 'file' for row in source.values())
        # 性能约束：冻结校验只读源、副本各一遍；落盘/稳定性复核不能偷加 hash。
        self.assertEqual(stats['phases']['preserve_source']['content_bytes_read'], size)
        self.assertEqual(stats['phases']['preserve_target']['content_bytes_read'], size)
        self.assertNotIn('content_bytes_read', stats['phases']['fsync'])
        self.assertEqual(stats['phases']['archive_write']['content_bytes_read'], size)
        self.assertEqual(stats['phases']['archive_verify']['content_bytes_read'], size)
        self.assertEqual(stats['phases']['restore_extract']['content_bytes_read'], size)
        self.assertEqual(stats['phases']['restore_verify']['content_bytes_read'], size)
        self.assertEqual(stats['content_bytes_read'], size * 6)
        self.assertEqual(stats['content_files_read'], files * 6)

    @unittest.skipUnless(os.geteuid() == 0, '不同 uid/gid 的恢复需要隔离 root 环境')
    def test_round_trip_preserves_distinct_owners(self):
        for path in self.source.rglob('*'):
            os.chown(path, 10001, 10002)
        expected = self.preserve()
        snapshot.write_archive(self.target, self.path, expected)
        destination = self.base / 'restored'
        snapshot.restore_archive(self.path, expected, destination)
        self.assertEqual((destination / 'data/empty').stat().st_uid, 10001)
        self.assertEqual((destination / 'data/empty').stat().st_gid, 10002)

    def test_repair_removes_only_obsolete_empty_directories(self):
        (self.target / 'obsolete/empty').mkdir(parents=True)
        self.preserve()
        self.assertFalse((self.target / 'obsolete').exists())
        (self.target / 'unexpected').write_text('not-owned-by-copy')
        with self.assertRaisesRegex(snapshot.IntegrityError, 'Unexpected snapshot file'):
            self.preserve()
        self.assertEqual((self.target / 'unexpected').read_text(), 'not-owned-by-copy')

    def test_missing_copied_file_is_not_recreated_or_accepted(self):
        (self.target / '.env').unlink()
        with self.assertRaisesRegex(snapshot.IntegrityError, 'file is missing'):
            self.preserve()

    def test_same_size_wrong_copy_content_is_rejected(self):
        path = self.target / '.env'
        path.write_bytes(b'X' * path.stat().st_size)
        with self.assertRaisesRegex(snapshot.IntegrityError, 'content or metadata'):
            self.preserve()

    def test_source_and_target_symlinks_are_rejected(self):
        for root in (self.source, self.target):
            with self.subTest(root=root):
                linked = root / 'data/link'
                linked.symlink_to(self.base / 'outside')
                with self.assertRaisesRegex(snapshot.IntegrityError, 'symlinks or special'):
                    self.preserve()
                linked.unlink()

    def test_symlink_in_root_parent_is_rejected(self):
        alias = self.base / 'alias'
        alias.symlink_to(self.source, target_is_directory=True)
        with self.assertRaises(OSError):
            snapshot.capture_layout(alias / 'data')
        with self.assertRaises(OSError):
            snapshot.capture_layout(alias)

    def test_special_file_is_rejected_without_blocking_open(self):
        fifo = self.target / 'data/fifo'
        os.mkfifo(fifo)
        with self.assertRaisesRegex(snapshot.IntegrityError, 'symlinks or special'):
            self.preserve()

    def test_source_hardlink_is_rejected_before_metadata_repair(self):
        target = self.target / '.env'
        target.unlink()
        os.link(self.source / '.env', target)
        before = (self.source / '.env').stat()
        with self.assertRaisesRegex(snapshot.IntegrityError, 'shares an inode'):
            self.preserve()
        self.assertEqual(snapshot.identity((self.source / '.env').stat()), snapshot.identity(before))

    def test_capture_detects_same_size_write_with_restored_mtime(self):
        path = self.source / 'data/nested/account.json.enc'
        original = snapshot._hash_fd
        changed = False
        def mutate(descriptor, *args):
            nonlocal changed
            result = original(descriptor, *args)
            if not changed and os.fstat(descriptor).st_ino == path.stat().st_ino:
                changed = True
                before = path.stat()
                path.write_bytes(b'X' * before.st_size)
                os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns))
            return result
        with mock.patch.object(snapshot, '_hash_fd', side_effect=mutate):
            with self.assertRaisesRegex(snapshot.IntegrityError, 'changed while inspected'):
                snapshot.capture_layout(self.source, selected=True)

    def test_capture_detects_late_change_to_already_scanned_file(self):
        path = self.source / '.env'
        original = snapshot._capture
        def mutate(tree, *args, **kwargs):
            result = original(tree, *args, **kwargs)
            if kwargs.get('content', True):
                before = path.stat()
                path.write_bytes(b'X' * before.st_size)
                os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns))
            return result
        with mock.patch.object(snapshot, '_capture', side_effect=mutate):
            with self.assertRaisesRegex(snapshot.IntegrityError, 'changed during capture'):
                snapshot.capture_layout(self.source)

    def test_capture_rejects_parent_replaced_by_symlink_during_walk(self):
        original = snapshot._hash_fd
        renamed = self.base / 'renamed-source'
        changed = False
        def mutate(descriptor, *args):
            nonlocal changed
            result = original(descriptor, *args)
            if not changed:
                changed = True
                self.source.rename(renamed)
                self.source.symlink_to(renamed, target_is_directory=True)
            return result
        with mock.patch.object(snapshot, '_hash_fd', side_effect=mutate):
            with self.assertRaises((OSError, snapshot.IntegrityError)):
                snapshot.capture_layout(self.source)

    def test_durable_seal_refuses_stale_identity_without_content_read(self):
        expected = snapshot.capture_layout(self.target)
        path = self.target / '.env'
        before = path.stat()
        path.write_bytes(b'X' * before.st_size)
        os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns))
        stats = {}
        with self.assertRaisesRegex(snapshot.IntegrityError, 'changed before durable seal'):
            snapshot.fsync_layout(self.target, expected, stats=stats)
        self.assertEqual(stats.get('content_bytes_read', 0), 0)

    def test_preserve_exports_both_captures_for_later_seal_without_hash(self):
        captures = []
        manifest = self.preserve(capture_sink=lambda target, source: captures.extend((target, source)))
        target, source = captures
        self.assertEqual(target.manifest, manifest)
        self.assertTrue(source.selected)
        stats = {}
        snapshot.verify_identity(self.source, source, stats=stats)
        snapshot.verify_identity(self.target, target, stats=stats)
        self.assertEqual(stats.get('content_bytes_read', 0), 0)
        (self.source / 'data/new-file').write_text('unexpected-write')
        with self.assertRaisesRegex(snapshot.IntegrityError, 'identity changed'):
            snapshot.verify_identity(self.source, source)

    def test_source_change_after_copy_is_rejected_before_preserve_returns(self):
        original = snapshot.fsync_layout
        def mutate(*args, **kwargs):
            result = original(*args, **kwargs)
            path = self.source / '.env'
            before = path.stat()
            path.write_bytes(b'X' * before.st_size)
            os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns))
            return result
        with mock.patch.object(snapshot, 'fsync_layout', side_effect=mutate):
            with self.assertRaisesRegex(snapshot.IntegrityError, 'Frozen source changed'):
                self.preserve()

    def test_archive_rejects_content_change_after_freeze_even_with_same_metadata(self):
        expected = self.preserve()
        path = self.target / '.env'
        before = path.stat()
        path.write_bytes(b'X' * before.st_size)
        os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns))
        with self.assertRaisesRegex(snapshot.IntegrityError, 'content changed during archive'):
            snapshot.write_archive(self.target, self.path, expected)

    def test_archive_rejects_write_during_stream_even_if_original_content_was_read(self):
        expected = self.preserve()
        path = self.target / '.env'
        original = snapshot._HashingReader.read
        changed = False
        def mutate(reader, size=-1):
            nonlocal changed
            data = original(reader, size)
            if reader.phase == 'archive_write' and not changed and data == b'fixture-env':
                changed = True
                before = path.stat()
                path.write_bytes(b'X' * before.st_size)
                os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns))
            return data
        with mock.patch.object(snapshot._HashingReader, 'read', mutate):
            with self.assertRaisesRegex(snapshot.IntegrityError, 'changed while inspected'):
                snapshot.write_archive(self.target, self.path, expected)

    def test_bad_archive_owner_mode_and_time_are_rejected(self):
        expected = self.preserve()
        for key in ('uid', 'gid', 'mode', 'mtime'):
            with self.subTest(key=key):
                def alter(name, member):
                    if name == 'data/empty':
                        if key == 'mtime':
                            member.pax_headers['mtime'] = '0.000000001'
                        else:
                            setattr(member, key, getattr(member, key) + 1)
                self.write_fixture_archive(expected, alter=alter)
                with self.assertRaisesRegex(snapshot.IntegrityError, 'metadata differs'):
                    snapshot.verify_archive(self.path, expected)

    def test_missing_extra_duplicate_and_traversal_members_are_rejected(self):
        expected = self.preserve()
        self.write_fixture_archive(expected, alter=lambda name, member: False if name == 'data/empty' else None)
        with self.assertRaisesRegex(snapshot.IntegrityError, 'member set differs'):
            snapshot.verify_archive(self.path, expected)
        for member_name in ('snapshot/unexpected', 'snapshot/data/empty', 'snapshot/../escaped',
                            '/absolute', 'snapshot//extra', 'snapshot/./extra'):
            with self.subTest(member=member_name):
                self.write_fixture_archive(expected, append=lambda archive: archive.addfile(tarfile.TarInfo(member_name)))
                with self.assertRaises(snapshot.IntegrityError):
                    snapshot.verify_archive(self.path, expected)

    def test_archive_links_and_special_members_are_rejected(self):
        expected = self.preserve()
        for kind in (tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.FIFOTYPE, tarfile.CHRTYPE):
            with self.subTest(kind=kind):
                def alter(name, member):
                    if name == 'data/empty':
                        member.type, member.linkname = kind, '/outside'
                self.write_fixture_archive(expected, alter=alter)
                with self.assertRaisesRegex(snapshot.IntegrityError, 'Linked or special'):
                    snapshot.verify_archive(self.path, expected)

    def test_truncated_gzip_and_corrupt_crc_are_rejected(self):
        expected = self.preserve()
        snapshot.write_archive(self.target, self.path, expected)
        raw = self.path.read_bytes()
        for bad in (raw[:-1], raw[:-8], raw[:-8] + bytes([raw[-8] ^ 1]) + raw[-7:]):
            with self.subTest(size=len(bad)):
                self.path.write_bytes(bad)
                with self.assertRaises((EOFError, OSError, tarfile.TarError, snapshot.IntegrityError)):
                    snapshot.verify_archive(self.path, expected)

    def test_hidden_nonzero_data_after_tar_eof_is_rejected(self):
        expected = self.preserve()
        snapshot.write_archive(self.target, self.path, expected)
        data = gzip.decompress(self.path.read_bytes())
        self.path.write_bytes(gzip.compress(data + b'hidden-member'))
        with self.assertRaisesRegex(snapshot.IntegrityError, 'Nonzero data after archive EOF'):
            snapshot.verify_archive(self.path, expected)

    def test_tar_end_markers_are_required_even_with_valid_gzip(self):
        expected = self.preserve()
        snapshot.write_archive(self.target, self.path, expected)
        raw = gzip.decompress(self.path.read_bytes())
        with tarfile.open(fileobj=io.BytesIO(raw), mode='r:') as archive:
            members = list(archive)
        last = members[-1]
        offset = last.offset_data + ((last.size + 511) // 512) * 512
        for padding in (0, 512):
            with self.subTest(padding=padding):
                self.path.write_bytes(gzip.compress(raw[:offset + padding]))
                with self.assertRaisesRegex(snapshot.IntegrityError, 'end marker'):
                    snapshot.verify_archive(self.path, expected)

    def test_multiple_archive_buffers_and_chunk_boundary_members_round_trip(self):
        for size in (512, snapshot.CHUNK, snapshot.CHUNK + 17):
            raw = os.urandom(size)
            (self.source / 'data' / str(size)).write_bytes(raw)
            (self.target / 'data' / str(size)).write_bytes(raw)
        expected = self.preserve()
        snapshot.write_archive(self.target, self.path, expected)
        self.assertGreater(self.path.stat().st_size, snapshot.CHUNK)
        snapshot.restore_archive(self.path, expected, self.base / 'restored')

    def test_corrupt_content_is_rejected_during_restore(self):
        expected = self.preserve()
        wanted = copy.deepcopy(expected)
        wanted['.env']['sha256'] = '0' * 64
        snapshot.write_archive(self.target, self.path, expected)
        with self.assertRaisesRegex(snapshot.IntegrityError, 'content differs'):
            snapshot.restore_archive(self.path, wanted, self.base / 'restored')

    def test_restore_verifies_actual_disk_bytes_after_extraction(self):
        expected = self.preserve()
        snapshot.write_archive(self.target, self.path, expected)
        destination = self.base / 'restored'
        original = snapshot._apply_metadata
        changed = False
        def mutate(descriptor, wanted, *args):
            nonlocal changed
            result = original(descriptor, wanted, *args)
            if not changed and wanted['type'] == 'file':
                changed = True
                path = destination / 'data/nested/account.json.enc'
                before = path.stat()
                path.write_bytes(b'X' * before.st_size)
                os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns))
            return result
        with mock.patch.object(snapshot, '_apply_metadata', side_effect=mutate):
            with self.assertRaisesRegex(snapshot.IntegrityError, 'Restored layout differs'):
                snapshot.restore_archive(self.path, expected, destination)

    def test_restore_does_not_follow_injected_parent_symlink(self):
        expected = self.preserve()
        snapshot.write_archive(self.target, self.path, expected)
        destination = self.base / 'restored'
        outside = self.base / 'outside'
        outside.mkdir()
        original = snapshot._consume_archive
        def inject(path, wanted, checkpoint, stats, phase, consume=None):
            if consume is not None:
                (destination / 'data').symlink_to(outside, target_is_directory=True)
            return original(path, wanted, checkpoint, stats, phase, consume)
        with mock.patch.object(snapshot, '_consume_archive', side_effect=inject):
            with self.assertRaisesRegex(snapshot.IntegrityError, 'Restore parent is not a real directory'):
                snapshot.restore_archive(self.path, expected, destination)
        self.assertEqual(list(outside.iterdir()), [])

    def test_no_overwrite_and_no_archive_inside_snapshot(self):
        expected = self.preserve()
        snapshot.write_archive(self.target, self.path, expected)
        with self.assertRaises(FileExistsError):
            snapshot.write_archive(self.target, self.path, expected)
        with self.assertRaisesRegex(snapshot.IntegrityError, 'outside snapshot'):
            snapshot.write_archive(self.target, self.target / 'new.tar.gz', expected)
        with self.assertRaises(FileExistsError):
            snapshot.restore_archive(self.path, expected, self.target)

    def test_checkpoint_failure_stops_without_claiming_success(self):
        expected = self.preserve()
        calls = 0
        def abort():
            nonlocal calls
            calls += 1
            if calls == 12:
                raise TimeoutError('fence budget exceeded')
        with self.assertRaisesRegex(TimeoutError, 'fence budget'):
            snapshot.write_archive(self.target, self.path, expected, checkpoint=abort)


if __name__ == '__main__':
    unittest.main()
