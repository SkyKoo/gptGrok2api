"""Project package installation and post-success scheduling without Docker."""
import copy
import fcntl
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import types
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location('rolling_backup_local', Path(__file__).with_name('rolling-deploy.py'))
r = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(r)


class BackupLocalTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        (self.root/'maintenance.lock').touch()
        (self.root/'tools').mkdir()
        (self.root/'tools/backup-core.py').write_text('# core helper\n')
        self.files = {name: ('# '+name+'\n').encode() for name in r.BACKUP_LOCAL_FILES}
        self.manifest = {'format': 'ogg-backup-local-package-v1', 'service': 'gptgrok2api',
                         'files': {name: r.digest(raw) for name, raw in self.files.items()}}
        self.prepared = {'files': self.files, 'manifest': self.manifest,
                         'package_id': r.digest(r.canonical_json(self.manifest))}
        self.release = {'source': str(self.root), 'commit': 'a'*40,
                        'backup_local_sha256': self.prepared['package_id']}

    def host(self, manifest=None, files=None):
        contents = {**(files or self.files), 'manifest.json': json.dumps(manifest or self.manifest).encode()}
        host = Mock()
        host.run.side_effect = lambda args: contents[args[-1].rsplit('/', 1)[1]]
        return host

    def test_reads_every_file_from_same_fixed_commit(self):
        host = self.host()
        self.assertEqual(r.backup_local_bytes(host, self.release), self.prepared)
        self.assertEqual(len(host.run.call_args_list), 5)
        self.assertTrue(all(call.args[0][-1].startswith('a'*40+':deploy/backup/local/')
                            for call in host.run.call_args_list))

    def test_missing_file_or_other_service_rejected(self):
        for change in ('missing', 'service'):
            manifest = copy.deepcopy(self.manifest)
            if change == 'missing':
                manifest['files'].pop('oracle-backup-daily.py')
            else:
                manifest['service'] = 'lumora'
            with self.assertRaises(r.ReleaseError):
                r.backup_local_bytes(self.host(manifest), self.release)

    def test_wrong_bytes_or_prepared_manifest_rejected(self):
        files = dict(self.files, **{'backup_retention.py': b'# unreviewed\n'})
        with self.assertRaisesRegex(r.ReleaseError, 'differs'):
            r.backup_local_bytes(self.host(files=files), self.release)
        with self.assertRaisesRegex(r.ReleaseError, 'prepared release'):
            r.backup_local_bytes(self.host(), dict(self.release, backup_local_sha256='b'*64))

    def test_installs_complete_version_before_current_switch(self):
        result = r.install_backup_local(self.root, self.prepared)
        current = self.root/'tools/backup-local/current'
        target = current.resolve()
        self.assertTrue(current.is_symlink())
        self.assertEqual(target.name, result['package_id'])
        self.assertEqual((self.root/'backup-policy').stat().st_mode & 0o777, 0o700)
        self.assertEqual({p.name for p in target.iterdir()},
                         set(r.BACKUP_LOCAL_FILES) | {'manifest.json', 'dependencies.json'})
        config = json.loads((target/'dependencies.json').read_text())
        self.assertEqual(config['services']['gptgrok2api']['helpers'], {
            str(self.root/'tools/backup-core.py'): r.file_digest(self.root/'tools/backup-core.py')})

    def test_reinstall_checks_immutable_code_and_refreshes_helper_pin(self):
        r.install_backup_local(self.root, self.prepared)
        target = (self.root/'tools/backup-local/current').resolve()
        (self.root/'tools/backup-core.py').write_text('# updated reviewed helper\n')
        r.install_backup_local(self.root, self.prepared)
        config = json.loads((target/'dependencies.json').read_text())
        self.assertEqual(config['services']['gptgrok2api']['helpers'][str(self.root/'tools/backup-core.py')],
                         r.file_digest(self.root/'tools/backup-core.py'))
        (target/'backup_retention.py').write_text('# modified\n')
        with self.assertRaisesRegex(r.ReleaseError, 'immutable'):
            r.install_backup_local(self.root, self.prepared)

    def test_configured_package_cannot_silently_disappear(self):
        r.install_backup_local(self.root, self.prepared)
        with self.assertRaisesRegex(r.ReleaseError, '--source'):
            r.prepare_backup_local_source(self.host(), self.root, None, 'a'*40)
        with self.assertRaisesRegex(r.ReleaseError, 'missing'):
            r.prepare_backup_local_source(self.host(), self.root, self.root, 'a'*40)

    def test_failed_atomic_install_keeps_previous_current(self):
        r.install_backup_local(self.root, self.prepared)
        current = self.root/'tools/backup-local/current'
        before = current.resolve()
        wrong = copy.deepcopy(self.prepared)
        wrong['files']['backup_retention.py'] = b'# differs\n'
        with self.assertRaises(r.ReleaseError):
            r.install_backup_local(self.root, wrong)
        self.assertEqual(current.resolve(), before)

    def test_linked_state_directory_is_refused_before_switching_current(self):
        outside = self.root/'unrelated'
        outside.mkdir()
        (self.root/'backup-policy').symlink_to(outside)
        with self.assertRaisesRegex(r.ReleaseError, 'ordinary directory'):
            r.install_backup_local(self.root, self.prepared)
        self.assertFalse((self.root/'tools/backup-local/current').is_symlink())

    def test_trigger_only_queues_service_and_failure_does_not_rollback(self):
        r.install_backup_local(self.root, self.prepared)
        with patch.object(r.subprocess, 'run', return_value=types.SimpleNamespace(returncode=0)) as run:
            result = r.trigger_backup_local(self.root, 'a'*40)
        self.assertEqual(result['status'], 'queued')
        self.assertEqual(run.call_args.args[0], ['systemctl', 'start', '--no-block', r.BACKUP_LOCAL_SERVICE])
        with patch.object(r.subprocess, 'run', side_effect=subprocess.TimeoutExpired('systemctl', 20)):
            self.assertEqual(r.trigger_backup_local(self.root, 'a'*40)['status'], 'deferred')

    def invoke_main(self, deploy, trigger):
        release = self.root/'release.json'
        release.write_text(json.dumps({'commit': 'a'*40}))
        argv = ['rolling-deploy.py', '--root', str(self.root), 'deploy', '--release', str(release)]
        with patch('sys.argv', argv), patch.object(r.os, 'geteuid', return_value=0), \
                patch.object(r.os, 'uname', return_value=types.SimpleNamespace(sysname='Linux', machine='aarch64')), \
                patch.object(r.signal, 'signal'), patch.object(r.RollingRelease, 'deploy', deploy), \
                patch.object(r, 'trigger_backup_local', trigger):
            r.main()

    def test_post_success_trigger_runs_after_maintenance_lock_release(self):
        deploy = Mock()
        def trigger(root, commit):
            deploy.assert_called_once()
            with (root/'maintenance.lock').open('r+') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return {'status': 'queued'}
        self.invoke_main(deploy, Mock(side_effect=trigger))

    def test_failed_release_never_triggers_retention(self):
        trigger = Mock()
        with self.assertRaises(r.ReleaseError):
            self.invoke_main(Mock(side_effect=r.ReleaseError('verification failed')), trigger)
        trigger.assert_not_called()

    def ready_fixture(self):
        backup = self.root/'backups/release'
        backup.mkdir(parents=True)
        (backup/'complete').write_text('{}')
        current = {'status': 'healthy', 'commit': 'a'*40, 'image_id': 'sha256:app',
                   'container': 'writer', 'frontdoor': 'front', 'upstream': 'http://writer:8080',
                   'backup': str(backup)}
        (self.root/'current.json').write_text(json.dumps(current))
        (self.root/'rolling-status.json').write_text(json.dumps({
            'status': 'success', 'phase': 'success', 'backup': str(backup)}))
        host = Mock()
        host.control.return_value = {'protocol': r.PROTOCOL, 'phase': 'active', 'paused': False, 'writer_lease': True}
        host.gateway.return_value = {'route': {'held': False, 'upstream': current['upstream']}}
        host.inspect.return_value = {'Image': 'sha256:app', 'State': {'Health': {'Status': 'healthy'}}}
        return backup, host, {'commit': 'a'*40, 'image_id': 'sha256:app'}

    def test_final_verification_publishes_ready_marker(self):
        backup, host, release = self.ready_fixture()
        r.verify_backup_local_ready(host, self.root, release)
        marker = json.loads((backup/'retention-ready.json').read_text())
        self.assertEqual((marker['status'], marker['commit'], marker['backup']),
                         ('verified', 'a'*40, str(backup)))
        self.assertTrue(marker['writer_verified'] and marker['route_verified'])

    def test_failed_final_writer_verification_does_not_admit_cleanup(self):
        backup, host, release = self.ready_fixture()
        host.control.return_value['writer_lease'] = False
        with self.assertRaisesRegex(r.ReleaseError, 'retention remains blocked'):
            r.verify_backup_local_ready(host, self.root, release)
        self.assertFalse((backup/'retention-ready.json').exists())


if __name__ == '__main__':
    unittest.main()
