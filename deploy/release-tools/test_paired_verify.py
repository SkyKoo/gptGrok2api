"""Offline paired-release verification, using synthetic assets and loopback HTTP."""
from contextlib import contextmanager
import functools
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
import json
from pathlib import Path
import shutil
import socket
import sys
import tarfile
import tempfile
import threading
import time
import types
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parent


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def load_verifier():
    # A real operation bundle supplies this plan; no production configuration is read.
    with tempfile.TemporaryDirectory() as directory:
        directory = Path(directory)
        for name in ('common.py', 'verify.py'):
            shutil.copy2(ROOT / 'paired' / name, directory / name)
        (directory / 'release-plan.json').write_text(json.dumps({
            'candidate_commit': 'a' * 40, 'base_commit': 'b' * 40,
            'branch': 'main', 'rolling_sha256': 'c' * 64}))
        common = load_module('paired_verify_common_test', directory / 'common.py')
        phases = load_module('paired_verify_phases_test', ROOT / 'paired_phases.py')
        with patch.dict(sys.modules, {'common': common, 'paired_phases': phases,
                                      'snapshot': types.ModuleType('snapshot')}):
            return load_module('paired_verify_test', directory / 'verify.py')


verify = load_verifier()
INDEX = b'<script type="module" src="/assets/main.js"></script>'
FILES = {'index.html': INDEX,
         'assets/main.js': b'import "./chunk.js"; import "./style.css";',
         'assets/chunk.js': b'import "./main.js";',
         'assets/style.css': b'body { color: black; }',
         'assets/stale.js': b'never requested'}


def archive_bytes(files=FILES, extra=()):
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode='w') as archive:
        for name, data in files.items():
            member = tarfile.TarInfo(name)
            member.size = len(data)
            archive.addfile(member, io.BytesIO(data))
        for member, data in extra:
            archive.addfile(member, io.BytesIO(data))
    return out.getvalue()


class AssetHost:
    def __init__(self, data):
        self.data, self.calls = data, []

    def run(self, command, *, output, timeout):
        self.calls.append((command, timeout))
        output.write(self.data)


@contextmanager
def http_fixture(callback, *, concurrent=False):
    observed = {'paths': [], 'active': 0, 'peak': 0, 'headers': []}
    lock, gate = threading.Lock(), threading.Event()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            with lock:
                observed['paths'].append(self.path)
                observed['headers'].append(dict(self.headers))
                observed['active'] += 1
                observed['peak'] = max(observed['peak'], observed['active'])
                if observed['active'] == 4:
                    gate.set()
            try:
                if concurrent:
                    gate.wait(1)
                    time.sleep(0.01)
                status, body, headers = callback(self.path)
                self.send_response(status)
                self.send_header('Content-Length', str(len(body)))
                for name, value in headers.items():
                    self.send_header(name, value)
                self.end_headers()
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                with lock:
                    observed['active'] -= 1

    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': 0.01}, daemon=True)
    thread.start()
    try:
        yield 'http://127.0.0.1:' + str(server.server_port), observed
    finally:
        gate.set()
        server.shutdown()
        server.server_close()
        thread.join(2)


class AssetArchiveTests(unittest.TestCase):
    def test_reads_all_assets_with_one_exec_and_no_extraction(self):
        host = AssetHost(archive_bytes())
        self.assertEqual(verify.image_web_files(host, 'candidate'), FILES)
        self.assertEqual(len(host.calls), 1)
        command, timeout = host.calls[0]
        self.assertEqual(command[:5], ['docker', 'exec', 'candidate', 'sh', '-c'])
        self.assertIn('assets/*.js assets/*.css', command[5])
        self.assertIn('exec tar -cf - "$@"', command[5])
        self.assertEqual(timeout, 30)

    def test_rejects_unsafe_duplicate_and_non_file_members(self):
        for name, kind in (('../private.js', tarfile.REGTYPE),
                           ('assets/../private.js', tarfile.REGTYPE),
                           ('/assets/absolute.js', tarfile.REGTYPE),
                           ('assets/main.js', tarfile.REGTYPE),
                           ('assets/linked.js', tarfile.SYMTYPE),
                           ('assets/hard.js', tarfile.LNKTYPE),
                           ('assets/device.js', tarfile.CHRTYPE),
                           ('assets/nested.js', tarfile.DIRTYPE),
                           ('data/accounts.json', tarfile.REGTYPE)):
            with self.subTest(name=name, kind=kind):
                member = tarfile.TarInfo(name)
                member.type = kind
                member.linkname = 'index.html' if kind in (tarfile.SYMTYPE, tarfile.LNKTYPE) else ''
                with self.assertRaisesRegex(RuntimeError, 'Unexpected, duplicate or linked'):
                    verify.image_web_files(AssetHost(archive_bytes(extra=[(member, b'')])), 'candidate')

    def test_rejects_missing_index(self):
        with self.assertRaisesRegex(RuntimeError, 'index is missing'):
            verify.image_web_files(AssetHost(archive_bytes({'assets/main.js': b''})), 'candidate')

    def test_rejects_oversized_member_before_reading_it(self):
        member = tarfile.TarInfo('index.html')
        member.size = 16 * 1024 * 1024 + 1
        with self.assertRaisesRegex(RuntimeError, 'verification limits'):
            verify.image_web_files(AssetHost(member.tobuf() + b'\0' * 1024), 'candidate')

    def test_rejects_truncated_archive(self):
        member = tarfile.TarInfo('index.html')
        member.size = 1000
        with self.assertRaises((RuntimeError, tarfile.TarError)):
            verify.image_web_files(AssetHost(member.tobuf() + b'x'), 'candidate')


class PublishedAssetsTests(unittest.TestCase):
    def test_parallel_http_verifies_complete_graph_for_both_origins(self):
        def response(path):
            key = path.removeprefix('/public').lstrip('/')
            return 200, FILES[key], {}

        with http_fixture(response, concurrent=True) as (base, observed):
            report = verify.verify_published_assets(INDEX, FILES, [base, base + '/public'])
        self.assertEqual(observed['peak'], 4)
        expected = {'/' + name for name in FILES if name not in ('index.html', 'assets/stale.js')}
        self.assertEqual(set(observed['paths']), expected | {'/public' + name for name in expected})
        self.assertEqual(len(observed['paths']), 6)
        self.assertEqual(report, [{'name': Path(name).name, 'sha256': hashlib.sha256(FILES[name]).hexdigest()}
                                 for name in sorted(n for n in FILES if '/' + n in expected)])
        self.assertTrue(all(headers.get('User-Agent') == 'curl/8.5.0'
                            and headers.get('Accept-Encoding') == 'identity'
                            and 'Authorization' not in headers for headers in observed['headers']))

    def test_rejects_wrong_bytes_and_non_200_without_retry(self):
        files = {'index.html': INDEX, 'assets/main.js': b'expected'}
        for status, data in ((200, b'different'), (404, b'expected')):
            with self.subTest(status=status), http_fixture(lambda path: (status, data, {})) as (base, observed):
                with self.assertRaisesRegex(RuntimeError, 'differs from image'):
                    verify.verify_published_assets(INDEX, files, [base])
                self.assertEqual(observed['paths'], ['/assets/main.js'])

    def test_redirect_is_rejected_without_requesting_target(self):
        with http_fixture(lambda path: (302, b'', {'Location': '/target'})) as (base, observed):
            with self.assertRaisesRegex(RuntimeError, 'differs from image'):
                verify.verify_published_assets(INDEX, {'assets/main.js': b'expected'}, [base])
            self.assertEqual(observed['paths'], ['/assets/main.js'])

    def test_http_timeout_fails_without_retry(self):
        def response(path):
            time.sleep(0.1)
            return 200, b'expected', {}

        with http_fixture(response) as (base, observed):
            with self.assertRaises((TimeoutError, socket.timeout)):
                verify.verify_published_assets(INDEX, {'assets/main.js': b'expected'}, [base],
                                               fetch=functools.partial(verify.get, timeout=0.02))
            self.assertEqual(observed['paths'], ['/assets/main.js'])

    def test_missing_dependency_and_unsafe_index_fail_before_http(self):
        for index, files in ((INDEX, {'assets/main.js': b'import "./missing.js";'}),
                             (b'<script src="/assets/../private.js"></script>', FILES),
                             (b'<html></html>', FILES)):
            with self.subTest(index=index), patch.object(verify, 'get') as fetch:
                with self.assertRaises(RuntimeError):
                    verify.verify_published_assets(index, files, ['http://example.invalid'])
                fetch.assert_not_called()


def container_info(name, health='healthy'):
    return {'Id': name + '-id', 'Image': name + '-image', 'RestartCount': 0,
            'State': {'Running': True, 'OOMKilled': False, 'Health': {'Status': health}},
            'Config': {'Env': []}, 'Mounts': [], 'NetworkSettings': {'Networks': {}},
            'HostConfig': {'PortBindings': {}}}


class HealthHost:
    def __init__(self, frames):
        self.frames = frames
        self.calls = []
        self.database_frames = {name: container_info(name) for name in verify.common.DEPENDENCIES.values()}

    def run(self, command, *, timeout):
        self.calls.append((command, timeout))
        frame = self.frames.pop(0) if len(self.frames) > 1 else self.frames[0]
        return json.dumps([frame]).encode()

    def inspect(self, name):
        self.calls.append(('database', name))
        return self.database_frames[name]


class GatewayHealthWiringTests(unittest.TestCase):
    def setUp(self):
        self.current = {'frontdoor': 'gateway'}
        self.baseline = {'containers': {name: verify.common.container_summary(container_info(name))
                                       for name in ['gateway', *verify.common.DEPENDENCIES.values()]}}
        now = [0.0]
        def sleep(seconds):
            now[0] += seconds
        original = verify.paired_phases.wait_stable_health
        patcher = patch.object(verify.paired_phases, 'wait_stable_health',
            side_effect=lambda inspect, expected, summarize: original(inspect, expected, summarize,
                timeout=0.3, interval=0.1, monotonic=lambda: now[0], sleep=sleep))
        self.wait = patcher.start()
        self.addCleanup(patcher.stop)

    def test_gateway_waits_for_health_and_databases_stay_strict(self):
        host = HealthHost([container_info('gateway', 'unhealthy'), container_info('gateway')])
        verify.verify_stable_containers(host, self.current, self.baseline)
        self.assertEqual(host.calls[:2], [(['docker', 'inspect', 'gateway'], 5)] * 2)
        self.assertEqual(host.calls[2:], [('database', name) for name in verify.common.DEPENDENCIES.values()])
        self.assertIs(self.wait.call_args.args[1], self.baseline['containers']['gateway'])
        self.assertIs(self.wait.call_args.args[2], verify.common.container_summary)

    def test_identity_change_fails_during_wait(self):
        changed = container_info('replacement')
        host = HealthHost([container_info('gateway', 'unhealthy'), changed])
        with self.assertRaisesRegex(RuntimeError, 'identity or configuration changed'):
            verify.verify_stable_containers(host, self.current, self.baseline)
        self.assertEqual(len(host.calls), 2)

    def test_gateway_wait_has_a_deadline(self):
        host = HealthHost([container_info('gateway', 'unhealthy')])
        with self.assertRaisesRegex(RuntimeError, 'did not recover'):
            verify.verify_stable_containers(host, self.current, self.baseline)
        self.assertEqual(len(host.calls), 4)

    def test_database_health_is_not_relaxed(self):
        host = HealthHost([container_info('gateway')])
        first = next(iter(host.database_frames))
        host.database_frames[first]['State']['Health']['Status'] = 'unhealthy'
        with self.assertRaisesRegex(RuntimeError, 'Stable database container changed'):
            verify.verify_stable_containers(host, self.current, self.baseline)
        self.assertEqual(host.calls, [(['docker', 'inspect', 'gateway'], 5), ('database', first)])


class AdobeRegistrationContractTests(unittest.TestCase):
    def row(self, **changes):
        value = dict(status='failed', creation_outcome='accepted', recovery_mode='manual_reregistration',
                     can_retry=False, can_retry_registration=False, can_resume_registration=False,
                     can_reregister=True, reregister_not_before='', retry_job_id='', imported=False,
                     verified=False, auto_retry=None)
        value.update(changes)
        return value

    def inspect(self, *rows):
        payload = dict(running=False, jobs=list(rows), jobs_total=len(rows), jobs_page=1)
        with patch.object(verify, 'get', return_value=(200, json.dumps(payload).encode())) as fetch:
            result = verify.verify_adobe_registration_get('synthetic-key')
        fetch.assert_called_once_with('http://127.0.0.1:8000/api/register/adobe/jobs?page=1', 'synthetic-key')
        return result

    def test_manual_new_session_and_protected_superseded_tasks(self):
        result = self.inspect(self.row(), self.row(can_reregister=False, retry_job_id='new-child'),
                              self.row(can_reregister=False))
        self.assertEqual(result['sampled_recovery_modes']['manual_reregistration'], 3)
        self.assertEqual(result['sampled_manual_registration_available'], 1)

    def test_media_continuation_and_saved_result_remain_supported(self):
        result = self.inspect(self.row(recovery_mode='resume_session', can_reregister=False,
                                       can_resume_registration=True),
                              self.row(recovery_mode='saved_result', can_reregister=False,
                                       imported=True, can_retry=True))
        self.assertEqual(result['sampled_recovery_modes']['resume_session'], 1)
        self.assertEqual(result['sampled_recovery_modes']['saved_result'], 1)

    def test_manual_mode_never_automatically_retries_or_resumes_old_signup(self):
        for key in ('can_retry', 'can_retry_registration', 'can_resume_registration'):
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                self.inspect(self.row(**{key: True}))

    def test_saved_active_completed_and_superseded_accounts_cannot_reregister(self):
        for changes in ({'imported': True}, {'verified': True}, {'retry_job_id': 'child'},
                        {'status': 'running'}, {'status': 'queued'}, {'status': 'completed'},
                        {'recovery_mode': 'saved_result'}, {'recovery_mode': 'resume_session'}):
            with self.subTest(changes=changes), self.assertRaises(RuntimeError):
                self.inspect(self.row(**changes))

    def test_manual_mode_requires_accepted_unsaved_identity_even_when_button_disabled(self):
        for changes in ({'creation_outcome': 'unknown'}, {'imported': True}, {'verified': True}):
            with self.subTest(changes=changes), self.assertRaises(RuntimeError):
                self.inspect(self.row(can_reregister=False, **changes))


if __name__ == '__main__':
    unittest.main()
