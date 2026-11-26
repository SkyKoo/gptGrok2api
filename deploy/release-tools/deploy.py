#!/usr/bin/env python3
"""发布 GPTGrok2API：一致备份、短暂停机切换和受保护的自动回滚。"""
from __future__ import annotations

import argparse
import datetime as dt
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.request
import uuid


def local_backup_support():
    """Share fixed-commit package validation with the rolling deployment entry."""
    spec = importlib.util.spec_from_file_location('cfm_local_backup_install',
                                                 Path(__file__).with_name('rolling-deploy.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


TOOLS_DIR = Path(__file__).resolve().parent
ROOT = TOOLS_DIR.parent if TOOLS_DIR.name == 'tools' else None
PROJECT = 'gptgrok2api'
APP_SERVICE = 'app'
WORKER_SERVICE = 'grok-browser'
REDIS_SERVICE = 'redis'
POSTGRES_SERVICE = 'postgres'
APP_URL = 'http://127.0.0.1:8000'
SCHEDULE_GUARD_SECONDS = 600
COMMAND_TIMEOUT_SECONDS = 300
PEER_CONTAINERS = (
    'lumora-app-1',
    'new-api-new-api-1',
    'icloud-hme-icloud-hme-1',
    'web-entry-caddy-1',
)
IMAGE_TASK_TERMINAL_STATUSES = frozenset({'success', 'error'})
VOLATILE_ACCOUNT_FIELDS = frozenset({
    'capability_quotas', 'limits_progress', 'last_token_refresh_at',
    'quota_last_attempt_at', 'quota_observed_at', 'restore_at',
    'survival_last_checked_at', 'survival_status', 'survival_last_probe_status',
    'survival_alive', 'survival_plan_type', 'survival_check_error',
})


class DeploymentError(RuntimeError):
    """部署被安全检查、切换或恢复流程拒绝。"""


def worker_container_name(info: dict) -> str:
    name = info.get('Name')
    if name not in ('/gptgrok2api-grok-browser-1', '/cfm-browser-worker'):
        raise DeploymentError('共享浏览器容器名称不在已验证范围内。')
    return name[1:]


def utc_stamp() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ')


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


def sha256_file(path: Path) -> str:
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def atomic_write(path: Path, content: bytes, mode: int = 0o600) -> None:
    temporary = path.with_name(path.name + '.tmp')
    with temporary.open('wb') as stream:
        stream.write(content)
        stream.flush()
        os.fsync(stream.fileno())
    os.chmod(temporary, mode)
    os.replace(temporary, path)


def parse_time(value: str) -> dt.datetime:
    return dt.datetime.fromisoformat(value.replace('Z', '+00:00'))


def json_bytes(value: object) -> bytes:
    return (json.dumps(value, ensure_ascii=False, indent=2) + '\n').encode()


def normalized_account_bytes(path: Path) -> bytes:
    """Canonicalize account records while excluding probe-only fields.

    The OpenAI survival and capability schedulers update quota observations and
    timestamps as soon as the app starts. Those fields are runtime telemetry,
    while account identity, credentials and enablement remain protected by the
    deployment manifest check.
    """
    value = json.loads(path.read_text())

    def clean(item: object) -> object:
        if isinstance(item, dict):
            return {
                key: clean(child) for key, child in item.items()
                if key not in VOLATILE_ACCOUNT_FIELDS
            }
        if isinstance(item, list):
            return [clean(child) for child in item]
        return item

    return json.dumps(clean(value), ensure_ascii=False, sort_keys=True, separators=(',', ':')).encode()


def pid_alive(pid: object) -> bool:
    try:
        value = int(pid)
        if value <= 0:
            return False
        os.kill(value, 0)
        return True
    except (TypeError, ValueError, OSError):
        return False


class Deployment:
    """管理线上 CFM app；默认只回滚程序和发布指针，不回灌业务数据。"""

    def __init__(self, root: Path = ROOT, schedule_guard: int = SCHEDULE_GUARD_SECONDS,
                 command_timeout: int = COMMAND_TIMEOUT_SECONDS):
        self.root = Path(root)
        self.config = self.root / 'compose.yaml'
        self.state_file = self.root / 'current.json'
        self.progress_file = self.root / 'deploy-status.json'
        self.env_file = self.root / '.env'
        self.data = self.root / 'data'
        self.logs = self.root / 'logs'
        self.redis_data = self.root / 'redis-data'
        self.backups = self.root / 'backups'
        self.schedule_guard = schedule_guard
        self.command_timeout = int(os.environ.get('GPTGROK2API_COMMAND_TIMEOUT_SECONDS', command_timeout))
        self.phase = 'preflight'
        self.operation_started_at = utc_now()
        self.phase_started_monotonic = time.monotonic()
        self.phase_durations: dict[str, float] = {}
        self.progress_status = 'idle'
        self.progress_commit = ''
        self.progress_image = ''
        self.backup: Path | None = None
        self.before_app: dict | None = None
        self.before_worker: dict | None = None
        self.before_redis: dict | None = None
        self.before_postgres: dict | None = None
        self.old_env = b''
        self.old_current = b''
        self.old_config = b''
        self.old_image = ''
        self.old_worker_image = ''
        self.new_image = ''
        self.new_worker_image = ''
        self.stopped = False
        self.switched = False
        self.health_verified = False
        self.lock_stream = None
        self.maintenance_token: str | None = None
        self.retention_cleanup_cutoff: float | None = None
        self.retention_cleanup_roots: tuple[str, ...] = ()
        self.release_source: Path | None = None
        self.local_backup_release: str | None = None

    def docker(self, *args: str, capture: bool = False, check: bool = True,
               input_text: str | None = None) -> str:
        result = subprocess.run(
            ['docker', *args],
            check=check,
            text=True,
            input=input_text,
            stdout=subprocess.PIPE if capture else None,
            stderr=subprocess.PIPE if capture else None,
            timeout=self.command_timeout,
        )
        if capture:
            return result.stdout.strip()
        return ''

    def compose(self, image: str | None, *args: str, template: Path | None = None,
                capture: bool = False, check: bool = True) -> str:
        environment = os.environ.copy()
        for key in list(environment):
            if key.startswith('COMPOSE_') or key.startswith('GPTGROK2API_'):
                environment.pop(key, None)
        if image:
            environment['GPTGROK2API_IMAGE'] = image
        command = [
            'docker', 'compose', '--project-name', PROJECT,
            '--project-directory', str(self.root),
            '--env-file', str(self.env_file),
            '-f', str(template or self.config),
            *args,
        ]
        result = subprocess.run(
            command,
            check=check,
            text=True,
            env=environment,
            stdout=subprocess.PIPE if capture else None,
            stderr=subprocess.PIPE if capture else None,
            timeout=self.command_timeout,
        )
        if capture:
            return result.stdout.strip()
        return ''

    def acquire_lock(self) -> None:
        self.root.mkdir(mode=0o750, parents=True, exist_ok=True)
        self.lock_stream = (self.root / 'maintenance.lock').open('a+')
        try:
            fcntl.flock(self.lock_stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise DeploymentError('已有 GPTGrok2API 运维操作正在执行。') from error

    def state(self) -> dict | None:
        if not self.state_file.exists():
            return None
        return json.loads(self.state_file.read_text())

    def _write_progress(self, status: str | None = None, detail: str | None = None,
                        **extra: object) -> None:
        """写入可供 SSH 断线后查询的发布进度，不记录密钥或完整 Compose 配置。"""
        if status:
            self.progress_status = status
        payload: dict[str, object] = {
            'status': self.progress_status,
            'phase': self.phase,
            'pid': os.getpid(),
            'started_at': self.operation_started_at,
            'phase_started_at': getattr(self, 'phase_started_at', self.operation_started_at),
            'phase_elapsed_seconds': round(time.monotonic() - self.phase_started_monotonic, 1),
            'phase_durations_seconds': dict(self.phase_durations),
            'updated_at': utc_now(),
        }
        if self.progress_commit:
            payload['commit'] = self.progress_commit
        if self.progress_image:
            payload['image'] = self.progress_image
        if self.backup:
            payload['backup'] = str(self.backup)
        if detail:
            payload['detail'] = detail[:500]
        payload.update(extra)
        try:
            self.root.mkdir(mode=0o750, parents=True, exist_ok=True)
            atomic_write(self.progress_file, json_bytes(payload), 0o600)
        except OSError:
            # 进度文件是可观测性增强，不能遮蔽真正的发布结果。
            pass

    def begin_operation(self, commit: str, image: str) -> None:
        self.operation_started_at = utc_now()
        self.phase = 'preflight'
        self.phase_started_at = self.operation_started_at
        self.phase_started_monotonic = time.monotonic()
        self.phase_durations = {}
        self.progress_status = 'running'
        self.progress_commit = commit
        self.progress_image = image
        self._write_progress(detail='开始发布前置检查')
        print(f'[deploy] phase=preflight commit={commit} image={image}', flush=True)

    def set_phase(self, phase: str, detail: str | None = None) -> None:
        elapsed = round(time.monotonic() - self.phase_started_monotonic, 1)
        self.phase_durations[self.phase] = elapsed
        self.phase = phase
        self.phase_started_at = utc_now()
        self.phase_started_monotonic = time.monotonic()
        self._write_progress(detail=detail)
        message = f'[deploy] phase={phase}'
        if detail:
            message += f' {detail}'
        print(message, flush=True)

    def finish_operation(self, status: str, **extra: object) -> None:
        self.phase_durations[self.phase] = round(time.monotonic() - self.phase_started_monotonic, 1)
        self._write_progress(status=status, **extra)

    def mark_failed_if_running(self, error: BaseException) -> None:
        progress = None
        try:
            if self.progress_file.exists():
                progress = json.loads(self.progress_file.read_text())
        except (OSError, json.JSONDecodeError):
            return
        if progress and progress.get('status') == 'running':
            self._write_progress(status='failed', detail=str(error), error=str(error)[:400])

    def require_layout(self) -> None:
        required = [self.config, self.env_file, self.data, self.logs, self.redis_data,
                    self.root / 'postgres-data']
        missing = [str(path) for path in required if not path.exists()]
        if missing:
            raise DeploymentError('运行目录缺少必要路径：' + ', '.join(missing))
        self.backups.mkdir(mode=0o700, exist_ok=True)
        os.chmod(self.backups, 0o700)

    def inspect_service(self, service: str) -> dict:
        container_id = self.compose(None, 'ps', '-q', service, capture=True)
        if not container_id:
            raise DeploymentError(f'{service} 没有 Compose 容器。')
        output = self.docker('inspect', container_id, capture=True)
        return json.loads(output)[0]

    def service_healthy(self, service: str) -> bool:
        try:
            container = self.inspect_service(service)
        except Exception:
            return False
        state = container.get('State', {})
        health = state.get('Health', {}).get('Status')
        return state.get('Running') is True and health in (None, 'healthy')

    def container_environment(self, container: dict) -> dict[str, str]:
        return dict(item.split('=', 1) for item in container.get('Config', {}).get('Env', []) if '=' in item)

    def app_key(self, admin: bool) -> str:
        app = self.before_app or self.inspect_service(APP_SERVICE)
        key = 'CHATGPT2API_ADMIN_KEY' if admin else 'CHATGPT2API_AUTH_KEY'
        value = self.container_environment(app).get(key)
        if not value:
            raise DeploymentError(f'app 环境中缺少 {key}。')
        return value

    def request(self, path: str, admin: bool = True, authenticated: bool = True,
                timeout: int = 20) -> tuple[int, bytes]:
        headers = {'Accept': 'application/json'}
        if authenticated:
            headers['Authorization'] = 'Bearer ' + self.app_key(admin)
        request = urllib.request.Request(APP_URL + path, headers=headers)
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def api_json(self, path: str, admin: bool = True) -> dict:
        status, raw = self.request(path, admin=admin)
        if status < 200 or status >= 300:
            raise DeploymentError(f'{path} 返回 HTTP {status}。')
        try:
            value = json.loads(raw)
        except json.JSONDecodeError as error:
            raise DeploymentError(f'{path} 返回内容不是 JSON。') from error
        if not isinstance(value, dict):
            raise DeploymentError(f'{path} 返回 JSON 类型异常。')
        return value

    def owns_maintenance(self) -> bool:
        try:
            return bool(self.maintenance_token) and (self.data / 'cfm-maintenance').read_text() == self.maintenance_token
        except OSError:
            return False

    def enter_maintenance(self) -> None:
        marker = self.data / 'cfm-maintenance'
        token = 'deploy:' + uuid.uuid4().hex
        try:
            with marker.open('x') as stream:
                os.chmod(marker, 0o600)
                stream.write(token)
                stream.flush()
                os.fsync(stream.fileno())
        except FileExistsError as error:
            raise DeploymentError('已有外部维护标记，拒绝覆盖。') from error
        self.maintenance_token = token

    def leave_maintenance(self) -> None:
        if not self.maintenance_token:
            return
        if not self.owns_maintenance():
            raise DeploymentError('维护标记已改变，拒绝删除其他操作者的标记。')
        (self.data / 'cfm-maintenance').unlink()
        self.maintenance_token = None

    def wait_maintenance_idle(self) -> dict:
        deadline = time.monotonic() + 45
        while True:
            snapshot = self.active_snapshot()
            if not snapshot.get('known'):
                raise DeploymentError('维护时无法确认活动状态。')
            if not snapshot.get('active'):
                return self.ensure_idle()
            if time.monotonic() >= deadline:
                raise DeploymentError('维护后任务未归零：' + json.dumps(snapshot, ensure_ascii=False))
            time.sleep(2)

    def configure_retention_cleanup(self) -> None:
        """Freeze eligibility before backup, using only recognized runtime paths."""
        self.retention_cleanup_cutoff = None
        self.retention_cleanup_roots = ()
        if not self.before_app:
            return
        env = self.container_environment(self.before_app)
        try:
            days = max(1, min(3650, int(env.get('GO_IMAGE_RETENTION_DAYS', '1'))))
        except ValueError:
            return  # Unknown configuration must fail closed.
        roots = []
        if env.get('GO_IMAGE_DATA_DIR', 'data/files/images') in ('data/files/images', '/app/data/files/images'):
            roots.append('data/files/images/')
        if env.get('GROK_DATA_DIR', 'data') in ('data', '/app/data'):
            roots.append('data/log_input_images/')
        if env.get('GROK_ROOT_DIR', '/app') != '/app':
            return
        self.retention_cleanup_roots = tuple(roots)
        self.retention_cleanup_cutoff = time.time() - days * 86400

    def file_manifest(self) -> dict[str, dict[str, object]]:
        result: dict[str, dict[str, object]] = {}
        if not self.data.exists():
            return result
        for path in self.data.rglob('*'):
            if not path.is_file() or path == self.data / 'logs.jsonl':
                continue
            relative = str(path.relative_to(self.root))
            if relative == 'data/cfm-maintenance' and self.owns_maintenance():
                continue
            if relative == 'data/accounts.json':
                normalized = normalized_account_bytes(path)
                result[relative] = {
                    'sha256': hashlib.sha256(normalized).hexdigest(),
                    'size': len(normalized),
                    'normalization': 'account-probe-fields-v1',
                }
                continue
            if relative in ('data/.cfm-writer.lock', 'data/.cfm-legacy-instance.json'):
                valid = (path.stat().st_uid == 10001 and path.stat().st_gid == 10001
                         and path.stat().st_mode & 0o777 == 0o600 and not path.is_symlink())
                if relative.endswith('.lock'):
                    valid = valid and path.stat().st_size == 0
                else:
                    try:
                        valid = valid and json.loads(path.read_text()) == {'protocol':'cfm-single-writer-v1', 'phase':'active','paused':False}
                    except (ValueError, OSError):
                        valid = False
                if not valid:
                    raise DeploymentError('滚动协议文件状态未知，禁止旧入口切换。')
                result[relative] = {'sha256':sha256_file(path), 'size':path.stat().st_size, 'validated_protocol_runtime':True}
                continue
            entry: dict[str, object] = {
                'sha256': sha256_file(path),
                'size': path.stat().st_size,
            }
            if (self.retention_cleanup_cutoff is not None and
                    relative.startswith(self.retention_cleanup_roots) and
                    not path.is_symlink() and
                    path.stat().st_mtime < self.retention_cleanup_cutoff):
                entry['retention_expired_before_backup'] = True
            if relative.startswith('data/image_tasks/') and relative.endswith('.json'):
                try:
                    entry['task_status'] = json.loads(path.read_text()).get('status')
                except (OSError, json.JSONDecodeError):
                    entry['task_status'] = None
            result[relative] = entry
        return result

    @staticmethod
    def allowed_manifest_change(path: str, before: dict[str, object] | None,
                                after: dict[str, object] | None) -> bool:
        """Allow terminal tasks or already-expired image deletions only."""
        if path in ('data/.cfm-writer.lock','data/.cfm-legacy-instance.json'):
            return before is None and after is not None and after.get('validated_protocol_runtime') is True
        if path.startswith(('data/files/images/', 'data/log_input_images/')):
            return (before is not None and after is None and
                    before.get('retention_expired_before_backup') is True)
        if not path.startswith('data/image_tasks/') or not path.endswith('.json'):
            return False
        return (
            before is not None and after is None and
            before.get('task_status') in IMAGE_TASK_TERMINAL_STATUSES
        )

    def peer_snapshot(self) -> dict[str, dict[str, str]]:
        result = {}
        for name in PEER_CONTAINERS:
            output = self.docker('inspect', name, capture=True, check=False)
            if not output:
                continue
            try:
                container = json.loads(output)[0]
            except (IndexError, json.JSONDecodeError):
                continue
            result[name] = {
                'id': container['Id'],
                'started_at': container.get('State', {}).get('StartedAt', ''),
            }
        return result

    def active_snapshot(self) -> dict:
        """读取可证明的业务活动状态；无法读取时返回 known=false。"""
        try:
            register = self.api_json('/api/register')['register']
            monitor = self.api_json('/api/monitor/realtime')
            schedule = self.api_json('/api/register/schedule')['schedule']
            relogin = self.api_json('/api/accounts/relogin').get('items', [])
            twofa = self.api_json('/api/accounts/twofa').get('items', [])
            survival = self.api_json('/api/register/openai/survival').get('survival', {})
            grok_jobs = self.api_json('/api/register/grok/jobs').get('jobs', [])
            model_refreshes = self.api_json('/api/accounts/models/refresh').get('active', 0)
            image_tasks = []
            for path in (self.data / 'image_tasks').glob('*.json'):
                image_tasks.append(json.loads(path.read_text()))
            schedule_next = None
            if schedule.get('enabled') and schedule.get('next_run_at'):
                schedule_next = (parse_time(schedule['next_run_at']) - dt.datetime.now(dt.timezone.utc)).total_seconds()
            active = {
                'maintenance_marker': (self.data / 'cfm-maintenance').exists(),
                'maintenance_owned': self.owns_maintenance(),
                'model_refreshes': model_refreshes,
                'registration_enabled': bool(register.get('enabled')),
                'registration_jobs': sum(item.get('status') in ('queued', 'running') for item in register.get('jobs', [])),
                'grok_registration_jobs': sum(item.get('status') in ('queued', 'running') for item in grok_jobs),
                'realtime_requests': len(monitor.get('active', [])),
                'image_tasks': sum(item.get('status') in ('queued', 'running') for item in image_tasks),
                'relogin': sum(not item.get('done') for item in relogin),
                'twofa': sum(item.get('status') == 'running' for item in twofa),
                'survival': bool(survival.get('running')),
                'schedule_enabled': bool(schedule.get('enabled')),
                'schedule_seconds': schedule_next,
            }
            active['schedule_window_unsafe'] = bool(
                schedule.get('enabled') and
                (schedule_next is None or schedule_next <= self.schedule_guard)
            )
            active['active'] = any([
                active['maintenance_marker'] and not active['maintenance_owned'],
                active['model_refreshes'], active['registration_enabled'],
                active['registration_jobs'], active['grok_registration_jobs'],
                active['realtime_requests'], active['image_tasks'], active['relogin'],
                active['twofa'], active['survival'],
                active['schedule_window_unsafe'],
            ])
            return {'known': True, **active}
        except Exception as error:
            return {'known': False, 'active': True, 'error': str(error)[:400]}

    def ensure_idle(self) -> dict:
        if not all(self.service_healthy(service) for service in
                   (APP_SERVICE, WORKER_SERVICE, REDIS_SERVICE, POSTGRES_SERVICE)):
            raise DeploymentError('app、Grok Worker、Redis 或 PostgreSQL 未处于 healthy 状态，拒绝发布。')
        snapshot = self.active_snapshot()
        if not snapshot.get('known'):
            raise DeploymentError('无法确认线上活动状态：' + str(snapshot.get('error', 'unknown')))
        if snapshot.get('active'):
            raise DeploymentError('线上仍有注册、登录、图片、请求或定时任务活动：' + json.dumps(snapshot, ensure_ascii=False))
        return snapshot

    def verify_candidate(self, image: str, worker_image: str, template: Path, commit: str) -> dict:
        if not template.is_file():
            raise DeploymentError(f'部署模板不存在：{template}')
        image_info = json.loads(self.docker('image', 'inspect', image, capture=True))[0]
        architecture = image_info.get('Architecture')
        if architecture != 'arm64' or image_info.get('Os') != 'linux':
            raise DeploymentError(f'镜像架构不是 linux/arm64：{architecture}')
        revision = image_info.get('Config', {}).get('Labels', {}).get('org.opencontainers.image.revision')
        if revision != commit:
            raise DeploymentError(f'镜像 revision {revision!r} 与提交 {commit} 不一致。')
        worker_info = json.loads(self.docker('image', 'inspect', worker_image, capture=True))[0]
        if worker_info.get('Architecture') != 'arm64' or worker_info.get('Os') != 'linux':
            raise DeploymentError('Grok Worker 镜像架构不是 linux/arm64。')
        worker_revision = worker_info.get('Config', {}).get('Labels', {}).get('org.opencontainers.image.revision')
        if worker_revision != commit:
            raise DeploymentError(f'Grok Worker 镜像 revision {worker_revision!r} 与提交不一致。')
        self.compose(image, 'config', '--quiet', template=template)
        return {
            'image_id': image_info['Id'],
            'worker_image_id': worker_info['Id'],
            'architecture': architecture,
            'revision': revision,
            'worker_revision': worker_revision,
        }

    def backup_consistent(self, backup: Path) -> dict:
        required = ['compose.yaml', '.env', 'current.json', 'data', 'logs', 'redis-data', 'postgres-data', 'tools']
        missing = [name for name in required if not (self.root / name).exists()]
        if missing:
            raise DeploymentError('一致备份缺少路径：' + ', '.join(missing))
        self.compose(self.old_image, 'stop', APP_SERVICE, WORKER_SERVICE, REDIS_SERVICE)
        self.stopped = True
        if any(self.service_healthy(service) for service in (APP_SERVICE, WORKER_SERVICE, REDIS_SERVICE)):
            raise DeploymentError('停止后 app、Grok Worker 或 Redis 仍在运行，拒绝继续。')
        for name, mode in (('compose.yaml', 0o644), ('.env', 0o600), ('current.json', 0o600)):
            saved = backup / name
            shutil.copy2(self.root / name, saved)
            os.chmod(saved, mode)
        caddy = Path('/opt/web-entry/Caddyfile')
        if caddy.exists():
            shutil.copy2(caddy, backup / 'Caddyfile')
        entries = required + (['access.json'] if (self.root / 'access.json').exists() else [])
        archive = backup / 'snapshot.tar.gz'
        subprocess.run(
            ['tar', '--numeric-owner', '-czf', str(archive), '-C', str(self.root), *entries],
            check=True,
            timeout=self.command_timeout,
        )
        archive_digest = sha256_file(archive)
        (backup / 'SHA256SUMS').write_text(archive_digest + '  snapshot.tar.gz\n')
        with tarfile.open(archive) as package:
            names = {member.name for member in package.getmembers()}
        for name in required:
            if name not in names:
                raise DeploymentError(f'备份归档缺少 {name}。')
        postgres = self.inspect_service(POSTGRES_SERVICE)
        dump_path = backup / 'postgres.dump'
        globals_path = backup / 'postgres-globals.sql'
        for command, output_path in (
            (['sh', '-c', 'exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc'], dump_path),
            (['sh', '-c', 'exec pg_dumpall -U "$POSTGRES_USER" --globals-only'], globals_path),
        ):
            with output_path.open('wb') as output:
                subprocess.run(
                    ['docker', 'exec', '-i', postgres['Id'], *command],
                    check=True, stdout=output, stderr=subprocess.PIPE, timeout=self.command_timeout,
                )
            if output_path.stat().st_size == 0:
                raise DeploymentError(f'PostgreSQL 备份为空：{output_path.name}')
        dump_digest = sha256_file(dump_path)
        globals_digest = sha256_file(globals_path)
        with (backup / 'SHA256SUMS').open('a') as sums:
            sums.write(f'{dump_digest}  postgres.dump\n{globals_digest}  postgres-globals.sql\n')
        with dump_path.open('rb') as dump:
            check = subprocess.run(
                ['docker', 'exec', '-i', postgres['Id'], 'pg_restore', '--list'],
                stdin=dump, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                check=True, timeout=self.command_timeout,
            )
        if not check.stdout.strip():
            raise DeploymentError('PostgreSQL custom dump 清单为空。')
        (backup / 'complete').touch()
        return {
            'archive': str(archive), 'archive_sha256': archive_digest, 'entries': entries,
            'postgres_dump': str(dump_path), 'postgres_dump_sha256': dump_digest,
            'postgres_globals': str(globals_path), 'postgres_globals_sha256': globals_digest,
        }

    def replace_env_images(self, image: str, worker_image: str) -> None:
        text = self.old_env.decode()
        lines = text.splitlines(keepends=True)
        replaced = {'app': False, 'worker': False}
        output = []
        for line in lines:
            if line.startswith('GPTGROK2API_IMAGE='):
                newline = '\n' if line.endswith('\n') else ''
                output.append('GPTGROK2API_IMAGE=' + image + newline)
                replaced['app'] = True
            elif line.startswith('GPTGROK2API_GROK_BROWSER_IMAGE='):
                newline = '\n' if line.endswith('\n') else ''
                output.append('GPTGROK2API_GROK_BROWSER_IMAGE=' + worker_image + newline)
                replaced['worker'] = True
            else:
                output.append(line)
        if not all(replaced.values()):
            raise DeploymentError('.env 缺少 app 或 Grok Worker 镜像配置。')
        atomic_write(self.env_file, ''.join(output).encode(), 0o600)

    def start_services(self, image: str, worker_image: str | None = None) -> None:
        del worker_image  # 镜像由 Compose 的 .env 读取，参数用于回滚接口保持对称。
        self.compose(image, 'up', '-d', '--no-deps', '--pull', 'never',
                     '--wait', '--wait-timeout', '120', POSTGRES_SERVICE, REDIS_SERVICE, WORKER_SERVICE)
        self.compose(image, 'up', '-d', '--no-deps', '--pull', 'never',
                     '--wait', '--wait-timeout', '120', APP_SERVICE)

    def worker_mounts_match(self, before: list, after: list) -> bool:
        """Allow only the approved first readonly license-file mount addition."""
        if before == after:
            return True
        target = '/root/.cloakbrowser/license.key'
        if any(item.get('Destination') == target for item in before):
            return False  # Existing license mounts may not change in this migration.
        added = [item for item in after if item.get('Destination') == target]
        if len(added) != 1:
            return False
        mount = added[0]
        # Docker's Mount API reports Mode='' for readonly binds; RW is authoritative.
        if (mount.get('Type') != 'bind' or
                mount.get('Source') != str(self.root / 'secrets' / 'cloakbrowser-license.key') or
                mount.get('RW') is not False or mount.get('Mode') not in ('', 'ro') or
                mount.get('Propagation') != 'rprivate'):
            return False
        return [item for item in after if item.get('Destination') != target] == before

    def verify_running(self, image: str, worker_image: str, before_manifest: dict, before_peers: dict,
                       before_schedule: dict | None, before_counts: dict) -> dict:
        app = self.inspect_service(APP_SERVICE)
        worker = self.inspect_service(WORKER_SERVICE)
        redis = self.inspect_service(REDIS_SERVICE)
        postgres = self.inspect_service(POSTGRES_SERVICE)
        image_info = json.loads(self.docker('image', 'inspect', image, capture=True))[0]
        worker_info = json.loads(self.docker('image', 'inspect', worker_image, capture=True))[0]
        if app.get('Config', {}).get('Image') != image or app.get('Image') != image_info['Id']:
            raise DeploymentError('运行中的 app 镜像与目标镜像不一致。')
        if app.get('State', {}).get('Health', {}).get('Status') != 'healthy':
            raise DeploymentError('新 app 未通过 Docker healthcheck。')
        if worker.get('Config', {}).get('Image') != worker_image or worker.get('Image') != worker_info['Id']:
            raise DeploymentError('运行中的 Grok Worker 镜像与目标镜像不一致。')
        if worker.get('State', {}).get('Health', {}).get('Status') != 'healthy':
            raise DeploymentError('新 Grok Worker 未通过 Docker healthcheck。')
        if redis.get('State', {}).get('Health', {}).get('Status') != 'healthy':
            raise DeploymentError('Redis 未通过 Docker healthcheck。')
        if postgres.get('State', {}).get('Health', {}).get('Status') != 'healthy':
            raise DeploymentError('PostgreSQL 未通过 Docker healthcheck。')
        if self.before_app and app.get('Mounts') != self.before_app.get('Mounts'):
            raise DeploymentError('app 挂载发生变化。')
        if self.before_app and app.get('HostConfig', {}).get('PortBindings') != self.before_app.get('HostConfig', {}).get('PortBindings'):
            raise DeploymentError('app 端口映射发生变化。')
        if self.before_app and set(app.get('NetworkSettings', {}).get('Networks', {})) != set(self.before_app.get('NetworkSettings', {}).get('Networks', {})):
            raise DeploymentError('app 网络发生变化。')
        if self.before_worker and not self.worker_mounts_match(self.before_worker.get('Mounts', []), worker.get('Mounts', [])):
            raise DeploymentError('Grok Worker 挂载发生变化。')
        if self.before_worker and set(worker.get('NetworkSettings', {}).get('Networks', {})) != set(self.before_worker.get('NetworkSettings', {}).get('Networks', {})):
            raise DeploymentError('Grok Worker 网络发生变化。')
        if self.before_redis and redis.get('Id') != self.before_redis.get('Id'):
            raise DeploymentError('Redis 容器被替换，拒绝视为成功。')
        if self.before_postgres and postgres.get('Id') != self.before_postgres.get('Id'):
            raise DeploymentError('PostgreSQL 容器被替换，拒绝视为成功。')
        status, raw = self.request('/health', authenticated=False)
        if status != 200:
            raise DeploymentError(f'/health 返回 HTTP {status}。')
        health = json.loads(raw)
        if health.get('status') != 'ok':
            raise DeploymentError('/health 状态不是 ok。')
        accounts = self.api_json('/api/accounts')
        registration = self.api_json('/api/register')['register']
        schedule = self.api_json('/api/register/schedule')['schedule']
        if accounts.get('total') != before_counts.get('accounts'):
            raise DeploymentError('账号数量在发布过程中变化。')
        if registration.get('jobs_total') != before_counts.get('registration_records'):
            raise DeploymentError('注册历史数量在发布过程中变化。')
        if before_schedule is not None and schedule != before_schedule:
            raise DeploymentError('注册定时配置在发布过程中变化。')
        after_manifest = self.file_manifest()
        changed_paths = []
        allowed_changes = []
        for path in sorted(set(before_manifest) | set(after_manifest)):
            before_entry = before_manifest.get(path)
            after_entry = after_manifest.get(path)
            if before_entry == after_entry:
                continue
            if self.allowed_manifest_change(path, before_entry, after_entry):
                allowed_changes.append(path)
                continue
            changed_paths.append(path)
        if changed_paths:
            raise DeploymentError('业务数据文件在维护期间发生变化：' + ', '.join(changed_paths[:12]))
        after_peers = self.peer_snapshot()
        if after_peers != before_peers:
            raise DeploymentError('其他服务容器发生变化。')
        return {
            'health': health,
            'accounts': accounts.get('total'),
            'registration_records': registration.get('jobs_total'),
            'data_files': len(after_manifest),
            'allowed_image_task_cleanup': allowed_changes,
            'peers_unchanged': True,
            'redis_container_retained': True,
            'postgres_container_retained': True,
            'worker_healthy': True,
        }

    def write_failure(self, backup: Path | None, result: dict) -> None:
        if not backup:
            return
        try:
            (backup / 'deploy-result.json').write_bytes(json_bytes(result))
        except OSError:
            pass

    def restore_program(self, old_image: str, backup: Path) -> None:
        # start_services() 可能在启动依赖后才启动 app；恢复前先清理候选服务。
        if self.new_image:
            self.compose(self.new_image, 'stop', APP_SERVICE, WORKER_SERVICE, REDIS_SERVICE, check=False)
        atomic_write(self.env_file, self.old_env, 0o600)
        atomic_write(self.config, self.old_config, 0o644)
        atomic_write(self.state_file, self.old_current, 0o600)
        self.start_services(old_image, self.old_worker_image)

    def safe_rollback_after_failure(self, failure: BaseException, result: dict) -> None:
        if not self.stopped:
            result.update(status='unchanged', failure=str(failure)[:400])
            self.write_failure(self.backup, result)
            return
        if self.phase == 'verify' and self.health_verified:
            activity = self.active_snapshot()
            if not activity.get('known') or activity.get('active'):
                result.update(
                    status='rollback_deferred',
                    failure=str(failure)[:400],
                    rollback='新版本可能已有业务写入，保留现场和当前数据，拒绝自动回灌旧数据。',
                    activity=activity,
                )
                self.write_failure(self.backup, result)
                return
        try:
            self.restore_program(self.old_image, self.backup or self.root / 'backups')
            result.update(status='rolled_back', failure=str(failure)[:400])
        except BaseException as rollback_error:
            result.update(status='rollback_failed', failure=str(failure)[:400], rollback_error=str(rollback_error)[:400])
        self.write_failure(self.backup, result)

    def require_legacy_strategy(self):
        current = self.state() or {}
        if current.get('release_strategy') == 'rolling-single-writer-v1' or (self.data/'.cfm-gateway.json').exists():
            raise DeploymentError('已进入滚动布局或交接中，禁止旧 deploy/apply/rollback；请使用 rolling 入口。')

    def activate(self, image: str, worker_image: str, commit: str, template: Path) -> None:
        try:
            self._activate(image, worker_image, commit, template)
        finally:
            if self.progress_status in ('rollback_failed', 'rollback_deferred'):
                print('保留本次维护标记：回滚状态未确认，需人工核实后恢复。', file=sys.stderr)
            else:
                self.leave_maintenance()

    def _activate(self, image: str, worker_image: str, commit: str, template: Path) -> None:
        if os.geteuid() != 0:
            raise DeploymentError('deploy.py 必须由 deploy.sh 通过 sudo 调用。')
        self.require_legacy_strategy()
        self.begin_operation(commit, image)
        self.require_layout()
        current = self.state()
        if not current:
            raise DeploymentError('没有 current.json；请先完成一次受控初始化。')
        self.old_image = current.get('image', '')
        self.old_worker_image = current.get('grok_browser_image', '')
        if not self.old_image:
            raise DeploymentError('current.json 缺少旧镜像。')
        if not self.old_worker_image:
            raise DeploymentError('current.json 缺少 Grok Worker 镜像。')
        self.new_image = image
        self.new_worker_image = worker_image
        self.before_app = self.inspect_service(APP_SERVICE)
        self.before_worker = self.inspect_service(WORKER_SERVICE)
        self.before_redis = self.inspect_service(REDIS_SERVICE)
        self.before_postgres = self.inspect_service(POSTGRES_SERVICE)
        if self.before_app.get('Config', {}).get('Image') != self.old_image:
            raise DeploymentError('current.json 与运行中的 app 镜像不一致，拒绝覆盖。')
        if self.before_worker.get('Config', {}).get('Image') != self.old_worker_image:
            raise DeploymentError('current.json 与运行中的 Grok Worker 镜像不一致，拒绝覆盖。')
        self.docker('image', 'inspect', self.old_image)
        backup_support = local_backup_support()
        source = self.release_source or current.get('source')
        backup_host = backup_support.Host()
        try:
            backup_local = backup_support.prepare_backup_local_source(backup_host, self.root, source, commit)
            backup_core = None
            if backup_local is not None:
                source = Path(source).resolve()
                core = backup_host.run(['git', '-c', 'safe.directory='+str(source), '-C', str(source),
                                        'show', commit+':'+backup_support.BACKUP_SOURCE])
                backup_core = backup_support.backup_tool_bytes(backup_host, {
                    'source': str(source), 'commit': commit, 'backup_tool_sha256': backup_support.digest(core)})
        except backup_support.ReleaseError as error:
            raise DeploymentError(str(error)) from error
        candidate = self.verify_candidate(image, worker_image, template, commit)
        self.old_env = self.env_file.read_bytes()
        self.old_current = self.state_file.read_bytes()
        self.old_config = self.config.read_bytes()
        self.ensure_idle()
        self.enter_maintenance()
        idle = self.wait_maintenance_idle()
        self.configure_retention_cleanup()
        before_manifest = self.file_manifest()
        before_peers = self.peer_snapshot()
        before_counts = {
            'accounts': self.api_json('/api/accounts').get('total'),
            'registration_records': self.api_json('/api/register')['register'].get('jobs_total'),
        }
        before_schedule = self.api_json('/api/register/schedule')['schedule']
        stamp = utc_stamp()
        self.backup = self.backups / f'{stamp}-{commit[:12]}'
        self.backup.mkdir(mode=0o700)
        (self.backup / 'preflight.json').write_bytes(json_bytes({
            'checked_at': utc_now(), 'commit': commit, 'image': image,
            'current': current, 'idle': idle, 'candidate': candidate,
            'before_manifest': before_manifest, 'before_peers': before_peers,
        }))
        result = {'started_at': utc_now(), 'commit': commit, 'image': image, 'phase': 'preflight'}
        try:
            self.set_phase('backup', '停止 app/Worker/Redis 并制作 PostgreSQL 一致备份')
            backup_info = self.backup_consistent(self.backup)
            result.update(backup=backup_info, phase='switch')
            self.set_phase('switch', '切换镜像并启动服务')
            shutil.copy2(template, self.config)
            os.chmod(self.config, 0o644)
            self.replace_env_images(image, worker_image)
            self.start_services(image, worker_image)
            self.health_verified = True
            self.switched = True
            result['phase'] = 'verify'
            self.set_phase('verify', '执行健康接口、数据和同机服务校验')
            verification = self.verify_running(image, worker_image, before_manifest, before_peers, before_schedule, before_counts)
            backup_support.install_backup_tool(self.root, backup_core)
            installed_local = backup_support.install_backup_local(self.root, backup_local)
            new_state = json.loads(self.old_current)
            new_state.update({
                'image': image, 'commit': commit, 'source_commit': commit,
                'architecture': 'arm64', 'image_id': candidate['image_id'],
                'grok_browser_image': worker_image,
                'grok_browser_image_id': candidate['worker_image_id'],
                'grok_browser_container': worker_container_name(self.inspect_service(WORKER_SERVICE)),
                'deployed_at': utc_now(), 'previous_image': self.old_image,
                'previous_image_id': self.before_app.get('Image'),
                'backup': str(self.backup), 'consistent_backup': str(self.backup),
                'template': str(template),
            })
            if source is not None:
                new_state['source'] = str(source)
            atomic_write(self.state_file, json_bytes(new_state), 0o600)
            result.update(status='success', verification=verification, finished_at=utc_now(),
                          local_backup_package=installed_local,
                          retention_requires_final_verification=installed_local is not None)
            self.finish_operation('success', finished_at=result['finished_at'])
            self.write_failure(self.backup, result)
            if installed_local is not None:
                atomic_write(self.backup/'retention-ready.json', json_bytes({
                    'status': 'verified', 'commit': commit, 'backup': str(self.backup),
                    'verified_at': utc_now(), 'host_verification_sha256': hashlib.sha256(json_bytes(verification)).hexdigest(),
                }), 0o600)
                self.local_backup_release = commit
            print(json.dumps(result, ensure_ascii=False, indent=2))
        except BaseException as failure:
            result['phase'] = self.phase
            self.safe_rollback_after_failure(failure, result)
            self.finish_operation(result.get('status', 'failed'), finished_at=utc_now(),
                                  error=str(failure)[:400])
            print(json.dumps(result, ensure_ascii=False, indent=2), file=sys.stderr)
            raise DeploymentError(f'发布失败，结果：{result.get("status", "unknown")}') from failure

    def apply(self) -> None:
        self.require_legacy_strategy()
        current = self.state()
        if not current:
            raise DeploymentError('没有 current.json，无法应用当前镜像。')
        template = self.root / 'tools' / 'compose.yaml'
        if not template.exists():
            template = self.config
        self.activate(current['image'], current['grok_browser_image'], current['commit'], template)

    def rollback(self) -> None:
        self.require_legacy_strategy()
        if os.geteuid() != 0:
            raise DeploymentError('deploy.py 必须由 deploy.sh 通过 sudo 调用。')
        self.require_layout()
        current = self.state()
        if not current or not current.get('backup'):
            raise DeploymentError('没有可用的发布备份。')
        backup = Path(current['backup'])
        previous_file = backup / 'current.json'
        previous_env = backup / '.env'
        previous_config = backup / 'compose.yaml'
        if not all(path.exists() for path in (previous_file, previous_env, previous_config)):
            raise DeploymentError('发布备份缺少旧版本配置。')
        previous = json.loads(previous_file.read_text())
        old_image = previous.get('image')
        if not old_image:
            raise DeploymentError('旧版本记录缺少镜像。')
        self.docker('image', 'inspect', old_image)
        old_running = self.inspect_service(APP_SERVICE)
        self.compose(current.get('image'), 'stop', APP_SERVICE, WORKER_SERVICE, REDIS_SERVICE)
        atomic_write(self.env_file, previous_env.read_bytes(), 0o600)
        atomic_write(self.config, previous_config.read_bytes(), 0o644)
        previous_worker = previous.get('grok_browser_image', '')
        if not previous_worker:
            raise DeploymentError('旧版本记录缺少 Grok Worker 镜像。')
        self.start_services(old_image, previous_worker)
        restored = json.loads(previous_file.read_text())
        restored.update({'rolled_back_at': utc_now(), 'rollback_from': current.get('image')})
        atomic_write(self.state_file, json_bytes(restored), 0o600)
        result = {'status': 'rolled_back', 'from': current.get('image'), 'to': old_image,
                  'previous_container_id': old_running.get('Id'), 'finished_at': utc_now()}
        (backup / 'manual-rollback-result.json').write_bytes(json_bytes(result))
        print(json.dumps(result, ensure_ascii=False, indent=2))

    def status(self) -> None:
        current = self.state()
        progress = None
        if self.progress_file.exists():
            try:
                progress = json.loads(self.progress_file.read_text())
                if progress.get('status') == 'running':
                    pid = progress.get('pid')
                    progress['pid_alive'] = pid_alive(pid)
                    progress['stale'] = not progress['pid_alive']
            except (OSError, json.JSONDecodeError) as error:
                progress = {'status': 'unreadable', 'error': str(error)[:400]}
        result = {'operation': progress, 'current': current,
                  'app_healthy': self.service_healthy(APP_SERVICE),
                  'worker_healthy': self.service_healthy(WORKER_SERVICE),
                  'postgres_healthy': self.service_healthy(POSTGRES_SERVICE),
                  'redis_healthy': self.service_healthy(REDIS_SERVICE)}
        try:
            result['compose_ps'] = self.compose(None, 'ps', capture=True)
        except Exception as error:
            result['compose_error'] = str(error)[:400]
        print(json.dumps(result, ensure_ascii=False, indent=2))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['deploy', 'apply', 'rollback', 'status'])
    parser.add_argument('--root', type=Path, default=ROOT, required=ROOT is None)
    parser.add_argument('--image')
    parser.add_argument('--worker-image')
    parser.add_argument('--commit')
    parser.add_argument('--template', type=Path)
    parser.add_argument('--source', type=Path, help='固定发布提交所在源码仓库，用于同步项目自有备份工具')
    args = parser.parse_args(argv)
    deployment = Deployment(args.root)
    deployment.release_source = args.source
    # status 只读进度和运行状态，发布进行中也必须可查询，不能竞争维护锁。
    if args.action == 'status':
        try:
            deployment.status()
        except (DeploymentError, OSError, subprocess.SubprocessError, json.JSONDecodeError) as error:
            print(f'错误：{error}', file=sys.stderr)
            return 1
        return 0
    deployment.acquire_lock()

    def interrupted(signum, _frame):
        raise DeploymentError(f'部署被信号 {signum} 中断。')

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    try:
        if args.action == 'deploy':
            if not args.image or not args.worker_image or not args.commit or not args.template:
                parser.error('deploy 需要 --image、--worker-image、--commit 和 --template。')
            deployment.activate(args.image, args.worker_image, args.commit, args.template)
        elif args.action == 'apply':
            deployment.apply()
        elif args.action == 'rollback':
            deployment.rollback()
        else:
            deployment.status()
    except (DeploymentError, OSError, subprocess.SubprocessError, json.JSONDecodeError) as error:
        deployment.mark_failed_if_running(error)
        print(f'错误：{error}', file=sys.stderr)
        return 1
    finally:
        if deployment.lock_stream is not None:
            deployment.lock_stream.close()
    if deployment.local_backup_release is not None:
        queued = local_backup_support().trigger_backup_local(args.root, deployment.local_backup_release)
        print(json.dumps({'event': 'local_backup_maintenance', **queued}))
    return 0


if __name__ == '__main__':
    sys.exit(main())
