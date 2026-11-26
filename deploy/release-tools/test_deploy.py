"""验证发布失败时的程序回滚与数据写入保护，不启动 Docker。"""
import json
import os
import time
import io
from pathlib import Path
import tempfile
import unittest
from contextlib import redirect_stdout

from deploy import Deployment, DeploymentError


class FakeDeployment(Deployment):
    def __init__(self, root):
        super().__init__(root)
        self.events = []
        self.activity = {'known': True, 'active': False}

    def compose(self, image, *args, **kwargs):
        self.events.append(('compose', image, args))
        return ''

    def start_services(self, image, worker_image=None):
        self.events.append(('start', image, worker_image))

    def active_snapshot(self):
        return self.activity


class RollbackTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        root.mkdir(exist_ok=True)
        self.app = FakeDeployment(root)
        self.app.env_file.write_text('GPTGROK2API_IMAGE=old:image\nSECRET=preserve\n')
        self.app.config.write_text('services: {old: {}}\n')
        self.app.state_file.write_text(json.dumps({'image': 'old:image', 'commit': 'old'}))
        self.app.data.mkdir()
        self.app.logs.mkdir()
        self.app.redis_data.mkdir()
        (root / 'postgres-data').mkdir()
        self.app.backups.mkdir()
        self.app.old_env = self.app.env_file.read_bytes()
        self.app.old_config = self.app.config.read_bytes()
        self.app.old_current = self.app.state_file.read_bytes()
        self.app.old_image = 'old:image'
        self.app.new_image = 'new:image'
        self.app.stopped = True
        self.app.switched = True
        self.app.health_verified = True
        self.app.backup = self.app.backups / 'release'
        self.app.backup.mkdir()

    def test_switch_failure_restores_program_without_restoring_data(self):
        self.app.env_file.write_text('GPTGROK2API_IMAGE=new:image\nSECRET=preserve\n')
        self.app.config.write_text('services: {new: {}}\n')
        (self.app.data / 'new-write.txt').write_text('keep this evidence')
        result = {}
        self.app.phase = 'switch'
        self.app.safe_rollback_after_failure(RuntimeError('healthcheck failed'), result)
        self.assertEqual(result['status'], 'rolled_back')
        self.assertEqual(self.app.env_file.read_bytes(), self.app.old_env)
        self.assertEqual(self.app.config.read_bytes(), self.app.old_config)
        self.assertEqual((self.app.data / 'new-write.txt').read_text(), 'keep this evidence')
        self.assertTrue(any(event[0] == 'start' and event[1] == 'old:image' for event in self.app.events))

    def test_verify_failure_with_unknown_activity_defers_rollback(self):
        self.app.phase = 'verify'
        self.app.activity = {'known': False, 'active': True, 'error': 'api unavailable'}
        result = {}
        self.app.safe_rollback_after_failure(RuntimeError('data check failed'), result)
        self.assertEqual(result['status'], 'rollback_deferred')
        self.assertFalse(any(event[0] == 'start' for event in self.app.events))


class ProgressTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.app = FakeDeployment(Path(self.temp.name))

    def test_progress_file_records_phases_and_final_status(self):
        self.app.begin_operation('abc123', 'gptgrok2api:sha-abc123')
        self.app.set_phase('backup', '制作一致备份')
        self.app.finish_operation('success', finished_at='2026-10-28T00:00:00+00:00')

        progress = json.loads(self.app.progress_file.read_text())
        self.assertEqual(progress['status'], 'success')
        self.assertEqual(progress['phase'], 'backup')
        self.assertEqual(progress['commit'], 'abc123')
        self.assertIn('preflight', progress['phase_durations_seconds'])
        self.assertEqual(progress['finished_at'], '2026-10-28T00:00:00+00:00')

    def test_status_can_read_running_progress_without_lock(self):
        self.app.begin_operation('abc123', 'gptgrok2api:sha-abc123')
        output = io.StringIO()
        with redirect_stdout(output):
            self.app.status()
        result = json.loads(output.getvalue())
        self.assertEqual(result['operation']['status'], 'running')
        self.assertTrue(result['operation']['pid_alive'])
        self.assertIn('compose_ps', result)


class ImageRetentionManifestTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.app = Deployment(Path(self.temp.name))
        self.app.before_app = {'Config': {'Env': ['GO_IMAGE_RETENTION_DAYS=2']}}
        self.app.configure_retention_cleanup()

    def file(self, name, age):
        path = self.app.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(b'fixture')
        stamp = time.time() - age * 86400
        os.utime(path, (stamp, stamp))
        return path

    def test_only_previously_expired_images_can_disappear(self):
        for name in ('data/files/images/old.png', 'data/files/images/old.png.meta.json', 'data/log_input_images/old.jpeg'):
            self.file(name, 3)
            entry = self.app.file_manifest()[name]
            self.assertTrue(self.app.allowed_manifest_change(name, entry, None))
            self.assertFalse(self.app.allowed_manifest_change(name, entry, {'sha256': 'changed'}))
            self.assertFalse(self.app.allowed_manifest_change(name, None, entry))

    def test_recent_images_and_unrelated_data_remain_protected(self):
        for name, age in [('data/files/images/new.png', 1.5), ('data/log_input_images/new.png', 1.5), ('data/account-secret.json', 10), ('data/files/videos/old.mp4', 10)]:
            self.file(name, age)
            entry = self.app.file_manifest()[name]
            self.assertFalse(self.app.allowed_manifest_change(name, entry, None))

    def test_unknown_runtime_configuration_disables_allowance(self):
        name = 'data/files/images/old.png'
        self.file(name, 10)
        for env in (['GO_IMAGE_RETENTION_DAYS=invalid'], ['GROK_ROOT_DIR=/other'], ['GO_IMAGE_DATA_DIR=/other/images']):
            self.app.before_app = {'Config': {'Env': env}}
            self.app.configure_retention_cleanup()
            self.assertFalse(self.app.allowed_manifest_change(name, self.app.file_manifest()[name], None))

    def test_manifest_uses_fixed_cutoff_and_does_not_follow_symlinks(self):
        name = 'data/files/images/old.png'
        path = self.file(name, 3)
        link = path.parent / 'alias.png'
        link.symlink_to(path)
        before = self.app.file_manifest()
        self.assertFalse(self.app.allowed_manifest_change('data/files/images/alias.png', before['data/files/images/alias.png'], None))
        self.assertEqual(before, self.app.file_manifest())
        self.app.retention_cleanup_cutoff = path.stat().st_mtime - 1
        self.assertFalse(self.app.allowed_manifest_change(name, self.app.file_manifest()[name], None))


class MaintenanceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.app = Deployment(Path(self.temp.name))
        self.app.data.mkdir()

    def test_owned_marker_is_released_and_not_in_business_manifest(self):
        self.app.enter_maintenance()
        self.assertTrue(self.app.owns_maintenance())
        self.assertNotIn('data/cfm-maintenance', self.app.file_manifest())
        self.app.leave_maintenance()
        self.assertFalse((self.app.data / 'cfm-maintenance').exists())

    def test_external_marker_is_never_overwritten_or_deleted(self):
        marker = self.app.data / 'cfm-maintenance'
        marker.write_text('operator')
        with self.assertRaises(DeploymentError):
            self.app.enter_maintenance()
        self.app.leave_maintenance()
        self.assertEqual(marker.read_text(), 'operator')

    def test_replaced_marker_is_retained(self):
        self.app.enter_maintenance()
        marker = self.app.data / 'cfm-maintenance'
        marker.write_text('other-operation')
        with self.assertRaises(DeploymentError):
            self.app.leave_maintenance()
        self.assertEqual(marker.read_text(), 'other-operation')

    def test_exception_cleans_own_marker_but_deferred_rollback_keeps_it(self):
        def fail(*args):
            self.app.enter_maintenance()
            raise DeploymentError('fixture')
        self.app._activate = fail
        with self.assertRaises(DeploymentError):
            self.app.activate('a', 'w', 'c', Path('template'))
        self.assertFalse((self.app.data / 'cfm-maintenance').exists())
        self.app.progress_status = 'rollback_deferred'
        with self.assertRaises(DeploymentError):
            self.app.activate('a', 'w', 'c', Path('template'))
        self.assertTrue(self.app.owns_maintenance())

    def test_model_refresh_is_active_even_with_owned_maintenance(self):
        responses = {
            '/api/register': {'register': {}},
            '/api/monitor/realtime': {},
            '/api/register/schedule': {'schedule': {}},
            '/api/accounts/relogin': {}, '/api/accounts/twofa': {},
            '/api/register/openai/survival': {}, '/api/register/grok/jobs': {},
            '/api/accounts/models/refresh': {'active': 1},
        }
        self.app.api_json = lambda path: responses[path]
        self.app.enter_maintenance()
        status = self.app.active_snapshot()
        self.assertTrue(status['known'])
        self.assertTrue(status['active'])
        responses['/api/accounts/models/refresh']['active'] = 0
        self.assertFalse(self.app.active_snapshot()['active'])
        (self.app.data / 'cfm-maintenance').write_text('someone-else')
        self.assertTrue(self.app.active_snapshot()['active'])



class WorkerLicenseMountTests(unittest.TestCase):
    def setUp(self):
        self.app = Deployment(Path('/opt/gptgrok2api'))
        self.mount = {'Type': 'bind', 'Source': '/opt/gptgrok2api/secrets/cloakbrowser-license.key',
                      'Destination': '/root/.cloakbrowser/license.key', 'RW': False,
                      'Mode': 'ro', 'Propagation': 'rprivate'}
        self.other = {'Type': 'bind', 'Source': '/cache', 'Destination': '/cache', 'RW': True}

    def test_accepts_only_new_readonly_license_and_preserves_other_mounts(self):
        self.assertTrue(self.app.worker_mounts_match([], [self.mount]))
        self.assertTrue(self.app.worker_mounts_match([], [{**self.mount, 'Mode': ''}]))
        self.assertTrue(self.app.worker_mounts_match([self.other], [self.other, self.mount]))
        self.assertTrue(self.app.worker_mounts_match([self.mount], [self.mount]))

    def test_rejects_wrong_source_mode_type_and_propagation(self):
        for change in ({'Source': '/other/key'}, {'RW': True}, {'Mode': 'rw'},
                       {'Type': 'volume'}, {'Propagation': 'rshared'}, {'Mode': None},
                       {'Mode': '', 'RW': True}):
            with self.subTest(change=change):
                self.assertFalse(self.app.worker_mounts_match([], [{**self.mount, **change}]))

    def test_rejects_removed_or_changed_other_mounts_and_duplicate_license(self):
        for before, after in [([self.other], [self.mount]),
                              ([], [self.mount, self.other]),
                              ([], [self.mount, self.mount]),
                              ([{**self.mount, 'Source': '/old/key'}], [self.mount]),
                              ([self.mount], [])]:
            with self.subTest(before=before, after=after):
                self.assertFalse(self.app.worker_mounts_match(before, after))


if __name__ == '__main__':
    unittest.main()
