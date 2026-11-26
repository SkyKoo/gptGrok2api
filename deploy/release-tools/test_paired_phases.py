"""Offline handoff/finalization fault injection; no Docker or production access."""
from contextlib import contextmanager
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from paired_phases import (PhaseError, ReleaseContext, finish_after_release,
                           seal_raw_snapshot, wait_stable_health)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def load_json(path):
    return json.loads(Path(path).read_text())


def file_sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def atomic_json(path, value):
    Path(path).write_text(json.dumps(value))


def container_summary(info):
    return {'id': info['Id'], 'image_id': info['Image'], 'restarts': info['RestartCount'],
            'running': info['State']['Running'], 'oom': info['State']['OOMKilled'],
            'health': info['State']['Health']['Status'], 'networks': info.get('networks', [])}


def load_template(name, common, snapshot=None):
    modules = {'common': common}
    if snapshot is not None:
        modules['snapshot'] = snapshot
    spec = importlib.util.spec_from_file_location('paired_test_' + name,
                Path(__file__).with_name('paired') / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    with patch.dict(sys.modules, modules):
        spec.loader.exec_module(module)
    return module


class FinalizationTests(unittest.TestCase):
    def fixture(self):
        context = ReleaseContext('commit', 'app-image', 'worker-image', '/backup')
        state = dict(context.__dict__, status='archive_pending', raw_backup_sealed=True,
                     traffic_switched=True, writer_resumed=True, old_writer_stopped=True)
        events = []
        def record(phase, **values):
            state.update(phase=phase, **values)
            events.append(copy.deepcopy(state))
        return context, state, events, record

    def test_seal_completion_is_never_recorded_before_durability_returns(self):
        calls = []
        seal_raw_snapshot(lambda: calls.append('fsync-complete'),
                          lambda phase, **values: calls.append((phase, values)))
        self.assertEqual(calls[0], 'fsync-complete')
        self.assertEqual(calls[1], ('frozen_backup_sealed', {'raw_backup_sealed': True}))
        record = Mock()
        with self.assertRaises(OSError):
            seal_raw_snapshot(Mock(side_effect=OSError('disk full')), record)
        record.assert_not_called()

    def test_archive_failure_retains_pending_and_can_resume_from_checkpoint(self):
        context, state, events, record = self.fixture()
        live = Mock()
        with self.assertRaises(OSError):
            finish_after_release(context, state, record=record, verify_live=live,
                                 finish_archive=Mock(side_effect=OSError('private-token')))
        self.assertEqual(state['status'], 'archive_pending')
        self.assertFalse(state['final_backup_verified'])
        self.assertNotIn('private-token', json.dumps(events))
        # The resumed process starts from serialized state; no apply callback is
        # available, and a retry verifies existing archive output in its callback.
        resumed = json.loads(json.dumps(state))
        def resumed_record(phase, **values):
            resumed.update(phase=phase, **values)
        archive = Mock()
        finish_after_release(context, resumed, record=resumed_record,
                             verify_live=live, finish_archive=archive)
        archive.assert_called_once_with()
        self.assertEqual(live.call_count, 3)
        self.assertEqual(resumed['status'], 'success')
        self.assertTrue(resumed['final_backup_verified'])

    def test_unconfirmed_handoff_or_wrong_identity_never_touches_archive(self):
        mutations = [('status', 'needs_attention'), ('raw_backup_sealed', False),
                     ('traffic_switched', False), ('writer_resumed', False),
                     ('old_writer_stopped', False), ('commit', 'other'),
                     ('image_id', 'other'), ('worker_image_id', 'other'), ('backup', '/other')]
        for key, value in mutations:
            with self.subTest(key=key):
                context, state, _, record = self.fixture()
                state[key] = value
                live, archive = Mock(), Mock()
                with self.assertRaises(PhaseError):
                    finish_after_release(context, state, record=record,
                                         verify_live=live, finish_archive=archive)
                archive.assert_not_called()
                live.assert_not_called()

    def test_live_drift_before_or_after_archive_prevents_success(self):
        for after in (False, True):
            with self.subTest(after=after):
                context, state, _, record = self.fixture()
                live = Mock(side_effect=[None, RuntimeError('changed')] if after
                            else RuntimeError('changed'))
                archive = Mock()
                with self.assertRaises(RuntimeError):
                    finish_after_release(context, state, record=record,
                                         verify_live=live, finish_archive=archive)
                self.assertEqual(archive.call_count, int(after))
                self.assertEqual(state['status'], 'archive_pending')

    def test_interruption_keeps_archive_pending(self):
        context, state, _, record = self.fixture()
        with self.assertRaises(KeyboardInterrupt):
            finish_after_release(context, state, record=record, verify_live=Mock(),
                                 finish_archive=Mock(side_effect=KeyboardInterrupt))
        self.assertEqual(state['status'], 'archive_pending')
        self.assertEqual(state['backup_finalization_error'], 'KeyboardInterrupt')


class HealthWaitTests(unittest.TestCase):
    def setUp(self):
        self.expected = dict(id='pinned', image_id='image', restarts=0, running=True,
                             health='healthy', oom=False, mounts=['fixed'])
        self.clock = 0

    def sleep(self, seconds):
        self.clock += seconds

    def wait(self, inspect, **kwargs):
        return wait_stable_health(inspect, self.expected, lambda info: info,
            monotonic=lambda: self.clock, sleep=self.sleep, **kwargs)

    def test_health_converges_without_relaxing_other_fields(self):
        inspect = Mock(side_effect=[dict(self.expected, health='unhealthy'),
                                    dict(self.expected, health='starting'), self.expected])
        self.assertEqual(self.wait(inspect, timeout=2), self.expected)
        self.assertEqual(inspect.call_count, 3)
        self.assertEqual(self.clock, 1)

    def test_changed_identity_configuration_restart_oom_or_exit_fails_immediately(self):
        for key, value in [('id', 'replacement'), ('image_id', 'other'), ('restarts', 1),
                           ('running', False), ('oom', True), ('mounts', ['changed'])]:
            with self.subTest(key=key):
                inspect = Mock(return_value=dict(self.expected, health='unhealthy', **{key: value}))
                with self.assertRaisesRegex(PhaseError, 'identity or configuration'):
                    self.wait(inspect, timeout=10)
                self.assertEqual(inspect.call_count, 1)
                self.assertEqual(self.clock, 0)

    def test_timeout_and_unbounded_budgets_are_rejected(self):
        inspect = Mock(return_value=dict(self.expected, health='unhealthy'))
        with self.assertRaisesRegex(PhaseError, 'deadline'):
            self.wait(inspect, timeout=1, interval=.75)
        self.assertEqual(self.clock, 1)
        for value in (0, -1, float('inf'), float('nan')):
            with self.subTest(value=value), self.assertRaises(PhaseError):
                self.wait(inspect, timeout=value)

    def test_slow_healthy_response_does_not_extend_deadline(self):
        def inspect():
            self.clock += 2
            return self.expected
        with self.assertRaisesRegex(PhaseError, 'deadline'):
            self.wait(inspect, timeout=1)


class PairedReleaseIntegrationTests(unittest.TestCase):
    def fixture(self, temporary, archive_error=None, seal_error=None):
        root = Path(temporary).resolve()
        op = root / 'op'; op.mkdir()
        (root / 'backups').mkdir()
        (root / 'data').mkdir()
        worker_manifest = op / 'worker-manifest.json'; worker_manifest.write_text('{}')
        calls = []
        common = SimpleNamespace(require=require, COMMIT='new-commit', PROTOCOL='protocol',
            WORKER='worker', DEPENDENCIES={'postgres': 'postgres'}, load=load_json,
            file_sha=file_sha, helper_hashes=lambda _: {}, saved_switches=lambda _: {},
            container_summary=container_summary,
            worker_health=lambda _: {'capacity': 1}, xai_authorization_queue_idle=lambda *_, **__: {'active': 0},
            check_image=lambda *_, **__: None,
            healthy=lambda item: item['State']['Running'] and not item['State']['OOMKilled']
                and item['State']['Health']['Status'] == 'healthy')
        class Snapshot:
            source_capture = object()
            def sync(self, frozen=False): calls.append('frozen-copy' if frozen else 'precopy')
            def database(self, *_): pass
            def business_files(self): return {'fixed': 'digest'}
            def seal(self):
                calls.append('seal')
                if seal_error: raise seal_error
            def finish(self):
                calls.append('archive')
                if archive_error: raise archive_error
        snapshot = Snapshot()
        snapshots = SimpleNamespace(make=lambda *_: snapshot, resume=Mock(return_value=snapshot))
        module = load_template('release', common, snapshots)
        value = module.Maintenance.__new__(module.Maintenance)
        value.root, value.op, value.state_path = root, op, op / 'release-state.json'
        value.maintenance_token = None
        value.old = dict(container='old', frontdoor='front', slot='blue', upstream='old:8080',
                         image='old-image', image_id='old-image', commit='old-commit')
        value.release = {'image': 'new-image', 'image_id': 'new-image'}
        value.baseline = {'worker': {'container': 'worker', 'image': 'worker-image', 'image_id': 'worker-image'},
            'candidate': value.release, 'current': value.old, 'containers': {'old': {'id': 'old-id'}},
            'switches': {}, 'helpers': {}, 'source': {}, 'backup_local_sha256': 'package',
            'worker_manifest_sha256': file_sha(worker_manifest)}
        value.args = SimpleNamespace(drain_timeout=1,
                                      worker_manifest=worker_manifest, baseline=op / 'baseline.json')
        atomic_json(value.args.baseline, value.baseline)
        value.prove = lambda: None
        value.drain_deadlines = []
        value.wait_idle_until = lambda _, deadline: value.drain_deadlines.append(deadline)
        value.dependency_checks = []
        value.unchanged_dependencies = lambda: value.dependency_checks.append('final')
        value.worker = lambda action: calls.append('worker-' + action)
        value.helper = lambda *_, **__: None
        guard_state = {'files': {'fixed': 'digest'}, 'database': {'tables': 'digest'}}
        def capture_state(name, *, before=None, source_capture=None):
            calls.append(name)
            if name in ('state-frozen.json', 'state-after.json'):
                self.assertIs(source_capture, snapshot.source_capture)
            if before is not None:
                require(before == guard_state, 'state changed')
            return copy.deepcopy(guard_state)
        value.capture_state = capture_state
        value.state_guard = lambda: SimpleNamespace(business_files=lambda state: state['files'])
        infos = {name: {'Id': name + '-id', 'Image': image, 'RestartCount': 0,
                       'State': {'Running': True, 'OOMKilled': False, 'Health': {'Status': 'healthy'}}}
                 for name, image in [('old', 'old-image'), ('candidate', 'new-image'), ('worker', 'worker-image')]}
        route = {'held': False, 'upstream': 'old:8080', 'epoch': 0}
        def gateway(_, action=None, upstream=None):
            if action:
                calls.append(action)
                route['epoch'] += 1
                if action == 'route': route['upstream'] = upstream
                if action in ('hold', 'release'): route['held'] = action == 'hold'
            return {'protocol': 'protocol', 'route': dict(route), 'active_requests': 0}
        def action(name, action):
            calls.append(name + '-' + action)
            return {'phase': 'frozen' if action == 'freeze' else 'active', 'writer_lease': action != 'freeze'}
        def stop(name):
            calls.append(name + '-stop')
            infos[name]['State']['Running'] = False
        def control(name, path='/status', body=None):
            if path == '/gateway/hold':
                self.assertEqual(body['epoch'], route['epoch'])
                return gateway(name, 'hold')
            return {'protocol': 'protocol', 'phase': 'active', 'writer_lease': True, 'paused': False}
        value.host = SimpleNamespace(inspect=lambda name: infos[name], gateway=gateway, action=action, checkpoint=Mock(),
            stop_empty=stop, control=control, run=Mock())
        def metadata(old, release, slot, status):
            return dict(old, **release, container='candidate', slot=slot, upstream='new:8080',
                        status=status, commit='new-commit', backup=str(value.work))
        engine = SimpleNamespace(start_slot=lambda *_: calls.append('start'), metadata=metadata)
        value.r = SimpleNamespace(SLOTS={'green': ('candidate', 'new:8080')}, atomic_json=atomic_json,
            ReleaseError=RuntimeError, retire_rolled_back_payload=Mock(),
            now=lambda: 'time', RollingRelease=lambda *_: engine, merge_assets=lambda *_: None,
            business_files=Mock(side_effect=AssertionError('legacy file scan must not run')),
            database_guard=Mock(side_effect=AssertionError('legacy DB scan must not run')),
            idle=lambda _: True, install_backup_tool=lambda *_: None, backup_tool_bytes=lambda *_: b'',
            backup_local_bytes=lambda *_: {'package_id': 'package'}, install_backup_local=lambda *_: {})
        value.reconcile_failure = Mock()
        return module, value, calls, snapshots

    def test_real_apply_seals_before_switch_and_archives_only_after_resume_and_old_stop(self):
        with tempfile.TemporaryDirectory() as temporary:
            _, value, calls, _ = self.fixture(temporary)
            value.apply()
            for earlier, later in [('seal', 'worker-switch'), ('release', 'candidate-resume'),
                                    ('candidate-resume', 'old-stop'), ('old-stop', 'archive'),
                                    ('precopy', 'old-pause'), ('old-freeze', 'frozen-copy'),
                                    ('state-frozen.json', 'worker-switch'),
                                    ('worker-switch', 'candidate-activate'),
                                    ('candidate-activate', 'state-after.json'),
                                    ('state-after.json', 'worker-verify-ready'),
                                    ('worker-verify-ready', 'route')]:
                self.assertLess(calls.index(earlier), calls.index(later))
            self.assertNotIn('worker-verify', calls)
            self.assertEqual(len(value.drain_deadlines), 2)
            self.assertEqual(value.drain_deadlines[0], value.drain_deadlines[1])
            self.assertEqual(value.dependency_checks, ['final'])
            self.assertEqual(sum(name.startswith('state-') for name in calls), 2)
            self.assertEqual(load_json(value.state_path)['status'], 'success')
            self.assertFalse((value.root / 'data' / 'cfm-maintenance').exists())
            timings = load_json(value.state_path)['timings']
            self.assertTrue(all(row['status'] == 'success' and row['elapsed_seconds'] >= 0
                                for row in timings))
            self.assertEqual(load_json(value.root / 'current.json')['status'], 'healthy')
            value.reconcile_failure.assert_not_called()

    def test_external_maintenance_marker_is_never_overwritten_or_removed(self):
        with tempfile.TemporaryDirectory() as temporary:
            _, value, _, _ = self.fixture(temporary)
            marker = value.root / 'data' / 'cfm-maintenance'
            marker.write_text('operator')
            with self.assertRaisesRegex(RuntimeError, 'external CFM maintenance marker'):
                value.enter_maintenance()
            value.leave_maintenance()
            self.assertEqual(marker.read_text(), 'operator')

    def test_replaced_maintenance_marker_is_retained_for_operator_review(self):
        with tempfile.TemporaryDirectory() as temporary:
            _, value, _, _ = self.fixture(temporary)
            value.enter_maintenance()
            marker = value.root / 'data' / 'cfm-maintenance'
            marker.write_text('operator')
            with self.assertRaisesRegex(RuntimeError, 'marker changed'):
                value.leave_maintenance()
            self.assertEqual(marker.read_text(), 'operator')

    def test_drain_wait_records_busy_counters_without_duplicate_status_reads(self):
        with tempfile.TemporaryDirectory() as temporary:
            module, value, _, _ = self.fixture(temporary)
            value.state = {'status': 'running', 'phase': 'draining', 'events': []}
            states = [
                {'jobs': {'survival': 1, 'images': 0}, 'active_requests': 0},
                {'jobs': {'survival': 0, 'images': 0}, 'active_requests': 0},
            ]
            value.host.control = Mock(side_effect=states)
            value.host.checkpoint = Mock()
            value.r.idle = lambda state: not state['jobs']['survival'] and not state['active_requests']
            with patch.object(module.time, 'sleep'):
                module.Maintenance.wait_idle_until(value, 'old', module.time.monotonic() + 5)
            self.assertEqual(value.host.control.call_count, 2)
            observed = [event for event in value.state['drain_waits']
                        if event['phase'] == 'observed']
            self.assertEqual(len(observed), 1)
            self.assertEqual(observed[0]['busy'], {'survival': 1})
            self.assertEqual(value.state['events'][-1]['phase'], 'drain_wait_finished')

    def test_shared_work_drains_before_hold_and_queue_is_checked_again(self):
        with tempfile.TemporaryDirectory() as temporary:
            module, value, calls, _ = self.fixture(temporary)
            remaining = iter((1, 0, 1, 0))
            def queue(*_, require_idle=True):
                calls.append('fenced-queue' if 'hold' in calls else 'observe-queue')
                return {'active': next(remaining)}
            module.common.xai_authorization_queue_idle = queue
            with patch.object(module.time, 'sleep'):
                value.apply()
            self.assertEqual(calls.count('observe-queue'), 2)
            self.assertEqual(calls.count('fenced-queue'), 2)
            self.assertLess(calls.index('observe-queue'), calls.index('hold'))
            self.assertLess(calls.index('hold'), calls.index('fenced-queue'))
            self.assertLess(calls.index('fenced-queue'), calls.index('old-freeze'))

    def test_busy_fenced_queue_times_out_without_freezing(self):
        with tempfile.TemporaryDirectory() as temporary:
            module, value, calls, _ = self.fixture(temporary)
            module.common.xai_authorization_queue_idle = lambda *_, **__: {'active': int('hold' in calls)}
            with self.assertRaisesRegex(RuntimeError, 'Shared work drain reached its deadline'):
                value.apply()
            self.assertNotIn('old-freeze', calls)

    def test_hold_reconciles_a_lost_reply_and_rejects_another_epoch(self):
        for actual_epoch, ok in ((8, True), (9, False)):
            with self.subTest(epoch=actual_epoch), tempfile.TemporaryDirectory() as temporary:
                module, value, _, _ = self.fixture(temporary)
                value.front = 'front'
                route = {'held': False, 'upstream': 'old:8080', 'epoch': 7}
                value.host.gateway = Mock(side_effect=[
                    {'route': route, 'active_requests': 0},
                    {'route': dict(route, held=True, epoch=actual_epoch), 'active_requests': 0}])
                value.host.control = Mock(side_effect=RuntimeError('reply lost'))
                if ok:
                    value.hold_idle_gateway(module.time.monotonic() + 1)
                else:
                    with self.assertRaisesRegex(RuntimeError, 'Ingress changed'):
                        value.hold_idle_gateway(module.time.monotonic() + 1)
                value.host.control.assert_called_once_with('front', '/gateway/hold', {'epoch': 7, 'upstream': ''})

    def test_real_apply_archive_failure_never_enters_recovery_and_finish_resumes_only_archive(self):
        with tempfile.TemporaryDirectory() as temporary:
            _, value, calls, snapshots = self.fixture(temporary, archive_error=OSError('disk full'))
            with self.assertRaises(OSError): value.apply()
            value.reconcile_failure.assert_not_called()
            self.assertEqual(load_json(value.state_path)['status'], 'archive_pending')
            self.assertEqual(load_json(value.root / 'current.json')['status'], 'healthy')
            before = list(calls)
            snapshots.resume.return_value = SimpleNamespace(finish=lambda: calls.append('archive-resume'))
            value.finish()
            self.assertEqual(calls, before + ['archive-resume'])
            self.assertEqual(load_json(value.state_path)['status'], 'success')
            snapshots.resume.assert_called_once()

    def test_failed_raw_seal_does_not_switch_worker_or_release_traffic(self):
        with tempfile.TemporaryDirectory() as temporary:
            _, value, calls, _ = self.fixture(temporary, seal_error=OSError('fsync failed'))
            with self.assertRaises(OSError): value.apply()
            self.assertNotIn('worker-switch', calls)
            self.assertNotIn('release', calls)
            value.reconcile_failure.assert_called_once()

    def test_existing_journal_rejects_apply_before_preflight(self):
        with tempfile.TemporaryDirectory() as temporary:
            module, value, _, _ = self.fixture(temporary)
            value.state_path.write_text('{}')
            with self.assertRaisesRegex(RuntimeError, 'instead of replaying apply'):
                module.Maintenance.prove(value)

    def test_activation_mutation_blocks_traffic(self):
        for boundary in ('state-after.json',):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as temporary:
                _, value, calls, _ = self.fixture(temporary)
                original = value.capture_state
                def capture(name, **kwargs):
                    if name == boundary:
                        raise RuntimeError('state changed')
                    return original(name, **kwargs)
                value.capture_state = capture
                with self.assertRaisesRegex(RuntimeError, 'state changed'):
                    value.apply()
                self.assertNotIn('route', calls)
                self.assertNotIn('release', calls)
                self.assertIn('candidate-activate', calls)
                self.assertTrue(value.state['activation_attempted'])
                self.assertEqual(value.state['timings'][-1]['status'], 'failed')
                value.reconcile_failure.assert_called_once()

    def test_precopy_failure_leaves_background_and_ingress_running(self):
        with tempfile.TemporaryDirectory() as temporary:
            _, value, calls, snapshots = self.fixture(temporary)
            snapshots.make().sync = Mock(side_effect=OSError('copy changed'))
            with self.assertRaises(OSError):
                value.apply()
            self.assertNotIn('old-pause', calls)
            self.assertNotIn('hold', calls)
            self.assertNotIn('worker-switch', calls)
            self.assertNotIn('candidate-activate', calls)

    def test_failed_step_timing_does_not_store_exception_details(self):
        with tempfile.TemporaryDirectory() as temporary:
            _, value, _, _ = self.fixture(temporary, archive_error=RuntimeError('secret-value'))
            with self.assertRaises(RuntimeError):
                value.apply()
            state = load_json(value.state_path)
            self.assertEqual(state['status'], 'archive_pending')
            self.assertEqual(state['timings'][-1]['error_type'], 'RuntimeError')
            self.assertNotIn('secret-value', json.dumps(state))

    def test_live_layout_retries_after_drain_before_hold_and_frozen_copy_remains_strict(self):
        for frozen_fails in (False, True):
            with self.subTest(frozen_fails=frozen_fails), tempfile.TemporaryDirectory() as temporary:
                _, value, calls, snapshots = self.fixture(temporary)
                precopy_attempts = 0
                def sync(frozen=False):
                    nonlocal precopy_attempts
                    calls.append('frozen-copy' if frozen else 'precopy')
                    if not frozen:
                        precopy_attempts += 1
                        if precopy_attempts == 1:
                            raise RuntimeError('Snapshot refuses symlinks')
                    elif frozen_fails:
                        raise RuntimeError('Snapshot refuses symlinks')
                snapshots.make().sync = sync
                if frozen_fails:
                    with self.assertRaisesRegex(RuntimeError, 'Snapshot refuses symlinks'):
                        value.apply()
                    self.assertNotIn('worker-switch', calls)
                    self.assertNotIn('release', calls)
                else:
                    value.apply()
                    self.assertEqual(value.state['status'], 'success')
                self.assertEqual(value.state['precopy_deferred'], 'resolved_after_drain')
                self.assertLess(calls.index('old-pause'), calls.index('precopy', calls.index('old-pause')))
                self.assertLess(calls.index('precopy', calls.index('old-pause')), calls.index('hold'))

    def test_old_program_recovery_checks_same_frozen_state_before_and_after_activation(self):
        for failed_boundary in (None, 'state-recovery-before.json', 'state-recovery-after.json'):
            with self.subTest(failed_boundary=failed_boundary), tempfile.TemporaryDirectory() as temporary:
                module, value, calls, _ = self.fixture(temporary)
                value.state = {'activation_attempted': False, 'status': 'running', 'events': []}
                value.old_name, value.front = 'old', 'front'
                value.work = value.root / 'backups' / 'attempt'
                value.work.mkdir()
                value.worker_attempted = True
                value.frozen_state = {'files': {'fixed': 'digest'}, 'database': {'tables': 'digest'}}
                value.snapshot = SimpleNamespace(source_capture=object())
                value.host.control = lambda _: {'phase': 'frozen', 'writer_lease': False}
                value.host.gateway('front', 'hold')
                value.engine = SimpleNamespace(owned_candidate='candidate',
                    stop_candidate=lambda _: calls.append('candidate-stop'),
                    start_slot=lambda *_: 'old')
                value.old_info = value.host.inspect('old')
                capture = value.capture_state
                def check(name, **kwargs):
                    self.assertEqual(kwargs.get('before'), value.frozen_state)
                    if name == failed_boundary:
                        raise RuntimeError('changed')
                    return capture(name, **kwargs)
                value.capture_state = check
                module.Maintenance.reconcile_failure(value)
                if failed_boundary is None:
                    self.assertEqual(value.state['status'], 'rolled_back')
                    value.r.retire_rolled_back_payload.assert_called_once_with(value.root, value.work)
                    for earlier, later in [('worker-rollback', 'state-recovery-before.json'),
                                            ('state-recovery-before.json', 'old-activate'),
                                            ('old-activate', 'state-recovery-after.json'),
                                            ('state-recovery-after.json', 'release')]:
                        self.assertLess(calls.index(earlier), calls.index(later))
                else:
                    value.r.retire_rolled_back_payload.assert_not_called()
                    self.assertEqual(value.state['status'], 'needs_attention')
                    self.assertNotIn('release', calls)
                    self.assertEqual('old-activate' in calls, failed_boundary == 'state-recovery-after.json')
                self.assertFalse(value.state['data_restore_performed'])

    def test_unknown_activation_result_preserves_instances_and_never_rolls_back(self):
        with tempfile.TemporaryDirectory() as temporary:
            module, value, calls, _ = self.fixture(temporary)
            value.state = {'activation_attempted': True, 'status': 'running', 'events': []}
            value.front = 'front'
            module.Maintenance.reconcile_failure(value)
            self.assertEqual(value.state['status'], 'needs_attention')
            self.assertTrue(value.state['instances_preserved'])
            self.assertEqual(calls, [])

    def test_old_program_recovery_does_not_reuse_rejected_frozen_source_proof(self):
        with tempfile.TemporaryDirectory() as temporary:
            module, value, calls, _ = self.fixture(temporary)
            value.state = {'activation_attempted': False, 'status': 'running', 'events': []}
            value.old_name, value.front = 'old', 'front'
            value.work = value.root / 'backups' / 'attempt'
            value.work.mkdir()
            value.worker_attempted = False
            value.frozen_state = None
            value.snapshot = SimpleNamespace(source_capture=object())
            value.host.control = lambda _: {'phase': 'frozen', 'writer_lease': False}
            value.host.gateway('front', 'hold')
            value.engine = SimpleNamespace(owned_candidate='candidate',
                stop_candidate=lambda _: calls.append('candidate-stop'), start_slot=lambda *_: 'old')
            value.old_info = value.host.inspect('old')
            capture = value.capture_state
            evidence = []

            def check(name, **kwargs):
                evidence.append((name, kwargs.get('source_capture')))
                return capture(name, **kwargs)

            value.capture_state = check
            module.Maintenance.reconcile_failure(value)
            self.assertEqual(evidence, [('state-recovery-baseline.json', None),
                                        ('state-recovery-after.json', None)])
            self.assertEqual(value.state['status'], 'rolled_back')
            self.assertIn('release', calls)

class CoordinatorResumeTests(unittest.TestCase):
    def retention_fixture(self, temporary):
        root = Path(temporary).resolve(); op = root / 'op'; op.mkdir()
        backup = root / 'backups/release'; backup.mkdir(parents=True)
        infos = {name: {'Id': name + '-id', 'Image': name + '-image', 'RestartCount': 0,
                        'State': {'Running': name != 'old', 'OOMKilled': False,
                                  'Health': {'Status': 'healthy'}}}
                 for name in ('app', 'worker', 'front', 'postgres', 'old')}
        state = {'status': 'success', 'phase': 'success', 'commit': 'commit', 'backup': str(backup),
                 'image_id': 'app-image', 'worker_image_id': 'worker-image', 'final_backup_verified': True,
                 'candidate': 'app', 'candidate_slot': 'green', 'candidate_container_id': 'app-id',
                 'worker_container_id': 'worker-id', 'candidate_summary': container_summary(infos['app']),
                 'worker_summary': container_summary(infos['worker']), 'retention_requires_final_verification': True}
        current = {'status': 'healthy', 'commit': 'commit', 'backup': str(backup), 'container': 'app',
                   'image_id': 'app-image', 'frontdoor': 'front', 'slot': 'green', 'upstream': 'app:8080'}
        baseline = {'helpers': {}, 'backup_policy': {}, 'candidate': {'image_id': 'app-image'},
                    'worker': {'image_id': 'worker-image'}, 'current': {'container': 'old', 'frontdoor': 'front'},
                    'containers': {name: container_summary(value) for name, value in infos.items()}}
        for path, value in ((op / 'release-state.json', state), (root / 'current.json', current),
                            (backup / 'release.json', state), (op / 'other-services-before.json', {'fixed': True}),
                            (op / 'verification.json', {'verified': True, 'commit': 'commit',
                                                       'image_id': 'app-image', 'backup': str(backup)})):
            atomic_json(path, value)
        atomic_json(op / 'other-services-after.json', {'verified': True, 'current_commit': 'commit',
                    'baseline_sha256': file_sha(op / 'other-services-before.json')})
        host = SimpleNamespace(inspect=lambda name: infos[name], run=Mock(),
            control=lambda _: {'protocol': 'protocol', 'phase': 'active', 'writer_lease': True, 'paused': False},
            gateway=lambda _: {'route': {'held': False, 'upstream': 'app:8080'}})
        r = SimpleNamespace(Host=lambda: host, now=lambda: 'first-time', atomic_json=atomic_json,
                            SLOTS={'green': ('app', 'app:8080')})
        common = SimpleNamespace(require=require, COMMIT='commit', PROTOCOL='protocol',
            WORKER='worker', DEPENDENCIES={'postgres': 'postgres'}, rolling=lambda _: r,
            load=load_json, file_sha=file_sha, helper_hashes=lambda _: {}, backup_policy_identity=lambda _: {},
            container_summary=container_summary,
            healthy=lambda info: info['State']['Running'] and not info['State']['OOMKilled']
                and info['State']['Health']['Status'] == 'healthy')
        return load_template('coordinator', common), root, op, backup, baseline, infos, r

    def test_existing_admission_revalidates_exact_runtime_without_overwriting_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            module, root, op, backup, baseline, _, r = self.retention_fixture(temporary)
            first = module.admit_retention(root, op, baseline)
            evidence = (backup / 'retention-ready.json').read_bytes()
            r.now = lambda: 'later-time'
            self.assertEqual(module.admit_retention(root, op, baseline), first)
            self.assertEqual((backup / 'retention-ready.json').read_bytes(), evidence)

    def test_transient_handoff_health_must_recover_without_pinning_failed_observation(self):
        with tempfile.TemporaryDirectory() as temporary:
            module, root, op, _, baseline, infos, _ = self.retention_fixture(temporary)
            state = load_json(op / 'release-state.json')
            state['candidate_summary']['health'] = 'unhealthy'
            atomic_json(op / 'release-state.json', state)
            module.admit_retention(root, op, baseline)
            infos['app']['State']['Health']['Status'] = 'unhealthy'
            with self.assertRaisesRegex(RuntimeError, 'unhealthy'):
                module.admit_retention(root, op, baseline)

    def test_existing_admission_rejects_same_image_replacement_restart_or_config_change(self):
        for name, field, changed in [('app', 'Id', 'same-image-new-container'),
                                     ('worker', 'Id', 'same-image-new-container'),
                                     ('front', 'Id', 'replacement'), ('postgres', 'Id', 'replacement'),
                                     ('worker', 'RestartCount', 1), ('app', 'networks', ['changed'])]:
            with self.subTest(name=name, field=field), tempfile.TemporaryDirectory() as temporary:
                module, root, op, backup, baseline, infos, _ = self.retention_fixture(temporary)
                module.admit_retention(root, op, baseline)
                evidence = (backup / 'retention-ready.json').read_bytes()
                infos[name][field] = changed
                with self.assertRaisesRegex(RuntimeError, 'identity or configuration changed'):
                    module.admit_retention(root, op, baseline)
                self.assertEqual((backup / 'retention-ready.json').read_bytes(), evidence)

    def test_finalize_pending_runs_finish_then_verification_and_never_apply(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve(); op = root / 'op'; op.mkdir()
            backup = root / 'backups/release'; backup.mkdir(parents=True)
            state = {'status': 'archive_pending', 'backup': str(backup)}
            atomic_json(op / 'release-state.json', state)
            baseline = {'helpers': {}, 'backup_policy': {}}
            atomic_json(op / 'baseline.json', baseline)
            common = SimpleNamespace(require=require, COMMIT='commit', host_required=lambda: None,
                                     load=load_json, helper_hashes=lambda _: {}, backup_policy_identity=lambda _: {})
            module = load_template('coordinator', common)
            calls = []
            @contextmanager
            def lock(_):
                yield
            def run(_op, name, arguments, *_, **__):
                calls.append((name, list(map(str, arguments))))
                if name == 'release.py':
                    self.assertIn('finish', arguments)
                    state['status'] = 'success'
                    atomic_json(op / 'release-state.json', state)
            with patch('sys.argv', ['coordinator.py', 'finalize', '--root', str(root), '--operation-dir', str(op)]), \
                 patch.object(module, 'exclusive', side_effect=lock), \
                 patch.object(module, 'run', side_effect=run), \
                 patch.object(module, 'admit_retention', return_value={'commit': 'commit'}):
                module.main()
            self.assertEqual([name for name, _ in calls], ['release.py', 'verify.py', 'verify-other-services.py'])
            self.assertFalse(any('apply' in args for _, args in calls))

    def test_verification_retry_preserves_prior_evidence(self):
        common = SimpleNamespace(require=require)
        module = load_template('coordinator', common)
        with tempfile.TemporaryDirectory() as temporary:
            op = Path(temporary)
            old = op / 'verification.json'; old.write_text('retained failure')
            target = module.next_evidence_path(op, old.name)
            self.assertNotEqual(target, old)
            self.assertEqual(target.parent, op)
            self.assertEqual(old.read_text(), 'retained failure')
            for name in ('../outside.json', '/outside.json', '', '.', None):
                with self.subTest(name=name), self.assertRaises(RuntimeError):
                    module.evidence_path(op, name)

    def test_capture_uses_requested_root_and_rejects_wrong_base_before_docker(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve() / 'custom-runtime'; root.mkdir()
            op = root / 'op'; op.mkdir()
            source = Path(__file__).with_name('paired') / 'verify-other-services.py'
            (op / source.name).write_bytes(source.read_bytes())
            common = SimpleNamespace(require=require, BASE_COMMIT='reviewed-base', WORKER='worker',
                                     host_required=lambda: None, helper_hashes=Mock(return_value={}), load=load_json)
            module = load_template('capture-services', common)
            current = {'commit': 'reviewed-base', 'status': 'healthy', 'container': 'custom-app'}
            atomic_json(root / 'current.json', current)
            rows = [{'Name': '/' + name, 'Id': name, 'Image': 'image', 'RestartCount': 0,
                     'State': {'Running': True, 'OOMKilled': False, 'Health': {'Status': 'healthy'},
                               'StartedAt': 'fixed-time'}, 'Config': {'Env': []}, 'Mounts': [],
                     'NetworkSettings': {'Networks': {}}, 'HostConfig': {'PortBindings': {}}}
                    for name in ('custom-app', 'worker', 'other-service')]
            argv = ['capture-services.py', '--root', str(root), '--operation-dir', str(op)]
            with patch('sys.argv', argv), patch.object(module.subprocess, 'check_output',
                    side_effect=[b'app\nworker\nother\n', json.dumps(rows).encode()]):
                module.main()
            report = load_json(op / 'other-services-before.json')
            self.assertEqual(report['excluded_containers'], ['custom-app', 'worker'])
            self.assertEqual([row['name'] for row in report['containers']], ['other-service'])
            current['commit'] = 'unreviewed-base'
            atomic_json(root / 'current.json', current)
            with patch('sys.argv', argv), patch.object(module.subprocess, 'check_output') as command:
                with self.assertRaisesRegex(RuntimeError, 'reviewed base'):
                    module.main()
                command.assert_not_called()


if __name__ == '__main__':
    unittest.main()
