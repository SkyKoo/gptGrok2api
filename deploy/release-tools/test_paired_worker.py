"""Offline Worker handoff proofs and bounded readiness; never starts Docker or a browser."""
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch


HERE = Path(__file__).resolve().parent
COMMON = SimpleNamespace(COMMIT='a' * 40)
spec = importlib.util.spec_from_file_location('paired_worker_test', HERE / 'paired' / 'worker-release.py')
worker_module = importlib.util.module_from_spec(spec)
with patch.dict(sys.modules, {'common': COMMON}):
    spec.loader.exec_module(worker_module)


def atomic_json(path, value):
    Path(path).write_text(json.dumps(value))


def inspect_fixture():
    return {'Id': 'container-one', 'Name': '/cfm-browser-worker', 'Image': 'image-new',
            'RestartCount': 0, 'State': {'StartedAt': '2026-11-16T00:00:00Z',
                                       'Running': True, 'OOMKilled': False},
            'Config': {'Env': ['PRIVATE_TOKEN=do-not-log'], 'Cmd': ['worker']},
            'HostConfig': {'ShmSize': 536870912}, 'Mounts': [],
            'NetworkSettings': {'Networks': {'default': {'Aliases': ['cfm-browser-worker']}}}}


class WorkerFixture(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.worker = worker_module.Worker.__new__(worker_module.Worker)
        w = self.worker
        w.root = Path(self.temp.name).resolve()
        w.op = w.root / 'operation'; w.op.mkdir()
        w.backup = w.op / 'worker-before'; w.backup.mkdir()
        w.m = {'commit': COMMON.COMMIT, 'image': 'image-tag', 'image_id': 'image-new'}
        w.r = SimpleNamespace(load=lambda path: json.loads(Path(path).read_text()),
                              atomic_json=atomic_json, now=lambda: '2026-11-16T00:00:01Z',
                              ReleaseError=type('ReleaseError', (RuntimeError,), {}))
        w.timeout = 2
        w.phase, w.failure_code = 'test', None
        self.info = inspect_fixture()
        self.health = dict(status='ok', service='cfm-browser-worker', capacity=1)
        self.checks = dict(adobe_route=True, invalid_admission=422, reservation_created=False,
                           capacity_preserved=True, browser_started=False)
        w.current_worker = Mock(side_effect=lambda *_: ('cfm-browser-worker', copy.deepcopy(self.info)))
        w.health = Mock(side_effect=lambda *_: dict(self.health))
        w.no_browsers = Mock(return_value={'active': 0, 'zombies': 0})
        w.fenced = Mock()
        w.verify_candidate_config = Mock()
        w.protected = Mock(return_value={'account': 'digest'})
        w.h = SimpleNamespace(run=Mock(return_value=json.dumps(self.checks).encode()))
        atomic_json(w.backup / 'worker-inspect.json', self.info)
        atomic_json(w.backup / 'protected.json', {'account': 'digest'})
        self.clock = 0
        self.addCleanup(patch.stopall)
        patch.object(worker_module.time, 'monotonic', side_effect=lambda: self.clock).start()
        patch.object(worker_module.time, 'sleep', side_effect=self.sleep).start()

    def sleep(self, value):
        self.clock += value


class ReadinessTests(WorkerFixture):
    def test_transient_health_converges_without_restarting(self):
        self.worker.health.side_effect = [OSError('transport'), {'status': 'starting'}, self.health]
        actual = self.worker.wait_health('image-new', 'cfm-browser-worker')
        self.assertEqual(actual['Id'], 'container-one')
        self.assertEqual(self.clock, 1)
        self.assertEqual(self.worker.failure_code, None)
        self.worker.no_browsers.assert_called_once()
        self.worker.h.run.assert_not_called()

    def test_image_exit_and_oom_fail_without_waiting_or_probing(self):
        for key, value, reason in [('Image', 'other-image', 'image_mismatch'),
                                   ('Running', False, 'process_stopped'), ('OOMKilled', True, 'oom')]:
            with self.subTest(key=key):
                self.info = inspect_fixture()
                (self.info if key == 'Image' else self.info['State'])[key] = value
                with self.assertRaisesRegex(RuntimeError, reason):
                    self.worker.wait_health('image-new', 'cfm-browser-worker')
                self.assertEqual(self.clock, 0)
                self.worker.health.assert_not_called()

    def test_replacement_or_restart_during_wait_is_not_accepted(self):
        for kind in ('Id', 'RestartCount', 'StartedAt'):
            with self.subTest(kind=kind):
                original = inspect_fixture(); changed = copy.deepcopy(original)
                if kind == 'StartedAt':
                    changed['State'][kind] = 'later'
                else:
                    changed[kind] = 'container-two' if kind == 'Id' else 1
                self.worker.current_worker.side_effect = [('cfm-browser-worker', original),
                                                          ('cfm-browser-worker', changed)]
                self.worker.health.side_effect = [{'status': 'starting'}, self.health]
                with self.assertRaisesRegex(RuntimeError, 'process_or_configuration_changed'):
                    self.worker.wait_health('image-new', 'cfm-browser-worker')

    def test_identity_change_during_browser_check_is_rejected(self):
        self.worker.no_browsers.side_effect = lambda *_: self.info.update(Id='replacement')
        with self.assertRaisesRegex(RuntimeError, 'process_or_configuration_changed'):
            self.worker.wait_health('image-new', 'cfm-browser-worker')

    def test_known_wrong_service_fails_fast_and_old_rollback_service_is_supported(self):
        self.health['service'] = 'grok-browser'
        with self.assertRaisesRegex(RuntimeError, 'health_service_mismatch'):
            self.worker.wait_health('image-new', 'cfm-browser-worker')
        self.assertEqual(self.clock, 0)
        self.assertEqual(self.worker.wait_health('image-new', 'grok-browser')['Id'], 'container-one')

    def test_non_idle_or_malformed_capacity_cannot_pass_and_wait_is_bounded(self):
        for capacity in (0, True, '1'):
            with self.subTest(capacity=capacity):
                self.clock = 0
                self.health['capacity'] = capacity
                with self.assertRaisesRegex(RuntimeError, 'deadline_exceeded'):
                    self.worker.wait_health('image-new', 'cfm-browser-worker')
                self.assertEqual(self.clock, 2)
                self.worker.no_browsers.assert_not_called()

    def test_invalid_budgets_and_slow_healthy_response_never_pass(self):
        for budget in (0, -1, float('inf'), float('nan')):
            self.worker.timeout = budget
            with self.assertRaisesRegex(RuntimeError, 'positive and finite'):
                self.worker.wait_health('image-new', 'cfm-browser-worker')
        self.worker.timeout = 1
        def slow(*_):
            self.clock += 2
            return self.health
        self.worker.health.side_effect = slow
        with self.assertRaisesRegex(RuntimeError, 'deadline_exceeded'):
            self.worker.wait_health('image-new', 'cfm-browser-worker')

    def test_browser_ownership_failure_is_not_swallowed(self):
        self.worker.no_browsers.side_effect = RuntimeError('active browser')
        with self.assertRaisesRegex(RuntimeError, 'active browser'):
            self.worker.wait_health('image-new', 'cfm-browser-worker')
        self.assertEqual(self.clock, 0)


class VerificationReceiptTests(WorkerFixture):
    def test_ready_reuses_full_rpc_and_content_proof_but_rechecks_fence_and_configuration(self):
        self.assertEqual(self.worker.verify()['phase'], 'verified')
        self.worker.protected.assert_called_once()
        self.worker.h.run.assert_called_once()
        proof = (self.worker.op / 'worker-verified.json').read_bytes()
        for _ in range(2):
            self.assertEqual(self.worker.verify_ready()['phase'], 'ready_verified')
        self.assertEqual(self.worker.fenced.call_count, 3)
        self.assertEqual(self.worker.verify_candidate_config.call_count, 3)
        self.worker.protected.assert_called_once()
        self.worker.h.run.assert_called_once()
        self.assertEqual((self.worker.op / 'worker-verified.json').read_bytes(), proof)
        self.assertNotIn(b'do-not-log', proof)

    def test_ready_requires_full_receipt_from_same_operation_and_manifest(self):
        with self.assertRaises(FileNotFoundError):
            self.worker.verify_ready()
        self.worker.verify()
        path = self.worker.op / 'worker-verified.json'
        original = json.loads(path.read_text())
        for key, value in [('full_verification', False), ('operation', '/different'), ('commit', 'other'),
                           ('manifest_sha256', 'other'), ('root', '/different'), ('version', 2),
                           ('verified_at', ''), ('checks', {})]:
            with self.subTest(key=key):
                atomic_json(path, dict(original, **{key: value}))
                with self.assertRaisesRegex(RuntimeError, 'no matching full verification receipt'):
                    self.worker.verify_ready()

    def test_ready_rejects_replacement_restart_environment_mount_and_runtime_drift(self):
        self.worker.verify()
        original = copy.deepcopy(self.info)
        mutations = [lambda d: d.update(Id='replacement'), lambda d: d.update(RestartCount=1),
                     lambda d: d['State'].update(StartedAt='later'),
                     lambda d: d['Config'].update(Env=['OTHER=value']),
                     lambda d: d['HostConfig'].update(ShmSize=1),
                     lambda d: d['Mounts'].append({'Destination': '/different'}),
                     lambda d: d['NetworkSettings']['Networks'].update(other={})]
        for mutate in mutations:
            with self.subTest(mutate=mutate):
                self.info = copy.deepcopy(original); mutate(self.info)
                with self.assertRaisesRegex(RuntimeError, 'changed since full verification'):
                    self.worker.verify_ready()

    def test_failed_full_verification_invalidates_earlier_receipt(self):
        self.worker.verify()
        self.worker.protected.return_value = {'account': 'changed'}
        with self.assertRaisesRegex(RuntimeError, 'protected account'):
            self.worker.verify()
        self.assertFalse((self.worker.op / 'worker-verified.json').exists())
        with self.assertRaises(FileNotFoundError):
            self.worker.verify_ready()

    def test_full_probe_failure_and_process_change_cannot_write_receipt(self):
        self.worker.h.run.side_effect = subprocess.TimeoutExpired('private command', 1)
        with self.assertRaises(subprocess.TimeoutExpired):
            self.worker.verify()
        self.assertFalse((self.worker.op / 'worker-verified.json').exists())
        def swapped(*_, **__):
            self.info['Id'] = 'replacement'
            return json.dumps(self.checks).encode()
        self.worker.h.run.side_effect = swapped
        with self.assertRaisesRegex(RuntimeError, 'changed during full verification'):
            self.worker.verify()
        self.assertFalse((self.worker.op / 'worker-verified.json').exists())

    def test_readiness_configuration_check_cannot_be_skipped(self):
        self.worker.verify()
        self.worker.verify_candidate_config.side_effect = RuntimeError('external config changed')
        with self.assertRaisesRegex(RuntimeError, 'external config changed'):
            self.worker.verify_ready()

    def test_ready_rejects_non_idle_health_without_waiting(self):
        self.worker.verify()
        self.worker.no_browsers.reset_mock()
        self.worker.health.side_effect = lambda *_: dict(
            status='ok', service='cfm-browser-worker', capacity=0)
        with self.assertRaisesRegex(RuntimeError, 'health_not_idle_or_contract_mismatch'):
            self.worker.verify_ready()
        self.assertEqual(self.clock, 0)
        self.worker.no_browsers.assert_not_called()

    def test_failure_evidence_contains_structural_fields_only(self):
        self.worker.phase, self.worker.failure_code = 'wait_health:inspect', 'image_mismatch'
        self.worker.failure_evidence(RuntimeError('PRIVATE_TOKEN=do-not-log'))
        raw = (self.worker.op / 'worker-error.json').read_text()
        self.assertNotIn('do-not-log', raw)
        value = json.loads(raw)
        self.assertEqual(value['reason_code'], 'image_mismatch')
        self.assertEqual(value['error_type'], 'RuntimeError')
        self.assertEqual(value['container']['image_id'], 'image-new')
        self.assertEqual(value['container']['restarts'], 0)


if __name__ == '__main__':
    unittest.main()
