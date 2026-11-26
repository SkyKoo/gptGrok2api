#!/usr/bin/env python3
"""One reviewed Adobe login isolation and email verification release with an explicit maintenance window.

No application request is replayed. After any activation attempt this tool never
rolls back data, starts the old program, or blindly retries the release. Use the
read-only status command to reconcile an interrupted/unknown outcome first.
"""
from __future__ import annotations

import argparse
from contextlib import contextmanager, nullcontext
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import time

import common
import snapshot
from paired_phases import ReleaseContext, finish_after_release, seal_raw_snapshot


class Maintenance:
    def __init__(self, args):
        self.args = args
        self.root, self.op = args.root, args.operation_dir
        self.r = common.rolling(self.root)
        self.host = self.r.Host()
        self.state_path = self.op / 'release-state.json'
        self.maintenance_token = None

    @property
    def maintenance_path(self):
        return self.root / 'data' / 'cfm-maintenance'

    def enter_maintenance(self):
        """Stop new scheduler rounds before waiting for existing work to drain."""
        path = self.maintenance_path
        common.require(path.parent.is_dir() and not path.parent.is_symlink(),
                       'Business data directory is unavailable')
        token = 'deploy:' + os.urandom(16).hex()
        try:
            descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL
                                 | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
        except FileExistsError as error:
            raise RuntimeError('An external CFM maintenance marker already exists') from error
        try:
            with os.fdopen(descriptor, 'w', encoding='ascii') as stream:
                stream.write(token)
                stream.flush()
                os.fsync(stream.fileno())
        except BaseException:
            try:
                path.unlink()
            except OSError:
                pass
            raise
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        self.maintenance_token = token

    def owns_maintenance(self):
        if not self.maintenance_token:
            return False
        try:
            info = self.maintenance_path.lstat()
            return (stat.S_ISREG(info.st_mode)
                    and self.maintenance_path.read_text(encoding='ascii') == self.maintenance_token)
        except (OSError, UnicodeError):
            return False

    def leave_maintenance(self):
        """Remove only the marker created by this release process."""
        if not self.maintenance_token:
            return
        common.require(self.owns_maintenance(),
                       'CFM maintenance marker changed; refusing to remove it')
        self.maintenance_path.unlink()
        directory = os.open(self.maintenance_path.parent,
                            os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        self.maintenance_token = None

    def event(self, phase, **values):
        self.state.update(phase=phase, updated_at=self.r.now(), **values)
        self.state['events'].append({'phase': phase, 'at': self.r.now()})
        self.r.atomic_json(self.state_path, self.state)
        self.r.atomic_json(self.root / 'rolling-status.json', self.state)
        # Retention reads this receipt only after final verification.
        if getattr(self, 'work', None) is not None:
            self.r.atomic_json(self.work / 'release.json', self.state)
        print(json.dumps({'phase': phase, 'status': self.state['status']}), flush=True)

    def helper(self, name, args, timeout=120):
        common.require(common.file_sha(self.op / name) == self.baseline['helpers'][name],
                       'Reviewed helper changed during maintenance')
        return common.run_helper(self.host, self.op, name, args, timeout=timeout)

    def worker(self, action):
        return self.helper('worker-release.py', ['--root', self.root, '--operation-dir', self.op,
                            '--candidate-manifest', self.args.worker_manifest, '--action', action], timeout=300)

    @contextmanager
    def measure(self, step):
        """记录安全的步骤耗时，异常详情不写入公开发布日志。"""
        started = time.monotonic()
        result = {'step': step, 'started_at': self.r.now(), 'status': 'success'}
        self.event(step + '_started')
        try:
            yield
        except BaseException as error:
            result.update(status='failed', error_type=type(error).__name__)
            raise
        finally:
            result['elapsed_seconds'] = round(time.monotonic() - started, 3)
            self.state.setdefault('timings', []).append(result)
            try:
                self.event(step + '_finished', last_step=result)
                print(json.dumps({'step': step, **result}), flush=True)
            except Exception:
                if result['status'] == 'success':
                    raise

    def state_guard(self):
        """只加载当前冻结包内已经绑定摘要的 guard。"""
        path = self.op / 'state-guard.py'
        common.require(common.file_sha(path) == self.baseline['helpers']['state-guard.py'],
                       'Reviewed state guard changed during maintenance')
        if not hasattr(self, '_state_guard'):
            spec = importlib.util.spec_from_file_location('cfm_paired_state_guard', path)
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)
            self._state_guard = module
        return self._state_guard

    def capture_state(self, name, *, before=None, source_capture=None):
        guard = self.state_guard()
        value = guard.capture(self.root, common.DEPENDENCIES['postgres'],
                              source_capture=source_capture)
        # 不覆盖之前的失败证据；比较失败也保留本轮实际状态。
        guard.private_write(self.op / name, value)
        if before is not None:
            guard.verify(before, value)
        return value

    def wait_idle(self, name, timeout):
        self.wait_idle_until(name, time.monotonic() + timeout)

    def hold_idle_gateway(self, deadline):
        status = self.host.gateway(self.front)
        original = status['route']
        common.require(original['held'] is False and original['upstream'] == self.old['upstream'],
                       'Ingress changed before hold')
        epoch = original['epoch']
        held = dict(original, held=True, epoch=epoch + 1)
        while True:
            self.host.checkpoint()
            common.require(time.monotonic() < deadline, 'Ingress did not drain before deadline')
            common.require(status['route'] == original, 'Ingress changed during drain')
            if status['active_requests'] == 0:
                try:
                    result = self.host.control(self.front, '/gateway/hold', {'epoch': epoch, 'upstream': ''})
                except (self.r.ReleaseError, subprocess.TimeoutExpired):
                    # A lost reply can follow a successful hold. The same epoch
                    # can be consumed only once; never adopt a newer epoch.
                    result = self.host.gateway(self.front)
                    if result['route'] != held:
                        common.require(result['route'] == original, 'Ingress changed after hold attempt')
                if result['route'] == held:
                    return
            time.sleep(.5)
            status = self.host.gateway(self.front)

    def wait_shared_work(self, deadline, *, fenced=False):
        """Drain shared work, including a task admitted just before the ingress hold."""
        previous = None
        while True:
            self.host.checkpoint()
            common.require(time.monotonic() < deadline, 'Shared work drain reached its deadline')
            capacity = common.worker_health(self.host).get('capacity')
            common.require(type(capacity) is int and capacity in (0, 1), 'Unexpected shared Worker capacity')
            try:
                queue = common.xai_authorization_queue_idle(self.host, self.root, self.old_name, require_idle=False)
            except Exception as error:
                # Keep queue contents and subprocess diagnostics out of the journal.
                reasons = {'Authorization queue status differs': 'unknown_status',
                           'Authorization queue read-only projection differs': 'invalid_projection',
                           'Configured admin key unavailable for authorization queue observation': 'admin_unavailable'}
                self.event('authorization_queue_read_failed', ingress_held=fenced,
                           reason=reasons.get(str(error), 'read_failed'))
                raise
            active = queue['active']
            common.require(time.monotonic() < deadline, 'Shared work drain reached its deadline')
            if capacity == 1 and active == 0:
                return queue
            if (capacity, active) != previous:
                self.event('shared_work_draining', worker_capacity=capacity, authorization_active=active,
                           ingress_held=fenced)
                previous = (capacity, active)
            time.sleep(min(2, max(0, deadline - time.monotonic())))

    def wait_idle_until(self, name, deadline):
        """Drain against one deadline shared by all handoff drain checks."""
        wait_number = getattr(self, '_drain_wait_number', 0) + 1
        self._drain_wait_number = wait_number
        started = time.monotonic()
        last_busy = None
        while True:
            state = self.host.control(name)
            if self.r.idle(state):
                break
            jobs = state.get('jobs') if isinstance(state.get('jobs'), dict) else {}
            busy = {key: value for key, value in jobs.items()
                    if isinstance(value, int) and value > 0}
            active_requests = state.get('active_requests')
            if isinstance(active_requests, int) and active_requests > 0:
                busy['active_requests'] = active_requests
            if busy != last_busy:
                self.state.setdefault('drain_waits', []).append({
                    'phase': 'observed', 'at': self.r.now(),
                    'wait_number': wait_number, 'busy': busy})
                self.event('drain_wait_observed', wait_number=wait_number,
                           busy=busy)
                last_busy = busy
            self.host.checkpoint()
            common.require(time.monotonic() < deadline, 'Natural task drain reached its deadline')
            time.sleep(0.5)
        self.state.setdefault('drain_waits', []).append({
            'phase': 'finished', 'at': self.r.now(), 'wait_number': wait_number,
            'waited_seconds': round(time.monotonic() - started, 3), 'busy': {}})
        self.event('drain_wait_finished', wait_number=wait_number,
                   waited_seconds=round(time.monotonic() - started, 3),
                   busy={})

    def unchanged_dependencies(self):
        # The legacy gateway health probe traverses held traffic and can time
        # out during this explicit maintenance window. Its loopback control
        # plane must still prove the exact original route and live process.
        for name in common.DEPENDENCIES.values():
            actual = self.host.inspect(name)
            expected = self.baseline['containers'][name]
            common.require(actual['Id'] == expected['id'] and common.healthy(actual),
                           'Database dependency changed or is unhealthy')
        actual = self.host.inspect(self.front)
        expected = self.baseline['containers'][self.front]
        summary = common.container_summary(actual)
        common.require(actual['Id'] == expected['id'] and actual['State']['Running']
                       and not actual['State']['OOMKilled'], 'Gateway process identity changed')
        common.require(all(summary[key] == expected[key] for key in summary if key != 'health'),
                       'Gateway runtime configuration changed')
        gateway = self.host.gateway(self.front)
        original_route = self.baseline['gateway']['route']
        held_route = dict(original_route, held=True, epoch=original_route['epoch'] + 1)
        common.require(gateway.get('protocol') == common.PROTOCOL
                       and gateway.get('route') == held_route
                       and gateway.get('active_requests') == 0,
                       'Gateway is not in the exact reviewed maintenance hold')

    def prove(self):
        common.require(not self.state_path.exists(),
                       'Release state already exists; inspect status instead of replaying apply')
        self.baseline = common.load(self.args.baseline)
        # The standalone preflight has already captured the complete business
        # size and symlink manifest. Reuse that reviewed size here so plan/apply
        # do not walk the large data/logs trees again. The live identity, route,
        # containers, worker contract and free-space reserve are still checked.
        # Browser lock discovery is intentionally limited to the initial full
        # preflight. The handoff checks Worker health and queue state again,
        # while the frozen snapshot rejects links and unstable files.
        live = common.preflight(
            self.root, self.args.release, self.args.worker_manifest, self.op,
            scan_business=False, required_business_bytes=self.baseline['business_bytes'],
            check_browser_locks=False)
        # Request/job counters and disk usage can legitimately change while serving.
        for key in ('current', 'source', 'candidate', 'worker', 'release_sha256',
                    'worker_manifest_sha256', 'worker_config_plan_sha256', 'helpers', 'production_files', 'backup_tool_sha256', 'backup_policy', 'backup_local_sha256', 'switches'):
            common.require(live[key] == self.baseline[key], 'Baseline changed: ' + key)
        common.require(live['gateway'].get('protocol') == self.baseline['gateway'].get('protocol')
                       and live['gateway'].get('route') == self.baseline['gateway'].get('route'),
                       'Gateway route or epoch changed after preflight')
        for name, expected in self.baseline['containers'].items():
            actual = live['containers'][name]
            common.require(actual == expected, 'A container changed after preflight')
        self.old = self.baseline['current']
        self.release = self.baseline['candidate']
        common.require(not self.state_path.exists(),
                       'Release state already exists; inspect status instead of replaying apply')

    def apply(self):
        self.prove()
        self.old_name = self.old['container']
        self.front = self.old['frontdoor']
        self.slot = 'blue' if self.old['slot'] == 'green' else 'green'
        self.candidate = self.r.SLOTS[self.slot][0]
        self.old_info = self.host.inspect(self.old_name)
        self.work = self.root / 'backups' / ('paired-' + common.COMMIT[:12] + '-'
                                           + time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
        self.work.mkdir(mode=0o700, parents=True)
        self.r.atomic_json(self.work / 'previous.json', self.old)
        self.r.atomic_json(self.work / 'baseline.json', self.baseline)
        self.state = {'version': 1, 'status': 'running', 'phase': 'prepared',
                      'commit': common.COMMIT, 'previous_commit': self.old['commit'],
                      'image_id': self.release['image_id'],
                      'worker_image_id': self.baseline['worker']['image_id'],
                      'candidate': self.candidate, 'candidate_slot': self.slot,
                      'backup': str(self.work), 'events': [], 'activation_attempted': False,
                      'release_attempted': False, 'data_restore_performed': False,
                      'retention_requires_final_verification': True}
        self.engine = self.r.RollingRelease(self.root, self.host)
        self.engine.work = self.work
        self.engine.front = self.front
        self.engine.owned_candidate = None
        self.snapshot = snapshot.make(self.r, self.root, self.work, self.host)
        self.worker_attempted = False
        self.frozen_state = None
        self.precopy_deferred = False
        self.event('prepared')
        try:
            self.r.merge_assets(self.host, self.old['image'], self.root / 'static-assets')
            self.r.merge_assets(self.host, self.release['image'], self.root / 'static-assets')
            self.event('candidate_start_attempted')
            self.engine.start_slot(self.old_info, self.release['image_id'], self.slot, 'candidate')
            self.event('standby_ready')
            # The application pause stops admission, while this marker stops
            # scheduler loops from starting another polling round. Existing
            # work is still allowed to finish and is counted by the normal
            # drain/seal boundary below.
            self.event('maintenance_marker_attempted')
            self.enter_maintenance()
            self.event('maintenance_marker_created', maintenance_marker=True)
            # 在线预热副本；只有冻结后的增量复制与完整验证才构成备份。
            with self.measure('online_precopy'):
                try:
                    self.snapshot.sync()
                except self.r.ReleaseError as error:
                    # 活跃浏览器可能临时创建 Singleton 链接或 socket。只延后这两种
                    # 已知布局拒绝；磁盘、权限及未知错误仍立即失败。
                    if str(error) not in ('Snapshot refuses symlinks', 'Snapshot refuses non-regular files'):
                        raise
                    self.precopy_deferred = True
                    self.event('online_precopy_deferred', precopy_deferred='live_layout')
            self.event('pause_attempted')
            self.host.action(self.old_name, 'pause')
            # The pre-hold and post-hold checks protect the same handoff
            # window. Start the shared deadline when draining begins so
            # candidate startup and online precopy do not consume it.
            drain_deadline = time.monotonic() + self.args.drain_timeout
            with self.measure('background_drain'):
                self.wait_idle_until(self.old_name, drain_deadline)
            common.require(self.host.gateway(self.front)['route']['upstream'] == self.old['upstream'],
                           'Ingress route changed during drain')
            if self.precopy_deferred:
                # The old writer is paused and naturally idle while ingress still
                # routes to it. Retry the copy here so the expensive full scan is
                # outside the gateway hold. The frozen pass remains mandatory.
                with self.measure('online_precopy_retry'):
                    self.snapshot.sync()
                self.event('online_precopy_completed_after_drain',
                           precopy_deferred='resolved_after_drain')
            self.wait_shared_work(drain_deadline)
            self.event('hold_attempted')
            self.hold_idle_gateway(drain_deadline)
            # An admitted request may race the initial drain and gateway fence.
            self.wait_idle_until(self.old_name, drain_deadline)
            self.event('checking_fenced_authorization_queue')
            # Give a late admission a short window under the fence. Both the
            # Worker and the writer's live queue must report empty in this pass.
            queue = self.wait_shared_work(min(drain_deadline, time.monotonic() + 45), fenced=True)
            common.require(queue['active'] == 0, 'Existing xAI authorization work must finish before release')
            self.event('xai_authorization_queue_drained', authorization_queue=queue)
            self.r.atomic_json(self.root / 'current.json', dict(self.old, status='switching',
                               candidate=self.candidate, candidate_slot=self.slot))
            self.event('freeze_attempted')
            frozen = self.host.action(self.old_name, 'freeze')
            common.require(frozen.get('phase') == 'frozen' and frozen.get('writer_lease') is False,
                           'Previous writer seal is unconfirmed')
            self.event('writer_frozen')
            with self.measure('frozen_copy'):
                self.snapshot.sync(frozen=True)
            with self.measure('database_export'):
                self.snapshot.database(common.DEPENDENCIES)
            # 冻结复制已读过源和副本内容，沿用其内存 Capture 并复核身份。
            with self.measure('frozen_state'):
                self.frozen_state = self.capture_state('state-frozen.json',
                                                      source_capture=self.snapshot.source_capture)
            before_files = self.snapshot.business_files()
            common.require(self.state_guard().business_files(self.frozen_state) == before_files,
                           'Frozen state differs from the backup file manifest')
            self.r.atomic_json(self.work / 'business-before.json',
                               {'files': before_files, 'database': self.frozen_state['database']})
            # Exact raw content/metadata and database dumps become durable here.
            # Compression/readback belongs after traffic and background resume.
            with self.measure('raw_seal'):
                seal_raw_snapshot(self.snapshot.seal, self.event)
            self.worker_attempted = True
            self.event('worker_switch_attempted')
            with self.measure('worker_switch'):
                # switch 自带完整验收，紧接着重复调用 verify 没有新增边界。
                self.worker('switch')
            # Persist intent BEFORE activation: a missing response may follow a
            # changed state. Never infer non-execution even for a no-migration release.
            self.event('activation_attempted', activation_attempted=True)
            self.host.action(self.candidate, 'activate')
            common.require(self.r.idle(self.host.control(self.candidate)),
                           'Candidate must remain paused, idle and leased after activation')
            with self.measure('state_after_activation'):
                # The candidate remains paused until this proof is complete.
                # The Worker switch has already passed its full fenced probe,
                # so a second state-guard scan before activation only repeats
                # the same database and business-tree checks. Keep one final
                # comparison here, immediately before any route change.
                self.capture_state('state-after.json', before=self.frozen_state,
                                   source_capture=self.snapshot.source_capture)
            self.event('state_unchanged_verified')
            self.host.run(['docker', 'exec', self.candidate, 'wget', '-q', '-T', '3', '-O', '/dev/null',
                           'http://127.0.0.1:8080/readyz'], timeout=8)
            with self.measure('worker_ready'):
                self.worker('verify-ready')
            self.unchanged_dependencies()
            common.require(common.saved_switches(self.root) == self.baseline['switches'],
                           'Saved registration switches changed')
            common.check_image(self.host, self.old['image'], self.old['image_id'], self.old['commit'], app=True)
            self.r.install_backup_tool(self.root, self.r.backup_tool_bytes(self.host, self.release))
            local_backup = self.r.backup_local_bytes(self.host, self.release)
            common.require(local_backup is not None and local_backup['package_id'] == self.baseline['backup_local_sha256'],
                           'Candidate local backup package differs from the reviewed policy')
            local_install = self.r.install_backup_local(self.root, local_backup)
            # The candidate is still paused and the old writer is sealed, so
            # removing the marker here cannot admit business work. If removal
            # is uncertain, traffic is not released and recovery preserves the
            # maintenance protection for operator review.
            self.leave_maintenance()
            self.event('maintenance_marker_removed', maintenance_marker=False)
            self.event('route_attempted', local_backup_package=local_install)
            self.host.gateway(self.front, 'route', self.r.SLOTS[self.slot][1])
            meta = self.engine.metadata(self.old, self.release, self.slot, status='switching')
            meta.update(grok_browser_container=self.baseline['worker']['container'],
                        grok_browser_image=self.baseline['worker']['image'],
                        grok_browser_image_id=self.baseline['worker']['image_id'],
                        grok_browser_commit=common.COMMIT, grok_browser_deployed_at=self.r.now(),
                        maintenance_operation=str(self.op), source_tree=self.baseline['source'])
            self.r.atomic_json(self.root / 'current.json', meta)
            self.event('release_attempted', release_attempted=True)
            self.host.gateway(self.front, 'release')
            self.event('traffic_released')
            self.host.action(self.candidate, 'resume')
            self.event('writer_resumed', writer_resumed=True)
            self.r.atomic_json(self.root / 'current.json', dict(meta, status='healthy'))
            # The old in-memory store is permanently sealed; it is never resumed.
            self.host.stop_empty(self.old_name)
            self.event('archive_pending', status='archive_pending', traffic_switched=True,
                       old_writer_stopped=True, state_unchanged_verified=True,
                       final_backup_verified=False,
                       candidate_container_id=self.host.inspect(self.candidate)['Id'],
                       worker_container_id=self.host.inspect(common.WORKER)['Id'],
                       candidate_summary=common.container_summary(self.host.inspect(self.candidate)),
                       worker_summary=common.container_summary(self.host.inspect(common.WORKER)))
        except BaseException:
            self.reconcile_failure()
            raise
        # Archive failures cannot enter the program recovery path above. The
        # live application has accepted traffic and may already contain writes.
        self.finish_backup()

    def finalization_context(self):
        return ReleaseContext(common.COMMIT, self.baseline['candidate']['image_id'],
                              self.baseline['worker']['image_id'], str(self.work))

    def verify_finalization_live(self):
        """Verify exact run identity without requiring ordinary jobs to be idle."""
        common.require(common.helper_hashes(self.op) == self.baseline['helpers'], 'Frozen helpers changed')
        common.require(common.file_sha(self.args.worker_manifest) == self.baseline['worker_manifest_sha256'],
                       'Worker manifest changed')
        current = common.load(self.root / 'current.json')
        common.require(current.get('status') == 'healthy' and current.get('commit') == common.COMMIT
                       and current.get('image_id') == self.baseline['candidate']['image_id']
                       and current.get('backup') == str(self.work)
                       and current.get('container') == self.state['candidate']
                       and current.get('frontdoor') == self.baseline['current']['frontdoor']
                       and current.get('upstream') == self.r.SLOTS[self.state['candidate_slot']][1],
                       'Backup finalization no longer matches the active release')
        app = self.host.inspect(current['container'])
        worker = self.host.inspect(common.WORKER)
        common.require(app['Id'] == self.state.get('candidate_container_id')
                       and app['Image'] == self.baseline['candidate']['image_id']
                       and app['RestartCount'] == 0 and common.healthy(app),
                       'Active application changed before backup finalization')
        common.require(worker['Id'] == self.state.get('worker_container_id')
                       and worker['Image'] == self.baseline['worker']['image_id']
                       and worker['RestartCount'] == 0 and common.healthy(worker),
                       'Active Worker changed before backup finalization')
        old_name = self.baseline['current']['container']
        old = self.host.inspect(old_name)
        common.require(old['Id'] == self.baseline['containers'][old_name]['id']
                       and old['State']['Running'] is False, 'Retired writer changed or resumed')
        control = self.host.control(current['container'])
        route = self.host.gateway(current['frontdoor'])
        common.require(control.get('protocol') == common.PROTOCOL and control.get('phase') == 'active'
                       and control.get('writer_lease') is True and control.get('paused') is False
                       and route.get('protocol') == common.PROTOCOL
                       and route['route'].get('held') is False
                       and route['route'].get('upstream') == current['upstream'],
                       'Active writer or ingress changed before backup finalization')
        self.host.run(['docker', 'exec', current['container'], 'wget', '-q', '-T', '3', '-O', '/dev/null',
                       'http://127.0.0.1:8080/readyz'], timeout=8)

    def finish_backup(self):
        def archive():
            with self.measure('archive_finalization'):
                return self.snapshot.finish()
        finish_after_release(self.finalization_context(), self.state, record=self.event,
                             verify_live=self.verify_finalization_live,
                             finish_archive=archive)

    def finish(self):
        """Resume backup finalization only; never replay activation or release."""
        self.baseline = common.load(self.args.baseline)
        self.state = common.load(self.state_path)
        self.work = Path(self.state['backup'])
        common.require(self.work.is_dir() and not self.work.is_symlink()
                       and self.work.parent == self.root / 'backups' and self.work.resolve() == self.work,
                       'Backup finalization path is outside the pinned backup root')
        self.finalization_context().verify(self.state)
        common.require(self.state.get('status') == 'archive_pending',
                       'Only an archive-pending release may resume finalization')
        self.verify_finalization_live()
        self.snapshot = snapshot.resume(self.r, self.root, self.work, self.host)
        self.finish_backup()

    def reconcile_failure(self):
        # The pipeline deadline bounds only the forward path. Recovery must be
        # allowed to restore a held ingress and writer lease after expiry.
        self.host.deadline = None
        self.host.overall_deadline = None
        if self.state['activation_attempted']:
            # A newly activated process may have initialized state even on failure.
            # If release's response was lost, inspect the gateway without routing,
            # stopping either process or replaying any business request.
            try:
                route = self.host.gateway(self.front)['route']
                self.event('needs_attention', status='needs_attention', gateway_route=route,
                           post_activation_recovery_required=True, instances_preserved=True)
            except BaseException:
                self.event('needs_attention', status='needs_attention', traffic_result='unknown',
                           instances_preserved=True)
            return
        try:
            old_state = self.host.control(self.old_name)
            gate = self.host.gateway(self.front)['route']
            common.require(gate['upstream'] == self.old['upstream'], 'Unexpected route during recovery')
            if old_state.get('phase') == 'active' and old_state.get('writer_lease') is True:
                common.require(not self.worker_attempted, 'Worker mutation with a live writer is unexpected')
                self.host.action(self.old_name, 'resume')
                if gate['held']:
                    self.host.gateway(self.front, 'release')
                if self.engine.owned_candidate:
                    self.engine.stop_candidate(self.engine.owned_candidate)
                self.r.atomic_json(self.root / 'current.json', self.old)
                self.leave_maintenance()
                self.event('aborted_before_seal', status='aborted', old_service_retained=True)
                return
            common.require(old_state.get('phase') == 'frozen' and old_state.get('writer_lease') is False
                           and gate['held'] is True, 'Writer seal/ingress outcome is unknown')
            if self.worker_attempted:
                # Helper reconciles actual image/config before deciding whether
                # replacement is needed; it never restores business files.
                self.worker('rollback')
            recovery_source = None
            if self.frozen_state is None:
                # A backup error may have happened before the guard was saved.
                # Capture the still-frozen live state before any fresh old app
                # initialization; incomplete backup is never used as a restore.
                self.frozen_state = self.capture_state('state-recovery-baseline.json')
            else:
                recovery_source = self.snapshot.source_capture
                self.capture_state('state-recovery-before.json', before=self.frozen_state,
                                   source_capture=recovery_source)
            if self.engine.owned_candidate:
                self.engine.stop_candidate(self.engine.owned_candidate)
            self.host.stop_empty(self.old_name)
            recovery = self.engine.start_slot(self.old_info, self.old['image_id'], self.old['slot'], 'recovery')
            self.event('old_program_recovery_activation_attempted')
            self.host.action(recovery, 'activate')
            common.require(self.r.idle(self.host.control(recovery)), 'Recovery writer is not paused and ready')
            self.capture_state('state-recovery-after.json', before=self.frozen_state,
                               source_capture=recovery_source)
            self.host.gateway(self.front, 'route', self.old['upstream'])
            self.host.gateway(self.front, 'release')
            self.host.action(recovery, 'resume')
            self.leave_maintenance()
            self.r.atomic_json(self.root / 'current.json', dict(self.old, recovered_at=self.r.now()))
            self.event('old_program_recovered', status='rolled_back', data_restore_performed=False)
            # Already generated plans may pin a rolling helper predating payload retirement.
            if hasattr(self.r, 'retire_rolled_back_payload'):
                self.r.retire_rolled_back_payload(self.root, self.work)
        except BaseException:
            self.event('needs_attention', status='needs_attention', instances_preserved=True,
                       data_restore_performed=False)


def parser():
    value = argparse.ArgumentParser(description=__doc__)
    value.add_argument('--root', type=Path, required=True)
    value.add_argument('--operation-dir', type=Path, default=Path(__file__).resolve().parent)
    commands = value.add_subparsers(dest='action', required=True)
    commands.add_parser('status')
    plan = commands.add_parser('plan')
    apply = commands.add_parser('apply')
    for command in (plan, apply):
        command.add_argument('--release', type=Path, required=True)
        command.add_argument('--worker-manifest', type=Path, required=True)
        command.add_argument('--baseline', type=Path, required=True)
        command.add_argument('--drain-timeout', type=int, default=600,
                             help='one shared natural-drain deadline for the handoff')
    finish = commands.add_parser('finish')
    finish.add_argument('--baseline', type=Path, required=True)
    finish.add_argument('--worker-manifest', type=Path, required=True)
    return value


def main():
    args = parser().parse_args()
    common.host_required()
    operation = Maintenance(args)
    if args.action == 'status':
        state = common.load(operation.state_path) if operation.state_path.exists() else None
        current = common.load(args.root / 'current.json')
        report = {'release': state, 'current': {key: current.get(key) for key in
                  ('commit', 'container', 'slot', 'status', 'candidate')},
                  'gateway': operation.host.gateway(current['frontdoor']), 'instances': {}}
        for name in {current['container'], (state or {}).get('candidate')} - {None}:
            try:
                info = operation.host.inspect(name)
                report['instances'][name] = {'running': info['State']['Running'], 'image_id': info['Image']}
                if info['State']['Running']:
                    report['instances'][name]['control'] = operation.host.control(name)
            except Exception:
                report['instances'][name] = {'observation': 'unavailable'}
        print(json.dumps(report, indent=2))
        return
    budget_check = common.require_pipeline_budget
    if args.action == 'finish' and hasattr(common, 'ensure_finalization_budget'):
        # Finalization is archive/read-only work after traffic is healthy. Its
        # marker is resumable, but it still consumes the original init-to-end
        # budget and can never create a second release window.
        common.ensure_finalization_budget(args.operation_dir)
        budget_check = common.require_finalization_budget
    budget_check(args.operation_dir)
    operation.host.overall_deadline = time.monotonic() + budget_check(args.operation_dir)
    if args.action in ('plan', 'apply'):
        common.require(args.drain_timeout > 0, 'Drain timeout must be positive')
    def interrupted(*_):
        raise KeyboardInterrupt()
    signal.signal(signal.SIGTERM, interrupted)
    timing = getattr(common, 'timing_phase', None)
    context = timing(args.operation_dir, 'release-' + args.action) if timing else nullcontext()
    with context:
        if args.action == 'plan':
            operation.prove()
            budget_check(args.operation_dir)
            print(json.dumps({'reviewed': True, 'commit': common.COMMIT,
                              'maintenance_window': True, 'data_restore_allowed': False,
                              'rollback_boundary': 'before first candidate activation attempt'}))
            return
        with (args.root / 'maintenance.lock').open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            if args.action == 'finish':
                operation.finish()
            else:
                operation.apply()
        # A successful release may cross the final timestamp while writing its
        # durable completion receipt. Preserve that success state for the
        # coordinator instead of reporting a false helper failure. An
        # archive-pending or incomplete state still has to respect the budget.
        if getattr(operation, 'state', {}).get('status') != 'success':
            budget_check(args.operation_dir)


if __name__ == '__main__':
    common.safe_main(main)
