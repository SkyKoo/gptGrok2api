"""固定缓存删除的离线安全回归；不启动浏览器、不接触真实账号目录。"""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('profile_cache_helper', Path(__file__).with_name('profile-cache-helper.py'))
cache = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cache)


class ProfileCacheTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='cfm-cache-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.run = self.root / 'profile-1'
        self.browser = self.run / 'browser'
        self.browser.mkdir(parents=True, mode=0o700)
        self.run.chmod(0o700)
        self.state = self.run / 'state.json'
        self.state.write_bytes(b'{"submission":"preserve","token":"fixture"}')
        self.lock = None
        for target, value in [('sys.platform', 'linux'), ('mountpoints', lambda: []), ('no_browser_references', lambda *_a, **_k: None)]:
            owner, name = (cache.sys, 'platform') if target == 'sys.platform' else (cache, target)
            p = patch.object(owner, name, value)
            p.start(); self.addCleanup(p.stop)
        self.addCleanup(self.close_lock)

    def close_lock(self):
        if self.lock is not None:
            os.close(self.lock); self.lock = None

    def lock_profile(self):
        self.lock = os.open(self.run / '.lock', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.write(self.lock, b'fixture-lock')

    def put(self, relative, data=b'cache-data'):
        target = self.browser / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        return target

    def apply(self, **kwargs):
        if self.lock is None:
            self.lock_profile()
        return cache.maintain(self.run, apply=True, lock_fd=self.lock, **kwargs)

    def test_only_exact_cache_roots_removed_and_session_bytes_preserved(self):
        saved = {}
        for name in ['Local State', 'Default/Cookies', 'Default/Preferences', 'Default/Web Data',
                     'Default/Local Storage/leveldb/000001.log', 'Default/Session Storage/000001.log',
                     'Default/IndexedDB/auth.db', 'Default/Sessions/session', 'Default/Service Worker/CacheStorage/item',
                     'Default/NewUnknownCache/item', 'Extensions/Cache/item']:
            target = self.put(name, b'session-fixture')
            saved[target] = target.read_bytes()
        for name in cache.CACHE_DIRS:
            self.put(name + '/nested/entry', b'0123456789')
        original = self.state.read_bytes()
        result = self.apply()
        self.assertEqual(result, dict(status='cleaned', code='cache_removed', files=8, bytes=80, removedFiles=8, removedBytes=80))
        self.assertEqual(self.state.read_bytes(), original)
        self.assertTrue((self.run / '.lock').exists())
        for name in cache.CACHE_DIRS:
            self.assertFalse((self.browser / name).exists())
        for target, content in saved.items():
            self.assertEqual(target.read_bytes(), content)

    def test_report_preserves_every_byte_and_creates_no_lock(self):
        target = self.put('Default/Cache/item')
        before = {p: (p.stat().st_mtime_ns, p.read_bytes()) for p in [target, self.state]}
        result = cache.maintain(self.run)
        self.assertEqual((result['status'], result['files'], result['removedFiles']), ('eligible', 1, 0))
        self.assertFalse((self.run / '.lock').exists())
        self.assertEqual(before, {p: (p.stat().st_mtime_ns, p.read_bytes()) for p in before})

    def test_busy_report_never_opens_cache(self):
        self.put('Default/Cache/item'); self.lock_profile()
        with patch.object(cache, 'scan_tree', side_effect=AssertionError('must not inspect')):
            self.assertEqual(cache.maintain(self.run)['code'], 'profile_busy')

    def test_second_unsafe_tree_rejects_all_before_first_delete(self):
        first = self.put('Default/Cache/entry')
        outside = self.root / 'keep'; outside.write_bytes(b'outside')
        second = self.browser / 'Default/Code Cache'; second.mkdir()
        (second / 'link').symlink_to(outside)
        result = self.apply()
        self.assertEqual((result['status'], result['code'], result['removedFiles']), ('blocked', 'unsafe_cache_entry', 0))
        self.assertEqual(first.read_bytes(), b'cache-data')
        self.assertEqual(outside.read_bytes(), b'outside')

    def test_parent_symlink_and_cache_root_symlink_are_refused(self):
        target = self.put('Default/Cache/entry')
        alias = self.root / 'alias'; alias.symlink_to(self.root, target_is_directory=True)
        self.assertEqual(cache.maintain(alias / self.run.name)['status'], 'blocked')
        (self.browser / 'ShaderCache').symlink_to(target.parent, target_is_directory=True)
        result = self.apply()
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(result['removedFiles'], 0)
        self.assertTrue(target.exists())

    def test_special_file_mount_and_permissions_each_refuse(self):
        target = self.put('Default/Cache/entry')
        fifo = target.parent / 'fifo'; os.mkfifo(fifo)
        self.assertEqual(self.apply()['code'], 'unsafe_cache_entry')
        fifo.unlink()
        with patch.object(cache, 'mountpoints', return_value=[target.parent]):
            self.assertEqual(self.apply()['code'], 'profile_contains_mount')
        self.browser.chmod(0o755)
        self.assertEqual(self.apply()['code'], 'profile_permissions_unsafe')
        self.assertTrue(target.exists())

    def test_singleton_artifacts_are_never_assumed_stale(self):
        target = self.put('Default/Cache/entry')
        for name in ('SingletonLock', 'SingletonSocket', 'SingletonCookie'):
            singleton = self.browser / name
            singleton.symlink_to('/nonexistent-fixture-target')
            result = self.apply()
            self.assertEqual(result['code'], 'browser_singleton_present')
            self.assertEqual(result['removedFiles'], 0)
            singleton.unlink()
        self.assertTrue(target.exists())

    def test_cross_device_tree_is_refused(self):
        target = self.put('Default/Cache/entry')
        fd = cache.open_directory(target.parent)
        try:
            with self.assertRaisesRegex(cache.Refused, 'cache_filesystem_changed'):
                cache.scan_tree(fd, os.fstat(fd).st_dev + 1, float('inf'))
        finally:
            os.close(fd)

    def test_replaced_lock_is_preserved_and_prevents_deletion(self):
        target = self.put('Default/Cache/entry')
        self.lock_profile()
        def replace_lock(*_a, **_k):
            (self.run / '.lock').unlink()
            (self.run / '.lock').write_bytes(b'new-owner')
        with patch.object(cache, 'no_browser_references', side_effect=replace_lock):
            result = self.apply()
        self.assertEqual((result['status'], result['code']), ('blocked', 'profile_lock_changed'))
        self.assertEqual((self.run / '.lock').read_bytes(), b'new-owner')
        self.assertTrue(target.exists())

    def test_mutation_after_scan_is_refused(self):
        target = self.put('Default/Cache/entry')
        with patch.object(cache, 'no_browser_references', side_effect=lambda *_a, **_k: target.write_bytes(b'changed')):
            result = self.apply()
        self.assertEqual(result['code'], 'cache_tree_changed')
        self.assertEqual(result['removedFiles'], 0)
        self.assertEqual(target.read_bytes(), b'changed')

    def test_partial_failure_retains_exact_confirmed_counts(self):
        first = self.put('Default/Cache/a', b'abc')
        second = self.put('Default/Cache/b', b'defgh')
        original = os.unlink
        def unlink(name, *args, **kwargs):
            if name == 'b':
                raise cache.Refused('cache_cleanup_budget')
            return original(name, *args, **kwargs)
        with patch.object(cache.os, 'unlink', side_effect=unlink):
            result = self.apply()
        self.assertEqual((result['status'], result['code'], result['files'], result['bytes']), ('partial', 'cache_cleanup_budget', 2, 8))
        self.assertEqual((result['removedFiles'], result['removedBytes']), (1, 3))
        self.assertFalse(first.exists()); self.assertTrue(second.exists())
        resumed = self.apply()
        self.assertEqual((resumed['status'], resumed['removedFiles'], resumed['removedBytes']), ('cleaned', 1, 5))

    def test_progress_is_bounded_and_contains_no_paths_or_credentials(self):
        for index in range(65):
            self.put(f'Default/Cache/{index:02}', b'x')
        rows = []
        result = self.apply(progress=lambda row: rows.append(dict(row)))
        self.assertEqual(result['removedFiles'], 65)
        self.assertEqual([row['removedFiles'] for row in rows], [64, 65])
        self.assertTrue(all(set(row) == {'status', 'code', 'files', 'bytes', 'removedFiles', 'removedBytes'} for row in rows))


class ProcessReferenceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='cfm-cache-proc-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.browser = self.root / 'browser'; self.browser.mkdir()
        self.proc = self.root / 'proc'; self.proc.mkdir()
        self.pid = self.proc / '123456789'; self.pid.mkdir()
        (self.pid / 'fd').mkdir()
        (self.pid / 'cmdline').write_bytes(b'fixture\0')
        p = patch.object(cache.sys, 'platform', 'linux'); p.start(); self.addCleanup(p.stop)

    def check(self, **kwargs):
        return cache.no_browser_references(self.browser, proc=self.proc, **kwargs)

    def test_user_data_dir_is_detected_in_current_namespace(self):
        (self.pid / 'cmdline').write_bytes(b'fixture\0--user-data-dir\0' + os.fsencode(self.browser) + b'\0')
        with self.assertRaisesRegex(cache.Refused, 'browser_still_running'):
            self.check()

    def test_container_argv_is_resolved_through_process_root(self):
        namespace = self.root / 'namespace'; (namespace / 'data').mkdir(parents=True)
        (namespace / 'data/browser').symlink_to(self.browser, target_is_directory=True)
        (self.pid / 'root').symlink_to(namespace, target_is_directory=True)
        (self.pid / 'cmdline').write_bytes(b'fixture\0--user-data-dir=/data/browser\0')
        with self.assertRaisesRegex(cache.Refused, 'browser_still_running'):
            self.check()

    def test_fd_hardlink_is_detected_by_inode(self):
        item = self.browser / 'item'; item.write_bytes(b'fixture')
        alias = self.root / 'outside-name'; os.link(item, alias)
        (self.pid / 'fd/7').symlink_to(alias)
        info = item.stat()
        with self.assertRaisesRegex(cache.Refused, 'browser_still_running'):
            self.check(protected={(info.st_dev, info.st_ino)})

    def test_inaccessible_process_refuses_but_disappeared_process_is_safe(self):
        original = Path.read_bytes
        def denied(path):
            if path == self.pid / 'cmdline':
                raise PermissionError('fixture denied')
            return original(path)
        with patch.object(Path, 'read_bytes', denied):
            with self.assertRaisesRegex(cache.Refused, 'process_check_unavailable'):
                self.check()
        (self.pid / 'cmdline').unlink()
        self.check()


if __name__ == '__main__':
    unittest.main()
