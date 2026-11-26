"""Offline package and atomic pointer fault tests; no SSH, sudo or production."""
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


HERE = Path(__file__).resolve().parent
INSTALLER = HERE / 'install-paired.sh'
PROGRAM = INSTALLER.read_text().split("<<'PY_PAIRED_INSTALL'\n", 1)[1].split('\nPY_PAIRED_INSTALL', 1)[0]
implementation = {'__name__': 'paired_installer_test'}
exec(compile(PROGRAM, str(INSTALLER), 'exec'), implementation)


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.source = self.root / 'source'
        (self.source / 'paired').mkdir(parents=True)
        for name in implementation['FILES']:
            (self.source / name).write_bytes(b'pass\n' if name.endswith('.py') else b'documentation\n')
        self.destination = self.root / 'installed'

    def bundle(self, label):
        archive = self.root / (label + '.tar.gz')
        receipt = implementation['package'](self.source, archive)
        staging = self.root / (label + '-staging')
        staging.mkdir(mode=0o700)
        with archive.open('rb') as stream:
            implementation['unpack'](stream, staging, receipt['content_sha256'])
        return archive, staging, receipt['content_sha256']

    def install(self, staging, version):
        return implementation['install_version'](staging, self.destination, version, os.geteuid())

    def test_real_offline_package_help_and_no_overwrite(self):
        archive = self.root / 'real.tar.gz'
        run = subprocess.run(['bash', str(INSTALLER), '--package', str(archive)], capture_output=True,
                             text=True, timeout=15)
        self.assertEqual(run.returncode, 0, run.stderr)
        receipt = json.loads(run.stdout)
        staging = self.root / 'real-staging'
        staging.mkdir(mode=0o700)
        with archive.open('rb') as stream:
            files = implementation['unpack'](stream, staging, receipt['content_sha256'])
        self.assertEqual(set(files), implementation['EXPECTED'])
        self.assertFalse(receipt['deployment_started'])
        again = subprocess.run(['bash', str(INSTALLER), '--package', str(archive)],
                               capture_output=True, timeout=15)
        self.assertNotEqual(again.returncode, 0)
        help_result = subprocess.run(['bash', str(INSTALLER), '--help'], capture_output=True,
                                     text=True, timeout=5)
        self.assertEqual(help_result.returncode, 0)
        self.assertIn('SSH别名 运行根目录', help_result.stdout)

    def test_installs_verified_versions_and_keeps_previous(self):
        _, staging, first = self.bundle('first')
        receipt = self.install(staging, first)
        self.assertIsNone(receipt['previous'])
        self.assertEqual(os.readlink(self.destination / 'current'), 'versions/' + first)
        implementation['verify_tree'](self.destination / 'versions' / first, first, os.geteuid())
        (self.source / 'PAIRED.md').write_text('revised documentation\n')
        _, staging, second = self.bundle('second')
        self.assertNotEqual(first, second)
        receipt = self.install(staging, second)
        self.assertEqual(receipt['previous'], 'versions/' + first)
        self.assertEqual(os.readlink(self.destination / 'current'), 'versions/' + second)
        self.assertTrue((self.destination / 'versions' / first).is_dir())

    def test_upgrade_accepts_verified_previous_package_with_retired_file(self):
        _, staging, first = self.bundle('first')
        self.install(staging, first)
        old_target = self.destination / 'versions' / first
        retired = old_target / 'paired' / 'rehearse.py'
        retired.write_bytes(b'pass\n')
        manifest_path = old_target / implementation['MANIFEST']
        manifest = json.loads(manifest_path.read_text())
        old_names = list(manifest['files']) + ['paired/rehearse.py']
        old_files = {name: (old_target / name).read_bytes() for name in old_names}
        old_manifest = implementation['manifest_for'](old_files, old_names)
        manifest_path.write_text(json.dumps(old_manifest, sort_keys=True, indent=2) + '\n')
        retired_version = old_manifest['content_sha256']
        retired_target = self.destination / 'versions' / retired_version
        old_target.rename(retired_target)
        (self.destination / 'current').unlink()
        (self.destination / 'current').symlink_to('versions/' + retired_version)
        (self.source / 'PAIRED.md').write_text('new version\n')
        _, staging, second = self.bundle('second')
        receipt = self.install(staging, second)
        self.assertEqual(receipt['previous'], 'versions/' + retired_version)
        self.assertEqual(os.readlink(self.destination / 'current'), 'versions/' + second)

    def test_corrupt_current_version_preserves_pointer(self):
        _, staging, first = self.bundle('first')
        self.install(staging, first)
        (self.destination / 'versions' / first / 'PAIRED.md').write_text('corrupt\n')
        (self.source / 'PAIRED.md').write_text('new version\n')
        _, staging, second = self.bundle('second')
        with self.assertRaisesRegex(RuntimeError, 'manifest or content digest differs'):
            self.install(staging, second)
        self.assertEqual(os.readlink(self.destination / 'current'), 'versions/' + first)
        self.assertFalse((self.destination / 'versions' / second).exists())

    def test_tampered_staging_or_bad_digest_never_creates_pointer(self):
        _, staging, version = self.bundle('input')
        with self.assertRaisesRegex(RuntimeError, 'manifest or content digest differs'):
            self.install(staging, '0' * 64)
        (staging / 'paired-deploy.py').write_text('raise RuntimeError("unreviewed")\n')
        with self.assertRaisesRegex(RuntimeError, 'manifest or content digest differs'):
            self.install(staging, version)
        self.assertFalse((self.destination / 'current').exists())

    def test_damaged_existing_target_does_not_switch_valid_current(self):
        _, staging, first = self.bundle('first')
        self.install(staging, first)
        (self.source / 'PAIRED.md').write_text('new version\n')
        _, staging, second = self.bundle('second')
        target = self.destination / 'versions' / second
        target.mkdir(mode=0o755)
        (target / 'paired').mkdir(mode=0o755)
        with self.assertRaisesRegex(RuntimeError, 'Unexpected or missing'):
            self.install(staging, second)
        self.assertEqual(os.readlink(self.destination / 'current'), 'versions/' + first)

    def test_unsafe_tar_members_fail_before_install(self):
        for name, kind in (('../escape', tarfile.REGTYPE),
                           ('paired-deploy.py', tarfile.SYMTYPE),
                           ('paired-deploy.py', tarfile.LNKTYPE),
                           ('paired-deploy.py', tarfile.CHRTYPE)):
            with self.subTest(name=name, kind=kind):
                raw = io.BytesIO()
                with tarfile.open(fileobj=raw, mode='w:gz') as archive:
                    member = tarfile.TarInfo(name)
                    member.type = kind
                    member.linkname = 'outside' if kind in (tarfile.SYMTYPE, tarfile.LNKTYPE) else ''
                    archive.addfile(member)
                raw.seek(0)
                with tempfile.TemporaryDirectory(dir=self.root) as staging:
                    with self.assertRaisesRegex(RuntimeError, 'unexpected, linked, duplicate'):
                        implementation['unpack'](raw, Path(staging), '0' * 64)
        self.assertFalse((self.root / 'escape').exists())
        self.assertFalse(self.destination.exists())


if __name__ == '__main__':
    unittest.main()
