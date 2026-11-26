"""Generated bundle identity and real raw-seal/archive/resume integration."""
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('paired_init', HERE / 'paired-deploy.py')
init = importlib.util.module_from_spec(spec)
spec.loader.exec_module(init)


class BundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        self.plan = {
            'operation': 'cfm-test-release',
            'base_commit': '1' * 40,
            'candidate_commit': '2' * 40,
            'branch': 'main',
            'schema_migration': False,
            'worker_change': 'image_only',
            'backup_policy_package': 'a' * 64,
            'compatible_contract': {
                'previous': {'state': 'b' * 64, 'browser': 'c' * 64},
                'candidate': {'state': 'd' * 64, 'browser': 'e' * 64},
            },
            'state_contract_inputs': ['internal/state.go'],
            'worker': {
                'service': 'grok-browser',
                'container': 'cfm-browser-worker',
                'architecture': 'arm64',
                'capacity': 1,
                'commit': '2' * 40,
                'python_cloakbrowser': '1.0.0',
                'python_playwright': '1.0.0',
                'node_cloakbrowser': '1.0.0',
                'node_playwright': '1.0.0',
                'node_minimum_major': 22,
            },
            'previous_worker': {
                'container': 'cfm-browser-worker',
                'commit': '1' * 40,
                'image': 'worker:test',
                'image_id': 'sha256:' + 'f' * 64,
            },
        }
        self.rolling = self.root / 'tools/rolling-deploy.py'
        self.rolling.parent.mkdir()
        self.rolling.write_bytes(b'# installed rolling helper\n')
        self.plan['rolling_sha256'] = hashlib.sha256(self.rolling.read_bytes()).hexdigest()
        self.plan_path = self.root / 'plan.json'
        self.plan_path.write_text(json.dumps(self.plan))
        self.bundle = self.root / self.plan['operation']

    def tearDown(self):
        self.temp.cleanup()

    def run_bundle(self, body):
        prefix = f"import sys;sys.path.insert(0,{str(self.bundle)!r})\n"
        result = subprocess.run([sys.executable, '-c', prefix + body], cwd=self.root,
                                text=True, capture_output=True, timeout=40)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_bundle_identity_and_no_overwrite_or_helper_substitution(self):
        init.initialize(self.plan_path, self.bundle)
        self.run_bundle('import common;common.helper_hashes(common.Path(__file__) if False else common.Path(common.__file__).parent)')
        with self.assertRaises(ValueError):
            init.initialize(self.plan_path, self.bundle)
        (self.bundle / 'snapshot.py').write_text('# changed\n')
        self.run_bundle("import common\ntry: common.helper_hashes(common.Path(common.__file__).parent)\nexcept RuntimeError: pass\nelse: raise AssertionError('modified helper accepted')")

    def test_invalid_plan_and_mismatched_operation_fail_without_bundle(self):
        self.plan['schema_migration'] = True
        self.plan_path.write_text(json.dumps(self.plan))
        with self.assertRaises(ValueError):
            init.initialize(self.plan_path, self.bundle)
        self.assertFalse(self.bundle.exists())
        self.plan['schema_migration'] = False
        self.plan_path.write_text(json.dumps(self.plan))
        with self.assertRaises(ValueError):
            init.initialize(self.plan_path, self.root / 'wrong')

    def test_same_app_commit_worker_catchup_requires_worker_to_advance(self):
        catchup = json.loads(json.dumps(self.plan))
        catchup['base_commit'] = catchup['candidate_commit']
        catchup['previous_worker']['commit'] = '0' * 40
        self.plan_path.write_text(json.dumps(catchup))
        init.initialize(self.plan_path, self.bundle)
        self.run_bundle("""
import common
from types import SimpleNamespace
from unittest.mock import patch
with patch.object(common.os, 'geteuid', return_value=0), \\
     patch.object(common.os, 'uname', return_value=SimpleNamespace(sysname='Linux', machine='aarch64')):
    common.host_required()
""")
        same_both = dict(catchup)
        same_both['operation'] = 'cfm-test-same-both'
        same_both['previous_worker'] = dict(catchup['previous_worker'], commit=catchup['candidate_commit'])
        same_path = self.root / 'same.json'
        same_path.write_text(json.dumps(same_both))
        with self.assertRaisesRegex(ValueError, 'Candidate must differ from base or previous Worker'):
            init.initialize(same_path, self.root / same_both['operation'])

    def test_host_init_from_stdin_is_atomic_and_rejects_a_stale_pin(self):
        command = [sys.executable, '-B', str(HERE / 'paired-deploy.py'), 'init', '--plan', '-',
                   '--root', str(self.root), '--output', str(self.bundle)]
        for body, message in ((self.plan_path.read_text()[:-1], 'JSON'),
                              (json.dumps(dict(self.plan, rolling_sha256='0' * 64)), 'SHA-256 differs')):
            with self.subTest(message=message):
                result = subprocess.run(command, input=body, text=True, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertFalse(self.bundle.exists())
        result = subprocess.run(command, input=self.plan_path.read_text(), text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(json.loads(result.stdout)['deployment_started'])
        manifest = json.loads((self.bundle / 'bundle-manifest.json').read_text())
        for name, digest in manifest['files'].items():
            self.assertEqual(hashlib.sha256((self.bundle / name).read_bytes()).hexdigest(), digest)

    def test_existing_bundle_is_verified_before_starting_any_phase(self):
        init.initialize(self.plan_path, self.bundle)
        for filename in ('snapshot.py', 'preflight.py'):
            path = self.bundle / filename
            original = path.read_bytes()
            for replacement in (None, b'# truncated'):
                with self.subTest(filename=filename, replacement=replacement):
                    if replacement is None:
                        path.unlink()
                    else:
                        path.write_bytes(replacement)
                    with patch.object(init.subprocess, 'run') as execute:
                        with self.assertRaisesRegex(RuntimeError, filename):
                            init.run_release(self.bundle, self.root, self.root)
                        execute.assert_not_called()
                    path.write_bytes(original)
        self.rolling.write_bytes(b'# changed after init\n')
        with patch.object(init.subprocess, 'run') as execute:
            with self.assertRaisesRegex(RuntimeError, 'Rolling helper SHA-256 differs'):
                init.run_release(self.bundle, self.root, self.root)
            execute.assert_not_called()

    def test_direct_build_reports_missing_file_before_loading_rolling_helper(self):
        init.initialize(self.plan_path, self.bundle)
        (self.bundle / 'snapshot.py').unlink()
        self.run_bundle("""
import build, contextlib, io, json
from unittest.mock import patch
with patch.object(build.common, 'host_required'), patch.object(build.common, 'rolling') as rolling:
    sys.argv = ['build.py', '--source', '.', '--root', '.']
    with contextlib.redirect_stdout(io.StringIO()) as output:
        try:
            build.common.safe_main(build.main)
        except SystemExit as error:
            assert error.code == 1
        else:
            raise AssertionError('incomplete bundle accepted')
    assert 'snapshot.py' in json.loads(output.getvalue())['message']
    rolling.assert_not_called()
""")

    def test_pipeline_deadline_is_persisted_across_commands(self):
        init.initialize(self.plan_path, self.bundle)
        event = json.loads((self.bundle / 'pipeline-events.jsonl').read_text().splitlines()[0])
        self.assertEqual(event['phase'], 'operation-created')
        self.assertEqual(event['budget_seconds'], 1200)
        self.run_bundle('import common; assert common.pipeline_remaining(common.Path(common.__file__).parent) > 1190')
        event['epoch'] = time.time() - 1201
        (self.bundle / 'pipeline-events.jsonl').write_text(json.dumps(event) + '\n')
        self.run_bundle('import common\ntry: common.require_pipeline_budget(common.Path(common.__file__).parent)\nexcept RuntimeError: pass\nelse: raise AssertionError("expired pipeline accepted")')

    def test_continuous_run_skips_redundant_plan_and_keeps_preparation_in_one_chain(self):
        init.initialize(self.plan_path, self.bundle)
        source = self.root / 'source'
        source.mkdir()
        with patch.object(init.subprocess, 'run',
                          return_value=subprocess.CompletedProcess([], 0)) as execute:
            init.run_release(self.bundle, self.root, source)
        commands = [call.args[0] for call in execute.call_args_list]
        names = [Path(command[2]).name for command in commands]
        self.assertEqual(names, ['build.py', 'worker-release.py', 'preflight.py', 'coordinator.py'])
        self.assertTrue(all(call.kwargs.get('timeout', 0) > 0 for call in execute.call_args_list))
        self.assertTrue(all(call.kwargs['timeout'] <= 1200 for call in execute.call_args_list))
        self.assertNotIn('plan', [str(item) for command in commands for item in command])
        self.assertIn('prepare', commands[1])
        self.assertIn('apply-and-verify', commands[-1])

    def test_continuous_run_stops_before_starting_a_phase_after_budget_expiry(self):
        init.initialize(self.plan_path, self.bundle)
        event = json.loads((self.bundle / 'pipeline-events.jsonl').read_text().splitlines()[0])
        event['epoch'] = time.time() - 1201
        (self.bundle / 'pipeline-events.jsonl').write_text(json.dumps(event) + '\n')
        source = self.root / 'source'
        source.mkdir()
        with patch.object(init.subprocess, 'run') as execute:
            with self.assertRaisesRegex(ValueError, 'budget is exhausted'):
                init.run_release(self.bundle, self.root, source)
        execute.assert_not_called()

    def test_run_can_initialize_once_and_refuses_existing_output(self):
        source = self.root / 'source'
        source.mkdir()
        with patch.object(init.subprocess, 'run',
                          return_value=subprocess.CompletedProcess([], 0)) as execute:
            with patch.object(init.sys, 'argv', ['paired-deploy.py', 'run',
                                                 '--plan', str(self.plan_path),
                                                 '--output', str(self.bundle),
                                                 '--root', str(self.root),
                                                 '--source', str(source)]):
                init.main()
        self.assertTrue((self.bundle / 'pipeline-events.jsonl').is_file())
        self.assertEqual(len(execute.call_args_list), 4)
        with self.assertRaises(ValueError):
            with patch.object(init.sys, 'argv', ['paired-deploy.py', 'run',
                                                 '--plan', str(self.plan_path),
                                                 '--output', str(self.bundle),
                                                 '--root', str(self.root),
                                                 '--source', str(source)]):
                init.main()

    def test_run_existing_operation_rejects_plan_and_output(self):
        init.initialize(self.plan_path, self.bundle)
        source = self.root / 'source'
        source.mkdir()
        with self.assertRaises(ValueError):
            with patch.object(init.sys, 'argv', ['paired-deploy.py', 'run',
                                                 '--operation-dir', str(self.bundle),
                                                 '--plan', str(self.plan_path),
                                                 '--output', str(self.bundle),
                                                 '--root', str(self.root),
                                                 '--source', str(source)]):
                init.main()

    def test_archive_finalization_budget_is_persisted_and_cannot_restart(self):
        init.initialize(self.plan_path, self.bundle)
        self.run_bundle('import common; common.ensure_finalization_budget(common.Path(common.__file__).parent)')
        events = (self.bundle / 'pipeline-events.jsonl').read_text().splitlines()
        self.assertEqual(sum(json.loads(line).get('phase') == 'archive-finalization' for line in events), 1)
        self.run_bundle('import common; common.ensure_finalization_budget(common.Path(common.__file__).parent)')
        events = (self.bundle / 'pipeline-events.jsonl').read_text().splitlines()
        self.assertEqual(sum(json.loads(line).get('phase') == 'archive-finalization' for line in events), 1)
        records = [json.loads(line) for line in events]
        for record in records:
            if record.get('phase') == 'archive-finalization':
                record['epoch'] = time.time() - 1201
        (self.bundle / 'pipeline-events.jsonl').write_text(
            ''.join(json.dumps(record, separators=(',', ':')) + '\n' for record in records))
        self.run_bundle('import common\ntry: common.require_finalization_budget(common.Path(common.__file__).parent)\nexcept RuntimeError: pass\nelse: raise AssertionError("expired finalization accepted")')

    def test_real_copy_seal_archive_resume_and_tampering(self):
        init.initialize(self.plan_path, self.bundle)
        self.run_bundle(f'''
import importlib.util, os
from pathlib import Path
from types import SimpleNamespace
import snapshot, common
s=importlib.util.spec_from_file_location('legacy', {str(HERE / 'rolling-deploy.py')!r})
r=importlib.util.module_from_spec(s);s.loader.exec_module(r)
root=Path('source').resolve();root.mkdir(mode=0o700)
(root/'data').mkdir();(root/'logs').mkdir();(root/'data/empty').mkdir()
(root/'data/file').write_bytes(b'fixture' * 10000)
(root/'data/file').chmod(0o640)
work=Path('work').resolve();work.mkdir(mode=0o700)
host=SimpleNamespace(checkpoint=lambda:None)
b=snapshot.make_legacy(r,root,work,host);b.sync(frozen=True)
for name in snapshot.DATABASE_FILES:(work/name).write_bytes(b'database fixture')
assert b.business_files()==r.business_files(root,host)
b.seal();assert not (work/'complete').exists()
assert common.load(work/'raw-seal.json')['raw_durable']
# An interrupted archive is evidence, and does not block a fresh archive attempt.
(work/'snapshot-interrupted.partial.tar.gz').write_bytes(b'incomplete')
b=snapshot.resume(r,root,work,host);b.finish();b.finish()
assert (work/'complete').exists()
assert (work/'snapshot-interrupted.partial.tar.gz').read_bytes()==b'incomplete'
restored=Path('restored').resolve()
snapshot.restore_archive(work/'snapshot.tar.gz',b.frozen_layout,restored)
assert snapshot.layout(restored)==b.frozen_layout
(work/'postgres.dump').write_bytes(b'tampered')
try:b.finish()
except RuntimeError:pass
else:raise AssertionError('DB tamper accepted')
''')

    def test_source_change_before_seal_never_creates_raw_receipt(self):
        init.initialize(self.plan_path, self.bundle)
        self.run_bundle(f'''
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import snapshot
s=importlib.util.spec_from_file_location('legacy', {str(HERE / 'rolling-deploy.py')!r})
r=importlib.util.module_from_spec(s);s.loader.exec_module(r)
root=Path('source').resolve();root.mkdir(mode=0o700)
(root/'data').mkdir();(root/'logs').mkdir();(root/'data/file').write_bytes(b'before')
work=Path('work').resolve();work.mkdir(mode=0o700)
b=snapshot.make_legacy(r,root,work,SimpleNamespace(checkpoint=lambda:None));b.sync(frozen=True)
for name in snapshot.DATABASE_FILES:(work/name).write_bytes(b'database fixture')
(root/'data/file').write_bytes(b'after!')
try:b.seal()
except RuntimeError:pass
else:raise AssertionError('source change accepted')
assert not (work/'raw-seal.json').exists()
''')

    def snapshot_case(self, body):
        """每个故障场景使用真正生成的 bundle、旧复制原语及独立私有目录。"""
        init.initialize(self.plan_path, self.bundle)
        self.run_bundle(f'''
import importlib.util, os
from pathlib import Path
from types import SimpleNamespace
from unittest import mock
import snapshot, common
s=importlib.util.spec_from_file_location('legacy', {str(HERE / 'rolling-deploy.py')!r})
r=importlib.util.module_from_spec(s);s.loader.exec_module(r)
root=Path('source').resolve();root.mkdir(mode=0o700)
(root/'data').mkdir();(root/'logs').mkdir();(root/'data/empty').mkdir()
(root/'data/file').write_bytes(b'fixture' * 1000)
work=Path('work').resolve();work.mkdir(mode=0o700)
host=SimpleNamespace(checkpoint=lambda:None)
b=snapshot.make_legacy(r,root,work,host);b.sync(frozen=True)
for name in snapshot.DATABASE_FILES:(work/name).write_bytes(b'database fixture')
def change_database():
    path=work/'postgres.dump';before=path.stat()
    path.write_bytes(b'X' * before.st_size)
    os.utime(path,ns=(before.st_atime_ns,before.st_mtime_ns))
def rejected(action, message):
    try:action()
    except (RuntimeError,OSError,EOFError):pass
    else:raise AssertionError(message)
{body}
''')

    def test_seal_rejects_database_symlink(self):
        self.snapshot_case('''
outside=Path('outside').resolve();outside.write_bytes(b'original')
(work/'postgres.dump').unlink();(work/'postgres.dump').symlink_to(outside)
rejected(b.seal,'linked database accepted')
assert not (work/'raw-seal.json').exists()
assert outside.read_bytes()==b'original'
''')

    def test_seal_rechecks_database_after_collecting_all_proofs(self):
        self.snapshot_case('''
original=snapshot.file_proof
def inject(path,**kwargs):
    proof=original(path,**kwargs)
    if Path(path).name=='snapshot-metadata.json':change_database()
    return proof
with mock.patch.object(snapshot,'file_proof',side_effect=inject):
    rejected(b.seal,'database changed after its proof but raw seal was accepted')
assert not (work/'raw-seal.json').exists()
''')

    def test_parent_directory_fsync_failure_does_not_leave_durable_receipt(self):
        self.snapshot_case('''
original=snapshot.sync_directory
def inject(path):
    if Path(path)==work.parent:raise OSError('injected parent fsync failure')
    return original(path)
with mock.patch.object(snapshot,'sync_directory',side_effect=inject):
    rejected(b.seal,'parent fsync failure ignored')
assert not (work/'raw-seal.json').exists(),'failure left a successful durable receipt'
''')

    def test_database_change_during_archive_never_creates_complete(self):
        self.snapshot_case('''
b.seal()
original=snapshot.integrity.write_archive
def inject(*args,**kwargs):
    report=original(*args,**kwargs);change_database();return report
with mock.patch.object(snapshot.integrity,'write_archive',side_effect=inject):
    rejected(b.finish,'database changed during archiving but complete was accepted')
assert not (work/'complete').exists()
''')

    def test_database_change_during_existing_archive_verification_is_rejected(self):
        self.snapshot_case('''
b.seal()
snapshot.integrity.write_archive(work/'snapshot',work/'snapshot.tar.gz',b.frozen_layout)
original=snapshot.integrity.verify_archive
def inject(*args,**kwargs):
    report=original(*args,**kwargs);change_database();return report
with mock.patch.object(snapshot.integrity,'verify_archive',side_effect=inject):
    rejected(b.finish,'database changed during archive verification but complete was accepted')
assert not (work/'complete').exists()
''')

    def test_linked_raw_receipt_is_rejected(self):
        self.snapshot_case('''
b.seal()
outside=Path('outside-seal.json').resolve()
(work/'raw-seal.json').rename(outside);(work/'raw-seal.json').symlink_to(outside)
rejected(b.finish,'linked raw receipt accepted')
assert not (work/'complete').exists()
''')

    def test_raw_receipt_format_metadata_flag_and_manifest_digest_are_enforced(self):
        self.snapshot_case('''
b.seal();original=common.load(work/'raw-seal.json')
for key,value in (('format',999),('metadata_verified',False),('manifest_sha256','0'*64)):
    changed=dict(original);changed[key]=value
    r.atomic_json(work/'raw-seal.json',changed)
    rejected(b.finish,'invalid raw receipt field accepted: '+key)
    assert not (work/'complete').exists()
''')

    def test_corrupt_existing_archive_does_not_get_overwritten_or_completed(self):
        self.snapshot_case('''
b.seal();archive=work/'snapshot.tar.gz';archive.write_bytes(b'incomplete')
rejected(b.finish,'corrupt existing archive accepted')
assert archive.read_bytes()==b'incomplete'
assert not (work/'complete').exists()
''')

    def test_write_archive_failure_preserves_partial_without_complete_and_can_resume(self):
        self.snapshot_case('''
b.seal();partials=[]
def inject(source,path,*args,**kwargs):
    path.write_bytes(b'interrupted');partials.append(path)
    raise OSError('injected archive interruption')
with mock.patch.object(snapshot.integrity,'write_archive',side_effect=inject):
    rejected(b.finish,'archive interruption ignored')
assert len(partials)==1 and partials[0].read_bytes()==b'interrupted'
assert not (work/'snapshot.tar.gz').exists() and not (work/'complete').exists()
resumed=snapshot.resume(r,root,work,host);resumed.finish()
assert (work/'complete').exists() and partials[0].read_bytes()==b'interrupted'
''')

    def test_resume_refuses_changed_raw_content_with_restored_size_and_mtime(self):
        self.snapshot_case('''
b.seal();path=work/'snapshot/data/file';before=path.stat()
path.write_bytes(b'X' * before.st_size);os.utime(path,ns=(before.st_atime_ns,before.st_mtime_ns))
rejected(lambda:snapshot.resume(r,root,work,host),'changed raw snapshot accepted by resume')
assert not (work/'complete').exists()
''')

    def test_database_mutation_during_proof_read_is_rejected(self):
        self.snapshot_case('''
original=os.read;mutated=False;inode=(work/'postgres.dump').stat().st_ino
def inject(fd,size):
    global mutated
    content=original(fd,size)
    if not mutated and os.fstat(fd).st_ino==inode:
        mutated=True;change_database()
    return content
with mock.patch.object(snapshot.os,'read',side_effect=inject):
    rejected(b.seal,'database changed while hashing but seal was accepted')
assert not (work/'raw-seal.json').exists()
''')


if __name__ == '__main__':
    unittest.main()
