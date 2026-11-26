"""Release fault injection without Docker or production access."""
import importlib.util
import gzip
import json
import io
import os
import tarfile
import shutil
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from deploy import worker_container_name, DeploymentError

spec=importlib.util.spec_from_file_location('rolling_deploy',Path(__file__).with_name('rolling-deploy.py'))
r=importlib.util.module_from_spec(spec); spec.loader.exec_module(r)


def state(phase='active',paused=False):
    return dict(protocol=r.PROTOCOL,phase=phase,paused=paused,writer_lease=phase=='active',active_requests=0,jobs={'background':0})


def info(name,image='old:image'):
    return {'Id':name+'-id','Image':image+'-id','State':{'Running':True,'Health':{'Status':'healthy'}},
        'Config':{'Image':image,'User':'10001:10001','Env':['GO_LISTEN_ADDR=:8080','GROK_DATA_DIR=/app/data'],
            'Labels':{'io.cfm.rolling.instance':'owned','io.cfm.rolling.slot':'blue'}},
        'Mounts':[{'Destination':'/app/'+n,'Source':'/fixture/'+n,'Type':'bind','RW':True} for n in ('data','logs')],
        'NetworkSettings':{'Networks':{'gptgrok2api_default':{},'new-api-cfm':{},'new-api-cpa':{}}}}


class FakeHost(r.Host):
    def __init__(self,bootstrap=False,fault=''):
        super().__init__(); self.bootstrap=bootstrap; self.fault=fault; self.events=[]; self.failed=False
        self.front='gptgrok2api-app-1'; self.old=self.front if bootstrap else r.SLOTS['blue'][0]
        self.states={self.old:state()}; self.infos={self.old:info(self.old)}
        self.route={'upstream':r.SLOTS['blue'][1],'held':False,'epoch':0,'protocol':r.PROTOCOL}
    def inspect(self,name): return self.infos.get(name,info(name))
    def image(self,name):
        return {'Id':name+'-id','Os':'linux','Architecture':'arm64','Config':{'Labels':{'org.opencontainers.image.revision':'a'*40,'io.cfm.rolling.protocol':r.PROTOCOL}}}
    def run(self,*args,**kwargs): self.checkpoint(); return b'PONG' if 'PING' in args[0] else b''
    def control(self,name,path='/status',body=None):
        if path=='/gateway/init': self.route['held']=True; return {'route':self.route.copy()}
        return self.states[name].copy()
    def action(self,name,action):
        self.events.append((name,action))
        if action==self.fault and not self.failed:
            self.failed=True
            if action=='activate': self.states[name]=state('failed'); self.states[name]['writer_lease']=True
            raise r.ReleaseError('injected failure')
        s=self.states[name]
        if action in ('pause','resume'): s['paused']=action=='pause'
        if action=='activate': self.states[name]=state(paused=True)
        if action=='freeze': s.update(phase='gateway' if name==self.front else 'frozen',writer_lease=False)
        return self.states[name].copy()
    def gateway(self,front,action=None,target=''):
        if self.fault=='unknown_release' and self.failed and action is None: raise r.ReleaseError('unreachable ingress')
        if action:
            self.events.append(('gateway',action))
            if action=='hold': self.route['held']=True
            if action=='route': self.route['upstream']=target
            if action=='release':
                self.route['held']=False
                if self.fault in ('release_reply','unknown_release'):
                    self.failed=True; raise r.ReleaseError('lost release reply')
            self.route['epoch']+=1
        return {'route':self.route.copy(),'wait_budget_seconds':60}
    def stop_empty(self,name):
        self.events.append((name,'stop')); self.infos[name]['State']['Running']=False
        self.states[name]['writer_lease']=False


class FakeSnapshot:
    def __init__(self,root,work,host,direct=False): self.root,self.work=Path(root),Path(work); self.direct=direct
    def sync(self,**kwargs): shutil.copytree(self.root/'data',self.work/'snapshot'/'data',dirs_exist_ok=True)
    def database(self,*args): pass
    def finish(self): pass
    def business_files(self): return r.business_files(self.work/'snapshot', r.Host())


class FailedBackupTests(unittest.TestCase):
    def test_payload_ownership_and_recovery_proof(self):
        for case in ('rolling', 'paired', 'pending', 'attention', 'complete', 'legacy', 'bad_previous', 'referenced'):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                root = Path(directory).resolve()
                work = root / 'backups' / 'attempt'
                prior = work.parent / 'prior'
                work.mkdir(parents=True)
                prior.mkdir()
                payload = ('snapshot.tar.gz', 'postgres.dump', 'postgres-globals.sql', 'redis.rdb')
                for name in payload:
                    (work / name).write_bytes(b'new')
                    (prior / name).write_bytes(b'prior')
                r.atomic_json(prior / 'checksums.json', {f: r.file_digest(prior / f) for f in payload})
                r.atomic_json(prior / 'complete', {'protocol': r.PROTOCOL})
                r.atomic_json(prior / 'release.json', {'status': 'success'})
                meta = {'status': 'healthy', 'commit': 'old', 'backup': str(prior)}
                r.atomic_json(work / 'previous.json', meta)
                r.atomic_json(root / 'current.json', dict(meta, backup=str(work)) if case == 'referenced' else meta)
                state = {'phase': 'program_rolled_back', 'status': 'rolled_back', 'data_restored': False}
                if case == 'paired':
                    state = {'phase': 'old_program_recovered', 'status': 'rolled_back', 'data_restore_performed': False}
                elif case in ('pending', 'attention'):
                    state['status'] = 'archive_pending' if case == 'pending' else 'needs_attention'
                elif case == 'complete':
                    (work / 'complete').touch()
                elif case == 'legacy':
                    (work / 'snapshot').mkdir()
                elif case == 'bad_previous':
                    (prior / 'redis.rdb').write_bytes(b'changed')
                r.atomic_json(work / 'release.json', state)
                r.retire_rolled_back_payload(root, work)
                r.retire_rolled_back_payload(root, work)
                for name in payload:
                    self.assertEqual((work / name).exists(), case not in ('rolling', 'paired'))
                    self.assertTrue((prior / name).is_file())
                self.assertEqual(r.load(work / 'release.json'), state)
                if case in ('rolling', 'paired'):
                    self.assertEqual(r.load(work / 'rollback-cleanup.json')['removed'], list(payload))


class DependencyNamingTests(unittest.TestCase):
    def test_legacy_deploy_records_name_for_subsequent_bootstrap(self):
        for name in ('gptgrok2api-grok-browser-1', 'cfm-browser-worker'):
            metadata={'grok_browser_container': worker_container_name({'Name': '/'+name})}
            self.assertEqual(r.dependency_names(metadata)['grok-browser'],name)
        for name in (None,'/other-service','cfm-browser-worker'):
            with self.assertRaises(DeploymentError):
                worker_container_name({'Name': name})

    def test_existing_deployments_keep_legacy_worker_identity(self):
        self.assertEqual(r.dependency_names({})['grok-browser'], 'gptgrok2api-grok-browser-1')

    def test_renamed_worker_and_database_names(self):
        self.assertEqual(r.dependency_names({'grok_browser_container': 'cfm-browser-worker'}),
                         {'postgres': 'gptgrok2api-postgres-1', 'redis': 'gptgrok2api-redis-1',
                          'grok-browser': 'cfm-browser-worker'})

    def test_unknown_worker_names_are_rejected(self):
        for name in ('', 'other-service', None, 'cfm-browser-worker-2'):
            with self.subTest(name=name), self.assertRaises(r.ReleaseError):
                r.dependency_names({'grok_browser_container': name})


class DeadlineTests(unittest.TestCase):
    def test_overall_deadline_is_checked_alongside_phase_deadline(self):
        host = r.Host()
        host.overall_deadline = r.time.monotonic() - 1
        with self.assertRaisesRegex(r.ReleaseError, 'deadline exhausted'):
            host.checkpoint()

    def test_phase_deadline_still_wins_when_overall_budget_is_longer(self):
        host = r.Host()
        host.overall_deadline = r.time.monotonic() + 1200
        host.deadline = r.time.monotonic() - 1
        with self.assertRaisesRegex(r.ReleaseError, 'deadline exhausted'):
            host.checkpoint()


class Release(r.RollingRelease):
    def start_slot(self,old,image,slot,suffix):
        name=r.SLOTS[slot][0]; self.host.events.append((name,'create:'+image))
        self.host.states[name]=state('standby'); self.host.infos[name]=info(name,image)
        self.owned_candidate=name; self.owned_runtime='owned'; return name


class ReleaseFaults(unittest.TestCase):
    def setup_release(self,bootstrap=False,fault=''):
        temporary=tempfile.TemporaryDirectory(); self.addCleanup(temporary.cleanup)
        root=Path(temporary.name); (root/'data').mkdir(); (root/'logs').mkdir()
        contract={'state':'same','browser':'same'}
        old={'image':'old:image','commit':'a'*40,'source':'/source','contract':contract}
        if not bootstrap: old.update(release_strategy=r.STRATEGY,status='healthy',container=r.SLOTS['blue'][0],frontdoor='gptgrok2api-app-1',slot='blue',upstream=r.SLOTS['blue'][1])
        r.atomic_json(root/'current.json',old)
        host=FakeHost(bootstrap,fault)
        for mount in host.infos[host.old]['Mounts']: mount['Source']=str(root/Path(mount['Destination']).name)
        release=Release(root,host)
        candidate={'image':'old:image' if bootstrap else 'new:image','commit':'a'*40,'base_commit':'a'*40,'source':'/source','contract':contract}
        candidate['image_id']=candidate['image']+'-id'
        for name,value in [('Snapshot',FakeSnapshot),('merge_assets',lambda *a:None),('database_guard',lambda *a:{'stable':True})]:
            mock=patch.object(r,name,value); mock.start(); self.addCleanup(mock.stop)
        return root,host,release,candidate
    def test_normal_switch_retains_dependencies_and_stops_only_retired(self):
        root,host,app,release=self.setup_release(); app.deploy(release)
        self.assertEqual(r.load(root/'rolling-status.json')['status'],'success')
        self.assertEqual(r.load(root/'current.json')['slot'],'green')
        self.assertEqual(r.load(root/'current.json')['backup'], str(app.work))
        self.assertFalse(host.route['held'])
        self.assertEqual([n for n,a in host.events if a=='stop'],[r.SLOTS['blue'][0]])
        self.assertLess(host.events.index(('gateway','release')),host.events.index((r.SLOTS['green'][0],'resume')))
    def test_configured_backup_cannot_be_omitted_by_next_release(self):
        root,host,app,release=self.setup_release()
        (root/'tools').mkdir(); (root/'tools'/'backup-core.py').write_text('# existing\n')
        with self.assertRaisesRegex(r.ReleaseError,'project script'):
            app.deploy(release)
        self.assertEqual(host.events,[])
    def test_renamed_worker_is_inspected_and_preserved_across_next_release(self):
        root,host,app,release=self.setup_release()
        metadata=r.load(root/'current.json')
        metadata['grok_browser_container']='cfm-browser-worker'
        r.atomic_json(root/'current.json',metadata)
        original=host.inspect; inspected=[]
        def inspect(name):
            self.assertNotEqual(name,'gptgrok2api-grok-browser-1')
            inspected.append(name)
            return original(name)
        with patch.object(host,'inspect',side_effect=inspect):
            app.deploy(release)
        self.assertIn('cfm-browser-worker',inspected)
        self.assertEqual(r.load(root/'current.json')['grok_browser_container'],'cfm-browser-worker')
        self.assertFalse(any(name=='cfm-browser-worker' for name,action in host.events))
    def test_project_backup_installed_only_after_successful_handoff(self):
        root,host,app,release=self.setup_release()
        raw=b'print("fixture")\n'
        with patch.object(r,'backup_tool_bytes',return_value=raw):
            app.deploy(release)
        self.assertEqual((root/'tools'/'backup-core.py').read_bytes(),raw)
        self.assertEqual((root/'tools'/'backup-core.py').stat().st_mode & 0o777,0o700)
    def test_failed_handoff_preserves_old_backup_script(self):
        root,host,app,release=self.setup_release(fault='freeze')
        (root/'tools').mkdir(); target=root/'tools'/'backup-core.py'; target.write_bytes(b'# old\n')
        with patch.object(r,'backup_tool_bytes',return_value=b'# new\n'), self.assertRaises(r.ReleaseError):
            app.deploy(release)
        self.assertEqual(target.read_bytes(),b'# old\n')
    def test_backup_source_digest_mismatch_rejected(self):
        host=FakeHost()
        with self.assertRaisesRegex(r.ReleaseError,'fixed release commit'):
            r.backup_tool_bytes(host,{'backup_tool_sha256':'a'*64,'source':'/source','commit':'b'*40})
    def test_bootstrap_preserves_existing_ingress(self):
        root,host,app,release=self.setup_release(True); app.deploy(release,True)
        self.assertEqual(host.states[host.front]['phase'],'gateway')
        self.assertFalse(any(n==host.front and a=='stop' for n,a in host.events))
    def test_failure_before_seal_keeps_old_service(self):
        root,host,app,release=self.setup_release(fault='freeze')
        with self.assertRaises(r.ReleaseError): app.deploy(release)
        self.assertEqual(r.load(root/'rolling-status.json')['status'],'aborted')
        self.assertFalse(host.states[host.old]['paused']); self.assertFalse(host.route['held'])
        self.assertTrue(host.infos[host.old]['State']['Running'])
    def test_activation_failure_recreates_prior_program_with_current_data(self):
        root,host,app,release=self.setup_release(fault='activate'); (root/'data'/'business').write_text('latest')
        with self.assertRaises(r.ReleaseError): app.deploy(release)
        self.assertEqual(r.load(root/'rolling-status.json')['status'],'rolled_back')
        self.assertNotIn('backup', r.load(root/'current.json'))
        self.assertIn((host.old,'create:old:image-id'),host.events)
        self.assertEqual((root/'data'/'business').read_text(),'latest')
        self.assertFalse(host.route['held'])

    def test_recovery_keeps_previous_backup_pointer(self):
        root, host, app, release = self.setup_release(fault='activate')
        previous = str(root / 'backups' / 'previous-success')
        r.atomic_json(root / 'current.json', dict(r.load(root / 'current.json'), backup=previous))
        with self.assertRaises(r.ReleaseError):
            app.deploy(release)
        self.assertEqual(r.load(root / 'current.json')['backup'], previous)
        self.assertEqual(r.load(app.work / 'release.json')['status'], 'rolled_back')
    def test_lost_release_reply_never_stops_possible_new_writer(self):
        for fault in ('release_reply','unknown_release'):
            with self.subTest(fault=fault):
                root,host,app,release=self.setup_release(fault=fault)
                with self.assertRaises(r.ReleaseError): app.deploy(release)
                self.assertEqual(r.load(root/'rolling-status.json')['status'],'needs_attention')
                self.assertFalse(any(a=='stop' for n,a in host.events))
                self.assertTrue(host.states[r.SLOTS['green'][0]]['writer_lease'])
    def test_initialization_change_keeps_fence_and_evidence(self):
        root,host,app,release=self.setup_release()
        with patch.object(r,'business_files',side_effect=[{'a':'old'},{'a':'old'},{'a':'changed'}]):
            with self.assertRaises(r.ReleaseError): app.deploy(release)
        self.assertTrue(host.route['held']); self.assertFalse(any(a=='stop' for n,a in host.events))
        self.assertEqual(r.load(root/'rolling-status.json')['status'],'needs_attention')
    def test_failed_initializer_that_changes_data_refuses_old_program(self):
        root,host,app,release=self.setup_release(fault='activate')
        with patch.object(r,'business_files',side_effect=[{'a':'old'},{'a':'old'},{'a':'changed'}]):
            with self.assertRaises(r.ReleaseError): app.deploy(release)
        self.assertTrue(host.route['held'])
        self.assertEqual(r.load(root/'rolling-status.json')['status'],'needs_attention')
        self.assertNotIn((host.old,'create:old:image-id'),host.events)

    def test_after_release_archive_failure_preserves_active_traffic(self):
        root,host,app,release=self.setup_release()
        with patch.object(FakeSnapshot,'finish',side_effect=OSError('archive error')):
            with self.assertRaises(OSError): app.deploy(release)
        self.assertFalse(host.route['held']); self.assertEqual(r.load(root/'current.json')['slot'],'green')
        self.assertNotIn((r.SLOTS['green'][0],'stop'),host.events)


class StorageAndTopology(unittest.TestCase):
    def test_direct_snapshot_uses_non_blocking_pigz_stderr(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root/'data').mkdir()
            (root/'logs').mkdir()
            (root/'data'/'asset.bin').write_bytes(b'asset' * 4096)
            work = root/'backup'; work.mkdir(mode=0o700)

            class CompressorInput:
                def __init__(self, output):
                    self.stream = gzip.GzipFile(fileobj=output, mode='wb', compresslevel=1)
                def write(self, value):
                    return self.stream.write(value)
                def flush(self):
                    return self.stream.flush()
                def close(self):
                    return self.stream.close()

            class FakeCompressor:
                instance = None
                def __init__(self, args, **kwargs):
                    FakeCompressor.instance = self
                    self.args = args
                    self.kwargs = kwargs
                    self.stdin = CompressorInput(kwargs['stdout'])
                def poll(self):
                    return None
                def wait(self):
                    return 0
                def kill(self):
                    self.stdin.close()

            snapshot = r.Snapshot(root, work, r.Host(), direct=True)
            with patch.object(r.shutil, 'which', return_value='/fake/pigz'), \
                    patch.object(r.subprocess, 'Popen', FakeCompressor):
                snapshot.sync(frozen=True)
            self.assertIs(FakeCompressor.instance.kwargs['stderr'], r.subprocess.DEVNULL)
            self.assertTrue((work/'snapshot.tar.gz').is_file())
            with tarfile.open(work/'snapshot.tar.gz', 'r:gz') as archive:
                self.assertIn('snapshot/data/asset.bin', archive.getnames())

    def test_daily_media_excluded_from_archive_but_in_cutover_guard(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            policy = root / 'tools/backup-local/current'
            policy.mkdir(parents=True)
            r.atomic_json(policy / 'manifest.json', {'release_media_policy': 'daily'})
            (root / 'logs').mkdir()
            work = root / 'batch'
            work.mkdir()
            media = ['files/images/a.jpg', 'files/videos/b.mp4', 'log_input_images/c.jpeg']
            core = ['files/images/a.jpg.meta.json', 'accounts.json', 'files/unknown.jpg']
            for name in media + core:
                p = root / 'data' / name
                p.parent.mkdir(exist_ok=True, parents=True)
                p.write_bytes(name.encode())
            snap = r.Snapshot(root, work, r.Host(), direct=True)
            snap.sync(frozen=True)
            for name in ('postgres.dump', 'postgres-globals.sql', 'redis.rdb'):
                (work / name).write_bytes(b'database')
            snap.finish()
            self.assertIs(r.load(work / 'complete')['media_included'], False)
            with tarfile.open(work / 'snapshot.tar.gz') as archive:
                names = archive.getnames()
                for name in core:
                    self.assertIn('snapshot/data/' + name, names)
                for name in media:
                    self.assertNotIn('snapshot/data/' + name, names)
            self.assertEqual(snap.business_files(), r.business_files(root, r.Host()))
            (root / 'data' / media[0]).write_bytes(b'changed')
            self.assertNotEqual(snap.business_files(), r.business_files(root, r.Host()))

    def test_direct_snapshot_writes_single_archive_and_restores(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d).resolve()
            os.chmod(root, 0o710)
            (root/'data').mkdir(mode=0o750)
            (root/'logs').mkdir(mode=0o750)
            (root/'data'/'asset.bin').write_bytes(b'asset' * 4096)
            work = root/'backup'; work.mkdir(mode=0o700)
            snapshot = r.Snapshot(root, work, r.Host(), direct=True)
            snapshot.sync(frozen=True)
            for name in ('postgres.dump', 'postgres-globals.sql', 'redis.rdb'):
                (work/name).write_bytes(b'database fixture')
            snapshot.finish()
            self.assertFalse((work/'snapshot').exists())
            self.assertTrue((work/'snapshot.tar.gz').is_file())
            manifest = json.loads((work/'snapshot-metadata.json').read_text())
            integrity = importlib.util.spec_from_file_location(
                'snapshot_integrity_for_direct_test', Path(__file__).with_name('snapshot_integrity.py'))
            module = importlib.util.module_from_spec(integrity)
            integrity.loader.exec_module(module)
            restored = root/'restored'
            module.restore_archive(work/'snapshot.tar.gz', manifest, restored)
            self.assertEqual(module.capture_layout(restored).manifest, manifest)
            self.assertEqual(manifest['.']['mode'], 0o700)
            self.assertEqual(restored.stat().st_mode & 0o777, 0o700)

    def test_direct_snapshot_finish_honors_overall_deadline(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root/'data').mkdir()
            (root/'logs').mkdir()
            (root/'data'/'asset.bin').write_bytes(b'asset' * 4096)
            work = root/'backup'; work.mkdir(mode=0o700)
            host = r.Host()
            snapshot = r.Snapshot(root, work, host, direct=True)
            snapshot.sync(frozen=True)
            for name in ('postgres.dump', 'postgres-globals.sql', 'redis.rdb'):
                (work/name).write_bytes(b'database fixture')
            host.overall_deadline = r.time.monotonic() - 1
            with self.assertRaisesRegex(r.ReleaseError, 'deadline exhausted'):
                snapshot.finish()

    def test_direct_snapshot_removes_partial_archive_after_failure(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root/'data').mkdir()
            (root/'logs').mkdir()
            (root/'data'/'asset.bin').write_bytes(b'asset' * 4096)
            work = root/'backup'; work.mkdir(mode=0o700)
            snapshot = r.Snapshot(root, work, r.Host(), direct=True)
            snapshot.frozen_manifest, _ = snapshot._capture_manifest(lambda: None)
            checks = [0]
            def fail_after_start():
                checks[0] += 1
                if checks[0] > 4:
                    raise r.ReleaseError('injected archive interruption')
            with self.assertRaisesRegex(r.ReleaseError, 'injected archive interruption'):
                snapshot._write_direct_archive(snapshot.frozen_manifest, fail_after_start)
            self.assertFalse((work/'snapshot.tar.gz.writing').exists())

    def test_direct_snapshot_ignores_browser_singleton_links(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root/'data'/'browser-worker'/'adobe'/'profile'/'browser').mkdir(parents=True)
            (root/'logs').mkdir()
            browser = root/'data'/'browser-worker'/'adobe'/'profile'/'browser'
            for name in r.EPHEMERAL_BROWSER_LINKS:
                (browser/name).symlink_to('/tmp/browser-runtime')
            (browser/'BrowserMetrics').mkdir()
            (browser/'BrowserMetrics'/'metrics.log').write_bytes(b'disposable')
            profile_lock = root/'data'/'browser-worker'/'adobe'/'profile'/'.lock'
            profile_lock.write_bytes(b'runtime')
            work = root/'backup'; work.mkdir(mode=0o700)
            snapshot = r.Snapshot(root, work, r.Host(), direct=True)
            snapshot.sync(frozen=True)
            self.assertNotIn('data/browser-worker/adobe/profile/browser/SingletonLock', snapshot.frozen_manifest)
            self.assertNotIn('data/browser-worker/adobe/profile/browser/SingletonSocket', snapshot.frozen_manifest)
            self.assertNotIn('data/browser-worker/adobe/profile/browser/BrowserMetrics/metrics.log', snapshot.frozen_manifest)
            self.assertNotIn('data/browser-worker/adobe/profile/.lock', snapshot.frozen_manifest)

    def test_direct_snapshot_still_rejects_unknown_browser_symlink(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            browser = root/'data'/'browser-worker'/'adobe'/'profile'/'browser'
            browser.mkdir(parents=True)
            (root/'logs').mkdir()
            (browser/'UnexpectedLink').symlink_to('/tmp/browser-runtime')
            work = root/'backup'; work.mkdir(mode=0o700)
            snapshot = r.Snapshot(root, work, r.Host(), direct=True)
            with self.assertRaisesRegex(r.ReleaseError, 'symlinks or special files'):
                snapshot.sync(frozen=True)

    def test_database_backup_reserve_uses_largest_completed_dump_set(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            backup = root/'backups'/'rolling-one'; backup.mkdir(parents=True)
            sizes = (r.DATABASE_DUMP_RESERVE + 1, 3, 11)
            for name, size in zip(('postgres.dump', 'postgres-globals.sql', 'redis.rdb'), sizes):
                with (backup/name).open('wb') as stream:
                    stream.truncate(size)
            self.assertEqual(r.database_backup_reserve(root), sum(sizes))

    def test_snapshot_removes_unstable_online_copy_deleted_before_fence(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d); (root/'data').mkdir(); (root/'logs').mkdir(); work=root/'backup'; work.mkdir()
            source=root/'data'/'racing'; source.write_text('before')
            snap=r.Snapshot(root,work,r.Host()); copy=r.shutil.copy2
            def racing(src,dst):
                result=copy(src,dst)
                if Path(src)==source: source.write_text('different while copied')
                return result
            with patch.object(r.shutil,'copy2',side_effect=racing): snap.sync()
            source.unlink(); snap.sync(frozen=True)
            self.assertFalse((work/'snapshot'/'data'/'racing').exists())
    def test_candidate_has_no_public_ports_or_shared_ingress_aliases(self):
        config=r.slot_config(info('original'),'new:image','green',Path('/runtime/owned'),Path('/assets'))
        svc=config['services']['green']
        self.assertNotIn('ports',svc)
        self.assertEqual(svc['environment']['CFM_ROLLING_BOOTSTRAP_ACTIVE'],'false')
        aliases=[alias for value in svc['networks'].values() for alias in value.get('aliases',[])]
        self.assertEqual(aliases,['cfm-green'])
    def test_public_static_assets_remain_readable_with_private_umask(self):
        archive=io.BytesIO()
        with tarfile.open(fileobj=archive,mode='w') as out:
            member=tarfile.TarInfo('./nested/chunk.js');member.size=7
            out.addfile(member,io.BytesIO(b'fixture'))
        class AssetsHost:
            def run(self,*args,**kwargs):return archive.getvalue()
        with tempfile.TemporaryDirectory() as d:
            target=Path(d)/'assets';previous=os.umask(0o077)
            try:r.merge_assets(AssetsHost(),'fixture:image',target)
            finally:os.umask(previous)
            self.assertEqual(target.stat().st_mode & 0o777,0o755)
            self.assertEqual((target/'nested').stat().st_mode & 0o777,0o755)
            self.assertEqual((target/'nested'/'chunk.js').stat().st_mode & 0o777,0o644)

    def test_contract_changes_require_exact_review(self):
        with self.assertRaises(r.ReleaseError): r.check_contract({'a':1},{'a':2})
        r.check_contract({'a':1},{'a':2},{'previous':{'a':1},'candidate':{'a':2}})
    def test_legacy_guard_blocks_rolling_and_switching(self):
        with tempfile.TemporaryDirectory() as d:
            for status in ('healthy','switching'):
                r.atomic_json(Path(d)/'current.json',dict(release_strategy=r.STRATEGY,status=status))
                with self.assertRaises(r.ReleaseError): r.require_legacy(d)

if __name__=='__main__': unittest.main()
